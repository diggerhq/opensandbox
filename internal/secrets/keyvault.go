package secrets

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// KeyVaultBackend reads secrets from Azure Key Vault.
//
// Authentication uses Azure DefaultAzureCredential (Managed Identity on VMs,
// CLI credentials locally) — no static keys to manage.
//
// Two modes:
//
//   - Get(name) — fetch a single secret by Key Vault name. Used by AzurePool
//     for dynamic image-version refresh and any other on-demand lookups.
//
//   - LoadAllToEnv (BulkLoader) — list every secret in the vault, map names to
//     env var names via NameMap, and Setenv anything not already set. Used at
//     server/worker bootstrap. ModePrefixFilter restricts to "server-*" or
//     "worker-*" depending on which binary is running, so a worker doesn't
//     pull stripe-secret-key etc. into its env.
type KeyVaultBackend struct {
	client *azsecrets.Client
	vault  string

	// NameMap maps Key Vault secret name → env var name. Only entries in
	// this map are loaded by LoadAllToEnv. Empty NameMap = noop bulk-load.
	NameMap map[string]string

	// ModePrefixFilter restricts LoadAllToEnv to secrets whose name starts
	// with "{ModePrefixFilter}-" (plus shared "pg-*" etc.). Empty = no filter.
	// Typical: "server" or "worker".
	ModePrefixFilter string
}

// NewKeyVaultBackend constructs a backend pointed at vaultName. Returns
// (nil, nil) if vaultName is empty (caller treats nil as "Key Vault
// disabled, fall back to EnvBackend"). Returns an error only if the
// Azure SDK can't create credentials/client at all.
func NewKeyVaultBackend(vaultName string, nameMap map[string]string, modePrefix string) (*KeyVaultBackend, error) {
	if vaultName == "" {
		return nil, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("keyvault: azure credential: %w", err)
	}
	vaultURL := fmt.Sprintf("https://%s.vault.azure.net/", vaultName)
	client, err := azsecrets.NewClient(vaultURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("keyvault: client: %w", err)
	}
	return &KeyVaultBackend{
		client:           client,
		vault:            vaultName,
		NameMap:          nameMap,
		ModePrefixFilter: modePrefix,
	}, nil
}

// Get returns the secret value for the named secret. The Key Vault `name`
// is the bare secret name, not a versioned URL — Key Vault returns the
// current version when version is "".
func (b *KeyVaultBackend) Get(ctx context.Context, name string) (string, error) {
	resp, err := b.client.GetSecret(ctx, name, "", nil)
	if err != nil {
		// Azure returns 404 wrapped in azcore.ResponseError; match by
		// substring rather than chase the type tree, same convention the
		// blobstore S3 client uses.
		if isKVNotFound(err) {
			return "", fmt.Errorf("keyvault://%s/%s: %w", b.vault, name, ErrNotFound)
		}
		return "", fmt.Errorf("keyvault: GetSecret %s/%s: %w", b.vault, name, err)
	}
	if resp.Value == nil {
		return "", fmt.Errorf("keyvault://%s/%s: %w", b.vault, name, ErrNotFound)
	}
	return *resp.Value, nil
}

// LoadAllToEnv lists every secret in the vault, applies NameMap +
// ModePrefixFilter, and Setenv anything not already in the process
// environment. Implements BulkLoader.
//
// Existing env vars take precedence — local overrides win, vault never
// clobbers an existing value. Returns (loaded, skipped, err).
func (b *KeyVaultBackend) LoadAllToEnv(ctx context.Context) (loaded, skipped int, err error) {
	if len(b.NameMap) == 0 {
		return 0, 0, nil
	}
	pager := b.client.NewListSecretPropertiesPager(nil)
	for pager.More() {
		page, perr := pager.NextPage(ctx)
		if perr != nil {
			return loaded, skipped, fmt.Errorf("keyvault: list secrets: %w", perr)
		}
		for _, prop := range page.Value {
			name := prop.ID.Name()
			envVar, mapped := b.NameMap[name]
			if !mapped {
				continue
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
			resp, gerr := b.client.GetSecret(ctx, name, "", nil)
			if gerr != nil {
				log.Printf("keyvault: failed to get secret %s: %v (skipping)", name, gerr)
				continue
			}
			if resp.Value == nil {
				continue
			}
			os.Setenv(envVar, *resp.Value)
			loaded++
		}
	}
	log.Printf("keyvault: loaded %d secrets from %s (%d skipped, already set)", loaded, b.vault, skipped)
	return loaded, skipped, nil
}

// isKVNotFound matches the various ways azsecrets signals "secret not
// found" in a way that doesn't require importing the typed error.
func isKVNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SecretNotFound") ||
		strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found")
}
