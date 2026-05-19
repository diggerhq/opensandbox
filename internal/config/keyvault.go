// Package config — Azure Key Vault implementation of SecretsProvider.
//
// Authentication uses Azure Default Credential (Managed Identity on VMs,
// CLI credentials locally). The trigger env var is OPENSANDBOX_AZURE_KEY_VAULT_NAME
// (legacy: SECRETS_VAULT_NAME); LoadSecrets() in secrets.go selects this
// provider when either is set.

package config

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// azureKeyVaultProvider fetches secrets by listing the vault and dereferencing
// every name that matches secretMapping for the current mode.
type azureKeyVaultProvider struct {
	vaultName string
}

func (p *azureKeyVaultProvider) Name() string { return "azure-keyvault" }

func (p *azureKeyVaultProvider) Load(ctx context.Context, mode string) (int, int, error) {
	vaultURL := fmt.Sprintf("https://%s.vault.azure.net/", p.vaultName)

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return 0, 0, fmt.Errorf("keyvault: azure credential: %w", err)
	}

	client, err := azsecrets.NewClient(vaultURL, cred, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("keyvault: client: %w", err)
	}

	loaded, skipped := 0, 0

	pager := client.NewListSecretPropertiesPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return loaded, skipped, fmt.Errorf("keyvault: list secrets: %w", err)
		}

		for _, prop := range page.Value {
			name := prop.ID.Name()
			envVar, mapped := secretMapping[name]
			if !mapped {
				continue
			}
			if !shouldLoadForMode(name, mode) {
				continue
			}
			// Skip the network round-trip when the env is already set —
			// callers can override locally without paying for the GET.
			if os.Getenv(envVar) != "" {
				skipped++
				continue
			}

			resp, err := client.GetSecret(ctx, name, "", nil)
			if err != nil {
				log.Printf("keyvault: failed to get secret %s: %v (skipping)", name, err)
				continue
			}
			if resp.Value == nil {
				continue
			}

			if setIfUnset(envVar, *resp.Value) {
				loaded++
			} else {
				skipped++
			}
		}
	}

	return loaded, skipped, nil
}
