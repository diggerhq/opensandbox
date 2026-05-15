package secrets

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// SecretsManagerBackend reads secrets from AWS Secrets Manager.
//
// Two access patterns are supported:
//
//  1. Get("arn:aws:secretsmanager:us-east-1:123:secret:my-secret-XYZ") returns
//     the SecretString of that secret directly. Useful when each secret is
//     a single value.
//
//  2. Get("KEY_NAME") with BundledARN configured returns secrets[KEY_NAME]
//     from a JSON-encoded bundle stored at BundledARN. Mirrors the existing
//     OPENSANDBOX_SECRETS_ARN bootstrap pattern.
type SecretsManagerBackend struct {
	client     *secretsmanager.Client
	bundledARN string // optional: JSON bundle keyed by secret name

	// NameMap translates bundle key → env var name. Mirrors KeyVaultBackend.NameMap
	// so the same kvMapping in internal/config can drive both Azure KV and AWS SM
	// bootstrap. Nil/empty = LoadAllToEnv writes bundle keys verbatim as env vars
	// (legacy behavior; only safe if the bundle already uses SCREAMING_SNAKE).
	NameMap map[string]string

	// ModePrefixFilter restricts LoadAllToEnv to bundle keys whose name starts
	// with "{ModePrefixFilter}-" (plus shared "pg-*"). Empty = no filter.
	// Typical: "server" or "worker". Same semantics as KeyVaultBackend.
	ModePrefixFilter string
}

// NewSecretsManagerBackend constructs a backend using the default AWS
// credential chain (IAM instance role preferred; env vars fallback).
//
// region may be empty — if so, derived from bundledARN or AWS_REGION env var.
// bundledARN may be empty if all callers pass full ARNs to Get.
func NewSecretsManagerBackend(ctx context.Context, region, bundledARN string) (*SecretsManagerBackend, error) {
	var opts []func(*awsconfig.LoadOptions) error
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	} else if r := regionFromARN(bundledARN); r != "" {
		opts = append(opts, awsconfig.WithRegion(r))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("secretsmanager: load AWS config: %w", err)
	}
	return &SecretsManagerBackend{
		client:     secretsmanager.NewFromConfig(cfg),
		bundledARN: bundledARN,
	}, nil
}

// Get returns the secret value for key. If key looks like an ARN
// (starts with "arn:"), reads it as a standalone secret. Otherwise,
// reads BundledARN as a JSON bundle and returns secrets[key].
func (b *SecretsManagerBackend) Get(ctx context.Context, key string) (string, error) {
	if strings.HasPrefix(key, "arn:") {
		return b.getRaw(ctx, key)
	}
	if b.bundledARN == "" {
		return "", fmt.Errorf("secretsmanager: %w (key %q not an ARN and no BundledARN configured)", ErrNotFound, key)
	}
	bundle, err := b.getRaw(ctx, b.bundledARN)
	if err != nil {
		return "", err
	}
	v, err := lookupBundledSecret(bundle, key)
	if err != nil {
		return "", err
	}
	return v, nil
}

// LoadAllToEnv satisfies BulkLoader. SecretsManager's bootstrap pattern is
// "one secret holds JSON; expand to env vars by key name." Reads BundledARN
// and Setenv each KEY=value pair from the JSON object, skipping any that are
// already present in the env (local overrides win).
//
// If NameMap is non-empty, bundle keys are translated to env var names via
// the map AND restricted to keys present in the map (allowlist). This mirrors
// KeyVaultBackend.LoadAllToEnv so a single kvMapping drives both backends —
// the Infisical sync writes bundle keys as kebab-case (server-jwt-secret,
// worker-cell-id, ...) and the map turns them into OPENSANDBOX_* env vars.
//
// If NameMap is empty, bundle keys are written verbatim (legacy path —
// caller responsible for bundle keys already being env-var-shaped).
//
// ModePrefixFilter additionally restricts to keys with "{mode}-" or "pg-"
// prefix, same as KeyVaultBackend.
func (b *SecretsManagerBackend) LoadAllToEnv(ctx context.Context) (loaded, skipped int, err error) {
	if b.bundledARN == "" {
		return 0, 0, nil
	}
	bundleStr, err := b.getRaw(ctx, b.bundledARN)
	if err != nil {
		return 0, 0, err
	}
	pairs, err := parseBundleJSONAll(bundleStr)
	if err != nil {
		return 0, 0, err
	}
	for k, v := range pairs {
		envVar := k
		if len(b.NameMap) > 0 {
			mapped, ok := b.NameMap[k]
			if !ok {
				continue // not in allowlist — silently skip
			}
			envVar = mapped
		}
		if b.ModePrefixFilter != "" &&
			!strings.HasPrefix(k, b.ModePrefixFilter+"-") &&
			!strings.HasPrefix(k, "pg-") &&
			!strings.HasPrefix(k, "shared-") {
			continue
		}
		if os.Getenv(envVar) != "" {
			skipped++
			continue
		}
		os.Setenv(envVar, v)
		loaded++
	}
	if len(b.NameMap) > 0 {
		log.Printf("secretsmanager: loaded %d secrets from bundle %s (%d skipped, already set)", loaded, b.bundledARN, skipped)
	}
	return loaded, skipped, nil
}

// LoadAllByNameList is the per-key counterpart to LoadAllToEnv: instead of
// pulling one bundled JSON secret, it fetches N individual SM secrets by
// name (one BatchGetSecretValue round-trip when N <= 20, paginated otherwise)
// and applies NameMap + ModePrefixFilter the same way KeyVaultBackend does.
//
// Use this when the secrets are stored as separate SM resources (which is
// what Infisical's AWS Secrets Manager sync writes by default — each
// Infisical secret becomes its own SM secret, named verbatim or with a
// configurable prefix).
//
// `names` is the list of SM secret names to fetch. The caller is expected
// to pass the kebab-case names from kvMapping; unknown names in SM are
// ignored and missing names in SM produce log warnings (not fatal).
func (b *SecretsManagerBackend) LoadAllByNameList(ctx context.Context, names []string) (loaded, skipped int, err error) {
	if len(names) == 0 {
		return 0, 0, nil
	}
	// BatchGetSecretValue caps at 20 names per call; chunk if needed.
	const batchSize = 20
	for i := 0; i < len(names); i += batchSize {
		end := i + batchSize
		if end > len(names) {
			end = len(names)
		}
		chunk := names[i:end]
		out, callErr := b.client.BatchGetSecretValue(ctx, &secretsmanager.BatchGetSecretValueInput{
			SecretIdList: chunk,
		})
		if callErr != nil {
			return loaded, skipped, fmt.Errorf("secretsmanager: BatchGetSecretValue: %w", callErr)
		}
		for _, sv := range out.SecretValues {
			if sv.Name == nil || sv.SecretString == nil {
				continue
			}
			name := *sv.Name
			val := *sv.SecretString
			envVar := name
			if len(b.NameMap) > 0 {
				mapped, ok := b.NameMap[name]
				if !ok {
					continue // not in allowlist
				}
				envVar = mapped
			}
			if b.ModePrefixFilter != "" &&
				!strings.HasPrefix(name, b.ModePrefixFilter+"-") &&
				!strings.HasPrefix(name, "pg-") {
				continue
			}
			if os.Getenv(envVar) != "" {
				skipped++
				continue
			}
			os.Setenv(envVar, val)
			loaded++
		}
		// out.Errors contains names that failed individually (e.g. not found).
		// Log but don't fail — missing entries are expected during partial migrations.
		for _, e := range out.Errors {
			if e.SecretId != nil && e.Message != nil {
				log.Printf("secretsmanager: %s: %s (skipping)", *e.SecretId, *e.Message)
			}
		}
	}
	log.Printf("secretsmanager: loaded %d secrets via batch list (%d skipped, already set)", loaded, skipped)
	return loaded, skipped, nil
}

func (b *SecretsManagerBackend) getRaw(ctx context.Context, arn string) (string, error) {
	out, err := b.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &arn,
	})
	if err != nil {
		return "", fmt.Errorf("secretsmanager: GetSecretValue %s: %w", arn, err)
	}
	if out.SecretString == nil {
		return "", fmt.Errorf("secretsmanager: %s has no string value", arn)
	}
	return *out.SecretString, nil
}

// regionFromARN extracts the region from "arn:aws:secretsmanager:REGION:..."
func regionFromARN(arn string) string {
	if !strings.HasPrefix(arn, "arn:") {
		return ""
	}
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

// lookupBundledSecret parses a JSON object and returns the value for key.
// Implemented inline rather than encoding/json so the keyvault backend can
// reuse the same shape if needed.
func lookupBundledSecret(jsonStr, key string) (string, error) {
	// Minimal JSON parse: find "key":"value" pairs without bringing the full
	// encoding/json import path into a hot bootstrap path.
	// For simplicity here we DO use encoding/json — the perf concern is
	// premature.
	return parseBundleJSON(jsonStr, key)
}
