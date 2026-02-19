package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/config"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/metrics"
	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("opensandbox-worker: starting (id=%s, region=%s)...", cfg.WorkerID, cfg.Region)

	client, err := podman.NewClient()
	if err != nil {
		log.Fatalf("failed to initialize podman: %v", err)
	}

	ctx := context.Background()
	version, err := client.Version(ctx)
	if err != nil {
		log.Fatalf("failed to get podman version: %v", err)
	}
	log.Printf("opensandbox-worker: using podman %s", version)

	// Initialize sandbox manager
	mgr := sandbox.NewManager(client)
	defer mgr.Close()

	// Initialize PTY manager
	podmanPath, _ := exec.LookPath("podman")
	ptyMgr := sandbox.NewPTYManager(podmanPath, client.AuthFile())
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
				sandboxID, result.CheckpointKey, result.SizeBytes)
			if store != nil {
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

	// Start gRPC server for control plane communication
	grpcServer := worker.NewGRPCServer(mgr, ptyMgr, sandboxDBMgr, checkpointStore, sbRouter)
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
	log.Printf("opensandbox-worker: starting HTTP server on %s", httpAddr)
	go func() {
		if err := httpServer.Start(httpAddr); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Pre-pull common template images in background (non-blocking)
	go func() {
		images := []string{
			"docker.io/library/ubuntu:22.04",
			"docker.io/library/python:3.12-slim",
			"docker.io/library/node:20-slim",
		}
		for _, img := range images {
			exists, _ := client.ImageExists(ctx, img)
			if !exists {
				log.Printf("opensandbox-worker: pulling %s...", img)
				if err := client.PullImage(ctx, img); err != nil {
					log.Printf("opensandbox-worker: warning: failed to pull %s: %v", img, err)
				}
			}
		}
		log.Println("opensandbox-worker: template images ready")
	}()

	// Start Redis heartbeat for control plane discovery
	if cfg.RedisURL != "" {
		// Determine the gRPC address to advertise to the control plane.
		grpcAdvertise := grpcAddr
		if addr := os.Getenv("OPENSANDBOX_GRPC_ADVERTISE"); addr != "" {
			grpcAdvertise = addr
		} else if allocID := os.Getenv("FLY_ALLOC_ID"); allocID != "" {
			grpcAdvertise = allocID + ".vm.opensandbox-worker.internal:9090"
		}

		hb, err := worker.NewRedisHeartbeat(cfg.RedisURL, cfg.WorkerID, cfg.Region, grpcAdvertise, cfg.HTTPAddr)
		if err != nil {
			log.Printf("opensandbox-worker: Redis heartbeat not available: %v", err)
		} else {
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

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("opensandbox-worker: shutting down...")
	grpcServer.Stop()
	if err := httpServer.Close(); err != nil {
		log.Printf("error closing HTTP server: %v", err)
	}
}
