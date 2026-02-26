// Package agent implements the in-VM sandbox agent that runs inside each
// Firecracker microVM. It serves gRPC over vsock and handles exec, file ops,
// PTY, and stats collection. This binary is statically compiled and injected
// into the VM rootfs image.
package agent

import (
	"log"
	"net"
	"sync"
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

	// PTY sessions
	ptyMu       sync.Mutex
	ptySessions map[string]*ptySession
	nextPTYPort uint32
}

// NewServer creates a new agent server.
func NewServer(version string) *Server {
	return &Server{
		startTime:   time.Now(),
		version:     version,
		ptySessions: make(map[string]*ptySession),
		nextPTYPort: PTYDataPortBase,
	}
}

// Serve starts the gRPC server on the given listener.
func (s *Server) Serve(lis net.Listener) error {
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(64 * 1024 * 1024), // 64MB for file transfers
		grpc.MaxSendMsgSize(64 * 1024 * 1024),
	)
	pb.RegisterSandboxAgentServer(grpcServer, s)

	log.Printf("agent: gRPC server listening")
	return grpcServer.Serve(lis)
}
