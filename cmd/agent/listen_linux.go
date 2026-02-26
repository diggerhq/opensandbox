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

// vsockListener implements net.Listener for AF_VSOCK sockets.
// Go's net.FileListener doesn't support AF_VSOCK, so we must wrap the raw fd.
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
		// Wrap the accepted fd directly as a raw vsock connection.
		// net.FileConn doesn't support AF_VSOCK, so we use our own wrapper.
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

// listenPortForPTY returns a ListenPortFunc suitable for PTY data ports.
// Inside Firecracker it uses native AF_VSOCK; outside, it falls back to Unix sockets.
func listenPortForPTY(port uint32) (net.Listener, error) {
	if hasVsock {
		return listenVsockPort(port)
	}
	// Fallback: Unix socket (for testing outside Firecracker)
	sockPath := fmt.Sprintf("/tmp/pty-%d.sock", port)
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("pty unix listen port %d: %w", port, err)
	}
	log.Printf("agent: PTY data listening on %s (vsock not available)", sockPath)
	return lis, nil
}

// hasVsock tracks whether we're inside a Firecracker VM with AF_VSOCK support.
var hasVsock bool

// listenVsock creates a vsock listener on port 1024.
func listenVsock() (net.Listener, error) {
	lis, err := listenVsockPort(agent.DefaultGRPCPort)
	if err == nil {
		hasVsock = true
		return lis, nil
	}

	// Fallback to Unix domain socket (testing outside Firecracker)
	return listenUnix(err)
}

// listenVsockPort creates a vsock listener on the given port.
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

func (c *rawVsockConn) Read(b []byte) (int, error)  { return c.file.Read(b) }
func (c *rawVsockConn) Write(b []byte) (int, error) { return c.file.Write(b) }
func (c *rawVsockConn) Close() error                { return c.file.Close() }

func (c *rawVsockConn) LocalAddr() net.Addr                     { return vsockAddr{} }
func (c *rawVsockConn) RemoteAddr() net.Addr                    { return vsockAddr{} }
func (c *rawVsockConn) SetDeadline(_ time.Time) error           { return nil }
func (c *rawVsockConn) SetReadDeadline(_ time.Time) error       { return nil }
func (c *rawVsockConn) SetWriteDeadline(_ time.Time) error      { return nil }
