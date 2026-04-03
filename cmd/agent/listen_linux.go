package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/agent"
	"golang.org/x/sys/unix"
)

// listenVsock creates the main agent listener.
// Tries virtio-serial first (QEMU), then vsock (Firecracker), then Unix socket (testing).
func listenVsock() (net.Listener, error) {
	// Try virtio-serial first — survives QEMU live migration.
	// The device path depends on the virtio device index (vportNpM where N is
	// the virtio-serial controller index). Check common paths.
	for _, path := range []string{"/dev/virtio-ports/agent", "/dev/vport0p1", "/dev/vport1p1", "/dev/vport2p1"} {
		if _, err := os.Stat(path); err == nil {
			lis, err := listenVirtioSerial(path)
			if err == nil {
				log.Printf("agent: listening on virtio-serial %s", path)
				return lis, nil
			}
			log.Printf("agent: virtio-serial open %s failed: %v", path, err)
		}
	}

	// Fall back to vsock (Firecracker path)
	lis, err := listenVsockPort(agent.DefaultGRPCPort)
	if err == nil {
		hasVsock = true
		return lis, nil
	}

	// Fall back to Unix domain socket (testing)
	return listenUnix(err)
}

// hasVsock tracks whether we're inside a VM with AF_VSOCK support.
var hasVsock bool

// listenPortForPTY returns a listener for PTY data ports.
// For virtio-serial, PTY data uses vsock if available, otherwise Unix sockets.
func listenPortForPTY(port uint32) (net.Listener, error) {
	if hasVsock {
		return listenVsockPort(port)
	}
	// Fallback: Unix socket
	sockPath := fmt.Sprintf("/tmp/pty-%d.sock", port)
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("pty unix listen port %d: %w", port, err)
	}
	log.Printf("agent: PTY data listening on %s", sockPath)
	return lis, nil
}

// --- virtio-serial listener ---

// virtioSerialListener wraps a virtio-serial device as a net.Listener.
// The device is a bidirectional byte stream — one "connection" at a time.
// When gRPC's Serve() loop calls Accept() after a connection drops (e.g.,
// after golden snapshot restore), we return a new conn wrapping the same fd.
type virtioSerialListener struct {
	f      *os.File
	once   sync.Once
	closed chan struct{}
	mu     sync.Mutex
	active bool // true when a connection is being served
}

func listenVirtioSerial(path string) (net.Listener, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open virtio-serial %s: %w", path, err)
	}

	// Mark CLOEXEC so the fd is closed on agent re-exec (upgrade).
	// Without this, syscall.Exec inherits the fd, the host never sees the
	// connection drop, and the new agent can't get a clean Accept.
	unix.CloseOnExec(int(f.Fd()))

	return &virtioSerialListener{
		f:      f,
		closed: make(chan struct{}),
	}, nil
}

func (l *virtioSerialListener) Accept() (net.Conn, error) {
	// Wait until no connection is active and the port is readable.
	// After loadvm/checkpoint restore, the virtio-serial port may be
	// "disconnected" until the host connects to the chardev Unix socket.
	// We poll for readability to avoid returning a conn that immediately errors.
	for {
		select {
		case <-l.closed:
			return nil, net.ErrClosed
		default:
		}

		l.mu.Lock()
		if l.active {
			l.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		l.mu.Unlock()

		// Drain any stale data from the previous gRPC session.
		// After a gRPC connection drops, the serial port may have leftover
		// bytes (GOAWAY frames, partial HTTP/2 frames). If we hand these to
		// the new gRPC session, it gets a protocol error and closes immediately.
		drainStaleData(l.f)

		// Wait for the host to connect (port becomes readable with fresh data)
		if !waitForReadable(l.f, 500*time.Millisecond) {
			continue
		}

		// Re-check and set active atomically under a single lock hold
		l.mu.Lock()
		if l.active {
			l.mu.Unlock()
			continue
		}
		l.active = true
		l.mu.Unlock()
		log.Printf("agent: virtio-serial Accept: port readable, accepting connection")
		return &virtioSerialConn{
			f: l.f,
			onClose: func() {
				l.mu.Lock()
				l.active = false
				l.mu.Unlock()
			},
		}, nil
	}
}

// drainStaleData reads and discards any pending bytes on the serial port.
// This clears leftover gRPC frames from a previous session so the next
// gRPC handshake starts clean.
func drainStaleData(f *os.File) {
	buf := make([]byte, 4096)
	for {
		// Non-blocking read: poll with 0 timeout
		fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0) // immediate, non-blocking
		if err != nil || n <= 0 {
			return // no data pending
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			return
		}
		// Read and discard
		nr, readErr := f.Read(buf)
		if nr > 0 {
			log.Printf("agent: virtio-serial: drained %d stale bytes", nr)
		}
		if readErr != nil || nr == 0 {
			return
		}
	}
}

// waitForReadable polls a file for readability using poll(2).
// Returns true if the fd becomes readable within the timeout.
func waitForReadable(f *os.File, timeout time.Duration) bool {
	fd := int32(f.Fd())
	timeoutMs := int(timeout.Milliseconds())
	fds := []unix.PollFd{{Fd: fd, Events: unix.POLLIN}}
	n, err := unix.Poll(fds, timeoutMs)
	if err != nil || n <= 0 {
		return false
	}
	return fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
}

// PrepareHibernate resets the active flag so that after migration restore,
// the Accept loop will poll for a new host-side connection instead of
// thinking the old connection is still active.
func (l *virtioSerialListener) PrepareHibernate() {
	l.mu.Lock()
	l.active = false
	l.mu.Unlock()
	log.Printf("agent: virtio-serial: prepared for hibernate (active=false)")
}

func (l *virtioSerialListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.f.Close()
}

func (l *virtioSerialListener) Addr() net.Addr {
	return virtioSerialAddr(l.f.Name())
}

type virtioSerialAddr string

func (a virtioSerialAddr) Network() string { return "virtio-serial" }
func (a virtioSerialAddr) String() string  { return string(a) }

// virtioSerialConn wraps an os.File as a net.Conn for gRPC.
// onClose is called when gRPC drops the connection, signaling the listener
// to accept a new one (e.g., after golden snapshot restore).
type virtioSerialConn struct {
	f       *os.File
	onClose func()
	once    sync.Once
}

func (c *virtioSerialConn) Read(b []byte) (int, error)  { return c.f.Read(b) }
func (c *virtioSerialConn) Write(b []byte) (int, error) { return c.f.Write(b) }
func (c *virtioSerialConn) Close() error {
	c.once.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
	// Don't close the file — the listener still owns it for future connections
	return nil
}
func (c *virtioSerialConn) LocalAddr() net.Addr                { return virtioSerialAddr("local") }
func (c *virtioSerialConn) RemoteAddr() net.Addr               { return virtioSerialAddr("remote") }
func (c *virtioSerialConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *virtioSerialConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *virtioSerialConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

// --- vsock support (Firecracker backward compat) ---

type vsockListener struct {
	fd     int
	port   uint32
	once   sync.Once
	closed chan struct{}
}

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock(%d:%d)", a.cid, a.port) }

func (l *vsockListener) Accept() (net.Conn, error) {
	for {
		nfd, _, err := unix.Accept(l.fd)
		if err != nil {
			select {
			case <-l.closed:
				return nil, net.ErrClosed
			default:
			}
			return nil, fmt.Errorf("vsock accept: %w", err)
		}
		return newRawVsockConn(nfd), nil
	}
}

func (l *vsockListener) Close() error {
	var err error
	l.once.Do(func() {
		close(l.closed)
		err = unix.Close(l.fd)
	})
	return err
}

func (l *vsockListener) Addr() net.Addr {
	return vsockAddr{cid: unix.VMADDR_CID_ANY, port: l.port}
}

func listenVsockPort(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{
		CID:  unix.VMADDR_CID_ANY,
		Port: port,
	}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen port %d: %w", port, err)
	}
	log.Printf("agent: listening on vsock port %d", port)
	return &vsockListener{fd: fd, closed: make(chan struct{}), port: port}, nil
}

func listenUnix(vsockErr error) (net.Listener, error) {
	sockPath := "/tmp/osb-agent.sock"
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("unix listen: %w (vsock: %v)", err, vsockErr)
	}
	log.Printf("agent: listening on %s (vsock not available: %v)", sockPath, vsockErr)
	return lis, nil
}

// rawVsockConn wraps a raw vsock fd as a net.Conn when net.FileConn fails.
type rawVsockConn struct {
	file *os.File
}

func newRawVsockConn(fd int) *rawVsockConn {
	return &rawVsockConn{file: os.NewFile(uintptr(fd), "vsock-raw")}
}

func (c *rawVsockConn) Read(b []byte) (int, error)         { return c.file.Read(b) }
func (c *rawVsockConn) Write(b []byte) (int, error)        { return c.file.Write(b) }
func (c *rawVsockConn) Close() error                       { return c.file.Close() }
func (c *rawVsockConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *rawVsockConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *rawVsockConn) SetDeadline(_ time.Time) error      { return nil }
func (c *rawVsockConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *rawVsockConn) SetWriteDeadline(_ time.Time) error { return nil }
