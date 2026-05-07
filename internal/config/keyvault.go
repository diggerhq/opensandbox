// Package config provides configuration loading from Azure Key Vault.
//
// If SECRETS_VAULT_NAME is set, LoadSecretsFromKeyVault fetches all secrets
// from the vault and maps them to environment variables. The mapping is:
//
//	Key Vault secret name    →  Environment variable
//	server-database-url      →  OPENSANDBOX_DATABASE_URL
//	server-jwt-secret        →  OPENSANDBOX_JWT_SECRET
//	worker-s3-secret-key     →  OPENSANDBOX_S3_SECRET_ACCESS_KEY
//	...etc
//
// Secrets already set in the environment are NOT overwritten — env vars take
// precedence over Key Vault. This allows local overrides for development.
//
// Authentication uses Azure Default Credential (Managed Identity on VMs,
// CLI credentials locally). No explicit credentials needed.
package config

import (
	"context"
	"os"
	"time"

	"github.com/opensandbox/opensandbox/internal/secrets"
)

// kvMapping maps Key Vault entry names to environment variable names. Despite
// the historical "secret" terminology this includes both real secrets (DB
// passwords, JWT keys) and non-secret cell config (region, cell_id, capacity
// tuning). The principle: only bootstrap pointers + per-VM identity stay in
// the env file; everything else is in KV so a cell's configuration is one
// source-of-truth rather than scattered across N worker.env files.
//
// Entries not in this map are silently ignored — the allowlist is the
// safety guard preventing a stray vault entry from accidentally shadowing
// an unrelated env var (e.g., PATH).
var kvMapping = map[string]string{
	// Server secrets
	"server-database-url":           "OPENSANDBOX_DATABASE_URL",
	"server-redis-url":              "OPENSANDBOX_REDIS_URL",
	"server-jwt-secret":             "OPENSANDBOX_JWT_SECRET",
	"server-api-key":                "OPENSANDBOX_API_KEY",
	"server-secret-encryption-key":  "OPENSANDBOX_SECRET_ENCRYPTION_KEY",
	"server-workos-api-key":         "WORKOS_API_KEY",
	"server-workos-client-id":       "WORKOS_CLIENT_ID",
	"server-cf-api-token":           "OPENSANDBOX_CF_API_TOKEN",
	"server-cf-zone-id":             "OPENSANDBOX_CF_ZONE_ID",
	"server-stripe-secret-key":      "STRIPE_SECRET_KEY",
	"server-stripe-webhook-secret":  "STRIPE_WEBHOOK_SECRET",
	"server-sentry-dsn":             "OPENSANDBOX_SENTRY_DSN",

	// Worker secrets
	"worker-jwt-secret":    "OPENSANDBOX_JWT_SECRET",
	"worker-database-url":  "OPENSANDBOX_DATABASE_URL",
	"worker-redis-url":     "OPENSANDBOX_REDIS_URL",
	"worker-s3-access-key": "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"worker-s3-secret-key": "OPENSANDBOX_S3_SECRET_ACCESS_KEY",
	"worker-sentry-dsn":    "OPENSANDBOX_SENTRY_DSN",

	// Worker per-cell config (non-secret but cell-scoped — every worker in the
	// cell shares these, so KV is the single source of truth)
	"worker-region":                       "OPENSANDBOX_REGION",
	"worker-cell-id":                      "OPENSANDBOX_CELL_ID",
	"worker-max-capacity":                 "OPENSANDBOX_MAX_CAPACITY",
	"worker-default-sandbox-memory-mb":    "OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB",
	"worker-default-sandbox-cpus":         "OPENSANDBOX_DEFAULT_SANDBOX_CPUS",
	"worker-default-sandbox-disk-mb":      "OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB",
	"worker-sandbox-domain":               "OPENSANDBOX_SANDBOX_DOMAIN",
	"worker-s3-bucket":                    "OPENSANDBOX_S3_BUCKET",
	"worker-s3-region":                    "OPENSANDBOX_S3_REGION",
	"worker-s3-endpoint":                  "OPENSANDBOX_S3_ENDPOINT",
	"worker-s3-force-path-style":          "OPENSANDBOX_S3_FORCE_PATH_STYLE",
	"worker-cf-event-endpoint":            "OPENSANDBOX_CF_EVENT_ENDPOINT",
	"worker-halt-list-url":                "OPENSANDBOX_HALT_LIST_URL",
	"worker-segment-write-key":            "SEGMENT_WRITE_KEY",

	// CF-cutover event pipe (worker)
	"worker-cf-event-secret":    "OPENSANDBOX_CF_EVENT_SECRET",
	"worker-cf-admin-secret":    "OPENSANDBOX_CF_ADMIN_SECRET",
	"worker-session-jwt-secret": "OPENSANDBOX_SESSION_JWT_SECRET",

	// Phase 2 — global blob store (Tigris primary)
	"worker-global-blob-name":              "OPENSANDBOX_GLOBAL_BLOB_NAME",
	"worker-global-blob-endpoint":          "OPENSANDBOX_GLOBAL_BLOB_ENDPOINT",
	"worker-global-blob-region":            "OPENSANDBOX_GLOBAL_BLOB_REGION",
	"worker-global-blob-access-key-id":     "OPENSANDBOX_GLOBAL_BLOB_ACCESS_KEY_ID",
	"worker-global-blob-secret-access-key": "OPENSANDBOX_GLOBAL_BLOB_SECRET_ACCESS_KEY",
	"worker-global-blob-goldens-bucket":    "OPENSANDBOX_GLOBAL_BLOB_GOLDENS_BUCKET",
	"worker-global-blob-templates-bucket":  "OPENSANDBOX_GLOBAL_BLOB_TEMPLATES_BUCKET",
	"worker-global-blob-events-bucket":     "OPENSANDBOX_GLOBAL_BLOB_EVENTS_BUCKET",

	// Phase 2 — global blob store (optional fallback)
	"worker-global-blob-fallback-name":              "OPENSANDBOX_GLOBAL_BLOB_FALLBACK_NAME",
	"worker-global-blob-fallback-endpoint":          "OPENSANDBOX_GLOBAL_BLOB_FALLBACK_ENDPOINT",
	"worker-global-blob-fallback-region":            "OPENSANDBOX_GLOBAL_BLOB_FALLBACK_REGION",
	"worker-global-blob-fallback-access-key-id":     "OPENSANDBOX_GLOBAL_BLOB_FALLBACK_ACCESS_KEY_ID",
	"worker-global-blob-fallback-secret-access-key": "OPENSANDBOX_GLOBAL_BLOB_FALLBACK_SECRET_ACCESS_KEY",

	// Shared
	"pg-password": "OPENSANDBOX_PG_PASSWORD",
}

// LoadSecretsFromKeyVault fetches secrets from Azure Key Vault and sets them
// as environment variables. Only loads secrets relevant to the current mode
// (server or worker), determined by the secret name prefix.
//
// Skips secrets that are already set in the environment — local env wins
// (the .env file is the "emergency override" path).
//
// Does nothing if SECRETS_VAULT_NAME is not set. Now delegates to the
// internal/secrets KeyVaultBackend so server, worker, and the AzurePool
// runtime image-refresh path share one implementation.
func LoadSecretsFromKeyVault() error {
	vaultName := os.Getenv("SECRETS_VAULT_NAME")
	if vaultName == "" {
		return nil // Key Vault not configured — use env file as-is
	}
	mode := os.Getenv("OPENSANDBOX_MODE") // "server" or "worker"

	be, err := secrets.NewKeyVaultBackend(vaultName, kvMapping, mode)
	if err != nil {
		return err
	}
	if be == nil {
		return nil // shouldn't happen — vaultName non-empty was checked above
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err = be.LoadAllToEnv(ctx)
	return err
}
