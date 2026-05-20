package config

import (
	"context"
	"os"
	"time"

	"github.com/opensandbox/opensandbox/internal/secrets"
)

// LoadSecretsFromSecretsManager is the AWS analogue of LoadSecretsFromKeyVault.
// Maps every kebab-case key from kvMapping into the process environment via
// the same translation Azure KV uses (server-jwt-secret → OPENSANDBOX_JWT_SECRET).
//
// Two trigger env vars, picked one at a time:
//
//   - OPENSANDBOX_SECRETS_ARN: single bundled SM secret with JSON SecretString.
//     One GetSecretValue, one IAM scope. Cheap ($0.40/mo) but requires the
//     producer (Infisical or otherwise) to write a JSON bundle.
//
//   - OPENSANDBOX_SECRETS_AWS_REGION: per-key mode. Each kvMapping key is its
//     own SM secret. BatchGetSecretValue fetches them in chunks of 20. This
//     is what Infisical's "Sync each secret to its own secret" mode produces.
//     Costs $0.40/mo per secret but matches Infisical's default sync shape.
//
// Existing env vars take precedence (emergency override path).
//
// Does nothing if neither trigger is set.
func LoadSecretsFromSecretsManager() error {
	bundleARN := os.Getenv("OPENSANDBOX_SECRETS_ARN")
	listRegion := os.Getenv("OPENSANDBOX_SECRETS_AWS_REGION")
	if bundleARN == "" && listRegion == "" {
		return nil // not configured; defer to env file / Azure KV / combined mode
	}
	mode := os.Getenv("OPENSANDBOX_MODE")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	be, err := secrets.NewSecretsManagerBackend(ctx, listRegion, bundleARN)
	if err != nil {
		return err
	}
	be.NameMap = kvMapping
	be.ModePrefixFilter = mode

	if bundleARN != "" {
		_, _, err = be.LoadAllToEnv(ctx)
		return err
	}
	// list/per-key mode — fetch each kvMapping key as its own SM secret
	names := make([]string, 0, len(kvMapping))
	for k := range kvMapping {
		names = append(names, k)
	}
	_, _, err = be.LoadAllByNameList(ctx, names)
	return err
}
