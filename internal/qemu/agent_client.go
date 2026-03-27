package qemu

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

const streamChunkSize = 256 * 1024 // 256KB per gRPC chunk

// AgentClient is the host-side gRPC client that connects to the
// osb-agent running inside a QEMU VM via AF_VSOCK.
type AgentClient struct {
	conn   *grpc.ClientConn
	client pb.SandboxAgentClient
}

// NewAgentClient connects to the agent via AF_VSOCK using the guest CID.
// QEMU uses the kernel's vhost-vsock directly — no UDS file needed.
// Port 1024 is the agent gRPC server inside the VM.
func NewAgentClient(guestCID uint32) (*AgentClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx,
		"passthrough:///vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  10 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   1 * time.Second,
			},
		}),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialVsock(ctx, guestCID, 1024)
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to agent at CID %d: %w", guestCID, err)
	}

	return &AgentClient{
		conn:   conn,
		client: pb.NewSandboxAgentClient(conn),
	}, nil
}

// dialVsock connects to a guest port via AF_VSOCK (kernel vhost-vsock).
func dialVsock(ctx context.Context, cid uint32, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("create AF_VSOCK socket: %w", err)
	}

	sa := &unix.SockaddrVM{
		CID:  cid,
		Port: port,
	}

	// Use a goroutine for connect with context cancellation
	connectDone := make(chan error, 1)
	go func() {
		connectDone <- unix.Connect(fd, sa)
	}()

	select {
	case err := <-connectDone:
		if err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("connect AF_VSOCK CID=%d port=%d: %w", cid, port, err)
		}
	case <-ctx.Done():
		unix.Close(fd)
		return nil, ctx.Err()
	}

	// Wrap the raw fd in a net.Conn
	file := fdToConn(fd)
	return file, nil
}

// fdToConn wraps a raw file descriptor into a net.Conn.
func fdToConn(fd int) net.Conn {
	// AF_VSOCK sockets are not supported by net.FileConn, so use
	// the raw syscall-based implementation directly.
	return &rawVsockConn{fd: fd}
}

// rawVsockConn is a fallback net.Conn implementation using raw syscalls.
type rawVsockConn struct {
	fd int
}

func (c *rawVsockConn) Read(b []byte) (int, error) {
	n, err := unix.Read(c.fd, b)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (c *rawVsockConn) Write(b []byte) (int, error) {
	return unix.Write(c.fd, b)
}

func (c *rawVsockConn) Close() error {
	return unix.Close(c.fd)
}

func (c *rawVsockConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (c *rawVsockConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (c *rawVsockConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *rawVsockConn) SetReadDeadline(t time.Time) error {
	return setSockTimeout(c.fd, unix.SO_RCVTIMEO, t)
}

func (c *rawVsockConn) SetWriteDeadline(t time.Time) error {
	return setSockTimeout(c.fd, unix.SO_SNDTIMEO, t)
}

// setSockTimeout sets SO_RCVTIMEO or SO_SNDTIMEO on a raw fd.
// A zero time clears the timeout (blocks indefinitely).
func setSockTimeout(fd int, opt int, t time.Time) error {
	var tv unix.Timeval
	if !t.IsZero() {
		d := time.Until(t)
		if d <= 0 {
			d = 1 * time.Microsecond // already expired — set minimal timeout
		}
		tv = unix.NsecToTimeval(int64(d))
	}
	// Zero tv clears the timeout
	return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, opt, &tv)
}

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }

// Close closes the gRPC connection.
func (c *AgentClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// SetEnvs sends sandbox-level environment variables to the in-VM agent.
func (c *AgentClient) SetEnvs(ctx context.Context, envs map[string]string) error {
	_, err := c.client.SetEnvs(ctx, &pb.SetEnvsRequest{Envs: envs})
	return err
}

// Ping verifies the agent is responsive.
func (c *AgentClient) Ping(ctx context.Context) (*pb.PingResponse, error) {
	return c.client.Ping(ctx, &pb.PingRequest{})
}

// Exec runs a command in the VM.
func (c *AgentClient) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	return c.client.Exec(ctx, req)
}

// ReadFile reads a file from the VM.
func (c *AgentClient) ReadFile(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.client.ReadFile(ctx, &pb.ReadFileRequest{Path: path})
	if err != nil {
		return nil, err
	}
	return resp.Content, nil
}

// WriteFile writes a file in the VM.
func (c *AgentClient) WriteFile(ctx context.Context, path string, content []byte) error {
	_, err := c.client.WriteFile(ctx, &pb.WriteFileRequest{Path: path, Content: content})
	return err
}

// ListDir lists a directory in the VM.
func (c *AgentClient) ListDir(ctx context.Context, path string) ([]*pb.DirEntry, error) {
	resp, err := c.client.ListDir(ctx, &pb.ListDirRequest{Path: path})
	if err != nil {
		return nil, err
	}
	return resp.Entries, nil
}

// MakeDir creates a directory in the VM.
func (c *AgentClient) MakeDir(ctx context.Context, path string) error {
	_, err := c.client.MakeDir(ctx, &pb.MakeDirRequest{Path: path})
	return err
}

// Remove removes a file/directory in the VM.
func (c *AgentClient) Remove(ctx context.Context, path string) error {
	_, err := c.client.Remove(ctx, &pb.RemoveRequest{Path: path})
	return err
}

// Exists checks if a path exists in the VM.
func (c *AgentClient) Exists(ctx context.Context, path string) (bool, error) {
	resp, err := c.client.Exists(ctx, &pb.ExistsRequest{Path: path})
	if err != nil {
		return false, err
	}
	return resp.Exists, nil
}

// Stat returns file metadata from the VM.
func (c *AgentClient) Stat(ctx context.Context, path string) (*pb.StatResponse, error) {
	return c.client.Stat(ctx, &pb.StatRequest{Path: path})
}

// Stats returns resource usage from the VM.
func (c *AgentClient) Stats(ctx context.Context) (*pb.StatsResponse, error) {
	return c.client.Stats(ctx, &pb.StatsRequest{})
}

// Shutdown gracefully stops the agent.
func (c *AgentClient) Shutdown(ctx context.Context) error {
	_, err := c.client.Shutdown(ctx, &pb.ShutdownRequest{})
	return err
}

// SetResourceLimits adjusts sandbox cgroup limits at runtime.
func (c *AgentClient) SetResourceLimits(ctx context.Context, maxPids int32, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec int64) error {
	_, err := c.client.SetResourceLimits(ctx, &pb.SetResourceLimitsRequest{
		MaxPids:        maxPids,
		MaxMemoryBytes: maxMemoryBytes,
		CpuMaxUsec:     cpuMaxUsec,
		CpuPeriodUsec:  cpuPeriodUsec,
	})
	return err
}

// SyncFS flushes all filesystem buffers inside the VM.
func (c *AgentClient) SyncFS(ctx context.Context) error {
	_, err := c.client.SyncFS(ctx, &pb.SyncFSRequest{})
	return err
}

// WriteFileBinary writes binary content to a path inside the VM.
func (c *AgentClient) WriteFileBinary(ctx context.Context, path string, content []byte, mode uint32) error {
	_, err := c.client.WriteFile(ctx, &pb.WriteFileRequest{
		Path:    path,
		Content: content,
		Mode:    mode,
	})
	return err
}

// GetVersion returns the agent's build version.
func (c *AgentClient) GetVersion(ctx context.Context) (string, error) {
	resp, err := c.client.GetVersion(ctx, &pb.GetVersionRequest{})
	if err != nil {
		return "", err
	}
	return resp.Version, nil
}

// Upgrade tells the agent to replace its binary and re-exec.
// The new binary must already be written to binaryPath inside the VM.
func (c *AgentClient) Upgrade(ctx context.Context, binaryPath string) error {
	_, err := c.client.Upgrade(ctx, &pb.UpgradeRequest{BinaryPath: binaryPath})
	return err
}

// PTYCreate creates a new PTY session in the VM.
func (c *AgentClient) PTYCreate(ctx context.Context, cols, rows int32, shell string) (sessionID string, dataPort uint32, err error) {
	resp, err := c.client.PTYCreate(ctx, &pb.PTYCreateRequest{
		Cols:  cols,
		Rows:  rows,
		Shell: shell,
	})
	if err != nil {
		return "", 0, err
	}
	return resp.SessionId, resp.DataPort, nil
}

// PTYResize resizes a PTY session.
func (c *AgentClient) PTYResize(ctx context.Context, sessionID string, cols, rows int32) error {
	_, err := c.client.PTYResize(ctx, &pb.PTYResizeRequest{
		SessionId: sessionID,
		Cols:      cols,
		Rows:      rows,
	})
	return err
}

// PTYKill terminates a PTY session.
func (c *AgentClient) PTYKill(ctx context.Context, sessionID string) error {
	_, err := c.client.PTYKill(ctx, &pb.PTYKillRequest{SessionId: sessionID})
	return err
}

// ExecSessionCreate creates a new exec session in the VM.
func (c *AgentClient) ExecSessionCreate(ctx context.Context, req *pb.ExecSessionCreateRequest) (string, error) {
	resp, err := c.client.ExecSessionCreate(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.SessionId, nil
}

// ExecSessionAttach connects to an exec session's bidi stream.
func (c *AgentClient) ExecSessionAttach(ctx context.Context) (pb.SandboxAgent_ExecSessionAttachClient, error) {
	return c.client.ExecSessionAttach(ctx)
}

// ExecSessionList lists all exec sessions in the VM.
func (c *AgentClient) ExecSessionList(ctx context.Context) ([]*pb.ExecSessionInfo, error) {
	resp, err := c.client.ExecSessionList(ctx, &pb.ExecSessionListRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// ExecSessionKill kills an exec session in the VM.
func (c *AgentClient) ExecSessionKill(ctx context.Context, sessionID string, signal int32) error {
	_, err := c.client.ExecSessionKill(ctx, &pb.ExecSessionKillRequest{
		SessionId: sessionID,
		Signal:    signal,
	})
	return err
}

// PTYAttach opens a bidirectional gRPC stream for PTY I/O.
func (c *AgentClient) PTYAttach(ctx context.Context) (pb.SandboxAgent_PTYAttachClient, error) {
	return c.client.PTYAttach(ctx)
}

// ReadFileStream opens a server-streaming gRPC call and returns an io.ReadCloser
// that yields the file contents in 256KB chunks, plus the total file size.
func (c *AgentClient) ReadFileStream(ctx context.Context, path string) (io.ReadCloser, int64, error) {
	stream, err := c.client.ReadFileStream(ctx, &pb.ReadFileStreamRequest{Path: path})
	if err != nil {
		return nil, 0, err
	}

	pr, pw := io.Pipe()
	var totalSize int64

	// Read first chunk to get total_size
	first, err := stream.Recv()
	if err != nil {
		pw.Close()
		pr.Close()
		return nil, 0, fmt.Errorf("recv first chunk: %w", err)
	}
	totalSize = first.TotalSize

	go func() {
		// Write first chunk data
		if _, err := pw.Write(first.Data); err != nil {
			pw.CloseWithError(err)
			return
		}
		// Read remaining chunks
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				pw.Close()
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := pw.Write(chunk.Data); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()

	return pr, totalSize, nil
}

// WriteFileStream sends a file in 256KB chunks via client-streaming gRPC.
// Returns the total bytes written.
func (c *AgentClient) WriteFileStream(ctx context.Context, path string, mode uint32, r io.Reader) (int64, error) {
	stream, err := c.client.WriteFileStream(ctx)
	if err != nil {
		return 0, err
	}

	// Always send the first message with path + mode, even for empty files.
	// Without this, an empty body sends no gRPC messages and the agent gets EOF.
	buf := make([]byte, streamChunkSize)
	n, readErr := r.Read(buf)
	firstMsg := &pb.WriteFileStreamRequest{
		Path: path,
		Mode: mode,
		Data: buf[:n],
	}
	if sendErr := stream.Send(firstMsg); sendErr != nil {
		return 0, fmt.Errorf("send first chunk: %w", sendErr)
	}

	if readErr != nil && readErr != io.EOF {
		return 0, fmt.Errorf("read source: %w", readErr)
	}

	// Send remaining chunks
	if readErr != io.EOF {
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&pb.WriteFileStreamRequest{Data: buf[:n]}); sendErr != nil {
					return 0, fmt.Errorf("send chunk: %w", sendErr)
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, fmt.Errorf("read source: %w", err)
			}
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return 0, err
	}
	return resp.BytesWritten, nil
}

// ConnectPTYData connects to the PTY data stream on the given vsock port.
// Uses AF_VSOCK directly instead of Firecracker's UDS protocol.
func (c *AgentClient) ConnectPTYData(guestCID uint32, dataPort uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialVsock(ctx, guestCID, dataPort)
	if err != nil {
		return nil, fmt.Errorf("connect to PTY data port %d: %w", dataPort, err)
	}
	return conn, nil
}

// NewAgentClientSocket connects to the agent via a Unix socket (virtio-serial chardev).
// Used by QEMU backend — virtio-serial survives QEMU live migration.
func NewAgentClientSocket(socketPath string) (*AgentClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx,
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  10 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   1 * time.Second,
			},
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024),
			grpc.MaxCallSendMsgSize(256*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to agent at %s: %w", socketPath, err)
	}

	return &AgentClient{
		conn:   conn,
		client: pb.NewSandboxAgentClient(conn),
	}, nil
}
