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
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// secretMapping maps Key Vault secret names to environment variable names.
// Only secrets in this map are loaded — unknown secrets in the vault are ignored.
//
// Prefix conventions:
//
//	server-*  — loaded only when OPENSANDBOX_MODE=server (control-plane secrets,
//	            autoscaler/Azure config, billing/auth integrations)
//	worker-*  — loaded only when OPENSANDBOX_MODE=worker. New worker-only
//	            secrets are rare; most are shared with the server. Existing
//	            entries are kept as legacy aliases until the cleanup PR drops them.
//	shared-*  — loaded in both modes. Use this for any secret or config value
//	            both binaries consume (DB/Redis URLs, JWT secret, S3, Axiom,
//	            sandbox defaults, sandbox domain). Prefer this prefix for new
//	            entries to avoid duplicating values across server-/worker-.
//	pg-       — grandfathered shared prefix; loaded in both modes.
var secretMapping = map[string]string{
	// ── Server-only ──────────────────────────────────────────────────────
	// Auth & API
	"server-api-key":               "OPENSANDBOX_API_KEY",
	"server-secret-encryption-key": "OPENSANDBOX_SECRET_ENCRYPTION_KEY",
	// WorkOS (auth provider)
	"server-workos-api-key":        "WORKOS_API_KEY",
	"server-workos-client-id":      "WORKOS_CLIENT_ID",
	"server-workos-redirect-uri":   "WORKOS_REDIRECT_URI",
	"server-workos-cookie-domain":  "WORKOS_COOKIE_DOMAIN",
	"server-workos-frontend-url":   "WORKOS_FRONTEND_URL",
	// Cloudflare (custom hostnames)
	"server-cf-api-token":          "OPENSANDBOX_CF_API_TOKEN",
	"server-cf-zone-id":            "OPENSANDBOX_CF_ZONE_ID",
	// Stripe (billing)
	"server-stripe-secret-key":     "STRIPE_SECRET_KEY",
	"server-stripe-webhook-secret": "STRIPE_WEBHOOK_SECRET",
	// Observability
	"server-sentry-dsn":            "OPENSANDBOX_SENTRY_DSN",
	// Azure compute pool / autoscaler
	"server-azure-resource-group":     "OPENSANDBOX_AZURE_RESOURCE_GROUP",
	"server-azure-subscription-id":    "OPENSANDBOX_AZURE_SUBSCRIPTION_ID",
	"server-azure-vm-size":            "OPENSANDBOX_AZURE_VM_SIZE",
	"server-azure-subnet-id":          "OPENSANDBOX_AZURE_SUBNET_ID",
	"server-azure-ssh-public-key":     "OPENSANDBOX_AZURE_SSH_PUBLIC_KEY",
	"server-azure-worker-identity-id": "OPENSANDBOX_AZURE_WORKER_IDENTITY_ID",
	"server-azure-key-vault-name":     "OPENSANDBOX_AZURE_KEY_VAULT_NAME",
	"server-min-workers":              "OPENSANDBOX_MIN_WORKERS",
	"server-max-workers":              "OPENSANDBOX_MAX_WORKERS",
	"server-idle-reserve":             "OPENSANDBOX_IDLE_RESERVE",
	// Legacy server-only duplicates of values now in shared-*. Kept so old KVs
	// that pre-date the shared- prefix keep working. A follow-up cleanup PR
	// will drop these once every deployment has a populated shared-* set.
	"server-database-url":          "OPENSANDBOX_DATABASE_URL",
	"server-redis-url":             "OPENSANDBOX_REDIS_URL",
	"server-jwt-secret":            "OPENSANDBOX_JWT_SECRET",
	"server-axiom-query-token":     "AXIOM_QUERY_TOKEN",
	"server-axiom-dataset":         "AXIOM_DATASET",

	// ── Worker-only ──────────────────────────────────────────────────────
	// Observability
	"worker-sentry-dsn": "OPENSANDBOX_SENTRY_DSN",
	// Legacy worker-only duplicates of values now in shared-*. Same story as
	// the server- legacy block: kept so unmigrated KVs still work; cleanup PR
	// drops them.
	"worker-jwt-secret":         "OPENSANDBOX_JWT_SECRET",
	"worker-database-url":       "OPENSANDBOX_DATABASE_URL",
	"worker-redis-url":          "OPENSANDBOX_REDIS_URL",
	"worker-s3-access-key":      "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"worker-s3-secret-key":      "OPENSANDBOX_S3_SECRET_ACCESS_KEY",
	"worker-axiom-ingest-token": "AXIOM_INGEST_TOKEN",
	"worker-axiom-dataset":      "AXIOM_DATASET",

	// ── Shared (loaded in both modes) ────────────────────────────────────
	// Connection strings
	"shared-database-url": "OPENSANDBOX_DATABASE_URL",
	"shared-redis-url":    "OPENSANDBOX_REDIS_URL",
	// Auth
	"shared-jwt-secret": "OPENSANDBOX_JWT_SECRET",
	// S3 / blob (checkpoint store)
	"shared-s3-bucket":            "OPENSANDBOX_S3_BUCKET",
	"shared-s3-region":            "OPENSANDBOX_S3_REGION",
	"shared-s3-endpoint":          "OPENSANDBOX_S3_ENDPOINT",
	"shared-s3-access-key-id":     "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"shared-s3-secret-access-key": "OPENSANDBOX_S3_SECRET_ACCESS_KEY",
	// Sandbox defaults
	"shared-sandbox-domain":            "OPENSANDBOX_SANDBOX_DOMAIN",
	"shared-default-sandbox-memory-mb": "OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB",
	"shared-default-sandbox-cpus":      "OPENSANDBOX_DEFAULT_SANDBOX_CPUS",
	"shared-default-sandbox-disk-mb":   "OPENSANDBOX_DEFAULT_SANDBOX_DISK_MB",
	// Axiom (sandbox session logs)
	"shared-axiom-ingest-token": "AXIOM_INGEST_TOKEN",
	"shared-axiom-query-token":  "AXIOM_QUERY_TOKEN",
	"shared-axiom-dataset":      "AXIOM_DATASET",
	// Postgres (grandfathered shared prefix)
	"pg-password": "OPENSANDBOX_PG_PASSWORD",
}

// LoadSecretsFromKeyVault fetches secrets from Azure Key Vault and sets them
// as environment variables. Only loads secrets relevant to the current mode
// (server or worker), determined by the secret name prefix.
//
// Skips secrets that are already set in the environment.
// Does nothing if SECRETS_VAULT_NAME is not set.
func LoadSecretsFromKeyVault() error {
	vaultName := os.Getenv("SECRETS_VAULT_NAME")
	if vaultName == "" {
		return nil // Key Vault not configured — use env file as-is
	}

	vaultURL := fmt.Sprintf("https://%s.vault.azure.net/", vaultName)
	mode := os.Getenv("OPENSANDBOX_MODE") // "server" or "worker"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fmt.Errorf("keyvault: azure credential: %w", err)
	}

	client, err := azsecrets.NewClient(vaultURL, cred, nil)
	if err != nil {
		return fmt.Errorf("keyvault: client: %w", err)
	}

	loaded := 0
	skipped := 0

	pager := client.NewListSecretPropertiesPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("keyvault: list secrets: %w", err)
		}

		for _, prop := range page.Value {
			name := prop.ID.Name()
			envVar, mapped := secretMapping[name]
			if !mapped {
				continue
			}

			// Only load secrets matching the current mode, or mode-agnostic
			// "shared" secrets that both server and worker need (the `pg-`
			// prefix is grandfathered in for the same reason). Without this
			// bypass, a single token like AXIOM_INGEST_TOKEN — which the
			// server needs to bake into worker.env at spawn time AND the
			// worker needs at startup — has to be duplicated under both
			// `server-` and `worker-` prefixes in KV. The `shared-` prefix
			// formalizes "this secret goes to both modes" as a real concept.
			if mode != "" &&
				!strings.HasPrefix(name, mode+"-") &&
				!strings.HasPrefix(name, "pg-") &&
				!strings.HasPrefix(name, "shared-") {
				continue
			}

			// Don't overwrite existing env vars — local config takes precedence
			if os.Getenv(envVar) != "" {
				skipped++
				continue
			}

			// Fetch the secret value
			resp, err := client.GetSecret(ctx, name, "", nil)
			if err != nil {
				log.Printf("keyvault: failed to get secret %s: %v (skipping)", name, err)
				continue
			}
			if resp.Value == nil {
				continue
			}

			os.Setenv(envVar, *resp.Value)
			loaded++
		}
	}

	log.Printf("keyvault: loaded %d secrets from %s (%d skipped, already set)", loaded, vaultName, skipped)
	return nil
}
