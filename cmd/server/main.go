package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"os/signal"
	"syscall"

	"time"

	"github.com/opensandbox/opensandbox/internal/api"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/billing"
	"github.com/opensandbox/opensandbox/internal/cloudflare"
	"github.com/opensandbox/opensandbox/internal/compute"
	"github.com/opensandbox/opensandbox/internal/config"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/crypto"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/observability"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
)

// ServerVersion is the control plane binary version, set at build time via -ldflags.
var ServerVersion = "dev"

func main() {
	// Load secrets from Azure Key Vault if configured (before config.Load reads env vars).
	if err := config.LoadSecretsFromKeyVault(); err != nil {
		log.Fatalf("failed to load secrets from Key Vault: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Sentry error reporting — no-op if OPENSANDBOX_SENTRY_DSN is unset.
	flushSentry := observability.Init(cfg, "control-plane", ServerVersion)
	defer flushSentry()
	defer observability.Recover()

	ctx := context.Background()

	// Server mode delegates sandbox management to workers via gRPC.
	// There is no local sandbox manager on the server.
	var mgr sandbox.Manager
	var ptyMgr *sandbox.PTYManager
	log.Printf("opensandbox: server mode — delegating sandbox management to workers via gRPC")

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

		// Configure encryption for project secrets.
		// Supports key rotation: OPENSANDBOX_SECRET_ENCRYPTION_KEY is the primary key,
		// OPENSANDBOX_SECRET_ENCRYPTION_KEY_V1..V9 are previous keys for decrypting
		// legacy secrets during rotation.
		if cfg.SecretEncryptionKey != "" {
			ring, err := crypto.NewKeyRingFromEnv()
			if err != nil {
				log.Fatalf("invalid encryption key config: %v", err)
			}
			if ring != nil {
				store.SetEncryptor(ring.AsEncryptor())
				log.Printf("opensandbox: project secret encryption configured (key version %d)", ring.PrimaryVersion())
			}
		}

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

	// Set sandbox domain for API responses
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
		opts.RedisClient = redisRegistry.RedisClient()
		log.Println("opensandbox: Redis worker registry started")

		// Create sandbox API proxy for routing data-plane requests to workers
		if opts.Store != nil && opts.JWTIssuer != nil {
			opts.SandboxAPIProxy = proxy.NewSandboxAPIProxy(opts.Store, redisRegistry, opts.JWTIssuer)
			log.Println("opensandbox: sandbox API proxy enabled (data-plane requests proxied to workers)")
		}
	}

	// Hoisted at function scope so the per-sandbox autoscaler (created
	// later, after the API server) can consult IsLeader() each tick — keeps
	// a single elector authoritative across both the cluster scaler and the
	// per-sandbox autoscaler in HA setups. nil when there's no compute pool
	// (combined / dev mode) — autoscaler then runs unconditionally.
	var leaderElector *controlplane.LeaderElector

	// Initialize compute pool + autoscaler (server mode)
	if cfg.Mode == "server" && redisRegistry != nil {
		var pool compute.Pool
		var poolName string

		if cfg.AzureSubscriptionID != "" && (cfg.AzureImageID != "" || cfg.AzureKeyVaultName != "") {
			// Build worker env template — new VMs get this via cloud-init.
			// GRPC_ADVERTISE, HTTP_ADDR, and WORKER_ID are patched by cloud-init
			// with the VM's actual private IP and hostname.
			// Workers need to reach Postgres/Redis on the control plane's private IP,
			// not localhost. Replace localhost with the control plane's IP.
			cpIP := os.Getenv("OPENSANDBOX_CONTROLPLANE_IP")
			workerDBURL := cfg.DatabaseURL
			workerRedisURL := cfg.RedisURL
			if cpIP != "" {
				workerDBURL = strings.ReplaceAll(workerDBURL, "localhost", cpIP)
				workerDBURL = strings.ReplaceAll(workerDBURL, "127.0.0.1", cpIP)
				workerRedisURL = strings.ReplaceAll(workerRedisURL, "localhost", cpIP)
				workerRedisURL = strings.ReplaceAll(workerRedisURL, "127.0.0.1", cpIP)
			}

			workerEnv := fmt.Sprintf(
				"OPENSANDBOX_MODE=worker\n"+
					"OPENSANDBOX_VM_BACKEND=qemu\n"+
					"OPENSANDBOX_QEMU_BIN=qemu-system-x86_64\n"+
					"OPENSANDBOX_DATA_DIR=/data/sandboxes\n"+
					"OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux\n"+
					"OPENSANDBOX_IMAGES_DIR=/data/firecracker/images\n"+
					"OPENSANDBOX_GRPC_ADVERTISE=PLACEHOLDER:9090\n"+
					"OPENSANDBOX_HTTP_ADDR=http://PLACEHOLDER:8081\n"+
					"OPENSANDBOX_JWT_SECRET=%s\n"+
					"OPENSANDBOX_WORKER_ID=PLACEHOLDER\n"+
					"OPENSANDBOX_REGION=%s\n"+
					"OPENSANDBOX_MAX_CAPACITY=%d\n"+
					"OPENSANDBOX_PORT=8081\n"+
					"OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=%d\n"+
					"OPENSANDBOX_DEFAULT_SANDBOX_CPUS=%d\n"+
					"OPENSANDBOX_DATABASE_URL=%s\n"+
					"OPENSANDBOX_REDIS_URL=%s\n"+
					"OPENSANDBOX_S3_BUCKET=%s\n"+
					"OPENSANDBOX_S3_REGION=%s\n"+
					"OPENSANDBOX_S3_ENDPOINT=%s\n"+
					"OPENSANDBOX_S3_ACCESS_KEY_ID=%s\n"+
					"OPENSANDBOX_S3_SECRET_ACCESS_KEY=%s\n"+
					"OPENSANDBOX_S3_FORCE_PATH_STYLE=%v\n"+
					"OPENSANDBOX_SANDBOX_DOMAIN=%s\n"+
					"OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB=%d\n"+
					"OPENSANDBOX_AZURE_KEY_VAULT_NAME=%s\n"+
					"SEGMENT_WRITE_KEY=%s\n"+
					"AXIOM_INGEST_TOKEN=%s\n"+
					"AXIOM_DATASET=%s\n",
				cfg.JWTSecret,
				cfg.Region,
				cfg.MaxCapacity,
				cfg.DefaultSandboxMemoryMB,
				cfg.DefaultSandboxCPUs,
				workerDBURL,
				workerRedisURL,
				cfg.S3Bucket,
				cfg.S3Region,
				cfg.S3Endpoint,
				cfg.S3AccessKeyID,
				cfg.S3SecretAccessKey,
				cfg.S3ForcePathStyle,
				cfg.SandboxDomain,
				cfg.DefaultSandboxDiskMB,
				cfg.AzureKeyVaultName,
				cfg.SegmentWriteKey,
				cfg.AxiomIngestToken,
				cfg.AxiomDataset,
			)
			workerEnvB64 := base64.StdEncoding.EncodeToString([]byte(workerEnv))

			azPool, err := compute.NewAzurePool(compute.AzurePoolConfig{
				SubscriptionID:   cfg.AzureSubscriptionID,
				ResourceGroup:    cfg.AzureResourceGroup,
				Region:           cfg.Region,
				VMSize:           cfg.AzureVMSize,
				ImageID:          cfg.AzureImageID,
				SubnetID:         cfg.AzureSubnetID,
				SSHPublicKey:     cfg.AzureSSHPublicKey,
				KeyVaultName:     cfg.AzureKeyVaultName,
				WorkerIdentityID: cfg.AzureWorkerIdentityID,
				WorkerEnvBase64:  workerEnvB64,
			})
			if err != nil {
				log.Fatalf("opensandbox: failed to create Azure pool: %v", err)
			}
			// If image not set statically but Key Vault is configured, fetch initial image
			if cfg.AzureImageID == "" && cfg.AzureKeyVaultName != "" {
				imgID, version, kvErr := azPool.RefreshAMI(context.Background())
				if kvErr != nil {
					log.Fatalf("opensandbox: Azure image not set and Key Vault fetch failed: %v", kvErr)
				}
				log.Printf("opensandbox: Azure image from Key Vault: %s (version=%s)", imgID, version)
			}
			pool = azPool
			poolName = fmt.Sprintf("Azure (size=%s, image=%s, keyvault=%s)", cfg.AzureVMSize, cfg.AzureImageID, cfg.AzureKeyVaultName)
		} else if cfg.EC2AMI != "" || cfg.EC2SSMParameterName != "" {
			// AWS EC2 compute pool (AMI from config or dynamically from SSM)
			ec2Pool, err := compute.NewEC2Pool(compute.EC2PoolConfig{
				Region:             cfg.S3Region,
				AccessKeyID:        cfg.S3AccessKeyID,
				SecretAccessKey:    cfg.S3SecretAccessKey,
				AMI:                cfg.EC2AMI,
				InstanceType:       cfg.EC2InstanceType,
				SubnetID:           cfg.EC2SubnetID,
				SecurityGroupID:    cfg.EC2SecurityGroupID,
				KeyName:            cfg.EC2KeyName,
				IAMInstanceProfile: cfg.EC2IAMInstanceProfile,
				SecretsARN:         cfg.SecretsARN,
				SSMParameterName:   cfg.EC2SSMParameterName,
			})
			if err != nil {
				log.Fatalf("opensandbox: failed to create EC2 pool: %v", err)
			}
			// If AMI not set statically but SSM is configured, fetch initial AMI from SSM
			if cfg.EC2AMI == "" && cfg.EC2SSMParameterName != "" {
				amiID, version, ssmErr := ec2Pool.RefreshAMI(context.Background())
				if ssmErr != nil {
					log.Fatalf("opensandbox: EC2 AMI not set and SSM fetch failed: %v", ssmErr)
				}
				log.Printf("opensandbox: EC2 AMI from SSM: %s (version=%s)", amiID, version)
			}
			pool = ec2Pool
			poolName = fmt.Sprintf("EC2 (ami=%s, type=%s, ssm=%s)", cfg.EC2AMI, cfg.EC2InstanceType, cfg.EC2SSMParameterName)
		}

		if pool != nil {
			scalerState := controlplane.NewRedisScalerState(redisRegistry.RedisClient())
			scaler := controlplane.NewScaler(controlplane.ScalerConfig{
				Pool:        pool,
				Registry:    redisRegistry,
				Store:       opts.Store,
				StateStore:  scalerState,
				WorkerImage: cfg.EC2WorkerImage,
				Cooldown:    time.Duration(cfg.ScaleCooldownSec) * time.Second,
				MinWorkers:  cfg.MinWorkersPerRegion,
				MaxWorkers:  cfg.MaxWorkersPerRegion,
				IdleReserve: cfg.IdleReserveWorkers,
			})
			defer scaler.Stop()

			// Leader election: only the leader runs the scaler. The
			// per-sandbox autoscaler (created later) consults this same
			// elector via IsLeader() to skip ticks when not leader, so we
			// don't double-fire scale decisions across CPs in HA setups.
			leaderElector = controlplane.NewLeaderElector(redisRegistry.RedisClient(), cfg.WorkerID)
			leaderElector.OnBecomeLeader(func() {
				scaler.Start()
				log.Printf("opensandbox: became leader, autoscaler started (%s)", poolName)
			})
			leaderElector.OnLoseLeadership(func() {
				scaler.Stop()
				log.Println("opensandbox: lost leadership, autoscaler stopped")
			})
			leaderElector.Start()
			defer leaderElector.Stop()
			log.Printf("opensandbox: leader election started (instance=%s)", leaderElector.InstanceID())
		}
	}

	// Background maintenance tasks
	if opts.Store != nil && redisRegistry != nil {
		observability.Go("maintenance-loop", func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				ctx := context.Background()

				// Stale migration recovery
				recovered, err := opts.Store.RecoverStaleMigrations(ctx, 60*time.Second)
				if err != nil {
					log.Printf("maintenance: stale migration recovery error: %v", err)
					observability.CaptureError(err, "area", "maintenance", "op", "recover_stale_migrations")
				} else if recovered > 0 {
					log.Printf("maintenance: reverted %d stale migrations", recovered)
				}

				// DB/worker reconciliation: mark sandboxes on dead workers as error
				liveWorkers := make(map[string]bool)
				for _, w := range redisRegistry.GetAllWorkers() {
					liveWorkers[w.ID] = true
				}
				orphaned, err := opts.Store.MarkOrphanedSandboxes(ctx, liveWorkers)
				if err != nil {
					log.Printf("maintenance: orphan reconciliation error: %v", err)
					observability.CaptureError(err, "area", "maintenance", "op", "mark_orphaned_sandboxes")
				} else if orphaned > 0 {
					log.Printf("maintenance: marked %d sandboxes as error (worker lost)", orphaned)
				}
			}
		})
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

	// Initialize Stripe billing (if configured)
	var stripeClient *billing.StripeClient
	if cfg.StripeSecretKey != "" {
		stripeClient = billing.NewStripeClient(cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.StripeSuccessURL, cfg.StripeCancelURL)
		stripeClient.TelegramAgentPriceID = cfg.StripeTelegramAgentPriceID
		if err := stripeClient.EnsureProducts(); err != nil {
			log.Printf("opensandbox: Stripe product setup failed: %v (billing may not work)", err)
		} else {
			log.Println("opensandbox: Stripe billing configured")
		}
		opts.StripeClient = stripeClient
	}

	// Create API server
	server := api.NewServer(mgr, ptyMgr, cfg.APIKey, opts)

	// Wire Axiom read-only token for the sandbox session logs API.
	// Token never leaves this process; the UI proxies its queries through
	// /api/sandboxes/:id/logs. Empty token disables the endpoint (503).
	server.SetAxiomQueryConfig(cfg.AxiomQueryToken, cfg.AxiomDataset)
	if cfg.AxiomQueryToken != "" {
		log.Printf("opensandbox: sandbox session logs read API enabled (dataset=%s)", cfg.AxiomDataset)
	}

	// Per-sandbox autoscaler. Tier-aligned (1/4/8/16 GB), opt-in per
	// sandbox via PUT /api/sandboxes/:id/autoscale.
	//
	// Leader-gated when an elector exists. With HA (multiple CPs) we don't
	// want two instances both reading stats and double-firing scale events.
	// SetSandboxLimits is technically idempotent for memory targets, but
	// the cooldown CAS races and the cooldown timestamp gets clobbered if
	// both CPs UPDATE — see ClaimAutoscaleEvent. Gating on the leader is
	// cheaper than relying on the CAS alone. When there's no elector
	// (single-CP / no cloud pool), isLeader is nil and the loop runs
	// unconditionally.
	if opts.Store != nil && redisRegistry != nil {
		var isLeader func() bool
		if leaderElector != nil {
			isLeader = leaderElector.IsLeader
		}
		autoscaler := controlplane.NewAutoscaler(opts.Store, redisRegistry, api.NewAutoscalerSetter(server), isLeader)
		autoscaler.Start(ctx)
		defer autoscaler.Stop()
		log.Println("opensandbox: per-sandbox autoscaler started (interval=30s, leader-gated)")
	}

	// Start usage reporter — reports Pro org usage to Stripe and deducts
	// free-tier trial credits (force-hibernates on empty) every 5 min.
	// redisRegistry may be nil in combined mode; reporter tolerates that by
	// logging instead of hibernating when free credits run out.
	if opts.Store != nil && stripeClient != nil {
		var workers billing.WorkerClientSource
		if redisRegistry != nil {
			workers = redisRegistry
		}
		reporter := billing.NewUsageReporter(opts.Store, stripeClient, workers)
		reporter.Start()
		defer reporter.Stop()
		log.Println("opensandbox: usage reporter started (interval=5m)")
	}

	// Phase-2 capacity allocator. Writes outbox rows for unified-mode
	// pro orgs after each settled bucket. Allocator skips legacy and
	// free orgs (see ListAllocatorCandidates); rollback is by
	// reverting the deploy.
	if opts.Store != nil {
		allocOpts := billing.CapacityReconcilerOpts{
			Interval: getDurationEnv("CAPACITY_ALLOCATOR_INTERVAL", 5*time.Minute),
			Settle:   getDurationEnv("CAPACITY_ALLOCATOR_SETTLE", 30*time.Minute),
			Lookback: getDurationEnv("CAPACITY_ALLOCATOR_LOOKBACK", 24*time.Hour),
			Limit:    getIntEnv("CAPACITY_ALLOCATOR_BATCH_LIMIT", 500),
		}
		allocator := billing.NewCapacityReconciler(opts.Store, allocOpts)
		allocator.Start()
		defer allocator.Stop()
		log.Printf("opensandbox: capacity allocator started (interval=%s, settle=%s, lookback=%s)",
			allocOpts.Interval, allocOpts.Settle, allocOpts.Lookback)
	}

	// Phase-3 billable-events sender. Ships pending outbox rows to
	// Stripe via meter events for orgs in `billing_mode='unified'`
	// with a Stripe customer ID. New orgs default to unified per
	// migration 031; existing orgs are pinned to legacy and stay
	// untouched on UsageReporter. Idempotency is per-row via
	// `billable_events.id` as Stripe meter event Identifier.
	if opts.Store != nil && stripeClient != nil {
		senderOpts := billing.BillableEventsSenderOpts{
			Interval: getDurationEnv("BILLABLE_EVENTS_SENDER_INTERVAL", 5*time.Minute),
			Batch:    getIntEnv("BILLABLE_EVENTS_SENDER_BATCH", 200),
		}
		sender := billing.NewBillableEventsSender(opts.Store, stripeClient, senderOpts)
		sender.Start()
		defer sender.Stop()
		log.Printf("opensandbox: billable events sender started (interval=%s, batch=%d)",
			senderOpts.Interval, senderOpts.Batch)
	}

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

	// Mark server as ready to accept traffic
	server.SetReady()
	log.Println("opensandbox: server ready")

	<-quit
	log.Println("opensandbox: shutting down...")

	// Phase 1: Mark not ready so load balancer stops sending traffic
	server.SetNotReady()
	log.Println("opensandbox: readiness set to false, waiting 5s for LB to detect...")
	time.Sleep(5 * time.Second)

	// Phase 2: Drain in-flight HTTP requests (25s timeout)
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer drainCancel()
	if err := server.Shutdown(drainCtx); err != nil {
		log.Printf("opensandbox: graceful shutdown error: %v, forcing close", err)
		server.Close()
	}
	log.Println("opensandbox: server stopped")
}

// getDurationEnv reads a Go duration string (e.g. "5m", "30m", "24h")
// from env or returns the default. Used for the capacity allocator
// knobs which are off the hot path of config.Load().
func getDurationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Printf("opensandbox: invalid duration in %s=%q, using default %s", key, v, def)
	}
	return def
}

func getIntEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		log.Printf("opensandbox: invalid int in %s=%q, using default %d", key, v, def)
	}
	return def
}
