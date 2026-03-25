// Package agent implements the in-VM sandbox agent that runs inside each
// Firecracker microVM. It serves gRPC over vsock and handles exec, file ops,
// PTY, and stats collection. This binary is statically compiled and injected
// into the VM rootfs image.
package agent

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	pb "github.com/opensandbox/opensandbox/proto/agent"
	"google.golang.org/grpc"
)

const (
	// DefaultGRPCPort is the vsock port the agent listens on for gRPC.
	DefaultGRPCPort = 1024
	// PTYDataPortBase is the base vsock port for PTY data streams.
	// Each PTY session gets PTYDataPortBase + N.
	PTYDataPortBase = 2000
)

// ListenPortFunc creates a net.Listener for the given port.
// The implementation is platform-specific (vsock inside Firecracker,
// Unix socket for testing outside Firecracker).
type ListenPortFunc func(port uint32) (net.Listener, error)

// Server is the gRPC agent server that runs inside a Firecracker VM.
type Server struct {
	pb.UnimplementedSandboxAgentServer

	startTime time.Time
	version   string

	// ListenPort creates a listener for PTY data ports.
	// Set by the main package based on the platform (vsock or unix).
	ListenPort ListenPortFunc

	// Sandbox-level environment variables (set once via SetEnvs RPC).
	// Injected into every Exec/ExecStream between baseEnv and per-command envs.
	envMu       sync.RWMutex
	sandboxEnvs []string

	// Exec sessions
	execMu       sync.RWMutex
	execSessions map[string]*execSession

	// PTY sessions
	ptyMu       sync.Mutex
	ptySessions map[string]*ptySession
	nextPTYPort uint32

	// gRPC server reference for hibernate GracefulStop
	mu         sync.Mutex
	grpcServer *grpc.Server
}

// NewServer creates a new agent server.
func NewServer(version string) *Server {
	return &Server{
		startTime:    time.Now(),
		version:      version,
		execSessions: make(map[string]*execSession),
		ptySessions:  make(map[string]*ptySession),
		nextPTYPort:  PTYDataPortBase,
	}
}

// Serve starts the gRPC server on the given listener.
// It stores the gRPC server reference so GracefulStop can be called externally
// (e.g., on SIGUSR1 to prepare for hibernate).
func (s *Server) Serve(lis net.Listener) error {
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(64 * 1024 * 1024), // 64MB for file transfers
		grpc.MaxSendMsgSize(64 * 1024 * 1024),
	)
	pb.RegisterSandboxAgentServer(grpcServer, s)

	s.mu.Lock()
	s.grpcServer = grpcServer
	s.mu.Unlock()

	log.Printf("agent: gRPC server listening")
	return grpcServer.Serve(lis)
}

// GracefulStop stops the gRPC server gracefully, allowing in-flight RPCs to complete.
// This causes Serve() to return, so the caller can re-enter Serve() for hibernate/wake.
func (s *Server) GracefulStop() {
	s.mu.Lock()
	gs := s.grpcServer
	s.mu.Unlock()
	if gs != nil {
		gs.GracefulStop()
	}
}

// GetVersion returns the agent's build version.
func (s *Server) GetVersion(ctx context.Context, req *pb.GetVersionRequest) (*pb.GetVersionResponse, error) {
	return &pb.GetVersionResponse{Version: s.version}, nil
}

// Upgrade replaces the agent binary on disk and re-execs the process.
// The new binary must already be written to req.BinaryPath by the worker.
// syscall.Exec replaces the process in-place — same PID 1, same cgroup,
// user processes are unaffected. The gRPC connection drops and the worker reconnects.
func (s *Server) Upgrade(ctx context.Context, req *pb.UpgradeRequest) (*pb.UpgradeResponse, error) {
	src := req.BinaryPath
	dst := "/usr/local/bin/osb-agent"

	// Validate the new binary exists and is executable
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("new binary not found at %s: %w", src, err)
	}
	if info.Size() < 1024 {
		return nil, fmt.Errorf("new binary too small (%d bytes), likely corrupt", info.Size())
	}

	// Replace the running binary. Linux allows unlinking a running executable
	// (the kernel keeps the inode alive until the process exits), so we remove
	// first, then rename the new binary into place.
	os.Remove(dst) // unlink running binary (safe on Linux)
	if err := os.Rename(src, dst); err != nil {
		// Rename fails across filesystems — fall back to copy
		data, readErr := os.ReadFile(src)
		if readErr != nil {
			return nil, fmt.Errorf("read new binary: %w", readErr)
		}
		if writeErr := os.WriteFile(dst, data, 0755); writeErr != nil {
			return nil, fmt.Errorf("write new binary: %w", writeErr)
		}
		os.Remove(src)
	}
	os.Chmod(dst, 0755)

	log.Printf("agent: upgrade scheduled (%s → %s)", src, dst)

	// Schedule re-exec after we return the response
	go func() {
		time.Sleep(200 * time.Millisecond) // let gRPC response flush
		log.Printf("agent: re-execing %s", dst)
		syscall.Exec(dst, os.Args, os.Environ())
		// If Exec fails, we're still running the old binary — log and continue
		log.Printf("agent: WARNING: exec failed, continuing with old binary")
	}()

	return &pb.UpgradeResponse{Ok: true}, nil
}
