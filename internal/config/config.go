package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Config holds all configuration for the opensandbox server.
type Config struct {
	Port       int
	APIKey     string
	WorkerAddr string
	Mode       string // "server", "worker", "combined"
	LogLevel   string

	// Database
	DatabaseURL string // PostgreSQL connection string
	DataDir     string // Local data directory for SQLite files

	// Auth
	JWTSecret string // Shared secret for sandbox-scoped JWTs

	// NATS
	NATSURL string // NATS server URL

	// Worker identity
	Region   string // Region identifier (e.g., "iad", "ams")
	WorkerID string // Unique worker ID (e.g., "w-iad-1")
	HTTPAddr string // Public HTTP address for direct SDK access

	// WorkOS
	WorkOSAPIKey       string
	WorkOSClientID     string
	WorkOSRedirectURI  string
	WorkOSCookieDomain string
	WorkOSFrontendURL  string // e.g. "http://localhost:3000" for Vite dev

	// Redis (Upstash) for worker discovery
	RedisURL string

	// Worker capacity
	MaxCapacity int

	// Sandbox subdomain routing
	SandboxDomain string // Base domain for sandbox subdomains (e.g., "workers.opensandbox.dev", default "localhost")

	// S3-compatible object storage for checkpoint hibernation
	S3Endpoint        string // e.g. "https://<account>.r2.cloudflarestorage.com"
	S3Bucket          string // e.g. "opensandbox-checkpoints"
	S3Region          string // defaults to Region if not set
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3ForcePathStyle  bool // true for R2/MinIO

	// Sandbox resource defaults (overridable per-sandbox via API)
	DefaultSandboxMemoryMB int // default RAM per sandbox (MB), default 1024
	DefaultSandboxCPUs     int // default vCPUs per sandbox, default 1
	DefaultSandboxDiskMB   int // default disk quota per sandbox (MB), 0 = no quota

	// QEMU VM configuration (worker mode)
	KernelPath string // Path to vmlinux kernel
	ImagesDir  string // Path to base rootfs images
	QEMUBin    string // Path to qemu-system binary (default: "qemu-system-x86_64")

	// AWS EC2 compute pool (server mode only — for auto-scaling worker machines)
	EC2AMI             string // Custom AMI for worker instances
	EC2InstanceType    string // e.g. "c7gd.metal", "r6gd.metal", "r7gd.metal"
	EC2SubnetID        string // VPC subnet for worker instances
	EC2SecurityGroupID string // Security group (allow 8080, 9090, 9091)
	EC2KeyName             string // SSH key pair name (for debugging)
	EC2WorkerImage         string // Docker image for containerized workers
	EC2IAMInstanceProfile  string // IAM instance profile for worker instances (Secrets Manager + S3)
	EC2SSMParameterName    string // SSM parameter name for dynamic AMI ID (e.g. /opensandbox/prod/worker-ami-id)

	// Azure compute pool (server mode — for auto-scaling worker VMs)
	AzureSubscriptionID string // Azure subscription ID
	AzureResourceGroup  string // Resource group for worker VMs
	AzureVMSize         string // e.g. "Standard_D16s_v5"
	AzureImageID        string // Custom image ID or URN
	AzureSubnetID       string // Full resource ID of the VNet subnet
	AzureSSHPublicKey   string // SSH public key for worker VMs
	AzureKeyVaultName   string // Key Vault name for dynamic image ID refresh (e.g. "opensandbox-prod")

	// Cloudflare (custom hostname for org sandbox domains)
	CFAPIToken string // Cloudflare API token with Custom Hostnames permission
	CFZoneID   string // Cloudflare zone ID for the shared zone (e.g. opencomputer.dev)

	// Autoscaler
	ScaleCooldownSec    int // Cooldown between scale-up actions (seconds), default 300
	MinWorkersPerRegion int // Minimum total workers per region, default 1
	MaxWorkersPerRegion int // Maximum workers per region (hard cap), default 10
	IdleReserveWorkers  int // Target idle workers for burst absorption, default 1

	// Stripe billing
	StripeSecretKey     string
	StripeWebhookSecret string
	StripeSuccessURL    string
	StripeCancelURL     string

	// Per-agent paywalled-feature prices (set in Stripe dashboard, referenced
	// by ID here). Empty = feature ungated on this deployment (dev mode).
	StripeTelegramAgentPriceID string

	// Segment analytics — if set, GB-minute usage events are shipped per org.
	SegmentWriteKey string

	// AWS Secrets Manager — if set, secrets are fetched at startup using IAM credentials.
	// The secret should be a JSON object with keys matching env var names (e.g. OPENSANDBOX_JWT_SECRET).
	// Env vars take precedence over secret values (for local overrides).
	SecretsARN string

	// Secret encryption key (hex-encoded 32 bytes / 64 hex chars) for encrypting
	// project secrets at rest in PostgreSQL. Required if using project secrets.
	SecretEncryptionKey string

	// Sentry error reporting. If SentryDSN is set, panics and captured errors
	// are shipped to Sentry. Environment defaults to Region.
	SentryDSN              string
	SentryEnvironment      string
	SentrySampleRate       float64 // 0.0–1.0, default 1.0 (capture every error)
	SentryTracesSampleRate float64 // 0.0–1.0, default 0.0 (tracing off)
}

// Load reads configuration from environment variables with sensible defaults.
// If OPENSANDBOX_SECRETS_ARN is set, secrets are fetched from AWS Secrets Manager
// first, then environment variables are applied on top (env vars take precedence).
func Load() (*Config, error) {
	// Fetch secrets from AWS Secrets Manager if configured.
	// This populates the process environment so subsequent os.Getenv calls pick them up.
	if arn := os.Getenv("OPENSANDBOX_SECRETS_ARN"); arn != "" {
		if err := loadSecretsManager(arn); err != nil {
			return nil, fmt.Errorf("failed to load secrets from %s: %w", arn, err)
		}
	}

	cfg := &Config{
		Port:       8080,
		APIKey:     os.Getenv("OPENSANDBOX_API_KEY"),
		WorkerAddr: envOrDefault("OPENSANDBOX_WORKER_ADDR", "localhost:9090"),
		Mode:       envOrDefault("OPENSANDBOX_MODE", "combined"),
		LogLevel:   envOrDefault("OPENSANDBOX_LOG_LEVEL", "info"),

		DatabaseURL: envOrDefault("OPENSANDBOX_DATABASE_URL", os.Getenv("DATABASE_URL")),
		DataDir:     envOrDefault("OPENSANDBOX_DATA_DIR", "/data/sandboxes"),
		JWTSecret:   os.Getenv("OPENSANDBOX_JWT_SECRET"),
		NATSURL:     envOrDefault("OPENSANDBOX_NATS_URL", "nats://localhost:4222"),
		Region:      envOrDefault("OPENSANDBOX_REGION", "local"),
		WorkerID:    envOrDefault("OPENSANDBOX_WORKER_ID", "w-local-1"),
		HTTPAddr:    envOrDefault("OPENSANDBOX_HTTP_ADDR", "http://localhost:8080"),

		WorkOSAPIKey:       os.Getenv("WORKOS_API_KEY"),
		WorkOSClientID:     os.Getenv("WORKOS_CLIENT_ID"),
		WorkOSRedirectURI:  envOrDefault("WORKOS_REDIRECT_URI", "http://localhost:8080/auth/callback"),
		WorkOSCookieDomain: os.Getenv("WORKOS_COOKIE_DOMAIN"),
		WorkOSFrontendURL:  os.Getenv("WORKOS_FRONTEND_URL"),

		RedisURL:    os.Getenv("OPENSANDBOX_REDIS_URL"),

		MaxCapacity: envOrDefaultInt("OPENSANDBOX_MAX_CAPACITY", 50),

		SandboxDomain: envOrDefault("OPENSANDBOX_SANDBOX_DOMAIN", "localhost"),

		S3Endpoint:        os.Getenv("OPENSANDBOX_S3_ENDPOINT"),
		S3Bucket:          os.Getenv("OPENSANDBOX_S3_BUCKET"),
		S3Region:          os.Getenv("OPENSANDBOX_S3_REGION"),
		S3AccessKeyID:     os.Getenv("OPENSANDBOX_S3_ACCESS_KEY_ID"),
		S3SecretAccessKey: os.Getenv("OPENSANDBOX_S3_SECRET_ACCESS_KEY"),
		S3ForcePathStyle:  os.Getenv("OPENSANDBOX_S3_FORCE_PATH_STYLE") == "true",

		DefaultSandboxMemoryMB: envOrDefaultInt("OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB", 256),
		DefaultSandboxCPUs:     envOrDefaultInt("OPENSANDBOX_DEFAULT_SANDBOX_CPUS", 1),
		DefaultSandboxDiskMB:   envOrDefaultInt("OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB", 0),

		KernelPath:     os.Getenv("OPENSANDBOX_KERNEL_PATH"),     // default derived from DataDir
		ImagesDir:      os.Getenv("OPENSANDBOX_IMAGES_DIR"),      // default derived from DataDir
		QEMUBin:        envOrDefault("OPENSANDBOX_QEMU_BIN", "qemu-system-x86_64"),

		EC2AMI:             os.Getenv("OPENSANDBOX_EC2_AMI"),
		EC2InstanceType:    envOrDefault("OPENSANDBOX_EC2_INSTANCE_TYPE", "c7gd.metal"),
		EC2SubnetID:        os.Getenv("OPENSANDBOX_EC2_SUBNET_ID"),
		EC2SecurityGroupID: os.Getenv("OPENSANDBOX_EC2_SECURITY_GROUP_ID"),
		EC2KeyName:         os.Getenv("OPENSANDBOX_EC2_KEY_NAME"),
		EC2WorkerImage:         envOrDefault("OPENSANDBOX_EC2_WORKER_IMAGE", "opensandbox-worker:latest"),
		EC2IAMInstanceProfile:  os.Getenv("OPENSANDBOX_EC2_IAM_INSTANCE_PROFILE"),
		EC2SSMParameterName:    os.Getenv("OPENSANDBOX_EC2_SSM_AMI_PARAM"),

		AzureSubscriptionID: os.Getenv("OPENSANDBOX_AZURE_SUBSCRIPTION_ID"),
		AzureResourceGroup:  os.Getenv("OPENSANDBOX_AZURE_RESOURCE_GROUP"),
		AzureVMSize:         envOrDefault("OPENSANDBOX_AZURE_VM_SIZE", "Standard_D16s_v5"),
		AzureImageID:        os.Getenv("OPENSANDBOX_AZURE_IMAGE_ID"),
		AzureSubnetID:       os.Getenv("OPENSANDBOX_AZURE_SUBNET_ID"),
		AzureSSHPublicKey:   os.Getenv("OPENSANDBOX_AZURE_SSH_PUBLIC_KEY"),
		AzureKeyVaultName:   os.Getenv("OPENSANDBOX_AZURE_KEY_VAULT_NAME"),

		CFAPIToken: os.Getenv("OPENSANDBOX_CF_API_TOKEN"),
		CFZoneID:   os.Getenv("OPENSANDBOX_CF_ZONE_ID"),

		ScaleCooldownSec:    envOrDefaultInt("OPENSANDBOX_SCALE_COOLDOWN_SEC", 300),
		MinWorkersPerRegion: envOrDefaultInt("OPENSANDBOX_MIN_WORKERS", 1),
		MaxWorkersPerRegion: envOrDefaultInt("OPENSANDBOX_MAX_WORKERS", 10),
		IdleReserveWorkers:  envOrDefaultInt("OPENSANDBOX_IDLE_RESERVE", 1),

		StripeSecretKey:            os.Getenv("STRIPE_SECRET_KEY"),
		StripeWebhookSecret:        os.Getenv("STRIPE_WEBHOOK_SECRET"),
		StripeTelegramAgentPriceID: os.Getenv("STRIPE_TELEGRAM_AGENT_PRICE_ID"),
		StripeSuccessURL:    envOrDefault("STRIPE_SUCCESS_URL", "http://localhost:3000/billing?success=true"),
		StripeCancelURL:     envOrDefault("STRIPE_CANCEL_URL", "http://localhost:3000/billing?cancelled=true"),

		SegmentWriteKey: os.Getenv("SEGMENT_WRITE_KEY"),

		SecretsARN: os.Getenv("OPENSANDBOX_SECRETS_ARN"),

		SecretEncryptionKey: os.Getenv("OPENSANDBOX_SECRET_ENCRYPTION_KEY"),

		SentryDSN:              os.Getenv("OPENSANDBOX_SENTRY_DSN"),
		SentryEnvironment:      os.Getenv("OPENSANDBOX_SENTRY_ENVIRONMENT"),
		SentrySampleRate:       envOrDefaultFloat("OPENSANDBOX_SENTRY_SAMPLE_RATE", 1.0),
		SentryTracesSampleRate: envOrDefaultFloat("OPENSANDBOX_SENTRY_TRACES_SAMPLE_RATE", 0.0),
	}

	if cfg.SentryEnvironment == "" {
		cfg.SentryEnvironment = cfg.Region
	}

	// Default S3 region to worker region for same-region storage
	if cfg.S3Region == "" {
		cfg.S3Region = cfg.Region
	}

	if portStr := os.Getenv("OPENSANDBOX_PORT"); portStr != "" {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid OPENSANDBOX_PORT %q: %w", portStr, err)
		}
		cfg.Port = port
	}

	return cfg, nil
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrDefaultInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrDefaultFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

// loadSecretsManager fetches a JSON secret from AWS Secrets Manager and sets
// any values as environment variables (only if not already set, so explicit
// env vars always win). Uses the default AWS credential chain (IAM instance
// profile on EC2, or ~/.aws/credentials locally).
func loadSecretsManager(arn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Extract region from ARN: arn:aws:secretsmanager:REGION:ACCOUNT:secret:NAME
	var opts []func(*awsconfig.LoadOptions) error
	if parts := strings.Split(arn, ":"); len(parts) >= 4 && parts[3] != "" {
		opts = append(opts, awsconfig.WithRegion(parts[3]))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	client := secretsmanager.NewFromConfig(awsCfg)
	result, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &arn,
	})
	if err != nil {
		return fmt.Errorf("GetSecretValue: %w", err)
	}

	if result.SecretString == nil {
		return fmt.Errorf("secret %s has no string value", arn)
	}

	var secrets map[string]string
	if err := json.Unmarshal([]byte(*result.SecretString), &secrets); err != nil {
		return fmt.Errorf("parse secret JSON: %w", err)
	}

	applied := 0
	for key, value := range secrets {
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
			applied++
		}
	}

	log.Printf("config: loaded %d secrets from Secrets Manager (%d keys in secret, env overrides take precedence)", applied, len(secrets))
	return nil
}
