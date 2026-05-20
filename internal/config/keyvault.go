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
	// Machine-size fallback lists (PR #209). Comma-separated ranked
	// instance types the autoscaler tries in order on quota / capacity
	// errors. Empty value = use the single VMSize / InstanceType
	// configured on the pool (pre-fallback behavior).
	"server-azure-vm-sizes":         "OPENSANDBOX_AZURE_VM_SIZES",
	"server-ec2-instance-types":     "OPENSANDBOX_EC2_INSTANCE_TYPES",
	// Legacy Axiom mappings — kept for backwards compat with existing prod
	// KVs that pre-date the `shared-` prefix. New deploys should use
	// `shared-axiom-*` instead. Safe to leave: in server mode only
	// `server-axiom-*` is loaded; in worker mode only `worker-axiom-*`. New
	// `shared-*` mappings below win for new envs that have only those.
	"server-axiom-query-token":      "AXIOM_QUERY_TOKEN",
	"server-axiom-dataset":          "AXIOM_DATASET",

	// Server-side cell config + shared secrets. These mirror the worker-* keys
	// of the same name — both sides need them, and the prefix filter loads only
	// secrets for the current mode. When dev1/prod consolidate to cell-* we can
	// drop the duplicates; for now this keeps the layout symmetric and explicit.
	"server-cell-id":            "OPENSANDBOX_CELL_ID",
	"server-region":             "OPENSANDBOX_REGION",
	"server-sandbox-domain":     "OPENSANDBOX_SANDBOX_DOMAIN",
	"server-cf-event-endpoint":  "OPENSANDBOX_CF_EVENT_ENDPOINT",
	"server-cf-event-secret":    "OPENSANDBOX_CF_EVENT_SECRET",
	"server-cf-admin-secret":    "OPENSANDBOX_CF_ADMIN_SECRET",
	"server-session-jwt-secret": "OPENSANDBOX_SESSION_JWT_SECRET",
	"server-halt-list-url":      "OPENSANDBOX_HALT_LIST_URL",

	// Worker secrets
	"worker-jwt-secret":         "OPENSANDBOX_JWT_SECRET",
	"worker-database-url":       "OPENSANDBOX_DATABASE_URL",
	"worker-redis-url":          "OPENSANDBOX_REDIS_URL",
	"worker-s3-access-key":      "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"worker-s3-secret-key":      "OPENSANDBOX_S3_SECRET_ACCESS_KEY",
	"worker-sentry-dsn":         "OPENSANDBOX_SENTRY_DSN",
	"worker-axiom-ingest-token": "AXIOM_INGEST_TOKEN", // legacy; superseded by shared-axiom-ingest-token
	"worker-axiom-dataset":      "AXIOM_DATASET",      // legacy; superseded by shared-axiom-dataset

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

	// Shared (mode-agnostic — loaded in both server and worker)
	"pg-password":               "OPENSANDBOX_PG_PASSWORD",
	"shared-axiom-ingest-token": "AXIOM_INGEST_TOKEN",
	"shared-axiom-query-token":  "AXIOM_QUERY_TOKEN",
	"shared-axiom-dataset":      "AXIOM_DATASET",
	// Platform-logs: Vector reads these from /etc/opensandbox/vector.env,
	// populated by populate-vector-env.service via its own IMDS+KV REST call
	// (not by this Go-side loader, because Vector starts as its own systemd
	// unit before the Go binary). The entries here exist for two reasons:
	//   1. Discoverability — kvMapping is the single source of truth for
	//      "what shared-* secrets does this deployment need in KV".
	//   2. Side-effect: the Go binary ALSO loads them into its own env at
	//      startup; future Go code that wants to surface platform-stream
	//      config (e.g. an admin endpoint) gets them for free.
	"shared-axiom-platform-ingest-token": "AXIOM_PLATFORM_TOKEN",
	"shared-axiom-platform-dataset":      "AXIOM_PLATFORM_DATASET",
	// Cell identifier — stamped on every log + metric event so platform
	// dashboards can filter per cell. Same dual-consumer pattern as the
	// platform-* secrets above: Vector reads it from /etc/opensandbox/vector.env
	// (written by populate-vector-env.sh) for its remap substitutions, and
	// the Go binary reads it from cfg.CellID (which falls back to
	// "<region>-default" when this isn't in KV — see config.go).
	"shared-cell-id": "OPENSANDBOX_CELL_ID",

	// Edge integration (mode-agnostic — both server and worker need them).
	// cf-edge-base-url is consumed by the CP's edgeclient for HMAC'd
	// /internal/templates + /internal/secret-stores lookups; secret-encryption-key
	// is the AES-256-GCM key shared with the api-edge Worker so the edge can
	// encrypt secret-store entries and any cell can decrypt them.
	"shared-cf-edge-base-url":      "OPENSANDBOX_CF_EDGE_BASE_URL",
	"shared-secret-encryption-key": "OPENSANDBOX_SECRET_ENCRYPTION_KEY",
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
