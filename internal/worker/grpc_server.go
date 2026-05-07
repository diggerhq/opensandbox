package worker

import (
	"context"
	"fmt"
	"io"
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
	"github.com/opensandbox/opensandbox/internal/observability"
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
	PrepareIncomingMigrationWithS3(ctx context.Context, sandboxID, rootfsS3Key, workspaceS3Key string, cpus, memMB, guestPort int, template string, checkpointStore *storage.CheckpointStore, overlayMode bool, sourceGoldenVersion string, secrets sandbox.MigrationSecrets) (incomingAddr string, hostPort int, err error)
	PreCopyDrives(ctx context.Context, sandboxID string, checkpointStore *storage.CheckpointStore) (rootfsKey, workspaceKey, goldenVersion string, baseCPU, baseMem, actualMem int, secrets sandbox.MigrationSecrets, err error)
	CompleteIncomingMigration(ctx context.Context, sandboxID string) error
	LiveMigrate(ctx context.Context, sandboxID, incomingAddr string) error
}

// CapacityChecker is implemented by VM managers that can report memory capacity.
// HostUsedMemoryMB reflects actual RAM pressure on the host (MemTotal−MemAvailable)
// and is the basis for migration admission control. TotalCommittedMemoryMB is kept
// for observability but is NOT used to gate scheduling any longer — committed
// over-reserves for idle sandboxes with large maxmem ceilings, forcing the
// cluster to over-provision workers for the same real workload.
type CapacityChecker interface {
	TotalCommittedMemoryMB() int
	HostMemoryMB() int
	HostUsedMemoryMB() int
}

// GoldenRebuilder is implemented by VM managers that support golden snapshot rebuild.
type GoldenRebuilder interface {
	RebuildGoldenSnapshot() (oldVersion, newVersion string, err error)
	GoldenVersion() string
}

// LogshipConfigurator is implemented by VM managers that can deliver
// log-shipping configuration to the in-VM agent over the existing
// worker→agent gRPC channel. Managers without an agent channel just
// don't implement this — the worker silently skips configuration for
// those sandboxes.
type LogshipConfigurator interface {
	ConfigureLogship(ctx context.Context, sandboxID, ingestToken, dataset, orgID string) error
}

type GRPCServer struct {
	pb.UnimplementedSandboxWorkerServer
	manager            sandbox.Manager
	migrator           LiveMigrator      // optional, set if manager supports live migration
	goldenRebuilder    GoldenRebuilder   // optional, set if manager supports golden rebuild
	router             *sandbox.SandboxRouter
	ptyManager         *sandbox.PTYManager
	execSessionManager *sandbox.ExecSessionManager
	sandboxDBs         *sandbox.SandboxDBManager
	checkpointStore    *storage.CheckpointStore
	store              *db.Store // nil if no DB configured
	server             *grpc.Server

	// Axiom log-shipping config. Empty token = log shipping disabled
	// (kill-switch). Set via SetAxiomConfig at startup.
	axiomIngestToken string
	axiomDataset     string
}

// SetAxiomConfig wires Axiom log-shipping credentials. The worker
// passes them down to each sandbox's agent on create via the
// LogshipConfigurator interface (implemented by the QEMU manager).
// Empty token disables shipping for every sandbox this worker boots.
func (s *GRPCServer) SetAxiomConfig(ingestToken, dataset string) {
	s.axiomIngestToken = ingestToken
	s.axiomDataset = dataset
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
		grpc.UnaryInterceptor(observability.UnaryServerInterceptor()),
		grpc.StreamInterceptor(observability.StreamServerInterceptor()),
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

// configureLogshipForSandbox delivers Axiom log-shipping configuration
// to the in-VM agent. Best-effort: a failure here does not fail the
// sandbox-create — log shipping just stays dormant for this sandbox.
// No-op if the worker has no Axiom token set or the manager doesn't
// implement LogshipConfigurator.
func (s *GRPCServer) configureLogshipForSandbox(ctx context.Context, sandboxID string) {
	if s.axiomIngestToken == "" {
		log.Printf("grpc: ConfigureLogship skipped for %s: no axiom ingest token set on worker", sandboxID)
		return
	}
	cfger, ok := s.manager.(LogshipConfigurator)
	if !ok {
		log.Printf("grpc: ConfigureLogship skipped for %s: manager %T does not implement LogshipConfigurator", sandboxID, s.manager)
		return
	}
	var orgID string
	if s.store != nil {
		orgID, _ = s.store.GetSandboxOrgID(ctx, sandboxID)
	}
	if err := cfger.ConfigureLogship(ctx, sandboxID, s.axiomIngestToken, s.axiomDataset, orgID); err != nil {
		log.Printf("grpc: ConfigureLogship for %s failed: %v (logs disabled for this sandbox)", sandboxID, err)
		return
	}
	log.Printf("grpc: ConfigureLogship sent for %s (org=%s, dataset=%s)", sandboxID, orgID, s.axiomDataset)
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
		NetworkEnabled:     &req.NetworkEnabled,
		ImageRef:           req.ImageRef,
		Port:               int(req.Port),
		SandboxID:          req.SandboxId,    // use server-assigned ID if provided
		CheckpointID:       req.CheckpointId, // for per-template golden snapshots
		EgressAllowlist:    req.EgressAllowlist,
		SecretAllowedHosts: parseSecretAllowedHosts(req.SecretAllowedHosts),
		SecretEnvs:         req.SecretEnvs,
		DiskMB:             int(req.DiskMb),
	}

	// Warm fork: if checkpoint_id is set, fork from the local checkpoint cache.
	// ForkFromCheckpoint uses the local cache directly — no S3 needed.
	if req.CheckpointId != "" {
		sb, err := s.manager.ForkFromCheckpoint(ctx, req.CheckpointId, cfg)
		if err == nil {
			if s.router != nil {
				// timeout == 0 means "persistent" (no auto-hibernate).
				timeout := cfg.Timeout
				if timeout < 0 {
					timeout = 0
				}
				s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
			}
			s.recordInitialScaleEvent(ctx, sb.ID, cfg)
			s.configureLogshipForSandbox(ctx, sb.ID)
			return &pb.CreateSandboxResponse{
				SandboxId: sb.ID,
				Status:    string(sb.Status),
			}, nil
		}
		// Cache miss path: try to recover by downloading the checkpoint from
		// S3, then retry the fork. The archive at TemplateRootfsKey holds
		// drives + memory dump + metadata — everything ForkFromCheckpoint
		// needs.
		notInCache := strings.Contains(err.Error(), "not found in cache")
		if !notInCache {
			// Some other error (rebase failure, agent reconnect, etc.).
			// Don't try to mask it — return the real reason.
			return nil, fmt.Errorf("fork from checkpoint %s: %w", req.CheckpointId, err)
		}
		if req.TemplateRootfsKey == "" || s.checkpointStore == nil {
			// We can't recover this fork — the controlplane gave us a
			// checkpoint id but no S3 key, and we don't have it cached
			// locally. Pre-fix this fell through to plain Create() at the
			// bottom of this function, silently producing an empty sandbox
			// from the base golden — exactly the "only lost+found"
			// symptom Oliviero hit on a checkpoint whose DB row had
			// empty rootfs_s3_key. Fail loud instead so the customer (and
			// us) sees an actionable error rather than a corrupt sandbox.
			return nil, fmt.Errorf("fork from checkpoint %s: not in local cache and no S3 key to recover from (DB row may be missing rootfs_s3_key)", req.CheckpointId)
		}
		log.Printf("grpc: warm fork %s: not in local cache, downloading from S3", req.CheckpointId)
		if dlErr := s.downloadFullCheckpoint(ctx, req.CheckpointId, req.TemplateRootfsKey); dlErr != nil {
			return nil, fmt.Errorf("fork from checkpoint %s: cache miss + S3 download failed: %w", req.CheckpointId, dlErr)
		}
		sb, retryErr := s.manager.ForkFromCheckpoint(ctx, req.CheckpointId, cfg)
		if retryErr != nil {
			return nil, fmt.Errorf("fork from checkpoint %s: retry after S3 download failed: %w", req.CheckpointId, retryErr)
		}
		if s.router != nil {
			timeout := cfg.Timeout
			if timeout < 0 {
				timeout = 0
			}
			s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
		}
		s.recordInitialScaleEvent(ctx, sb.ID, cfg)
		s.configureLogshipForSandbox(ctx, sb.ID)
		return &pb.CreateSandboxResponse{
			SandboxId: sb.ID,
			Status:    string(sb.Status),
		}, nil
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

	// Register with sandbox router for rolling timeout tracking.
	// timeout == 0 means "persistent" (no auto-hibernate).
	if s.router != nil {
		timeout := cfg.Timeout
		if timeout < 0 {
			timeout = 0
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

	s.recordInitialScaleEvent(ctx, sb.ID, cfg)

	s.configureLogshipForSandbox(ctx, sb.ID)

	return &pb.CreateSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
	}, nil
}

// recordInitialScaleEvent writes a sandbox_scale_events row marking the start
// of billable usage for a freshly-created sandbox. Called from every successful
// CreateSandbox return path — fork from checkpoint (local + S3-fallback) and
// from-scratch — so billing accounting works for forks too. Without this on
// the fork paths, sandbox_scale_events stays empty for the org, the
// usage-reporter excludes it from ListFreeOrgIDsWithOpenUsage / ListBillableOrgIDs,
// and no credits/usage are deducted or reported to Stripe.
//
// Best-effort: never returns an error. Defaults mirror the worker's own
// fallbacks for unset CPU/memory/disk so downstream pricing math is consistent.
func (s *GRPCServer) recordInitialScaleEvent(ctx context.Context, sandboxID string, cfg types.SandboxConfig) {
	if s.store == nil {
		return
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = 1024
	}
	cpuPct := (memMB * 100) / 1024
	if cpuPct < 100 {
		cpuPct = 100
	}
	diskMB := cfg.DiskMB
	if diskMB <= 0 {
		diskMB = 20480
	}
	orgID, _ := s.store.GetSandboxOrgID(ctx, sandboxID)
	if orgID == "" {
		return
	}
	if err := s.store.RecordScaleEvent(ctx, sandboxID, orgID, memMB, cpuPct, diskMB); err != nil {
		log.Printf("grpc: failed to record initial scale event for %s: %v", sandboxID, err)
	}
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

	// Register with sandbox router after explicit wake.
	// timeout == 0 means "persistent" (no auto-hibernate).
	if s.router != nil {
		timeout := int(req.Timeout)
		if timeout < 0 {
			timeout = 0
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

	// Resume billing scale event after wake. Disk size is preserved across wake —
	// pass 0 so RecordScaleEvent inherits disk_mb from the prior event.
	if s.store != nil {
		memMB := 1024 // TODO: get actual memory from sandbox state
		cpuPct := 100
		orgID, _ := s.store.GetSandboxOrgID(ctx, sb.ID)
		if orgID != "" {
			if err := s.store.RecordScaleEvent(ctx, sb.ID, orgID, memMB, cpuPct, 0); err != nil {
				log.Printf("grpc: failed to record scale event on wake for %s: %v", sb.ID, err)
			}
		}
	}

	return &pb.WakeSandboxResponse{
		SandboxId: sb.ID,
		Status:    string(sb.Status),
	}, nil
}

func (s *GRPCServer) RebootSandbox(ctx context.Context, req *pb.RebootSandboxRequest) (*pb.RebootSandboxResponse, error) {
	if err := s.manager.RebootSandbox(ctx, req.SandboxId); err != nil {
		return nil, fmt.Errorf("reboot sandbox: %w", err)
	}
	return &pb.RebootSandboxResponse{}, nil
}

func (s *GRPCServer) PowerCycleSandbox(ctx context.Context, req *pb.PowerCycleSandboxRequest) (*pb.PowerCycleSandboxResponse, error) {
	port, err := s.manager.PowerCycleSandbox(ctx, req.SandboxId)
	if err != nil {
		return nil, fmt.Errorf("power-cycle sandbox: %w", err)
	}
	return &pb.PowerCycleSandboxResponse{
		HostPort: int32(port),
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

	// The onReady callback fires after the S3 upload completes inside
	// CreateCheckpoint. We only use it to trigger golden-snapshot prep here;
	// the DB row is marked ready by the API using the actual keys + size from
	// the gRPC response (see api/sandbox.go SetCheckpointReady call).
	//
	// Why not also mark ready here: the previous version of this callback
	// called SetCheckpointReady(cpID, "", "", 0) — empty strings — and lost a
	// race against the API's proper write under timeout. When the gRPC ctx
	// timed out (large checkpoints, ~5 min budget), the API marked the row
	// failed; minutes later the worker's upload finished, this onReady fired,
	// and SetCheckpointReady flipped the row back to status='ready' with
	// empty keys. Forks then saw a "ready" checkpoint with no S3 key and
	// silently fell through to a fresh Create — empty /home/sandbox, only
	// lost+found. Removing the worker-side write makes the API the single
	// source of truth for the DB row's ready state.
	prepareGolden := req.PrepareGolden
	mgr := s.manager
	var onReady func()
	if prepareGolden {
		onReady = func() {
			type goldenPreparer interface {
				RegisterTemplateGoldenFromCache(checkpointID string)
			}
			if gp, ok := mgr.(goldenPreparer); ok {
				gp.RegisterTemplateGoldenFromCache(checkpointID)
			}
		}
	}

	// onReady is called by CreateCheckpoint AFTER S3 upload completes (inside
	// the upload goroutine). This ensures the checkpoint data is in S3 before
	// it's marked "ready" — forks poll for "ready" before downloading.
	// The gRPC call returns immediately with the S3 keys — the CP's fork path
	// polls for checkpoint readiness and blocks until onReady fires.
	rootfsKey, workspaceKey, sizeBytes, err := s.manager.CreateCheckpoint(ctx, req.SandboxId, checkpointID, s.checkpointStore, onReady)
	if err != nil {
		return nil, fmt.Errorf("create checkpoint failed: %w", err)
	}

	return &pb.CreateCheckpointResponse{
		RootfsS3Key:    rootfsKey,
		WorkspaceS3Key: workspaceKey,
		SizeBytes:      sizeBytes,
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
		// Check for both .qcow2 (from CreateCheckpoint) and .ext4 (from hibernate/legacy)
		cachedRootfs := s.manager.CheckpointCachePath(checkpointID, "rootfs.qcow2")
		if cachedRootfs == "" {
			cachedRootfs = s.manager.CheckpointCachePath(checkpointID, "rootfs.ext4")
		}
		cachedWorkspace := s.manager.CheckpointCachePath(checkpointID, "workspace.qcow2")
		if cachedWorkspace == "" {
			cachedWorkspace = s.manager.CheckpointCachePath(checkpointID, "workspace.ext4")
		}
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

	// Fast path: check local template cache (.qcow2 from CreateCheckpoint, .ext4 from legacy)
	cachedRootfs := s.manager.TemplateCachePath(templateID, "rootfs.qcow2")
	if cachedRootfs == "" {
		cachedRootfs = s.manager.TemplateCachePath(templateID, "rootfs.ext4")
	}
	cachedWorkspace := s.manager.TemplateCachePath(templateID, "workspace.qcow2")
	if cachedWorkspace == "" {
		cachedWorkspace = s.manager.TemplateCachePath(templateID, "workspace.ext4")
	}
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
// and caches the drives locally for future use.
func (s *GRPCServer) downloadAndCacheTemplateDrives(ctx context.Context, templateID, rootfsKey, workspaceKey string) (string, string, error) {
	if s.checkpointStore == nil {
		return "", "", fmt.Errorf("checkpoint store not configured")
	}

	cacheDir := filepath.Join(s.manager.DataDir(), "templates", templateID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", fmt.Errorf("create template cache dir: %w", err)
	}

	cachedRootfs := filepath.Join(cacheDir, "rootfs.qcow2")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.qcow2")

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
// and caches the drives locally for cross-worker fork.
func (s *GRPCServer) downloadAndCacheCheckpointDrives(ctx context.Context, checkpointID, rootfsKey, workspaceKey string) (string, string, error) {
	if s.checkpointStore == nil {
		return "", "", fmt.Errorf("checkpoint store not configured")
	}

	cacheDir := filepath.Join(s.manager.DataDir(), "checkpoints", checkpointID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", fmt.Errorf("create checkpoint cache dir: %w", err)
	}

	// Archives from CreateCheckpoint contain .qcow2 files.
	// Legacy hibernate archives may contain .ext4 files.
	cachedRootfs := filepath.Join(cacheDir, "rootfs.qcow2")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.qcow2")

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
// downloadFullCheckpoint downloads a full checkpoint archive (drives + memory + metadata)
// from S3 and extracts it into the checkpoint cache directory for ForkFromCheckpoint.
func (s *GRPCServer) downloadFullCheckpoint(ctx context.Context, checkpointID, s3Key string) error {
	if s.checkpointStore == nil {
		return fmt.Errorf("checkpoint store not configured")
	}

	cacheDir := filepath.Join(s.manager.DataDir(), "checkpoint-snapshots", checkpointID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("create cache dir: %w", err)
	}

	// Download archive
	archivePath := filepath.Join(cacheDir, "checkpoint-download.tar.zst")
	rc, err := s.checkpointStore.Download(ctx, s3Key)
	if err != nil {
		return fmt.Errorf("download %s: %w", s3Key, err)
	}
	f, err := os.Create(archivePath)
	if err != nil {
		rc.Close()
		return fmt.Errorf("create archive file: %w", err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		rc.Close()
		os.Remove(archivePath)
		return fmt.Errorf("write archive: %w", err)
	}
	f.Close()
	rc.Close()

	// Extract (tar.zst)
	if err := extractArchiveCmd(archivePath, cacheDir); err != nil {
		os.Remove(archivePath)
		return fmt.Errorf("extract archive: %w", err)
	}
	os.Remove(archivePath)

	log.Printf("grpc: checkpoint %s: downloaded and cached from S3", checkpointID)
	return nil
}

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

	// Record scale event for billing. Disk size is not affected by SetSandboxLimits;
	// pass 0 so RecordScaleEvent inherits disk_mb from the prior event.
	if s.store != nil && req.MaxMemoryBytes > 0 {
		memMB := int(req.MaxMemoryBytes / (1024 * 1024))
		cpuPct := int(req.CpuMaxUsec / 1000) // 100000us → 100%
		orgID, _ := s.store.GetSandboxOrgID(ctx, req.SandboxId)
		if orgID != "" {
			if err := s.store.RecordScaleEvent(ctx, req.SandboxId, orgID, memMB, cpuPct, 0); err != nil {
				log.Printf("grpc: failed to record scale event for %s: %v", req.SandboxId, err)
			}
		}
	}

	return &pb.SetSandboxLimitsResponse{}, nil
}

func (s *GRPCServer) UpdateSandboxSecret(ctx context.Context, req *pb.UpdateSandboxSecretRequest) (*pb.UpdateSandboxSecretResponse, error) {
	if req.SandboxId == "" || req.SecretName == "" {
		return nil, fmt.Errorf("sandbox_id and secret_name required")
	}
	updated, err := s.manager.UpdateSandboxSecret(ctx, req.SandboxId, req.SecretName, req.Value)
	if err != nil {
		return nil, fmt.Errorf("update secret: %w", err)
	}
	return &pb.UpdateSandboxSecretResponse{Updated: updated}, nil
}

// SetMigrator sets the live migration handler (call after NewGRPCServer if the manager supports it).
func (s *GRPCServer) SetMigrator(m LiveMigrator) {
	s.migrator = m
}

// SetGoldenRebuilder sets the golden snapshot rebuild handler.
func (s *GRPCServer) SetGoldenRebuilder(r GoldenRebuilder) {
	s.goldenRebuilder = r
}

func (s *GRPCServer) PreCopyDrives(ctx context.Context, req *pb.PreCopyDrivesRequest) (*pb.PreCopyDrivesResponse, error) {
	if s.migrator == nil {
		return nil, fmt.Errorf("live migration not supported on this worker")
	}
	rootfsKey, workspaceKey, goldenVersion, baseCPU, baseMem, actualMem, secrets, err := s.migrator.PreCopyDrives(ctx, req.SandboxId, s.checkpointStore)
	if err != nil {
		return nil, fmt.Errorf("pre-copy drives: %w", err)
	}
	resp := &pb.PreCopyDrivesResponse{
		RootfsKey:      rootfsKey,
		WorkspaceKey:   workspaceKey,
		GoldenVersion:  goldenVersion,
		BaseMemoryMb:   int32(baseMem),
		BaseCpuCount:   int32(baseCPU),
		ActualMemoryMb: int32(actualMem),
	}
	// Marshal the secrets-proxy session into the proto. Skipped silently
	// when the sandbox has no secret store registered (empty maps).
	if len(secrets.SealedTokens) > 0 {
		resp.SealedTokens = secrets.SealedTokens
		resp.EgressAllowlist = secrets.EgressAllowlist
		resp.SealedNames = secrets.SealedNames
		if len(secrets.TokenHosts) > 0 {
			resp.TokenHosts = make(map[string]*pb.HostList, len(secrets.TokenHosts))
			for tok, hosts := range secrets.TokenHosts {
				resp.TokenHosts[tok] = &pb.HostList{Hosts: hosts}
			}
		}
	}
	return resp, nil
}

func (s *GRPCServer) PrepareMigrationIncoming(ctx context.Context, req *pb.PrepareMigrationIncomingRequest) (*pb.PrepareMigrationIncomingResponse, error) {
	if s.migrator == nil {
		return nil, fmt.Errorf("live migration not supported on this worker")
	}

	// Capacity guard — actual-memory-based. The caller passes the source VM's
	// current RSS as TargetMemoryMb (i.e., the physical RAM the migration will
	// really land on this host). We admit if that fits inside free host
	// memory after a 10% safety margin. This replaces committed-memory
	// admission: committed over-reserved for idle sandboxes with large
	// maxmem ceilings, which caused drains to stall on workers that had
	// plenty of real headroom.
	if req.TargetMemoryMb > 0 {
		if cc, ok := s.manager.(CapacityChecker); ok {
			hostTotalMB := cc.HostMemoryMB()
			hostUsedMB := cc.HostUsedMemoryMB()
			reserveMB := hostTotalMB / 10
			availableMB := hostTotalMB - hostUsedMB - reserveMB
			if int(req.TargetMemoryMb) > availableMB {
				return nil, fmt.Errorf("insufficient_capacity: migration target needs %dMB but only %dMB actual available (used=%dMB/%dMB)",
					req.TargetMemoryMb, availableMB, hostUsedMB, hostTotalMB)
			}
		}
	}

	// Unmarshal the secrets-proxy session from the request, if any. The
	// orchestrator will have copied this from PreCopyDrives. Empty when
	// the sandbox has no secret store.
	var secrets sandbox.MigrationSecrets
	if len(req.SealedTokens) > 0 {
		secrets.SealedTokens = req.SealedTokens
		secrets.EgressAllowlist = req.EgressAllowlist
		secrets.SealedNames = req.SealedNames
		if len(req.TokenHosts) > 0 {
			secrets.TokenHosts = make(map[string][]string, len(req.TokenHosts))
			for tok, hl := range req.TokenHosts {
				if hl != nil {
					secrets.TokenHosts[tok] = hl.Hosts
				}
			}
		}
	}

	var (
		addr     string
		hostPort int
		err      error
	)
	if req.RootfsS3Key != "" && req.WorkspaceS3Key != "" {
		addr, hostPort, err = s.migrator.PrepareIncomingMigrationWithS3(ctx,
			req.SandboxId, req.RootfsS3Key, req.WorkspaceS3Key,
			int(req.CpuCount), int(req.MemoryMb), int(req.GuestPort), req.Template, s.checkpointStore, req.OverlayMode, req.SourceGoldenVersion, secrets)
	} else {
		addr, hostPort, err = s.migrator.PrepareIncomingMigration(ctx,
			req.SandboxId, req.RootfsPath, req.WorkspacePath,
			int(req.CpuCount), int(req.MemoryMb), int(req.GuestPort), req.Template)
	}
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

func (s *GRPCServer) RebuildGoldenSnapshot(ctx context.Context, req *pb.RebuildGoldenSnapshotRequest) (*pb.RebuildGoldenSnapshotResponse, error) {
	if s.goldenRebuilder == nil {
		return nil, fmt.Errorf("golden snapshot rebuild not supported on this worker")
	}
	oldVersion, newVersion, err := s.goldenRebuilder.RebuildGoldenSnapshot()
	if err != nil {
		return nil, fmt.Errorf("rebuild golden snapshot: %w", err)
	}
	return &pb.RebuildGoldenSnapshotResponse{
		OldVersion: oldVersion,
		NewVersion: newVersion,
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
