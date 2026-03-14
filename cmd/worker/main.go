package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/config"
	"github.com/opensandbox/opensandbox/internal/db"
	fc "github.com/opensandbox/opensandbox/internal/firecracker"
	"github.com/opensandbox/opensandbox/internal/metrics"
	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/secretsproxy"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/internal/template"
	"github.com/opensandbox/opensandbox/internal/worker"
	"github.com/opensandbox/opensandbox/pkg/types"
	agentpb "github.com/opensandbox/opensandbox/proto/agent"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("opensandbox-worker: starting (id=%s, region=%s)...", cfg.WorkerID, cfg.Region)

	ctx := context.Background()

	// Initialize Firecracker-based sandbox manager
	fcCfg := fc.Config{
		DataDir:         cfg.DataDir,
		KernelPath:      cfg.KernelPath,
		ImagesDir:       cfg.ImagesDir,
		FirecrackerBin:  cfg.FirecrackerBin,
		DefaultMemoryMB: cfg.DefaultSandboxMemoryMB,
		DefaultCPUs:     cfg.DefaultSandboxCPUs,
		DefaultDiskMB:   cfg.DefaultSandboxDiskMB,
	}

	fcMgr, err := fc.NewManager(fcCfg)
	if err != nil {
		log.Fatalf("failed to initialize Firecracker manager: %v", err)
	}
	defer fcMgr.Close()
	log.Println("opensandbox-worker: Firecracker VM manager initialized")

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
			// Wire secrets proxy into VM lifecycle
			fcMgr.SetSecretsProxy(secretsProxy)
		}
	}

	// Clean up orphaned Firecracker processes + TAP devices BEFORE starting golden snapshot.
	// Must run first to avoid killing the golden snapshot VM (race condition).
	fcMgr.CleanupOrphanedProcesses()

	// Prepare golden snapshot for fast default VM creation (~500ms vs ~2s cold boot)
	go func() {
		if err := fcMgr.PrepareGoldenSnapshot(); err != nil {
			log.Printf("opensandbox-worker: golden snapshot preparation failed: %v (cold boot fallback active)", err)
		}
	}()

	// The Firecracker manager implements sandbox.Manager
	var mgr sandbox.Manager = fcMgr

	// Initialize PTY manager using Firecracker agent gRPC
	// Initialize exec session manager using Firecracker agent gRPC
	execMgr := sandbox.NewAgentExecSessionManager(func(sandboxID string, req types.ExecSessionCreateRequest) (*sandbox.ExecSessionHandle, error) {
		agent, err := fcMgr.GetAgent(sandboxID)
		if err != nil {
			return nil, fmt.Errorf("get agent for %s: %w", sandboxID, err)
		}

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

		// Create a pipe for stdin: writes to stdinW are forwarded to the gRPC stream
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

		// Attach to the session to pipe output into the host-side scrollback
		go func() {
			defer close(done)
			defer stdinR.Close()
			stream, err := agent.ExecSessionAttach(context.Background())
			if err != nil {
				return
			}
			// Send first message with session_id
			if err := stream.Send(&agentpb.ExecSessionInput{SessionId: sessionID}); err != nil {
				return
			}

			// Forward stdin pipe to gRPC stream in a separate goroutine
			go func() {
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
			}()

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
		}()

		return handle, nil
	})
	defer execMgr.CloseAll()

	// Initialize PTY manager using Firecracker agent gRPC
	ptyMgr := sandbox.NewAgentPTYManager(func(sandboxID string, req types.PTYCreateRequest) (*sandbox.PTYSessionHandle, error) {
		agent, err := fcMgr.GetAgent(sandboxID)
		if err != nil {
			return nil, fmt.Errorf("get agent for %s: %w", sandboxID, err)
		}
		vsockPath, err := fcMgr.GetVsockPath(sandboxID)
		if err != nil {
			return nil, fmt.Errorf("get vsock path for %s: %w", sandboxID, err)
		}

		cols := int32(req.Cols)
		if cols <= 0 {
			cols = 80
		}
		rows := int32(req.Rows)
		if rows <= 0 {
			rows = 24
		}

		sessionID, dataPort, err := agent.PTYCreate(context.Background(), cols, rows, req.Shell)
		if err != nil {
			return nil, fmt.Errorf("create PTY in VM: %w", err)
		}

		// Connect to the PTY data stream via vsock
		conn, err := agent.ConnectPTYData(vsockPath, dataPort)
		if err != nil {
			_ = agent.PTYKill(context.Background(), sessionID)
			return nil, fmt.Errorf("connect PTY data: %w", err)
		}

		done := make(chan struct{})

		return &sandbox.PTYSessionHandle{
			ID:        sessionID,
			SandboxID: sandboxID,
			PTY:       conn,
			Done:      done,
		}, nil
	})
	defer ptyMgr.CloseAll()

	// Initialize per-sandbox SQLite manager
	sandboxDBMgr := sandbox.NewSandboxDBManager(cfg.DataDir)
	defer sandboxDBMgr.Close()

	// JWT issuer for validating sandbox tokens
	if cfg.JWTSecret == "" {
		log.Fatalf("OPENSANDBOX_JWT_SECRET is required for worker mode")
	}
	jwtIssuer := auth.NewJWTIssuer(cfg.JWTSecret)

	// Initialize S3 checkpoint store for hibernation (if configured)
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

		// Enable local NVMe checkpoint cache if data dir is configured
		if cfg.DataDir != "" {
			cacheDir := filepath.Join(cfg.DataDir, "checkpoints")
			if err := checkpointStore.SetCacheDir(cacheDir); err != nil {
				log.Printf("opensandbox-worker: warning: checkpoint cache disabled: %v", err)
			}
		}
	}

	// Initialize PostgreSQL store for checkpoint lookups (auto-wake)
	var store *db.Store
	dbURL := cfg.DatabaseURL
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL != "" {
		var err error
		store, err = db.NewStore(ctx, dbURL)
		if err != nil {
			log.Printf("opensandbox-worker: warning: failed to connect to database: %v (auto-wake disabled)", err)
		} else {
			defer store.Close()
			log.Println("opensandbox-worker: PostgreSQL store connected (auto-wake enabled)")

			// Local NVMe recovery: scan for sandbox data left from a previous run
			recoveries := fcMgr.RecoverLocalSandboxes()
			if len(recoveries) > 0 {
				snapshotCount, workspaceCount := 0, 0
				for _, r := range recoveries {
					session, err := store.GetSandboxSession(ctx, r.SandboxID)
					if err != nil {
						log.Printf("opensandbox-worker: no DB session for %s, skipping recovery", r.SandboxID)
						continue
					}
					if r.HasSnapshot {
						// Full snapshot on NVMe — create hibernation record so doWake finds local files
						_, _ = store.CreateHibernation(ctx, r.SandboxID, session.OrgID,
							"local://"+r.SandboxID, 0, session.Region, session.Template, session.Config)
						_ = store.UpdateSandboxSessionStatus(ctx, r.SandboxID, "hibernated", nil)
						snapshotCount++
					} else {
						// Workspace only — create local sentinel hibernation for cold boot
						_, _ = store.CreateHibernation(ctx, r.SandboxID, session.OrgID,
							"local://"+r.SandboxID, 0, session.Region, session.Template, session.Config)
						_ = store.UpdateSandboxSessionStatus(ctx, r.SandboxID, "hibernated", nil)
						workspaceCount++
					}
				}
				if snapshotCount+workspaceCount > 0 {
					log.Printf("opensandbox-worker: local recovery: %d with snapshot, %d workspace-only", snapshotCount, workspaceCount)
				}
			}

			// Mark any remaining stale "running" sessions (no local data) as stopped
			_, stopped, err := store.ReconcileWorkerSessions(ctx, cfg.WorkerID)
			if err != nil {
				log.Printf("opensandbox-worker: warning: session reconciliation failed: %v", err)
			} else if stopped > 0 {
				log.Printf("opensandbox-worker: reconciled %d unrecoverable sessions as stopped", stopped)
			}
		}
	}

	// Initialize SandboxRouter for rolling timeouts, auto-wake, and command routing
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
				// Create hibernation record so wake-on-request can find it
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

	// Start Prometheus metrics server on :9091
	metricsSrv := metrics.StartMetricsServer(":9091")
	defer metricsSrv.Close()
	log.Println("opensandbox-worker: metrics server started on :9091")

	// Initialize template builder (Podman as build tool → ext4 images)
	var builder *template.Builder
	podmanClient, podmanErr := podman.NewClient()
	if podmanErr != nil {
		log.Printf("opensandbox-worker: podman not available: %v (template building disabled)", podmanErr)
	} else {
		imagesDir := fcCfg.ImagesDir
		agentPath := filepath.Join(filepath.Dir(os.Args[0]), "osb-agent")
		if _, err := os.Stat(agentPath); err != nil {
			agentPath = "/usr/local/bin/osb-agent"
		}
		builder = template.NewBuilder(podmanClient, imagesDir, agentPath)
		log.Printf("opensandbox-worker: template builder configured (images=%s, agent=%s)", imagesDir, agentPath)
	}

	// Start gRPC server for control plane communication
	grpcServer := worker.NewGRPCServer(mgr, ptyMgr, execMgr, sandboxDBMgr, checkpointStore, sbRouter, builder, store)
	grpcAddr := ":9090"
	log.Printf("opensandbox-worker: starting gRPC server on %s", grpcAddr)
	go func() {
		if err := grpcServer.Start(grpcAddr); err != nil {
			log.Printf("gRPC server error: %v", err)
		}
	}()

	// Initialize subdomain reverse proxy
	var sbProxy *proxy.SandboxProxy
	if cfg.SandboxDomain != "" {
		sbProxy = proxy.New(cfg.SandboxDomain, mgr, sbRouter)
		log.Printf("opensandbox-worker: subdomain proxy configured (*.%s)", cfg.SandboxDomain)
	}

	// Start HTTP server for direct SDK access
	httpServer := worker.NewHTTPServer(mgr, ptyMgr, execMgr, jwtIssuer, sandboxDBMgr, sbProxy, sbRouter, cfg.SandboxDomain)
	httpAddr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("opensandbox-worker: starting HTTP server on %s", httpAddr)
	go func() {
		if err := httpServer.Start(httpAddr); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Start Redis heartbeat for control plane discovery
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
				log.Printf("opensandbox-worker: EC2 instance ID: %s", machineID)
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

	// Start NATS event publisher if configured
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

	// Start periodic SyncFS to keep workspace.ext4 crash-consistent on NVMe
	autosaver := worker.NewWorkspaceAutosaver(mgr, fcMgr, 5*time.Minute)
	autosaver.Start()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("opensandbox-worker: graceful shutdown starting...")

	// 1. Stop accepting new work
	grpcServer.Stop()
	if err := httpServer.Close(); err != nil {
		log.Printf("error closing HTTP server: %v", err)
	}

	// Stop autosaver before hibernating
	autosaver.Stop()

	// 2. Hibernate all running sandboxes for seamless resume
	if checkpointStore != nil {
		vms, _ := mgr.List(context.Background())
		if len(vms) > 0 {
			log.Printf("opensandbox-worker: hibernating %d sandboxes...", len(vms))
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			results := fcMgr.HibernateAll(shutCtx, checkpointStore)
			cancel()

			for _, r := range results {
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

			// 3. Wait for async S3 uploads to complete
			log.Println("opensandbox-worker: waiting for S3 uploads...")
			fcMgr.WaitUploads(3 * time.Minute)
			log.Println("opensandbox-worker: graceful shutdown complete")
		}
	}
}
