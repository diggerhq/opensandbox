package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/config"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/metrics"
	"github.com/opensandbox/opensandbox/internal/proxy"
	qm "github.com/opensandbox/opensandbox/internal/qemu"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/secretsproxy"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/internal/worker"
	"github.com/opensandbox/opensandbox/pkg/types"
	agentpb "github.com/opensandbox/opensandbox/proto/agent"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("opensandbox-worker: starting (id=%s, region=%s, backend=%s)...", cfg.WorkerID, cfg.Region, cfg.VMBackend)

	ctx := context.Background()

	var mgr sandbox.Manager

	// Backend-specific exec session factory
	var execSessionFactory func(sandboxID string, req types.ExecSessionCreateRequest) (*sandbox.ExecSessionHandle, error)
	// Backend-specific PTY session factory
	var ptySessionFactory func(sandboxID string, req types.PTYCreateRequest) (*sandbox.PTYSessionHandle, error)
	// Backend-specific autosaver syncer
	var autosaverSyncer worker.SyncFSer
	// Backend-specific graceful shutdown
	var doGracefulShutdown func(checkpointStore *storage.CheckpointStore, store *db.Store)

	// Initialize secrets proxy for MITM token substitution.
	// Runs on :3128 — VMs route HTTPS through this to keep real secrets off-VM.
	secretsCA, err := secretsproxy.LoadOrCreateCA(filepath.Join(cfg.DataDir, "proxy-ca"))
	if err != nil {
		log.Printf("opensandbox-worker: secrets proxy CA failed: %v (secrets proxy disabled)", err)
	}
	var secretsProxy *secretsproxy.SecretsProxy
	if secretsCA != nil {
		secretsProxy, err = secretsproxy.NewSecretsProxy(secretsCA, "0.0.0.0:3128")
		if err != nil {
			log.Printf("opensandbox-worker: secrets proxy listen failed: %v", err)
		} else {
			secretsProxy.Start()
			defer secretsProxy.Stop()
			log.Println("opensandbox-worker: secrets proxy started on :3128")
		}
	}
	// QEMU backend
	{
		qmCfg := qm.Config{
			DataDir:         cfg.DataDir,
			KernelPath:      cfg.KernelPath,
			ImagesDir:       cfg.ImagesDir,
			QEMUBin:         cfg.QEMUBin,
			DefaultMemoryMB: cfg.DefaultSandboxMemoryMB,
			DefaultCPUs:     cfg.DefaultSandboxCPUs,
			DefaultDiskMB:   cfg.DefaultSandboxDiskMB,
		}

		qmMgr, err := qm.NewManager(qmCfg)
		if err != nil {
			log.Fatalf("failed to initialize QEMU manager: %v", err)
		}
		defer qmMgr.Close()
		log.Println("opensandbox-worker: QEMU VM manager initialized")

		if secretsProxy != nil {
			qmMgr.SetSecretsProxy(secretsProxy)
		}

		qmMgr.CleanupOrphanedProcesses()

		// Prepare golden snapshot for fast VM creation
		if err := qmMgr.PrepareGoldenSnapshot(); err != nil {
			log.Printf("opensandbox-worker: WARNING: golden snapshot failed, using cold boot: %v", err)
		}

		mgr = qmMgr
		autosaverSyncer = qmMgr

		// Start metadata server (169.254.169.254 equivalent, served on :8888)
		metadataSrv := worker.NewMetadataServer(qmMgr, cfg.Region)
		metadataSrv.Start(":8888")
		defer metadataSrv.Close()
		qmMgr.SetMetadataCallbacks(metadataSrv.RegisterSandbox, metadataSrv.UnregisterSandbox)
		log.Println("opensandbox-worker: metadata server started on :8888")

		execSessionFactory = func(sandboxID string, req types.ExecSessionCreateRequest) (*sandbox.ExecSessionHandle, error) {
			agent, err := qmMgr.GetAgent(sandboxID)
			if err != nil {
				return nil, fmt.Errorf("get agent for %s: %w", sandboxID, err)
			}
			return createExecSessionQEMU(agent, sandboxID, req)
		}

		ptySessionFactory = func(sandboxID string, req types.PTYCreateRequest) (*sandbox.PTYSessionHandle, error) {
			agent, err := qmMgr.GetAgent(sandboxID)
			if err != nil {
				return nil, fmt.Errorf("get agent for %s: %w", sandboxID, err)
			}
			return createPTYSessionQEMU(agent, sandboxID, req)
		}

		doGracefulShutdown = func(checkpointStore *storage.CheckpointStore, store *db.Store) {
			if checkpointStore == nil {
				return
			}
			vms, _ := mgr.List(context.Background())
			if len(vms) == 0 {
				return
			}
			log.Printf("opensandbox-worker: hibernating %d sandboxes...", len(vms))
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			results := qmMgr.HibernateAll(shutCtx, checkpointStore)
			cancel()
			processHibernateResults(results, store, func(r interface{}) (string, string, error) {
				hr := r.(qm.HibernateAllResult)
				return hr.SandboxID, hr.HibernationKey, hr.Err
			})
			log.Println("opensandbox-worker: waiting for S3 uploads...")
			qmMgr.WaitUploads(3 * time.Minute)
			log.Println("opensandbox-worker: graceful shutdown complete")
		}

		// Wire up local recovery
		if dbURL := getDBURL(cfg); dbURL != "" {
			if store, err := db.NewStore(ctx, dbURL); err == nil {
				defer store.Close()
				recoverLocalQEMU(ctx, qmMgr, store, cfg)
			}
		}

	}

	// Initialize exec session manager
	execMgr := sandbox.NewAgentExecSessionManager(execSessionFactory)
	defer execMgr.CloseAll()

	// Initialize PTY manager
	ptyMgr := sandbox.NewAgentPTYManager(ptySessionFactory)
	defer ptyMgr.CloseAll()

	// Initialize per-sandbox SQLite manager
	sandboxDBMgr := sandbox.NewSandboxDBManager(cfg.DataDir)
	defer sandboxDBMgr.Close()

	// JWT issuer
	if cfg.JWTSecret == "" {
		log.Fatalf("OPENSANDBOX_JWT_SECRET is required for worker mode")
	}
	jwtIssuer := auth.NewJWTIssuer(cfg.JWTSecret)

	// S3 checkpoint store
	var checkpointStore *storage.CheckpointStore
	if cfg.S3Bucket != "" {
		var err error
		checkpointStore, err = storage.NewCheckpointStore(storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Bucket:          cfg.S3Bucket,
			Region:          cfg.S3Region,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			ForcePathStyle:  cfg.S3ForcePathStyle,
		})
		if err != nil {
			log.Fatalf("failed to initialize checkpoint store: %v", err)
		}
		log.Printf("opensandbox-worker: S3 checkpoint store configured (bucket=%s, region=%s)", cfg.S3Bucket, cfg.S3Region)

		if cfg.DataDir != "" {
			cacheDir := filepath.Join(cfg.DataDir, "checkpoints")
			if err := checkpointStore.SetCacheDir(cacheDir); err != nil {
				log.Printf("opensandbox-worker: warning: checkpoint cache disabled: %v", err)
			}
		}
	}

	// PostgreSQL store
	var store *db.Store
	dbURL := getDBURL(cfg)
	if dbURL != "" {
		var err error
		store, err = db.NewStore(ctx, dbURL)
		if err != nil {
			log.Printf("opensandbox-worker: warning: failed to connect to database: %v (auto-wake disabled)", err)
		} else {
			defer store.Close()
			log.Println("opensandbox-worker: PostgreSQL store connected (auto-wake enabled)")

			_, stopped, err := store.ReconcileWorkerSessions(ctx, cfg.WorkerID)
			if err != nil {
				log.Printf("opensandbox-worker: warning: session reconciliation failed: %v", err)
			} else if stopped > 0 {
				log.Printf("opensandbox-worker: reconciled %d unrecoverable sessions as stopped", stopped)
			}
		}
	}

	// Sandbox router
	sbRouter := sandbox.NewSandboxRouter(sandbox.RouterConfig{
		Manager:         mgr,
		CheckpointStore: checkpointStore,
		Store:           store,
		WorkerID:        cfg.WorkerID,
		OnHibernate: func(sandboxID string, result *sandbox.HibernateResult) {
			log.Printf("opensandbox-worker: sandbox %s auto-hibernated (key=%s, size=%d bytes)",
				sandboxID, result.HibernationKey, result.SizeBytes)
			execMgr.RemoveSessions(sandboxID)
			if store != nil {
				session, err := store.GetSandboxSession(context.Background(), sandboxID)
				if err == nil {
					_, _ = store.CreateHibernation(context.Background(), sandboxID, session.OrgID,
						result.HibernationKey, result.SizeBytes, session.Region, session.Template, session.Config)
				}
				_ = store.UpdateSandboxSessionStatus(context.Background(), sandboxID, "hibernated", nil)
			}
		},
		OnKill: func(sandboxID string) {
			log.Printf("opensandbox-worker: sandbox %s killed on timeout", sandboxID)
			execMgr.RemoveSessions(sandboxID)
			if store != nil {
				_ = store.UpdateSandboxSessionStatus(context.Background(), sandboxID, "stopped", nil)
			}
		},
	})
	defer sbRouter.Close()
	log.Println("opensandbox-worker: sandbox router initialized (rolling timeouts, auto-wake)")

	// Metrics
	metricsSrv := metrics.StartMetricsServer(":9091")
	defer metricsSrv.Close()
	log.Println("opensandbox-worker: metrics server started on :9091")

	// gRPC server (nil builder — template building via podman not needed for QEMU)
	grpcServer := worker.NewGRPCServer(mgr, ptyMgr, execMgr, sandboxDBMgr, checkpointStore, sbRouter, nil, store)
	grpcAddr := ":9090"
	log.Printf("opensandbox-worker: starting gRPC server on %s", grpcAddr)
	go func() {
		if err := grpcServer.Start(grpcAddr); err != nil {
			log.Printf("gRPC server error: %v", err)
		}
	}()

	// Subdomain proxy
	var sbProxy *proxy.SandboxProxy
	if cfg.SandboxDomain != "" {
		sbProxy = proxy.New(cfg.SandboxDomain, mgr, sbRouter)
		log.Printf("opensandbox-worker: subdomain proxy configured (*.%s)", cfg.SandboxDomain)
	}

	// HTTP server
	httpServer := worker.NewHTTPServer(mgr, ptyMgr, execMgr, jwtIssuer, sandboxDBMgr, sbProxy, sbRouter, cfg.SandboxDomain)
	httpAddr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("opensandbox-worker: starting HTTP server on %s", httpAddr)
	go func() {
		if err := httpServer.Start(httpAddr); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Redis heartbeat
	if cfg.RedisURL != "" {
		grpcAdvertise := grpcAddr
		if addr := os.Getenv("OPENSANDBOX_GRPC_ADVERTISE"); addr != "" {
			grpcAdvertise = addr
		}

		hb, err := worker.NewRedisHeartbeat(cfg.RedisURL, cfg.WorkerID, cfg.Region, grpcAdvertise, cfg.HTTPAddr)
		if err != nil {
			log.Printf("opensandbox-worker: Redis heartbeat not available: %v", err)
		} else {
			if machineID := worker.GetEC2InstanceID(); machineID != "" {
				hb.SetMachineID(machineID)
				log.Printf("opensandbox-worker: instance ID: %s", machineID)
			}
			hb.Start(func() (int, int, float64, float64) {
				count, _ := mgr.Count(context.Background())
				cpuPct, memPct := worker.SystemStats()
				return cfg.MaxCapacity, count, cpuPct, memPct
			})
			defer hb.Stop()
			log.Println("opensandbox-worker: Redis heartbeat started")
		}
	}

	// NATS
	if cfg.NATSURL != "" {
		pub, err := worker.NewEventPublisher(cfg.NATSURL, cfg.Region, cfg.WorkerID, sandboxDBMgr)
		if err != nil {
			log.Printf("opensandbox-worker: NATS not available: %v (continuing without event sync)", err)
		} else {
			pub.Start()
			pub.StartHeartbeat(func() (int, int, float64, float64) {
				count, _ := mgr.Count(context.Background())
				cpuPct, memPct := worker.SystemStats()
				return cfg.MaxCapacity, count, cpuPct, memPct
			})
			defer pub.Stop()
			log.Println("opensandbox-worker: NATS event publisher started")
		}
	}

	// Periodic SyncFS
	autosaver := worker.NewWorkspaceAutosaver(mgr, autosaverSyncer, 5*time.Minute)
	autosaver.Start()

	// Usage collector for billing (samples cgroup stats every 60s, flushes to DB every 5 min)
	if store != nil {
		usageCollector := worker.NewUsageCollector(mgr, store)
		usageCollector.Start()
		defer usageCollector.Stop()
	}

	// Pressure monitor: watches host RAM/disk and triggers hibernate/migration.
	// Disabled by default — enable with OPENSANDBOX_PRESSURE_MONITOR=true.
	// Not useful with a single worker since there's nowhere to migrate to.
	if qemuMgr, ok := mgr.(*qm.Manager); ok && os.Getenv("OPENSANDBOX_PRESSURE_MONITOR") == "true" {
		pressureMonitor := qm.NewPressureMonitor(qemuMgr, cfg.DataDir, qm.DefaultThresholds(), qm.PressureCallbacks{
			OnLevelChange: func(from, to qm.PressureLevel) {
				log.Printf("opensandbox-worker: pressure %s → %s", from, to)
			},
			OnHibernateIdle: func(sandboxIDs []string) {
				for _, id := range sandboxIDs {
					if checkpointStore != nil {
						_, err := mgr.Hibernate(context.Background(), id, checkpointStore)
						if err != nil {
							log.Printf("pressure-hibernate %s: %v", id, err)
						}
					}
				}
			},
			OnHibernateAll: func() {
				if checkpointStore != nil && doGracefulShutdown != nil {
					doGracefulShutdown(checkpointStore, store)
				}
			},
		})
		pressureMonitor.Start()
		defer pressureMonitor.Stop()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("opensandbox-worker: graceful shutdown starting...")

	grpcServer.Stop()
	if err := httpServer.Close(); err != nil {
		log.Printf("error closing HTTP server: %v", err)
	}

	autosaver.Stop()

	doGracefulShutdown(checkpointStore, store)
}

// getDBURL returns the database URL from config or environment.
func getDBURL(cfg *config.Config) string {
	if cfg.DatabaseURL != "" {
		return cfg.DatabaseURL
	}
	return os.Getenv("DATABASE_URL")
}

// createExecSessionQEMU creates an exec session using a QEMU agent client.
func createExecSessionQEMU(agent *qm.AgentClient, sandboxID string, req types.ExecSessionCreateRequest) (*sandbox.ExecSessionHandle, error) {
	agentPB := &agentpb.ExecSessionCreateRequest{
		Command:               req.Command,
		Args:                  req.Args,
		Envs:                  req.Env,
		Cwd:                   req.Cwd,
		TimeoutSeconds:        int32(req.Timeout),
		MaxRunAfterDisconnect: int32(req.MaxRunAfterDisconnect),
	}

	sessionID, err := agent.ExecSessionCreate(context.Background(), agentPB)
	if err != nil {
		return nil, fmt.Errorf("create exec session in VM: %w", err)
	}

	scrollback := sandbox.NewScrollbackBuffer(0)
	done := make(chan struct{})
	stdinR, stdinW := io.Pipe()

	handle := &sandbox.ExecSessionHandle{
		ID:          sessionID,
		SandboxID:   sandboxID,
		Command:     req.Command,
		Args:        req.Args,
		Running:     true,
		StartedAt:   time.Now(),
		Done:        done,
		Scrollback:  scrollback,
		StdinWriter: stdinW,
		OnKill: func(signal int) error {
			stdinW.Close()
			return agent.ExecSessionKill(context.Background(), sessionID, int32(signal))
		},
	}

	go runExecStreamQEMU(agent, sessionID, stdinR, done, scrollback, handle)

	return handle, nil
}

// runExecStreamQEMU attaches to an exec session stream (QEMU backend).
func runExecStreamQEMU(agent *qm.AgentClient, sessionID string, stdinR *io.PipeReader, done chan struct{}, scrollback *sandbox.ScrollbackBuffer, handle *sandbox.ExecSessionHandle) {
	defer close(done)
	defer stdinR.Close()
	stream, err := agent.ExecSessionAttach(context.Background())
	if err != nil {
		return
	}
	if err := stream.Send(&agentpb.ExecSessionInput{SessionId: sessionID}); err != nil {
		return
	}
	go forwardStdin(stdinR, stream)
	consumeExecOutput(stream, scrollback, handle)
}

// forwardStdin pipes stdin data to a gRPC stream.
func forwardStdin(stdinR *io.PipeReader, stream agentpb.SandboxAgent_ExecSessionAttachClient) {
	buf := make([]byte, 4096)
	for {
		n, err := stdinR.Read(buf)
		if err != nil {
			return
		}
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			if err := stream.Send(&agentpb.ExecSessionInput{Stdin: data}); err != nil {
				return
			}
		}
	}
}

// consumeExecOutput reads output from a gRPC exec stream into a scrollback buffer.
func consumeExecOutput(stream agentpb.SandboxAgent_ExecSessionAttachClient, scrollback *sandbox.ScrollbackBuffer, handle *sandbox.ExecSessionHandle) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return
		}
		switch msg.Type {
		case agentpb.ExecSessionOutput_STDOUT:
			scrollback.Write(1, msg.Data)
		case agentpb.ExecSessionOutput_STDERR:
			scrollback.Write(2, msg.Data)
		case agentpb.ExecSessionOutput_EXIT:
			exitCode := int(msg.ExitCode)
			handle.ExitCode = &exitCode
			handle.Running = false
			return
		case agentpb.ExecSessionOutput_SCROLLBACK_END:
			// Transition from scrollback replay to live
		}
	}
}

// grpcPTYConn adapts a PTYAttach bidi gRPC stream into an io.ReadWriteCloser
// with a Resize method, suitable for PTYSessionHandle.PTY.
type grpcPTYConn struct {
	stream    agentpb.SandboxAgent_PTYAttachClient
	buf       []byte
	cancel    context.CancelFunc
	closeOnce sync.Once
	exited    bool
	exitCode  int
}

func (c *grpcPTYConn) Read(p []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(p, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}
	msg, err := c.stream.Recv()
	if err != nil {
		return 0, err
	}
	if msg.Exited {
		c.exited = true
		c.exitCode = int(msg.ExitCode)
		return 0, io.EOF
	}
	n := copy(p, msg.Data)
	if n < len(msg.Data) {
		c.buf = msg.Data[n:]
	}
	return n, nil
}

func (c *grpcPTYConn) Write(p []byte) (int, error) {
	data := make([]byte, len(p))
	copy(data, p)
	if err := c.stream.Send(&agentpb.PTYInput{Stdin: data}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *grpcPTYConn) Resize(cols, rows int) error {
	return c.stream.Send(&agentpb.PTYInput{Cols: int32(cols), Rows: int32(rows)})
}

func (c *grpcPTYConn) Close() error {
	c.closeOnce.Do(func() { c.cancel() })
	return nil
}

// createPTYSessionQEMU creates a PTY session using gRPC PTYAttach (QEMU backend).
func createPTYSessionQEMU(agent *qm.AgentClient, sandboxID string, req types.PTYCreateRequest) (*sandbox.PTYSessionHandle, error) {
	cols := int32(req.Cols)
	if cols <= 0 {
		cols = 80
	}
	rows := int32(req.Rows)
	if rows <= 0 {
		rows = 24
	}

	// 1. Create the PTY session (allocates shell + pty in the VM)
	sessionID, _, err := agent.PTYCreate(context.Background(), cols, rows, req.Shell)
	if err != nil {
		return nil, fmt.Errorf("create PTY in VM: %w", err)
	}

	// 2. Open bidi gRPC stream for PTY I/O
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := agent.PTYAttach(ctx)
	if err != nil {
		cancel()
		_ = agent.PTYKill(context.Background(), sessionID)
		return nil, fmt.Errorf("attach PTY stream: %w", err)
	}

	// 3. Send first message with session_id to bind the stream
	if err := stream.Send(&agentpb.PTYInput{SessionId: sessionID}); err != nil {
		cancel()
		_ = agent.PTYKill(context.Background(), sessionID)
		return nil, fmt.Errorf("send PTY session ID: %w", err)
	}

	// 4. Wrap in grpcPTYConn
	conn := &grpcPTYConn{
		stream: stream,
		cancel: cancel,
	}

	done := make(chan struct{})
	return &sandbox.PTYSessionHandle{
		ID:        sessionID,
		SandboxID: sandboxID,
		PTY:       conn,
		Done:      done,
	}, nil
}

// processHibernateResults handles results from HibernateAll for both backends.
func processHibernateResults(results interface{}, store *db.Store, extract func(interface{}) (string, string, error)) {
	switch rs := results.(type) {
	case []qm.HibernateAllResult:
		for _, r := range rs {
			if r.Err != nil {
				log.Printf("opensandbox-worker: hibernate failed for %s: %v", r.SandboxID, r.Err)
				if store != nil {
					errMsg := "hibernate failed on shutdown: " + r.Err.Error()
					_ = store.UpdateSandboxSessionStatus(context.Background(), r.SandboxID, "stopped", &errMsg)
				}
				continue
			}
			log.Printf("opensandbox-worker: hibernated %s (key=%s)", r.SandboxID, r.HibernationKey)
			if store != nil {
				session, err := store.GetSandboxSession(context.Background(), r.SandboxID)
				if err == nil {
					_, _ = store.CreateHibernation(context.Background(), r.SandboxID, session.OrgID,
						r.HibernationKey, 0, session.Region, session.Template, session.Config)
					_ = store.UpdateSandboxSessionStatus(context.Background(), r.SandboxID, "hibernated", nil)
				}
			}
		}
	}
}

// recoverLocalQEMU handles local disk recovery for QEMU backend.
func recoverLocalQEMU(ctx context.Context, qmMgr *qm.Manager, store *db.Store, cfg *config.Config) {
	recoveries := qmMgr.RecoverLocalSandboxes()
	if len(recoveries) == 0 {
		return
	}
	snapshotCount, workspaceCount := 0, 0
	for _, r := range recoveries {
		session, err := store.GetSandboxSession(ctx, r.SandboxID)
		if err != nil {
			log.Printf("opensandbox-worker: no DB session for %s, skipping recovery", r.SandboxID)
			continue
		}
		_, _ = store.CreateHibernation(ctx, r.SandboxID, session.OrgID,
			"local://"+r.SandboxID, 0, session.Region, session.Template, session.Config)
		_ = store.UpdateSandboxSessionStatus(ctx, r.SandboxID, "hibernated", nil)
		if r.HasSnapshot {
			snapshotCount++
		} else {
			workspaceCount++
		}
	}
	if snapshotCount+workspaceCount > 0 {
		log.Printf("opensandbox-worker: local recovery: %d with snapshot, %d workspace-only", snapshotCount, workspaceCount)
	}
}

