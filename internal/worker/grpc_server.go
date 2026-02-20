package worker

import (
	"context"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/internal/template"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// GRPCServer implements the SandboxWorker gRPC service for control plane communication.
type GRPCServer struct {
	pb.UnimplementedSandboxWorkerServer
	manager         *sandbox.Manager
	router          *sandbox.SandboxRouter
	ptyManager      *sandbox.PTYManager
	sandboxDBs      *sandbox.SandboxDBManager
	checkpointStore *storage.CheckpointStore
	builder         *template.Builder
	server          *grpc.Server
}

// NewGRPCServer creates a new gRPC server wrapping the sandbox manager.
func NewGRPCServer(mgr *sandbox.Manager, ptyMgr *sandbox.PTYManager, sandboxDBs *sandbox.SandboxDBManager, checkpointStore *storage.CheckpointStore, router *sandbox.SandboxRouter, builder *template.Builder) *GRPCServer {
	s := &GRPCServer{
		manager:         mgr,
		router:          router,
		ptyManager:      ptyMgr,
		sandboxDBs:      sandboxDBs,
		checkpointStore: checkpointStore,
		builder:         builder,
		server: grpc.NewServer(
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             5 * time.Second,
				PermitWithoutStream: true,
			}),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				Time:    30 * time.Second,
				Timeout: 10 * time.Second,
			}),
		),
	}
	pb.RegisterSandboxWorkerServer(s.server, s)
	return s
}

// Start starts the gRPC server on the given address.
func (s *GRPCServer) Start(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	return s.server.Serve(lis)
}

// Stop gracefully stops the gRPC server.
func (s *GRPCServer) Stop() {
	s.server.GracefulStop()
}

func (s *GRPCServer) CreateSandbox(ctx context.Context, req *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	cfg := types.SandboxConfig{
		Template:       req.Template,
		Timeout:        int(req.Timeout),
		Envs:           req.Envs,
		MemoryMB:       int(req.MemoryMb),
		CpuCount:       int(req.CpuCount),
		NetworkEnabled: req.NetworkEnabled,
		ImageRef:       req.ImageRef,
	}

	sb, err := s.manager.Create(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create sandbox: %w", err)
	}

	// Register with sandbox router for rolling timeout tracking
	if s.router != nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 300
		}
		s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
	}

	// Initialize per-sandbox SQLite
	if s.sandboxDBs != nil {
		sdb, err := s.sandboxDBs.Get(sb.ID)
		if err == nil {
			_ = sdb.LogEvent("created", map[string]string{
				"sandbox_id": sb.ID,
				"template":   cfg.Template,
			})
		}
	}

	return &pb.CreateSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
	}, nil
}

func (s *GRPCServer) DestroySandbox(ctx context.Context, req *pb.DestroySandboxRequest) (*pb.DestroySandboxResponse, error) {
	if err := s.manager.Kill(ctx, req.SandboxId); err != nil {
		return nil, fmt.Errorf("failed to destroy sandbox: %w", err)
	}

	// Unregister from sandbox router
	if s.router != nil {
		s.router.Unregister(req.SandboxId)
	}

	// Clean up SQLite
	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(req.SandboxId)
	}

	return &pb.DestroySandboxResponse{}, nil
}

func (s *GRPCServer) GetSandbox(ctx context.Context, req *pb.GetSandboxRequest) (*pb.GetSandboxResponse, error) {
	sb, err := s.manager.Get(ctx, req.SandboxId)
	if err != nil {
		return nil, fmt.Errorf("sandbox not found: %w", err)
	}

	return &pb.GetSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
		Template:  sb.Template,
		StartedAt: sb.StartedAt.Unix(),
		EndAt:     sb.EndAt.Unix(),
	}, nil
}

func (s *GRPCServer) ListSandboxes(ctx context.Context, _ *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	sandboxes, err := s.manager.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}

	var results []*pb.GetSandboxResponse
	for _, sb := range sandboxes {
		results = append(results, &pb.GetSandboxResponse{
			SandboxId: sb.ID,
			Status:    string(sb.Status),
			Template:  sb.Template,
			StartedAt: sb.StartedAt.Unix(),
			EndAt:     sb.EndAt.Unix(),
		})
	}

	return &pb.ListSandboxesResponse{Sandboxes: results}, nil
}

func (s *GRPCServer) ExecCommand(ctx context.Context, req *pb.ExecCommandRequest) (*pb.ExecCommandResponse, error) {
	cfg := types.ProcessConfig{
		Command: req.Command,
		Args:    req.Args,
		Env:     req.Envs,
		Cwd:     req.Cwd,
		Timeout: int(req.Timeout),
	}

	var result *types.ProcessResult

	routeOp := func(ctx context.Context) error {
		var err error
		result, err = s.manager.Exec(ctx, req.SandboxId, cfg)
		return err
	}

	// Route through sandbox router (handles auto-wake, rolling timeout reset)
	if s.router != nil {
		if err := s.router.Route(ctx, req.SandboxId, "exec", routeOp); err != nil {
			return nil, fmt.Errorf("exec failed: %w", err)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return nil, fmt.Errorf("exec failed: %w", err)
		}
	}

	return &pb.ExecCommandResponse{
		ExitCode: int32(result.ExitCode),
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

func (s *GRPCServer) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	var content string

	routeOp := func(ctx context.Context) error {
		var err error
		content, err = s.manager.ReadFile(ctx, req.SandboxId, req.Path)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(ctx, req.SandboxId, "readFile", routeOp); err != nil {
			return nil, fmt.Errorf("read file failed: %w", err)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return nil, fmt.Errorf("read file failed: %w", err)
		}
	}

	return &pb.ReadFileResponse{Content: []byte(content)}, nil
}

func (s *GRPCServer) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	routeOp := func(ctx context.Context) error {
		return s.manager.WriteFile(ctx, req.SandboxId, req.Path, string(req.Content))
	}

	if s.router != nil {
		if err := s.router.Route(ctx, req.SandboxId, "writeFile", routeOp); err != nil {
			return nil, fmt.Errorf("write file failed: %w", err)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return nil, fmt.Errorf("write file failed: %w", err)
		}
	}

	return &pb.WriteFileResponse{}, nil
}

func (s *GRPCServer) ListDir(ctx context.Context, req *pb.ListDirRequest) (*pb.ListDirResponse, error) {
	var entries []types.EntryInfo

	routeOp := func(ctx context.Context) error {
		var err error
		entries, err = s.manager.ListDir(ctx, req.SandboxId, req.Path)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(ctx, req.SandboxId, "listDir", routeOp); err != nil {
			return nil, fmt.Errorf("list dir failed: %w", err)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return nil, fmt.Errorf("list dir failed: %w", err)
		}
	}

	var pbEntries []*pb.DirEntry
	for _, e := range entries {
		pbEntries = append(pbEntries, &pb.DirEntry{
			Name:  e.Name,
			IsDir: e.IsDir,
			Size:  e.Size,
			Path:  e.Path,
		})
	}

	return &pb.ListDirResponse{Entries: pbEntries}, nil
}

// ExecCommandStream and PTY streaming RPCs are not needed since
// SDKs connect directly to the worker HTTP/WS server.
// Stubbed out to satisfy the interface.

func (s *GRPCServer) ExecCommandStream(_ *pb.ExecCommandRequest, _ pb.SandboxWorker_ExecCommandStreamServer) error {
	return fmt.Errorf("streaming exec not implemented, use HTTP API directly")
}

func (s *GRPCServer) CreatePTY(ctx context.Context, req *pb.CreatePTYRequest) (*pb.CreatePTYResponse, error) {
	ptyReq := types.PTYCreateRequest{
		Cols:  int(req.Cols),
		Rows:  int(req.Rows),
		Shell: req.Shell,
	}

	session, err := s.ptyManager.CreateSession(req.SandboxId, ptyReq)
	if err != nil {
		return nil, fmt.Errorf("create PTY failed: %w", err)
	}

	return &pb.CreatePTYResponse{SessionId: session.ID}, nil
}

func (s *GRPCServer) PTYStream(_ pb.SandboxWorker_PTYStreamServer) error {
	return fmt.Errorf("PTY streaming not implemented via gRPC, use WebSocket API directly")
}

func (s *GRPCServer) HibernateSandbox(ctx context.Context, req *pb.HibernateSandboxRequest) (*pb.HibernateSandboxResponse, error) {
	if s.checkpointStore == nil {
		return nil, fmt.Errorf("hibernation not configured on this worker")
	}

	result, err := s.manager.Hibernate(ctx, req.SandboxId, s.checkpointStore)
	if err != nil {
		return nil, fmt.Errorf("failed to hibernate sandbox: %w", err)
	}

	// Mark hibernated in sandbox router
	if s.router != nil {
		s.router.MarkHibernated(req.SandboxId, 600*time.Second)
	}

	// Clean up per-sandbox SQLite
	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(req.SandboxId)
	}

	return &pb.HibernateSandboxResponse{
		SandboxId:     result.SandboxID,
		CheckpointKey: result.CheckpointKey,
		SizeBytes:     result.SizeBytes,
	}, nil
}

func (s *GRPCServer) WakeSandbox(ctx context.Context, req *pb.WakeSandboxRequest) (*pb.WakeSandboxResponse, error) {
	if s.checkpointStore == nil {
		return nil, fmt.Errorf("hibernation not configured on this worker")
	}

	sb, err := s.manager.Wake(ctx, req.SandboxId, req.CheckpointKey, s.checkpointStore, int(req.Timeout))
	if err != nil {
		return nil, fmt.Errorf("failed to wake sandbox: %w", err)
	}

	// Register with sandbox router after explicit wake
	if s.router != nil {
		timeout := int(req.Timeout)
		if timeout <= 0 {
			timeout = 300
		}
		s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
	}

	// Re-initialize per-sandbox SQLite
	if s.sandboxDBs != nil {
		sdb, err := s.sandboxDBs.Get(sb.ID)
		if err == nil {
			_ = sdb.LogEvent("woke", map[string]string{
				"sandbox_id": sb.ID,
			})
		}
	}

	return &pb.WakeSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
	}, nil
}

func (s *GRPCServer) BuildTemplate(ctx context.Context, req *pb.BuildTemplateRequest) (*pb.BuildTemplateResponse, error) {
	if s.builder == nil {
		return nil, fmt.Errorf("template builder not configured on this worker")
	}

	imageRef, buildLog, err := s.builder.Build(ctx, req.Dockerfile, req.Name, req.Tag, req.EcrImageRef)
	if err != nil {
		return nil, fmt.Errorf("template build failed: %w", err)
	}

	return &pb.BuildTemplateResponse{
		ImageRef: imageRef,
		BuildLog: buildLog,
	}, nil
}

func (s *GRPCServer) GetSandboxStats(ctx context.Context, req *pb.GetSandboxStatsRequest) (*pb.GetSandboxStatsResponse, error) {
	stats, err := s.manager.Stats(ctx, req.SandboxId)
	if err != nil {
		return nil, fmt.Errorf("failed to get sandbox stats: %w", err)
	}

	return &pb.GetSandboxStatsResponse{
		CpuPercent: stats.CPUPercent,
		MemUsage:   stats.MemUsage,
		MemLimit:   stats.MemLimit,
		NetInput:   stats.NetInput,
		NetOutput:  stats.NetOutput,
		Pids:       int32(stats.PIDs),
	}, nil
}
