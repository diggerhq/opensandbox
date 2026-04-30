// Package secrets provides a cloud-neutral runtime secrets fetcher.
//
// Implementations:
//   - EnvBackend         (default; reads from OS env vars)
//   - KeyVaultBackend    (Azure Key Vault, in keyvault.go — build-tag isolated)
//   - SecretsManagerBackend (AWS Secrets Manager, in secretsmanager.go — build-tag isolated)
//
// The CP picks a backend based on cfg.SecretsProvider at startup. Most
// callers can use the default Env backend; cloud-specific code paths
// (e.g., AzurePool's dynamic image refresh) take a Backend explicitly.
package secrets

import (
	"context"
	"errors"
	"os"
)

// ErrNotFound means the requested key has no value in this backend.
var ErrNotFound = errors.New("secret not found")

// Backend retrieves a single secret value by key. Implementations should be
// safe for concurrent use — they're typically goroutine-shared at startup.
type Backend interface {
	Get(ctx context.Context, key string) (string, error)
}

// EnvBackend reads secrets from process environment variables.
// Useful for local development and as a fallback when no cloud backend
// is configured.
type EnvBackend struct{}

// NewEnvBackend returns a Backend that reads from os.Getenv.
func NewEnvBackend() *EnvBackend { return &EnvBackend{} }

// Get returns the value of the env var named by key, or ErrNotFound.
func (b *EnvBackend) Get(_ context.Context, key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", ErrNotFound
	}
	return v, nil
}
