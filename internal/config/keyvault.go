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
var secretMapping = map[string]string{
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

	// Worker secrets
	"worker-jwt-secret":         "OPENSANDBOX_JWT_SECRET",
	"worker-database-url":       "OPENSANDBOX_DATABASE_URL",
	"worker-redis-url":          "OPENSANDBOX_REDIS_URL",
	"worker-s3-access-key":      "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"worker-s3-secret-key":      "OPENSANDBOX_S3_SECRET_ACCESS_KEY",
	"worker-sentry-dsn":         "OPENSANDBOX_SENTRY_DSN",
	"worker-axiom-ingest-token": "AXIOM_INGEST_TOKEN", // legacy; superseded by shared-axiom-ingest-token
	"worker-axiom-dataset":      "AXIOM_DATASET",      // legacy; superseded by shared-axiom-dataset

	// Shared (mode-agnostic — loaded in both server and worker)
	"pg-password":               "OPENSANDBOX_PG_PASSWORD",
	"shared-axiom-ingest-token": "AXIOM_INGEST_TOKEN",
	"shared-axiom-query-token":  "AXIOM_QUERY_TOKEN",
	"shared-axiom-dataset":      "AXIOM_DATASET",
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
