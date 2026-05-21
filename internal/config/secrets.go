// Package config — multi-cloud secret loading.
//
// A SecretsProvider knows how to fetch secrets from a cloud secret store and
// populate them as environment variables, honoring the same conventions:
//
//   - secretMapping is the single source of truth for "what logical secret
//     name maps to what env var". Both Azure Key Vault and AWS Secrets
//     Manager providers use the same map; the difference is the transport.
//   - Mode-prefix filtering: only `<mode>-*`, `pg-*`, and `shared-*` secrets
//     are loaded, where mode is the value of OPENSANDBOX_MODE ("server" or
//     "worker"). Without this a server would try to load worker-only
//     secrets and vice versa.
//   - Env-var precedence: secrets already set in os.Environ() are NOT
//     overwritten. Lets local dev shells override.
//
// LoadSecrets() is the entrypoint — it picks a provider by environment:
//
//	OPENSANDBOX_AZURE_KEY_VAULT_NAME or SECRETS_VAULT_NAME → Azure
//	OPENSANDBOX_AWS_SECRETS_PREFIX                        → AWS Secrets Manager
//	(neither set)                                          → no-op
//
// Both env vars set is treated as "both providers run in order" — useful for
// migrations.

package config

import (
	"context"
	"log"
	"os"
	"strings"
	"time"
)

// SecretsProvider is implemented by per-cloud secret loaders. Implementations
// must honor mode-prefix filtering and env-var precedence; see Load doc.
type SecretsProvider interface {
	// Name returns a short identifier used only for log messages.
	Name() string

	// Load fetches secrets and sets matching env vars. Returns (loaded,
	// skipped, err). `mode` is the value of OPENSANDBOX_MODE; pass ""
	// for mode-agnostic loaders.
	Load(ctx context.Context, mode string) (loaded int, skipped int, err error)
}

// secretMapping is the canonical map of "logical secret name → env var
// name". Implementations look up each fetched secret's logical name here
// to decide what env var to set.
//
// Logical secret names are prefixed:
//
//	server-*  loaded only when OPENSANDBOX_MODE=server
//	worker-*  loaded only when OPENSANDBOX_MODE=worker
//	pg-*      grandfathered shared (Postgres password) — loaded in both modes
//	shared-*  mode-agnostic; loaded in both modes
//
// Backends apply different transports for fetching these by name (KV list-
// then-get, SM list-by-prefix-then-get). The names themselves are the same
// across clouds — operators see the same logical inventory regardless of
// where the cell runs.
var secretMapping = map[string]string{
	// ----- Server secrets -----
	"server-database-url":          "OPENSANDBOX_DATABASE_URL",
	"server-redis-url":             "OPENSANDBOX_REDIS_URL",
	"server-jwt-secret":            "OPENSANDBOX_JWT_SECRET",
	"server-api-key":               "OPENSANDBOX_API_KEY",
	"server-secret-encryption-key": "OPENSANDBOX_SECRET_ENCRYPTION_KEY",
	"server-workos-api-key":        "WORKOS_API_KEY",
	"server-workos-client-id":      "WORKOS_CLIENT_ID",
	"server-cf-api-token":          "OPENSANDBOX_CF_API_TOKEN",
	"server-cf-zone-id":            "OPENSANDBOX_CF_ZONE_ID",
	"server-stripe-secret-key":     "STRIPE_SECRET_KEY",
	"server-stripe-webhook-secret": "STRIPE_WEBHOOK_SECRET",
	"server-sentry-dsn":            "OPENSANDBOX_SENTRY_DSN",
	"server-azure-vm-sizes":        "OPENSANDBOX_AZURE_VM_SIZES",
	"server-ec2-instance-types":    "OPENSANDBOX_EC2_INSTANCE_TYPES",

	// Legacy Axiom mappings — kept for backwards compat with existing prod
	// KVs that pre-date the `shared-` prefix. New deploys should use
	// `shared-axiom-*` instead.
	"server-axiom-query-token": "AXIOM_QUERY_TOKEN",
	"server-axiom-dataset":     "AXIOM_DATASET",

	// ----- Worker secrets -----
	"worker-jwt-secret":         "OPENSANDBOX_JWT_SECRET",
	"worker-database-url":       "OPENSANDBOX_DATABASE_URL",
	"worker-redis-url":          "OPENSANDBOX_REDIS_URL",
	"worker-s3-access-key":      "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"worker-s3-secret-key":      "OPENSANDBOX_S3_SECRET_ACCESS_KEY",
	"worker-sentry-dsn":         "OPENSANDBOX_SENTRY_DSN",
	"worker-axiom-ingest-token": "AXIOM_INGEST_TOKEN",
	"worker-axiom-dataset":      "AXIOM_DATASET",

	// ----- Shared / mode-agnostic -----
	"pg-password":                        "OPENSANDBOX_PG_PASSWORD",
	"shared-axiom-ingest-token":          "AXIOM_INGEST_TOKEN",
	"shared-axiom-query-token":           "AXIOM_QUERY_TOKEN",
	"shared-axiom-dataset":               "AXIOM_DATASET",
	"shared-axiom-platform-ingest-token": "AXIOM_PLATFORM_TOKEN",
	"shared-axiom-platform-dataset":      "AXIOM_PLATFORM_DATASET",
	"shared-cell-id":                     "OPENCOMPUTER_CELL_ID",
}

// shouldLoadForMode returns true if the given logical secret name applies to
// the running mode. Empty mode loads everything; otherwise only matching
// prefixes plus `pg-*` and `shared-*` pass.
func shouldLoadForMode(name, mode string) bool {
	if mode == "" {
		return true
	}
	return strings.HasPrefix(name, mode+"-") ||
		strings.HasPrefix(name, "pg-") ||
		strings.HasPrefix(name, "shared-")
}

// setIfUnset writes name=value to the process environment, but only when
// name is not already set. Returns true if the value was applied.
func setIfUnset(name, value string) bool {
	if os.Getenv(name) != "" {
		return false
	}
	_ = os.Setenv(name, value)
	return true
}

// LoadSecrets picks a provider by environment and loads secrets from it.
// Mode is read from OPENSANDBOX_MODE. Returns nil and does nothing if no
// provider is configured — local dev with a worker.env file works as-is.
//
// If both Azure and AWS providers are configured (unusual but legal during
// a cross-cloud migration), both run in declaration order.
func LoadSecrets() error {
	mode := os.Getenv("OPENSANDBOX_MODE")

	var providers []SecretsProvider
	if name := azureVaultName(); name != "" {
		providers = append(providers, &azureKeyVaultProvider{vaultName: name})
	}
	if prefix := os.Getenv("OPENSANDBOX_AWS_SECRETS_PREFIX"); prefix != "" {
		providers = append(providers, &awsSecretsManagerProvider{prefix: prefix})
	}

	if len(providers) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, p := range providers {
		loaded, skipped, err := p.Load(ctx, mode)
		if err != nil {
			return err
		}
		log.Printf("secrets: %s loaded %d, skipped %d (already set)", p.Name(), loaded, skipped)
	}
	return nil
}

// azureVaultName reads the Azure KV name from either of the two env vars
// historically used. Kept here so both `secrets.go` and `keyvault.go` see
// the same lookup precedence.
func azureVaultName() string {
	if v := os.Getenv("OPENSANDBOX_AZURE_KEY_VAULT_NAME"); v != "" {
		return v
	}
	return os.Getenv("SECRETS_VAULT_NAME")
}
