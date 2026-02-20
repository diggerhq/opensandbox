package config

import (
	"fmt"
	"os"
	"strconv"
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
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
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
