package firecracker

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	pb "github.com/opensandbox/opensandbox/proto/agent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentClient is the host-side gRPC client that connects to the
// osb-agent running inside a Firecracker VM via vsock.
type AgentClient struct {
	conn   *grpc.ClientConn
	client pb.SandboxAgentClient
}

// NewAgentClient connects to the agent via the Firecracker vsock UDS.
// In --no-api mode, Firecracker creates a single UDS file (vsock.sock).
// To reach a guest port, the host connects to the UDS and sends
// "CONNECT <port>\n". Firecracker responds with "OK <buf_size>\n"
// and the connection is then relayed to the guest.
func NewAgentClient(vsockPath string) (*AgentClient, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx,
		// Use passthrough scheme — the custom dialer handles the actual connection
		"passthrough:///vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialVsock(ctx, vsockPath, 1024)
		}),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(64*1024*1024),
			grpc.MaxCallSendMsgSize(64*1024*1024),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to agent at %s: %w", vsockPath, err)
	}

	return &AgentClient{
		conn:   conn,
		client: pb.NewSandboxAgentClient(conn),
	}, nil
}

// dialVsock connects to a guest port via Firecracker's vsock UDS protocol.
// Protocol: connect to the UDS, send "CONNECT <port>\n", read "OK ...\n".
func dialVsock(ctx context.Context, vsockPath string, port int) (net.Conn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}

	d := net.Dialer{Deadline: deadline}
	conn, err := d.DialContext(ctx, "unix", vsockPath)
	if err != nil {
		return nil, fmt.Errorf("dial vsock UDS %s: %w", vsockPath, err)
	}

	// Send CONNECT command
	_ = conn.SetDeadline(deadline)
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send CONNECT %d: %w", port, err)
	}

	// Read response line (e.g., "OK 1073741824\n")
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read vsock response: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT failed: %s", line)
	}

	// Clear deadline — gRPC manages its own timeouts
	_ = conn.SetDeadline(time.Time{})

	// Wrap to handle the buffered reader (there may be buffered data from the OK line)
	return &vsockConn{Conn: conn, reader: reader}, nil
}

// vsockConn wraps a net.Conn with a bufio.Reader to handle buffered reads
// from the CONNECT handshake.
type vsockConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *vsockConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// Close closes the gRPC connection.
func (c *AgentClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
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

// SyncFS flushes all filesystem buffers inside the VM without exiting the agent.
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

// ConnectPTYData connects to the PTY data stream on the given vsock port.
// Returns a raw TCP-like connection for bidirectional PTY I/O.
func (c *AgentClient) ConnectPTYData(vsockPath string, dataPort uint32) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialVsock(ctx, vsockPath, int(dataPort))
	if err != nil {
		return nil, fmt.Errorf("connect to PTY data port %d: %w", dataPort, err)
	}
	return conn, nil
}
