package worker

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/grpctls"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/sparse"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// GRPCServer implements the SandboxWorker gRPC service for control plane communication.
// LiveMigrator is implemented by VM managers that support live migration (e.g. QEMU).
type LiveMigrator interface {
	PrepareIncomingMigration(ctx context.Context, sandboxID, rootfsPath, workspacePath string, cpus, memMB, guestPort int, template string) (incomingAddr string, hostPort int, err error)
	CompleteIncomingMigration(ctx context.Context, sandboxID string) error
	LiveMigrate(ctx context.Context, sandboxID, incomingAddr string) error
}

type GRPCServer struct {
	pb.UnimplementedSandboxWorkerServer
	manager            sandbox.Manager
	migrator           LiveMigrator // optional, set if manager supports live migration
	router             *sandbox.SandboxRouter
	ptyManager         *sandbox.PTYManager
	execSessionManager *sandbox.ExecSessionManager
	sandboxDBs         *sandbox.SandboxDBManager
	checkpointStore    *storage.CheckpointStore
	store              *db.Store // nil if no DB configured
	server             *grpc.Server
}

// NewGRPCServer creates a new gRPC server wrapping the sandbox manager.
// If OPENSANDBOX_GRPC_TLS_* env vars are set, the server uses mTLS.
func NewGRPCServer(mgr sandbox.Manager, ptyMgr *sandbox.PTYManager, execMgr *sandbox.ExecSessionManager, sandboxDBs *sandbox.SandboxDBManager, checkpointStore *storage.CheckpointStore, router *sandbox.SandboxRouter, builder interface{}, store *db.Store) *GRPCServer {
	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(256 * 1024 * 1024), // 256MB for large file transfers
		grpc.MaxSendMsgSize(256 * 1024 * 1024),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	}

	// Enable mTLS if configured
	if grpctls.Enabled() {
		creds, err := grpctls.ServerCredentials()
		if err != nil {
			log.Fatalf("grpc: failed to load TLS credentials: %v", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(creds))
		log.Println("grpc: mTLS enabled for worker gRPC server")
	}

	s := &GRPCServer{
		manager:            mgr,
		router:             router,
		ptyManager:         ptyMgr,
		execSessionManager: execMgr,
		sandboxDBs:         sandboxDBs,
		checkpointStore:    checkpointStore,
		store:              store,
		server:             grpc.NewServer(serverOpts...),
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

// parseSecretAllowedHosts converts the proto map (env var → comma-separated hosts)
// to the internal map (env var → host slice). Returns nil if input is empty.
func parseSecretAllowedHosts(m map[string]string) map[string][]string {
	if len(m) == 0 {
		return nil
	}
	result := make(map[string][]string, len(m))
	for name, hosts := range m {
		if hosts != "" {
			result[name] = strings.Split(hosts, ",")
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (s *GRPCServer) CreateSandbox(ctx context.Context, req *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	cfg := types.SandboxConfig{
		Template:           req.Template,
		Timeout:            int(req.Timeout),
		Envs:               req.Envs,
		MemoryMB:           int(req.MemoryMb),
		CpuCount:           int(req.CpuCount),
		NetworkEnabled:     req.NetworkEnabled,
		ImageRef:           req.ImageRef,
		Port:               int(req.Port),
		SandboxID:          req.SandboxId,    // use server-assigned ID if provided
		CheckpointID:       req.CheckpointId, // for per-template golden snapshots
		EgressAllowlist:    req.EgressAllowlist,
		SecretAllowedHosts: parseSecretAllowedHosts(req.SecretAllowedHosts),
	}

	// Warm fork: if checkpoint_id is set, fork from the local checkpoint cache.
	// ForkFromCheckpoint uses the local cache directly — no S3 needed.
	// Skip resolveTemplateDrives here: it tries S3 first which can hang on DNS
	// failures, and ForkFromCheckpoint already handles cache lookup internally.
	if req.CheckpointId != "" {
		sb, err := s.manager.ForkFromCheckpoint(ctx, req.CheckpointId, cfg)
		if err == nil {
			// Register with sandbox router for rolling timeout tracking
			if s.router != nil {
				timeout := cfg.Timeout
				if timeout <= 0 {
					timeout = 300
				}
				s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
			}
			return &pb.CreateSandboxResponse{
				SandboxId: sb.ID,
				Status:    string(sb.Status),
			}, nil
		}
		log.Printf("grpc: ForkFromCheckpoint %s failed: %v, falling back to standard Create", req.CheckpointId, err)
	}

	// Handle sandbox snapshot template: resolve S3 keys to local paths.
	if req.TemplateRootfsKey != "" && req.TemplateWorkspaceKey != "" {
		localRootfs, localWorkspace, err := s.resolveTemplateDrives(ctx, req.TemplateRootfsKey, req.TemplateWorkspaceKey)
		if err != nil {
			return nil, fmt.Errorf("resolve template drives: %w", err)
		}
		cfg.TemplateRootfsKey = "local://" + localRootfs
		cfg.TemplateWorkspaceKey = "local://" + localWorkspace
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

	// Record initial scale event for billing
	if s.store != nil {
		memMB := cfg.MemoryMB
		if memMB <= 0 {
			memMB = 1024 // default
		}
		cpuPct := (memMB * 100) / 1024
		if cpuPct < 100 {
			cpuPct = 100
		}
		orgID := "00000000-0000-0000-0000-000000000001" // TODO: resolve from session
		if err := s.store.RecordScaleEvent(ctx, sb.ID, orgID, memMB, cpuPct); err != nil {
			log.Printf("grpc: failed to record initial scale event for %s: %v", sb.ID, err)
		}
	}

	return &pb.CreateSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
	}, nil
}

func (s *GRPCServer) DestroySandbox(ctx context.Context, req *pb.DestroySandboxRequest) (*pb.DestroySandboxResponse, error) {
	// End billing scale event before destroying
	if s.store != nil {
		if err := s.store.EndScaleEvent(ctx, req.SandboxId); err != nil {
			log.Printf("grpc: failed to end scale event for %s: %v", req.SandboxId, err)
		}
	}

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

func (s *GRPCServer) ExecSessionCreate(ctx context.Context, req *pb.ExecSessionCreateRequest) (*pb.ExecSessionCreateResponse, error) {
	if s.execSessionManager == nil {
		return nil, fmt.Errorf("exec sessions not configured on this worker")
	}

	createReq := types.ExecSessionCreateRequest{
		Command:               req.Command,
		Args:                  req.Args,
		Env:                   req.Envs,
		Cwd:                   req.Cwd,
		Timeout:               int(req.TimeoutSeconds),
		MaxRunAfterDisconnect: int(req.MaxRunAfterDisconnect),
	}

	var session *sandbox.ExecSessionHandle

	routeOp := func(_ context.Context) error {
		var err error
		session, err = s.execSessionManager.CreateSession(req.SandboxId, createReq)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(ctx, req.SandboxId, "execSessionCreate", routeOp); err != nil {
			return nil, fmt.Errorf("exec session create failed: %w", err)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return nil, fmt.Errorf("exec session create failed: %w", err)
		}
	}

	return &pb.ExecSessionCreateResponse{SessionId: session.ID}, nil
}

func (s *GRPCServer) ExecSessionList(ctx context.Context, req *pb.ExecSessionListRequest) (*pb.ExecSessionListResponse, error) {
	if s.execSessionManager == nil {
		return nil, fmt.Errorf("exec sessions not configured on this worker")
	}

	sessions := s.execSessionManager.ListSessions(req.SandboxId)

	var entries []*pb.ExecSessionInfoEntry
	for _, si := range sessions {
		entry := &pb.ExecSessionInfoEntry{
			SessionId: si.SessionID,
			Command:   si.Command,
			Args:      si.Args,
			Running:   si.Running,
			StartedAt: 0,
		}
		if si.ExitCode != nil {
			entry.ExitCode = int32(*si.ExitCode)
		}
		entries = append(entries, entry)
	}

	return &pb.ExecSessionListResponse{Sessions: entries}, nil
}

func (s *GRPCServer) ExecSessionKill(ctx context.Context, req *pb.ExecSessionKillRequest) (*pb.ExecSessionKillResponse, error) {
	if s.execSessionManager == nil {
		return nil, fmt.Errorf("exec sessions not configured on this worker")
	}

	signal := int(req.Signal)
	if signal == 0 {
		signal = 9
	}

	routeOp := func(_ context.Context) error {
		return s.execSessionManager.KillSession(req.SessionId, signal)
	}

	if s.router != nil {
		if err := s.router.Route(ctx, req.SandboxId, "execSessionKill", routeOp); err != nil {
			return nil, fmt.Errorf("exec session kill failed: %w", err)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return nil, fmt.Errorf("exec session kill failed: %w", err)
		}
	}

	return &pb.ExecSessionKillResponse{}, nil
}

func (s *GRPCServer) HibernateSandbox(ctx context.Context, req *pb.HibernateSandboxRequest) (*pb.HibernateSandboxResponse, error) {
	if s.checkpointStore == nil {
		return nil, fmt.Errorf("hibernation not configured on this worker")
	}

	// End billing scale event (sandbox going to sleep)
	if s.store != nil {
		if err := s.store.EndScaleEvent(ctx, req.SandboxId); err != nil {
			log.Printf("grpc: failed to end scale event on hibernate for %s: %v", req.SandboxId, err)
		}
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
		CheckpointKey: result.HibernationKey,
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

	// Resume billing scale event after wake
	if s.store != nil {
		memMB := 1024 // TODO: get actual memory from sandbox state
		cpuPct := 100
		orgID := "00000000-0000-0000-0000-000000000001"
		if err := s.store.RecordScaleEvent(ctx, sb.ID, orgID, memMB, cpuPct); err != nil {
			log.Printf("grpc: failed to record scale event on wake for %s: %v", sb.ID, err)
		}
	}

	return &pb.WakeSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
	}, nil
}

func (s *GRPCServer) BuildTemplate(ctx context.Context, req *pb.BuildTemplateRequest) (*pb.BuildTemplateResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "deprecated")
}

func (s *GRPCServer) SaveAsTemplate(ctx context.Context, req *pb.SaveAsTemplateRequest) (*pb.SaveAsTemplateResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "deprecated")
}

func (s *GRPCServer) CreateCheckpoint(ctx context.Context, req *pb.CreateCheckpointRequest) (*pb.CreateCheckpointResponse, error) {
	if s.checkpointStore == nil {
		return nil, fmt.Errorf("checkpoint store not configured on this worker")
	}

	checkpointID := req.CheckpointId
	if _, err := uuid.Parse(checkpointID); err != nil {
		return nil, fmt.Errorf("invalid checkpoint ID: %w", err)
	}

	// The onReady callback fires after the async mem file move + S3 upload completes.
	// If prepare_golden is set, it also creates a golden snapshot from the cache.
	prepareGolden := req.PrepareGolden
	mgr := s.manager
	var onReady func()
	if s.store != nil {
		cpID, _ := uuid.Parse(checkpointID)
		onReady = func() {
			if err := s.store.SetCheckpointReady(context.Background(), cpID, "", "", 0); err != nil {
				log.Printf("grpc: CreateCheckpoint: failed to mark checkpoint %s ready: %v", checkpointID, err)
			} else {
				log.Printf("grpc: CreateCheckpoint: checkpoint %s is now ready", checkpointID)
			}
			// Create golden snapshot after mem file is in place
			if prepareGolden {
				type goldenPreparer interface {
					RegisterTemplateGoldenFromCache(checkpointID string)
				}
				if gp, ok := mgr.(goldenPreparer); ok {
					gp.RegisterTemplateGoldenFromCache(checkpointID)
				}
			}
		}
	} else if prepareGolden {
		onReady = func() {
			type goldenPreparer interface {
				RegisterTemplateGoldenFromCache(checkpointID string)
			}
			if gp, ok := mgr.(goldenPreparer); ok {
				gp.RegisterTemplateGoldenFromCache(checkpointID)
			}
		}
	}

	rootfsKey, workspaceKey, err := s.manager.CreateCheckpoint(ctx, req.SandboxId, checkpointID, s.checkpointStore, onReady)
	if err != nil {
		return nil, fmt.Errorf("create checkpoint failed: %w", err)
	}

	return &pb.CreateCheckpointResponse{
		RootfsS3Key:    rootfsKey,
		WorkspaceS3Key: workspaceKey,
	}, nil
}

// RestoreCheckpoint reverts a running sandbox to a checkpoint using QEMU's loadvm.
// The snapshot is already stored inside the qcow2 files — no S3 download needed.
func (s *GRPCServer) RestoreCheckpoint(ctx context.Context, req *pb.RestoreCheckpointRequest) (*pb.RestoreCheckpointResponse, error) {
	if err := s.manager.RestoreFromCheckpoint(ctx, req.SandboxId, req.CheckpointId); err != nil {
		return nil, fmt.Errorf("restore checkpoint: %w", err)
	}
	return &pb.RestoreCheckpointResponse{Success: true}, nil
}

// resolveTemplateDrives resolves S3 template/checkpoint keys to local file paths.
// Uses local cache when available (instant reflink), otherwise downloads from S3.
// Handles both template keys (templates/{id}/...) and checkpoint keys (checkpoints/{sandboxID}/{checkpointID}/...).
func (s *GRPCServer) resolveTemplateDrives(ctx context.Context, rootfsKey, workspaceKey string) (localRootfs, localWorkspace string, err error) {
	// Try checkpoint key format first: checkpoints/{sandboxID}/{checkpointID}/rootfs.tar.zst
	if checkpointID := extractCheckpointID(rootfsKey); checkpointID != "" {
		// Fast path: check local checkpoint cache
		cachedRootfs := s.manager.CheckpointCachePath(checkpointID, "rootfs.ext4")
		cachedWorkspace := s.manager.CheckpointCachePath(checkpointID, "workspace.ext4")
		if cachedRootfs != "" && cachedWorkspace != "" {
			log.Printf("grpc: create from checkpoint %s: using local cache", checkpointID)
			return cachedRootfs, cachedWorkspace, nil
		}

		// Slow path: download from S3 and cache locally
		log.Printf("grpc: create from checkpoint %s: downloading from S3 (rootfs=%s, workspace=%s)", checkpointID, rootfsKey, workspaceKey)
		return s.downloadAndCacheCheckpointDrives(ctx, checkpointID, rootfsKey, workspaceKey)
	}

	// Template key format: templates/{id}/rootfs.tar.zst
	templateID := extractTemplateID(rootfsKey)
	if templateID == "" {
		return "", "", fmt.Errorf("cannot extract template/checkpoint ID from key: %s", rootfsKey)
	}

	// Fast path: check local template cache
	cachedRootfs := s.manager.TemplateCachePath(templateID, "rootfs.ext4")
	cachedWorkspace := s.manager.TemplateCachePath(templateID, "workspace.ext4")
	if cachedRootfs != "" && cachedWorkspace != "" {
		log.Printf("grpc: create from template %s: using local cache", templateID)
		return cachedRootfs, cachedWorkspace, nil
	}

	// Slow path: download from S3 and cache locally
	log.Printf("grpc: create from template %s: downloading from S3 (rootfs=%s, workspace=%s)", templateID, rootfsKey, workspaceKey)
	return s.downloadAndCacheTemplateDrives(ctx, templateID, rootfsKey, workspaceKey)
}

// extractTemplateID extracts the template ID from an S3 key like "templates/{id}/rootfs.tar.zst".
func extractTemplateID(s3Key string) string {
	parts := strings.Split(s3Key, "/")
	if len(parts) >= 2 && parts[0] == "templates" {
		return parts[1]
	}
	return ""
}

// extractCheckpointID extracts the checkpoint ID from an S3 key like "checkpoints/{sandboxID}/{checkpointID}/rootfs.tar.zst".
func extractCheckpointID(s3Key string) string {
	parts := strings.Split(s3Key, "/")
	if len(parts) >= 3 && parts[0] == "checkpoints" {
		return parts[2] // parts[1] is sandboxID, parts[2] is checkpointID
	}
	return ""
}

// downloadAndCacheTemplateDrives downloads template archives from S3, extracts them,
// and caches the ext4 drives locally for future use.
func (s *GRPCServer) downloadAndCacheTemplateDrives(ctx context.Context, templateID, rootfsKey, workspaceKey string) (string, string, error) {
	if s.checkpointStore == nil {
		return "", "", fmt.Errorf("checkpoint store not configured")
	}

	cacheDir := filepath.Join(s.manager.DataDir(), "templates", templateID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", fmt.Errorf("create template cache dir: %w", err)
	}

	cachedRootfs := filepath.Join(cacheDir, "rootfs.ext4")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.ext4")

	// Download and extract rootfs (tar.zst)
	if err := downloadAndExtract(ctx, s.checkpointStore, rootfsKey, cacheDir, extractArchiveCmd); err != nil {
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("download rootfs: %w", err)
	}

	// Download and extract workspace (sparse.zst)
	if err := downloadAndExtract(ctx, s.checkpointStore, workspaceKey, cachedWorkspace, extractSparseCmd); err != nil {
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("download workspace: %w", err)
	}

	log.Printf("grpc: template %s: cached locally at %s", templateID, cacheDir)
	return cachedRootfs, cachedWorkspace, nil
}

// downloadAndCacheCheckpointDrives downloads checkpoint archives from S3, extracts them,
// and caches the ext4 drives locally for future use.
func (s *GRPCServer) downloadAndCacheCheckpointDrives(ctx context.Context, checkpointID, rootfsKey, workspaceKey string) (string, string, error) {
	if s.checkpointStore == nil {
		return "", "", fmt.Errorf("checkpoint store not configured")
	}

	cacheDir := filepath.Join(s.manager.DataDir(), "checkpoints", checkpointID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", fmt.Errorf("create checkpoint cache dir: %w", err)
	}

	cachedRootfs := filepath.Join(cacheDir, "rootfs.ext4")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.ext4")

	// Download and extract rootfs (tar.zst)
	if err := downloadAndExtract(ctx, s.checkpointStore, rootfsKey, cacheDir, extractArchiveCmd); err != nil {
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("download rootfs: %w", err)
	}

	// Download and extract workspace (sparse.zst)
	if err := downloadAndExtract(ctx, s.checkpointStore, workspaceKey, cachedWorkspace, extractSparseCmd); err != nil {
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("download workspace: %w", err)
	}

	log.Printf("grpc: checkpoint %s: cached locally at %s", checkpointID, cacheDir)
	return cachedRootfs, cachedWorkspace, nil
}

// extractFunc defines how to extract a downloaded archive to a destination path.
type extractFunc func(archivePath, destPath string) error

// extractArchiveCmd extracts a tar.zst archive to a directory.
func extractArchiveCmd(archivePath, destDir string) error {
	cmd := exec.Command("tar", "--zstd", "-xf", archivePath, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// extractSparseCmd extracts a sparse.zst archive to a file using the sparse restore format.
func extractSparseCmd(archivePath, destPath string) error {
	return sparse.Restore(archivePath, destPath)
}

// downloadAndExtract downloads an S3 object to a temp file, extracts it, and removes the temp file.
func downloadAndExtract(ctx context.Context, store *storage.CheckpointStore, s3Key, dest string, extract extractFunc) error {
	data, err := store.Download(ctx, s3Key)
	if err != nil {
		return fmt.Errorf("download %s: %w", s3Key, err)
	}

	tmpFile, err := os.CreateTemp("", "osb-template-*")
	if err != nil {
		data.Close()
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.ReadFrom(data); err != nil {
		tmpFile.Close()
		data.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	tmpFile.Close()
	data.Close()

	return extract(tmpPath, dest)
}

// SetSandboxLimits adjusts resource limits (memory, CPU, PIDs) on a running sandbox.
// Memory increases trigger virtio-mem hotplug; decreases adjust cgroup limits only.
func (s *GRPCServer) SetSandboxLimits(ctx context.Context, req *pb.SetSandboxLimitsRequest) (*pb.SetSandboxLimitsResponse, error) {
	if err := s.manager.SetResourceLimits(ctx, req.SandboxId, req.MaxPids, req.MaxMemoryBytes, req.CpuMaxUsec, req.CpuPeriodUsec); err != nil {
		return nil, fmt.Errorf("set resource limits: %w", err)
	}

	// Record scale event for billing
	if s.store != nil && req.MaxMemoryBytes > 0 {
		memMB := int(req.MaxMemoryBytes / (1024 * 1024))
		cpuPct := int(req.CpuMaxUsec / 1000) // 100000us → 100%
		orgID := "00000000-0000-0000-0000-000000000001" // TODO: resolve from sandbox session
		if err := s.store.RecordScaleEvent(ctx, req.SandboxId, orgID, memMB, cpuPct); err != nil {
			log.Printf("grpc: failed to record scale event for %s: %v", req.SandboxId, err)
		}
	}

	return &pb.SetSandboxLimitsResponse{}, nil
}

// SetMigrator sets the live migration handler (call after NewGRPCServer if the manager supports it).
func (s *GRPCServer) SetMigrator(m LiveMigrator) {
	s.migrator = m
}

func (s *GRPCServer) PrepareMigrationIncoming(ctx context.Context, req *pb.PrepareMigrationIncomingRequest) (*pb.PrepareMigrationIncomingResponse, error) {
	if s.migrator == nil {
		return nil, fmt.Errorf("live migration not supported on this worker")
	}
	addr, hostPort, err := s.migrator.PrepareIncomingMigration(ctx,
		req.SandboxId, req.RootfsPath, req.WorkspacePath,
		int(req.CpuCount), int(req.MemoryMb), int(req.GuestPort), req.Template)
	if err != nil {
		return nil, fmt.Errorf("prepare incoming migration: %w", err)
	}
	return &pb.PrepareMigrationIncomingResponse{
		IncomingAddr: addr,
		HostPort:     int32(hostPort),
	}, nil
}

func (s *GRPCServer) LiveMigrate(ctx context.Context, req *pb.LiveMigrateRequest) (*pb.LiveMigrateResponse, error) {
	if s.migrator == nil {
		return nil, fmt.Errorf("live migration not supported on this worker")
	}
	if err := s.migrator.LiveMigrate(ctx, req.SandboxId, req.IncomingAddr); err != nil {
		return nil, fmt.Errorf("live migrate: %w", err)
	}
	return &pb.LiveMigrateResponse{}, nil
}

func (s *GRPCServer) CompleteMigrationIncoming(ctx context.Context, req *pb.CompleteMigrationIncomingRequest) (*pb.CompleteMigrationIncomingResponse, error) {
	if s.migrator == nil {
		return nil, fmt.Errorf("live migration not supported on this worker")
	}
	if err := s.migrator.CompleteIncomingMigration(ctx, req.SandboxId); err != nil {
		return nil, fmt.Errorf("complete incoming migration: %w", err)
	}
	return &pb.CompleteMigrationIncomingResponse{}, nil
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
