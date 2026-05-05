package secretsproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// AzureKVStore is a KVStore backed by Azure Key Vault. Uses the
// DefaultAzureCredential chain (managed identity in production, az-cli /
// service principal in dev).
type AzureKVStore struct {
	client *azsecrets.Client
}

// NewAzureKVStore opens a client against the named Key Vault. vaultName is
// the short name (e.g. "opensandbox-dev-kv"); the URL is derived. Returns
// nil + error if credentials can't be obtained or the vault URL is invalid.
func NewAzureKVStore(vaultName string) (*AzureKVStore, error) {
	if vaultName == "" {
		return nil, errors.New("vault name required")
	}
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure credentials: %w", err)
	}
	url := fmt.Sprintf("https://%s.vault.azure.net/", vaultName)
	client, err := azsecrets.NewClient(url, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("kv client: %w", err)
	}
	return &AzureKVStore{client: client}, nil
}

func (s *AzureKVStore) Get(ctx context.Context, name string) ([]byte, error) {
	resp, err := s.client.GetSecret(ctx, name, "", nil)
	if err != nil {
		// Azure SDK doesn't expose a direct "not found" sentinel, but the
		// ResponseError carries the status code.
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil, ErrNotFound
		}
		// SecretNotFound is the canonical body code for the same condition;
		// older SDK versions surface it as a string rather than via status.
		if strings.Contains(err.Error(), "SecretNotFound") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if resp.Value == nil {
		return nil, ErrNotFound
	}
	return []byte(*resp.Value), nil
}

func (s *AzureKVStore) Set(ctx context.Context, name string, value []byte) error {
	v := string(value)
	_, err := s.client.SetSecret(ctx, name, azsecrets.SetSecretParameters{Value: &v}, nil)
	return err
}
