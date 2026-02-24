package ecr

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
)

// Config holds ECR connection settings.
type Config struct {
	Registry   string // e.g. "086971355112.dkr.ecr.us-east-2.amazonaws.com"
	Repository string // e.g. "opensandbox-templates"
	Region     string // e.g. "us-east-2"
	AccessKey  string
	SecretKey  string
}

// IsConfigured returns true if ECR settings are provided.
// Works with either static credentials or IAM (when AccessKey is empty but Registry+Repository are set).
func (c *Config) IsConfigured() bool {
	return c.Registry != "" && c.Repository != ""
}

// GetAuthToken retrieves a Docker-compatible auth token from ECR.
// Returns (username, password) suitable for `podman login`.
// If AccessKey is empty, uses the default AWS credential chain (IAM instance profile on EC2).
func GetAuthToken(ctx context.Context, cfg *Config) (string, string, error) {
	var client *ecr.Client

	if cfg.AccessKey != "" {
		// Static credentials
		awsCfg := aws.Config{
			Region:      cfg.Region,
			Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		}
		client = ecr.NewFromConfig(awsCfg)
	} else {
		// IAM credential chain
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
			awsconfig.WithRegion(cfg.Region),
		)
		if err != nil {
			return "", "", fmt.Errorf("failed to load AWS config for ECR: %w", err)
		}
		client = ecr.NewFromConfig(awsCfg)
	}
	output, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", "", fmt.Errorf("failed to get ECR auth token: %w", err)
	}

	if len(output.AuthorizationData) == 0 {
		return "", "", fmt.Errorf("no authorization data returned from ECR")
	}

	// Token is base64-encoded "username:password"
	encoded := *output.AuthorizationData[0].AuthorizationToken
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", fmt.Errorf("failed to decode ECR auth token: %w", err)
	}

	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("unexpected ECR auth token format")
	}

	return parts[0], parts[1], nil
}

// ImageRef returns the full ECR image reference for an org-scoped template.
// Format: {registry}/{repo}:{orgPrefix}-{name}-{tag}
func ImageRef(cfg *Config, orgID, name, tag string) string {
	// Use first 8 chars of orgID as prefix to keep tags manageable
	prefix := orgID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return fmt.Sprintf("%s/%s:%s-%s-%s", cfg.Registry, cfg.Repository, prefix, name, tag)
}

// PublicImageRef returns the full ECR image reference for a public template.
// Format: {registry}/{repo}:public-{name}-{tag}
func PublicImageRef(cfg *Config, name, tag string) string {
	return fmt.Sprintf("%s/%s:public-%s-%s", cfg.Registry, cfg.Repository, name, tag)
}
