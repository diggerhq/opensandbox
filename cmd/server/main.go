package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"time"

	"github.com/opensandbox/opensandbox/internal/api"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/cloudflare"
	"github.com/opensandbox/opensandbox/internal/compute"
	"github.com/opensandbox/opensandbox/internal/config"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/ecr"
	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ctx := context.Background()

	// Initialize Podman (optional in server mode — no container runtime needed)
	var mgr sandbox.Manager
	var ptyMgr *sandbox.PTYManager
	podmanClient, err := podman.NewClient()
	if err != nil {
		if cfg.Mode == "server" {
			log.Printf("opensandbox: podman not available (server-only mode, sandbox execution disabled): %v", err)
		} else {
			log.Fatalf("failed to initialize podman: %v", err)
		}
	} else {
		version, err := podmanClient.Version(ctx)
		if err != nil {
			if cfg.Mode == "server" {
				log.Printf("opensandbox: podman not responding (server-only mode, sandbox execution disabled): %v", err)
			} else {
				log.Fatalf("failed to get podman version: %v", err)
			}
		} else {
			log.Printf("opensandbox: using podman %s", version)

			mgr = sandbox.NewManager(podmanClient,
				sandbox.WithDataDir(cfg.DataDir),
				sandbox.WithDefaultMemoryMB(cfg.DefaultSandboxMemoryMB),
				sandbox.WithDefaultCPUs(cfg.DefaultSandboxCPUs),
				sandbox.WithDefaultDiskMB(cfg.DefaultSandboxDiskMB),
			)
			defer mgr.Close()

			podmanPath, _ := exec.LookPath("podman")
			ptyMgr = sandbox.NewPTYManager(podmanPath, podmanClient.AuthFile())
			defer ptyMgr.CloseAll()
		}
	}

	// Build server options
	opts := &api.ServerOpts{
		Mode:     cfg.Mode,
		WorkerID: cfg.WorkerID,
		Region:   cfg.Region,
		HTTPAddr: cfg.HTTPAddr,
	}

	// Initialize PostgreSQL if configured
	if cfg.DatabaseURL != "" {
		store, err := db.NewStore(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatalf("failed to connect to database: %v", err)
		}
		defer store.Close()

		log.Println("opensandbox: running database migrations...")
		if err := store.Migrate(ctx); err != nil {
			log.Fatalf("failed to run migrations: %v", err)
		}
		log.Println("opensandbox: database migrations complete")

		opts.Store = store
	} else {
		log.Println("opensandbox: no DATABASE_URL configured, running without PostgreSQL")
	}

	// Initialize JWT issuer if configured
	if cfg.JWTSecret != "" {
		opts.JWTIssuer = auth.NewJWTIssuer(cfg.JWTSecret)
		log.Println("opensandbox: JWT issuer configured")
	}

	// Initialize per-sandbox SQLite manager
	sandboxDBMgr := sandbox.NewSandboxDBManager(cfg.DataDir)
	defer sandboxDBMgr.Close()
	opts.SandboxDBs = sandboxDBMgr
	log.Printf("opensandbox: SQLite data directory: %s", cfg.DataDir)

	// Configure WorkOS if credentials are set
	if cfg.WorkOSAPIKey != "" && cfg.WorkOSClientID != "" {
		opts.WorkOSConfig = &auth.WorkOSConfig{
			APIKey:       cfg.WorkOSAPIKey,
			ClientID:     cfg.WorkOSClientID,
			RedirectURI:  cfg.WorkOSRedirectURI,
			CookieDomain: cfg.WorkOSCookieDomain,
			FrontendURL:  cfg.WorkOSFrontendURL,
		}
		log.Println("opensandbox: WorkOS authentication configured")
	}

	// Initialize S3 checkpoint store for hibernation (if configured)
	if cfg.S3Bucket != "" {
		checkpointStore, err := storage.NewCheckpointStore(storage.S3Config{
			Endpoint:        cfg.S3Endpoint,
			Bucket:          cfg.S3Bucket,
			Region:          cfg.S3Region,
			AccessKeyID:     cfg.S3AccessKeyID,
			SecretAccessKey: cfg.S3SecretAccessKey,
			ForcePathStyle:  cfg.S3ForcePathStyle,
		})
		if err != nil {
			log.Printf("opensandbox: failed to initialize checkpoint store: %v (continuing without hibernation)", err)
		} else {
			opts.CheckpointStore = checkpointStore
			log.Printf("opensandbox: S3 checkpoint store configured (bucket=%s, region=%s)", cfg.S3Bucket, cfg.S3Region)
		}
	}

	// Initialize ECR config for template images (if configured)
	if cfg.ECRRegistry != "" {
		ecrCfg := &ecr.Config{
			Registry:   cfg.ECRRegistry,
			Repository: cfg.ECRRepository,
			Region:     cfg.S3Region, // reuse S3 region (same AWS account)
			AccessKey:  cfg.S3AccessKeyID,
			SecretKey:  cfg.S3SecretAccessKey,
		}
		opts.ECRConfig = ecrCfg
		log.Printf("opensandbox: ECR configured (registry=%s, repo=%s)", cfg.ECRRegistry, cfg.ECRRepository)
	}

	// Initialize SandboxRouter for rolling timeouts, auto-wake, and command routing
	if mgr != nil {
		workerID := cfg.WorkerID
		if workerID == "" {
			workerID = "w-local-1"
		}
		sbRouter := sandbox.NewSandboxRouter(sandbox.RouterConfig{
			Manager:         mgr,
			CheckpointStore: opts.CheckpointStore,
			Store:           opts.Store,
			WorkerID:        workerID,
			OnHibernate: func(sandboxID string, result *sandbox.HibernateResult) {
				log.Printf("opensandbox: sandbox %s auto-hibernated (key=%s, size=%d bytes)",
					sandboxID, result.CheckpointKey, result.SizeBytes)
				if opts.Store != nil {
					_ = opts.Store.UpdateSandboxSessionStatus(context.Background(), sandboxID, "hibernated", nil)
				}
			},
			OnKill: func(sandboxID string) {
				log.Printf("opensandbox: sandbox %s killed on timeout", sandboxID)
				if opts.Store != nil {
					_ = opts.Store.UpdateSandboxSessionStatus(context.Background(), sandboxID, "stopped", nil)
				}
			},
		})
		defer sbRouter.Close()
		opts.Router = sbRouter
		log.Println("opensandbox: sandbox router initialized (rolling timeouts, auto-wake)")

		// Initialize subdomain reverse proxy
		if cfg.SandboxDomain != "" {
			sbProxy := proxy.New(cfg.SandboxDomain, mgr, sbRouter)
			opts.SandboxProxy = sbProxy
			opts.SandboxDomain = cfg.SandboxDomain
			log.Printf("opensandbox: subdomain proxy configured (*.%s)", cfg.SandboxDomain)
		}
	}

	// Set sandbox domain for API responses (works in both server and combined mode)
	if cfg.SandboxDomain != "" && cfg.SandboxDomain != "localhost" {
		opts.SandboxDomain = cfg.SandboxDomain
		log.Printf("opensandbox: sandbox domain configured (%s)", cfg.SandboxDomain)
	}

	// Initialize Redis worker registry in server mode
	var redisRegistry *controlplane.RedisWorkerRegistry
	if cfg.Mode == "server" && cfg.RedisURL != "" {
		var err error
		redisRegistry, err = controlplane.NewRedisWorkerRegistry(cfg.RedisURL)
		if err != nil {
			log.Fatalf("failed to connect to Redis: %v", err)
		}
		redisRegistry.Start()
		defer redisRegistry.Stop()
		opts.WorkerRegistry = redisRegistry
		log.Println("opensandbox: Redis worker registry started")
	}

	// Initialize EC2 compute pool + autoscaler (server mode with AWS configured)
	if cfg.Mode == "server" && cfg.EC2AMI != "" && redisRegistry != nil {
		ec2Pool, err := compute.NewEC2Pool(compute.EC2PoolConfig{
			Region:             cfg.S3Region, // reuse S3 region (same AWS account)
			AccessKeyID:        cfg.S3AccessKeyID,
			SecretAccessKey:    cfg.S3SecretAccessKey,
			AMI:                cfg.EC2AMI,
			InstanceType:       cfg.EC2InstanceType,
			SubnetID:           cfg.EC2SubnetID,
			SecurityGroupID:    cfg.EC2SecurityGroupID,
			KeyName:            cfg.EC2KeyName,
			IAMInstanceProfile: cfg.EC2IAMInstanceProfile,
			SecretsARN:         cfg.SecretsARN,
		})
		if err != nil {
			log.Fatalf("opensandbox: failed to create EC2 pool: %v", err)
		}

		scaler := controlplane.NewScaler(controlplane.ScalerConfig{
			Pool:        ec2Pool,
			Registry:    redisRegistry,
			WorkerImage: cfg.EC2WorkerImage,
			Cooldown:    time.Duration(cfg.ScaleCooldownSec) * time.Second,
		})
		scaler.Start()
		defer scaler.Stop()
		log.Printf("opensandbox: EC2 autoscaler started (ami=%s, type=%s)", cfg.EC2AMI, cfg.EC2InstanceType)
	}

	// Initialize control plane subdomain proxy (server mode only).
	// Routes *.workers.opensandbox.ai requests to the correct worker
	// by looking up sandbox → worker mapping in PG + Redis registry.
	if cfg.Mode == "server" && cfg.SandboxDomain != "" && opts.Store != nil && redisRegistry != nil {
		cpProxy := proxy.NewControlPlaneProxy(cfg.SandboxDomain, opts.Store, redisRegistry)
		opts.ControlPlaneProxy = cpProxy
		log.Printf("opensandbox: control plane subdomain proxy configured (*.%s)", cfg.SandboxDomain)
	}

	// Initialize Cloudflare client for custom hostnames (if configured)
	if cfg.CFAPIToken != "" && cfg.CFZoneID != "" {
		opts.CFClient = cloudflare.NewClient(cfg.CFAPIToken, cfg.CFZoneID)
		log.Println("opensandbox: Cloudflare custom hostnames configured")
	}

	// Create API server
	server := api.NewServer(mgr, ptyMgr, cfg.APIKey, opts)

	// Start NATS sync consumer if both PG and NATS are configured
	if opts.Store != nil && cfg.NATSURL != "" {
		consumer, err := db.NewSyncConsumer(opts.Store, cfg.NATSURL)
		if err != nil {
			log.Printf("opensandbox: NATS sync consumer not available: %v (continuing without)", err)
		} else {
			if err := consumer.Start(); err != nil {
				log.Printf("opensandbox: failed to start NATS sync consumer: %v", err)
			} else {
				defer consumer.Stop()
				log.Println("opensandbox: NATS sync consumer started")
			}
		}
	}

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("opensandbox: starting server on %s (mode=%s)", addr, cfg.Mode)

	go func() {
		if err := server.Start(addr); err != nil {
			log.Printf("server error: %v", err)
		}
	}()

	<-quit
	log.Println("opensandbox: shutting down...")
	if err := server.Close(); err != nil {
		log.Printf("error closing server: %v", err)
	}
}
