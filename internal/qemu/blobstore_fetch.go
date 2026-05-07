package qemu

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/opensandbox/opensandbox/internal/blobstore"
)

// ensureBaseImageFromBlob makes sure ImagesDir/default.ext4 exists locally.
// If it's already on disk, returns nil immediately (no-op for the steady
// state where the AMI baked in default.ext4).
//
// Otherwise, if a global blob store is configured, fetches default.ext4
// from {GlobalBlobGoldensBucket}/{GlobalBlobGoldenKey} and writes it to
// ImagesDir/default.ext4 atomically.
//
// Used by:
//   - PrepareGoldenSnapshot (worker startup) — ensure local copy before
//     building the runtime golden snapshot
//   - First-run on a fresh cell whose AMI doesn't carry the rootfs (e.g.
//     a thin AMI shared across clouds)
//
// Returns nil with a log warning if blobstore is unconfigured — callers
// proceed with whatever's local (or fail downstream with a clear error).
func (m *Manager) ensureBaseImageFromBlob(ctx context.Context) error {
	target := filepath.Join(m.cfg.ImagesDir, "default.ext4")
	if fileExists(target) {
		return nil // already cached locally
	}

	if m.cfg.GlobalBlob == nil {
		log.Printf("qemu: %s missing and no global blob store configured", target)
		return nil // not an error here — caller logs/handles missing rootfs
	}

	bucket := m.cfg.GlobalBlobGoldensBucket
	key := m.cfg.GlobalBlobGoldenKey
	if key == "" {
		key = "default.ext4"
	}
	if bucket == "" {
		return errors.New("qemu: GlobalBlobGoldensBucket required to fetch from blob store")
	}

	log.Printf("qemu: %s missing, fetching from %s://%s/%s", target, m.cfg.GlobalBlob.Name(), bucket, key)
	t0 := time.Now()

	// Use a long timeout — golden blob is multi-GB.
	dlCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	if err := blobstore.Download(dlCtx, m.cfg.GlobalBlob, bucket, key, target); err != nil {
		// Clean up any half-written tmp file (Download already does this on
		// most paths but be defensive).
		os.Remove(target + ".tmp")
		return fmt.Errorf("download %s/%s: %w", bucket, key, err)
	}

	// Sanity check the downloaded file is non-trivial.
	st, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("stat downloaded ext4: %w", err)
	}
	if st.Size() < 64*1024*1024 {
		// Anything under 64MB is almost certainly corrupt — a real rootfs
		// is multi-GB. Refuse to proceed; let the caller surface the error.
		os.Remove(target)
		return fmt.Errorf("downloaded ext4 suspiciously small (%d bytes), refusing to use", st.Size())
	}

	log.Printf("qemu: fetched %s from blob store (%.1fMB, %dms)",
		target, float64(st.Size())/(1024*1024), time.Since(t0).Milliseconds())
	return nil
}
