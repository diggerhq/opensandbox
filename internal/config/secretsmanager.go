// Package config — AWS Secrets Manager implementation of SecretsProvider.
//
// Authentication uses the AWS Default Credential chain (EC2 IAM role on
// instances, AWS_PROFILE / SSO / env locally). The trigger env var is
// OPENSANDBOX_AWS_SECRETS_PREFIX; LoadSecrets() in secrets.go selects this
// provider when it is set.
//
// Layout: secrets are stored under a flat per-cell prefix, e.g.
//
//	opencomputer/aws-us-east-2-poc/worker-jwt-secret
//	opencomputer/aws-us-east-2-poc/worker-redis-url
//	opencomputer/aws-us-east-2-poc/shared-axiom-ingest-token
//
// The provider lists everything under the prefix, strips it, looks up the
// remaining logical name in secretMapping (cloud-agnostic, defined in
// secrets.go), and sets the matching env var if not already populated.

package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// awsSecretsManagerProvider fetches secrets by listing the cell's prefix and
// dereferencing every name that matches secretMapping for the current mode.
type awsSecretsManagerProvider struct {
	prefix string
}

func (p *awsSecretsManagerProvider) Name() string { return "aws-secretsmanager" }

func (p *awsSecretsManagerProvider) Load(ctx context.Context, mode string) (int, int, error) {
	// Region: explicit env var wins (matches the cell config), else AWS_REGION,
	// else fall through to the default chain (IMDS on EC2). Passing nothing
	// when LoadDefaultConfig can't resolve a region anywhere makes the first
	// API call return "Missing Region", which surfaces unclearly — prefer the
	// explicit env-var path even on EC2.
	region := os.Getenv("OPENSANDBOX_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}

	var loadOpts []func(*awsconfig.LoadOptions) error
	if region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return 0, 0, fmt.Errorf("secretsmanager: load aws config: %w", err)
	}
	client := secretsmanager.NewFromConfig(cfg)

	loaded, skipped := 0, 0

	var nextToken *string
	for {
		out, err := client.ListSecrets(ctx, &secretsmanager.ListSecretsInput{
			MaxResults: aws.Int32(100),
			NextToken:  nextToken,
			Filters: []types.Filter{
				{
					Key:    types.FilterNameStringTypeName,
					Values: []string{p.prefix},
				},
			},
		})
		if err != nil {
			return loaded, skipped, fmt.Errorf("secretsmanager: list: %w", err)
		}

		for _, entry := range out.SecretList {
			if entry.Name == nil {
				continue
			}
			fullName := *entry.Name
			// `name` filter is a prefix-match; defensively strip and skip if not ours.
			if !strings.HasPrefix(fullName, p.prefix) {
				continue
			}
			logicalName := strings.TrimPrefix(fullName, p.prefix)

			envVar, mapped := secretMapping[logicalName]
			if !mapped {
				continue
			}
			if !shouldLoadForMode(logicalName, mode) {
				continue
			}
			if os.Getenv(envVar) != "" {
				skipped++
				continue
			}

			val, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
				SecretId: aws.String(fullName),
			})
			if err != nil {
				log.Printf("secretsmanager: failed to get secret %s: %v (skipping)", logicalName, err)
				continue
			}
			if val.SecretString == nil {
				continue
			}

			if setIfUnset(envVar, *val.SecretString) {
				loaded++
			} else {
				skipped++
			}
		}

		if out.NextToken == nil {
			break
		}
		nextToken = out.NextToken
	}

	return loaded, skipped, nil
}
