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

	// Worker secrets
	"worker-jwt-secret":    "OPENSANDBOX_JWT_SECRET",
	"worker-database-url":  "OPENSANDBOX_DATABASE_URL",
	"worker-redis-url":     "OPENSANDBOX_REDIS_URL",
	"worker-s3-access-key": "OPENSANDBOX_S3_ACCESS_KEY_ID",
	"worker-s3-secret-key": "OPENSANDBOX_S3_SECRET_ACCESS_KEY",

	// Shared
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

			// Only load secrets matching the current mode (or shared secrets)
			if mode != "" && !strings.HasPrefix(name, mode+"-") && !strings.HasPrefix(name, "pg-") {
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
