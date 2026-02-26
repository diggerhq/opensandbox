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

	// ECR for template images
	ECRRegistry   string // e.g. "086971355112.dkr.ecr.us-east-2.amazonaws.com"
	ECRRepository string // e.g. "opensandbox-templates"

	// Sandbox resource defaults (overridable per-sandbox via API)
	DefaultSandboxMemoryMB int // default RAM per sandbox (MB), default 1024
	DefaultSandboxCPUs     int // default vCPUs per sandbox, default 1
	DefaultSandboxDiskMB   int // default disk quota per sandbox (MB), 0 = no quota

	// Firecracker microVM configuration (worker mode)
	FirecrackerBin string // Path to firecracker binary (default: "firecracker")
	KernelPath     string // Path to vmlinux kernel (default: $DataDir/firecracker/vmlinux-arm64)
	ImagesDir      string // Path to base rootfs images (default: $DataDir/firecracker/images/)

	// AWS EC2 compute pool (server mode only — for auto-scaling worker machines)
	EC2AMI             string // Custom AMI with Firecracker pre-installed
	EC2InstanceType    string // e.g. "c7gd.metal", "r6gd.metal", "r7gd.metal"
	EC2SubnetID        string // VPC subnet for worker instances
	EC2SecurityGroupID string // Security group (allow 8080, 9090, 9091)
	EC2KeyName             string // SSH key pair name (for debugging)
	EC2WorkerImage         string // Docker image for containerized workers
	EC2IAMInstanceProfile  string // IAM instance profile for worker instances (Secrets Manager + S3)

	// Autoscaler
	ScaleCooldownSec int // Cooldown between scale-up actions (seconds), default 300

	// AWS Secrets Manager — if set, secrets are fetched at startup using IAM credentials.
	// The secret should be a JSON object with keys matching env var names (e.g. OPENSANDBOX_JWT_SECRET).
	// Env vars take precedence over secret values (for local overrides).
	SecretsARN string
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

		ECRRegistry:   os.Getenv("OPENSANDBOX_ECR_REGISTRY"),
		ECRRepository: envOrDefault("OPENSANDBOX_ECR_REPOSITORY", "opensandbox-templates"),

		DefaultSandboxMemoryMB: envOrDefaultInt("OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB", 1024),
		DefaultSandboxCPUs:     envOrDefaultInt("OPENSANDBOX_DEFAULT_SANDBOX_CPUS", 1),
		DefaultSandboxDiskMB:   envOrDefaultInt("OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB", 0),

		FirecrackerBin: envOrDefault("OPENSANDBOX_FIRECRACKER_BIN", "firecracker"),
		KernelPath:     os.Getenv("OPENSANDBOX_KERNEL_PATH"),     // default derived from DataDir
		ImagesDir:      os.Getenv("OPENSANDBOX_IMAGES_DIR"),      // default derived from DataDir

		EC2AMI:             os.Getenv("OPENSANDBOX_EC2_AMI"),
		EC2InstanceType:    envOrDefault("OPENSANDBOX_EC2_INSTANCE_TYPE", "c7gd.metal"),
		EC2SubnetID:        os.Getenv("OPENSANDBOX_EC2_SUBNET_ID"),
		EC2SecurityGroupID: os.Getenv("OPENSANDBOX_EC2_SECURITY_GROUP_ID"),
		EC2KeyName:         os.Getenv("OPENSANDBOX_EC2_KEY_NAME"),
		EC2WorkerImage:         envOrDefault("OPENSANDBOX_EC2_WORKER_IMAGE", "opensandbox-worker:latest"),
		EC2IAMInstanceProfile:  os.Getenv("OPENSANDBOX_EC2_IAM_INSTANCE_PROFILE"),

		ScaleCooldownSec: envOrDefaultInt("OPENSANDBOX_SCALE_COOLDOWN_SEC", 300),

		SecretsARN: os.Getenv("OPENSANDBOX_SECRETS_ARN"),
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
