package qemu

import (
	"context"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

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
			grpc.MaxCallRecvMsgSize(64*1024*1024),
			grpc.MaxCallSendMsgSize(64*1024*1024),
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

// SyncFS flushes all filesystem buffers inside the VM.
func (c *AgentClient) SyncFS(ctx context.Context) error {
	_, err := c.client.SyncFS(ctx, &pb.SyncFSRequest{})
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

// NewAgentClientTCP connects to the agent via TCP using the guest IP.
// Used by QEMU backend — TCP over virtio-net survives QEMU migration.
func NewAgentClientTCP(guestIP string) (*AgentClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	addr := fmt.Sprintf("%s:%d", guestIP, 1024)
	conn, err := grpc.DialContext(ctx,
		addr,
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
			grpc.MaxCallRecvMsgSize(64*1024*1024),
			grpc.MaxCallSendMsgSize(64*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to agent at %s: %w", addr, err)
	}

	return &AgentClient{
		conn:   conn,
		client: pb.NewSandboxAgentClient(conn),
	}, nil
}

// ConnectPTYDataTCP connects to a PTY data port via TCP.
func ConnectPTYDataTCP(guestIP string, dataPort uint32) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%d", guestIP, dataPort)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to PTY data %s: %w", addr, err)
	}
	return conn, nil
}
