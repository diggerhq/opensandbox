package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/certmanager"
	"github.com/opensandbox/opensandbox/internal/config"
	"github.com/opensandbox/opensandbox/internal/db"
	fc "github.com/opensandbox/opensandbox/internal/firecracker"
	"github.com/opensandbox/opensandbox/internal/metrics"
	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/internal/template"
	"github.com/opensandbox/opensandbox/internal/worker"
	"github.com/opensandbox/opensandbox/pkg/types"
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
	grpcServer := worker.NewGRPCServer(mgr, ptyMgr, sandboxDBMgr, checkpointStore, sbRouter, builder, store)
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
	httpServer := worker.NewHTTPServer(mgr, ptyMgr, jwtIssuer, sandboxDBMgr, sbProxy, sbRouter, cfg.SandboxDomain)
	httpAddr := fmt.Sprintf(":%d", cfg.Port)

	// Initialize cert fetcher for TLS (if S3 cert bucket is configured)
	certBucket := cfg.CertS3Bucket
	if certBucket == "" {
		certBucket = cfg.S3Bucket // default to same bucket
	}
	var certFetcher *certmanager.CertFetcher
	if certBucket != "" && cfg.CertS3Prefix != "" {
		var fetchErr error
		certFetcher, fetchErr = certmanager.NewCertFetcher(certmanager.FetcherConfig{
			S3Bucket:       certBucket,
			S3Prefix:       cfg.CertS3Prefix,
			S3Region:       cfg.S3Region,
			AccessKeyID:    cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			LocalCertDir:   filepath.Join(cfg.DataDir, "tls"),
		})
		if fetchErr != nil {
			log.Printf("opensandbox-worker: cert fetcher init failed: %v (TLS disabled)", fetchErr)
		} else {
			// Set up fallback renewal — if cert is close to expiry and server
			// hasn't renewed, this worker will attempt renewal directly.
			if cfg.Route53HostedZoneID != "" && cfg.ACMEEmail != "" {
				cm, cmErr := certmanager.NewCertManager(certmanager.Config{
					Domain:         cfg.SandboxDomain,
					HostedZoneID:   cfg.Route53HostedZoneID,
					S3Bucket:       certBucket,
					S3Prefix:       cfg.CertS3Prefix,
					S3Region:       cfg.S3Region,
					AccessKeyID:    cfg.S3AccessKeyID,
					SecretAccessKey: cfg.S3SecretAccessKey,
					ACMEEmail:      cfg.ACMEEmail,
				})
				if cmErr != nil {
					log.Printf("opensandbox-worker: fallback cert renewal disabled: %v", cmErr)
				} else {
					certFetcher.SetRenewer(cm)
					log.Println("opensandbox-worker: fallback cert renewal enabled (worker can renew if server is down)")
				}
			}

			if err := certFetcher.FetchAndStore(ctx); err != nil {
				log.Printf("opensandbox-worker: initial cert fetch failed: %v (TLS disabled, will retry)", err)
				certFetcher = nil
			} else {
				certFetcher.StartRefreshLoop(ctx)
				log.Println("opensandbox-worker: TLS cert loaded from S3, refresh loop started")
			}
		}
	}

	// Wire cert fetcher into health endpoint
	if certFetcher != nil {
		httpServer.SetCertFetcher(certFetcher)
	}

	// Serve HTTPS on :443 if cert is available, always serve HTTP for VPC-internal traffic
	if certFetcher != nil {
		tlsServer := worker.NewHTTPServer(mgr, ptyMgr, jwtIssuer, sandboxDBMgr, sbProxy, sbRouter, cfg.SandboxDomain)
		tlsServer.SetCertFetcher(certFetcher)
		log.Println("opensandbox-worker: starting HTTPS server on :443 (Let's Encrypt wildcard)")
		go func() {
			if err := tlsServer.StartTLSWithCert(":443", certFetcher.GetCertificate); err != nil {
				log.Printf("HTTPS server error: %v", err)
			}
		}()
	}

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
