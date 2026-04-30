package secrets

import (
	"context"
	"fmt"
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
