package blobstore_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/opensandbox/opensandbox/internal/blobstore"
)

// TestFallback_LiveMigration exercises NewMigrationFallback against a real
// Tigris primary + Azure fallback. Skipped unless these env vars are set:
//
//	BLOBSTORE_TEST_ENDPOINT             https://t3.storage.dev
//	BLOBSTORE_TEST_ACCESS_KEY           tid_...
//	BLOBSTORE_TEST_SECRET_KEY           tsec_...
//	BLOBSTORE_TEST_BUCKET               tigris bucket name
//	BLOBSTORE_TEST_AZURE_ACCOUNT        azure storage account name
//	BLOBSTORE_TEST_AZURE_KEY            azure account key
//	BLOBSTORE_TEST_AZURE_CONTAINER      azure container override
//	BLOBSTORE_TEST_PROBE_KEY            a key that exists in Azure but NOT Tigris
//
// Confirms: migration mode falls through to Azure on Tigris NotFound; HA
// mode does NOT fall through.
func TestFallback_LiveMigration(t *testing.T) {
	endpoint := os.Getenv("BLOBSTORE_TEST_ENDPOINT")
	azureAccount := os.Getenv("BLOBSTORE_TEST_AZURE_ACCOUNT")
	probeKey := os.Getenv("BLOBSTORE_TEST_PROBE_KEY")
	if endpoint == "" || azureAccount == "" || probeKey == "" {
		t.Skip("BLOBSTORE_TEST_* + AZURE_* + PROBE_KEY env not set; skipping live migration smoke")
	}

	primary, err := blobstore.NewS3(blobstore.S3Config{
		Name:            "tigris",
		Endpoint:        endpoint,
		Region:          "auto",
		AccessKeyID:     os.Getenv("BLOBSTORE_TEST_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("BLOBSTORE_TEST_SECRET_KEY"),
		UsePathStyle:    true,
		Bucket:          os.Getenv("BLOBSTORE_TEST_BUCKET"),
	})
	if err != nil {
		t.Fatalf("NewS3 primary: %v", err)
	}
	fallback, err := blobstore.NewAzure(blobstore.AzureConfig{
		Name:        "azure-blob",
		AccountName: azureAccount,
		AccountKey:  os.Getenv("BLOBSTORE_TEST_AZURE_KEY"),
		Bucket:      os.Getenv("BLOBSTORE_TEST_AZURE_CONTAINER"),
	})
	if err != nil {
		t.Fatalf("NewAzure fallback: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("MigrationMode_FallsThroughOnNotFound", func(t *testing.T) {
		store, err := blobstore.NewMigrationFallback(primary, fallback)
		if err != nil {
			t.Fatalf("NewMigrationFallback: %v", err)
		}
		// Caller passes the primary's bucket (Tigris); fallback override
		// rewrites to its own container.
		r, err := store.Get(ctx, "ignored-because-overridden", probeKey)
		if err != nil {
			t.Fatalf("Get via migration fallback: %v", err)
		}
		body, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(body) == 0 {
			t.Fatalf("expected non-empty body from Azure fallback, got 0 bytes")
		}
		t.Logf("MIGRATION: fetched %d bytes from %s via fallback chain (probe=%q)", len(body), store.Name(), probeKey)
	})

	t.Run("HAMode_DoesNotFallThroughOnNotFound", func(t *testing.T) {
		store, err := blobstore.NewFallback(primary, fallback)
		if err != nil {
			t.Fatalf("NewFallback: %v", err)
		}
		_, err = store.Get(ctx, "ignored", probeKey)
		if !errors.Is(err, blobstore.ErrNotFound) {
			t.Fatalf("HA mode: want ErrNotFound (primary authoritative), got %v", err)
		}
		t.Logf("HA: got expected ErrNotFound from primary (no fall-through)")
	})
}
