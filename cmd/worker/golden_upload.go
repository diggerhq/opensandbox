package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/opensandbox/opensandbox/internal/blobstore"
	"github.com/opensandbox/opensandbox/internal/config"
	qm "github.com/opensandbox/opensandbox/internal/qemu"
)

// uploadGolden is invoked by the `opensandbox-worker golden-upload <path>`
// subcommand. It pushes the local default.ext4 to the configured global blob
// store under two keys:
//
//	{GoldensBucket}/default.ext4              — the "current" pointer
//	{GoldensBucket}/bases/{hash}/default.ext4 — versioned by content hash
//
// Workers default to fetching `default.ext4` on cache miss; cross-golden
// migrations look up `bases/{hash}/default.ext4` by hash. Uploading both
// covers both call sites in one shot.
//
// Used as:
//   - One-shot bootstrap on dev2: ssh in, run this once, populate Tigris
//   - Future Packer post-processor: run after the AMI build's rootfs step
//
// Reads OPENSANDBOX_GLOBAL_BLOB_* env vars for endpoint + creds. Fails loud
// if blobstore isn't configured.
func uploadGolden(path string) error {
	// Bootstrap from KV the same way the main worker does — so this
	// subcommand picks up worker-global-blob-* secrets when run on a
	// host whose worker.env points at a KV. Without this, the user
	// would have to manually export every Tigris env var inline.
	if err := config.LoadSecretsFromKeyVault(); err != nil {
		return fmt.Errorf("keyvault bootstrap: %w", err)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}
	if cfg.GlobalBlobEndpoint == "" || cfg.GlobalBlobAccessKeyID == "" {
		return fmt.Errorf("OPENSANDBOX_GLOBAL_BLOB_ENDPOINT and _ACCESS_KEY_ID required")
	}
	if cfg.GlobalBlobGoldensBucket == "" {
		return fmt.Errorf("OPENSANDBOX_GLOBAL_BLOB_GOLDENS_BUCKET required")
	}

	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	hash, err := qm.ComputeGoldenVersion(path)
	if err != nil {
		return fmt.Errorf("compute hash: %w", err)
	}

	store, err := blobstore.NewS3(blobstore.S3Config{
		Name:            cfg.GlobalBlobName,
		Endpoint:        cfg.GlobalBlobEndpoint,
		Region:          cfg.GlobalBlobRegion,
		AccessKeyID:     cfg.GlobalBlobAccessKeyID,
		SecretAccessKey: cfg.GlobalBlobSecretAccessKey,
		UsePathStyle:    cfg.GlobalBlobUsePathStyle,
	})
	if err != nil {
		return fmt.Errorf("blobstore init: %w", err)
	}
	if store == nil {
		return fmt.Errorf("blobstore returned nil — check endpoint/access-key env vars")
	}

	versionedKey := fmt.Sprintf("bases/%s/default.ext4", hash)
	bucket := cfg.GlobalBlobGoldensBucket

	log.Printf("uploading %s (%.1fMB, hash=%s) to %s://%s/{default.ext4, %s}",
		path, float64(st.Size())/(1024*1024), hash, store.Name(), bucket, versionedKey)
	t0 := time.Now()

	// Long timeout — multi-GB upload over residential or cross-region network.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// Upload versioned key first — it's idempotent (hash-named), so a later
	// retry won't double-charge if the second upload fails.
	if err := blobstore.Upload(ctx, store, bucket, versionedKey, path); err != nil {
		return fmt.Errorf("upload %s: %w", versionedKey, err)
	}
	log.Printf("uploaded %s (%dms)", versionedKey, time.Since(t0).Milliseconds())

	t1 := time.Now()
	if err := blobstore.Upload(ctx, store, bucket, "default.ext4", path); err != nil {
		return fmt.Errorf("upload default.ext4: %w", err)
	}
	log.Printf("uploaded default.ext4 (%dms)", time.Since(t1).Milliseconds())

	log.Printf("done in %s", time.Since(t0).Round(time.Millisecond))
	return nil
}
