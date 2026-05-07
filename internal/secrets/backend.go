// Package secrets provides a cloud-neutral runtime secrets fetcher.
//
// Implementations:
//   - EnvBackend            (default; reads from OS env vars)
//   - KeyVaultBackend       (Azure Key Vault — keyvault.go)
//   - SecretsManagerBackend (AWS Secrets Manager — secretsmanager.go)
//   - GCPSecretManager etc. (future; mirror the same pattern)
//
// Most paas-style clouds offer a Key-Vault-equivalent (HashiCorp Vault,
// Doppler, Infisical, Bitwarden Secrets Manager, etc). All of them satisfy
// the Backend interface; adding one is a new file implementing Get + the
// optional BulkLoader, plus a case in cmd/server/main.go's selector.
//
// The CP/worker picks a backend based on cfg.SecretsProvider at startup.
// Cloud-specific code paths (AzurePool dynamic image refresh, server
// bootstrap config) take a Backend explicitly.
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

// BulkLoader is the optional bootstrap pattern: load every secret in a
// scope into the process environment in one call. Used at server/worker
// startup to populate config without a per-secret Get call. Backends that
// don't naturally enumerate (e.g., raw HashiCorp Vault paths) can omit it.
//
// Returned counts: loaded = newly-set env vars; skipped = ones already
// present in env (env wins by convention — local overrides for dev).
type BulkLoader interface {
	LoadAllToEnv(ctx context.Context) (loaded, skipped int, err error)
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
