package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opensandbox/opensandbox/internal/storage"
)

// rebaseCheckpointToCurrentBase migrates a checkpoint's rootfs.qcow2 from an old
// golden version to the current base image. Uses qemu-img rebase to copy only the
// blocks that differ between the old and new base into the overlay — the rest is
// resolved via the new base's backing file reference. Result: thin overlay against
// the current base, with size = original overlay + delta between bases.
func (m *Manager) rebaseCheckpointToCurrentBase(ctx context.Context, checkpointDir, oldGoldenVersion string) error {
	rootfs := filepath.Join(checkpointDir, "rootfs.qcow2")
	if !fileExists(rootfs) {
		return fmt.Errorf("rootfs.qcow2 not found in %s", checkpointDir)
	}

	oldBasePath, err := downloadOldBase(ctx, m.checkpointStore, oldGoldenVersion)
	if err != nil {
		return fmt.Errorf("download old base %s: %w", oldGoldenVersion, err)
	}

	// Step 1: point overlay at the downloaded old base (metadata-only repoint).
	rebaseCmd := exec.CommandContext(ctx, "qemu-img", "rebase", "-u", "-b", oldBasePath, "-F", "raw", rootfs)
	if out, err := rebaseCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rebase to old base: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Step 2: rebase to the new base. qemu-img compares old_base with new_base block
	// by block; blocks that differ get copied into the overlay so it reads correctly
	// on top of the new base. Preserves internal savevm snapshots.
	newBasePath := filepath.Join(m.cfg.ImagesDir, "default.ext4")
	if !fileExists(newBasePath) {
		return fmt.Errorf("current base %s not found on disk", newBasePath)
	}
	rebaseToCurrent := exec.CommandContext(ctx, "qemu-img", "rebase", "-b", newBasePath, "-F", "raw", rootfs)
	if out, err := rebaseToCurrent.CombinedOutput(); err != nil {
		return fmt.Errorf("rebase to current base: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// oldBaseCache prevents duplicate downloads of the same old base version.
var (
	oldBaseMu    sync.Mutex
	oldBaseCache = map[string]string{} // goldenVersion → local temp path
)

// downloadOldBase downloads an old base image from S3, caching in /tmp.
// Multiple concurrent callers for the same version share one download.
func downloadOldBase(ctx context.Context, store *storage.CheckpointStore, goldenVersion string) (string, error) {
	oldBaseMu.Lock()
	if path, ok := oldBaseCache[goldenVersion]; ok {
		oldBaseMu.Unlock()
		if fileExists(path) {
			return path, nil
		}
	}
	oldBaseMu.Unlock()

	key := fmt.Sprintf("bases/%s/default.ext4", goldenVersion)
	localPath := filepath.Join(os.TempDir(), fmt.Sprintf("old-base-%s.ext4", goldenVersion))

	reader, err := store.Download(ctx, key)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", key, err)
	}
	defer reader.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	if _, err := io.Copy(f, reader); err != nil {
		f.Close()
		os.Remove(localPath)
		return "", fmt.Errorf("write base image: %w", err)
	}
	f.Close()

	oldBaseMu.Lock()
	oldBaseCache[goldenVersion] = localPath
	oldBaseMu.Unlock()

	return localPath, nil
}

// cleanupOldBases removes cached old base images from /tmp.
func cleanupOldBases() {
	oldBaseMu.Lock()
	defer oldBaseMu.Unlock()
	for ver, path := range oldBaseCache {
		os.Remove(path)
		delete(oldBaseCache, ver)
	}
}

// updateCheckpointGoldenVersion rewrites snapshot-meta.json with the new golden version.
func updateCheckpointGoldenVersion(checkpointDir, newGoldenVersion string) error {
	metaPath := filepath.Join(checkpointDir, "snapshot", "snapshot-meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}
	meta.GoldenVersion = newGoldenVersion
	updated, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	return os.WriteFile(metaPath, updated, 0644)
}

// uploadBaseImageIfNew uploads the base ext4 image to S3 if this golden version
// hasn't been archived yet. Enables old checkpoints to be rebased when the base
// image changes across Packer builds.
// UploadBaseImageIfNew archives the current base image to S3 if not already stored.
func (m *Manager) UploadBaseImageIfNew() {
	m.uploadBaseImageIfNew(m.GoldenVersion())
}

func (m *Manager) uploadBaseImageIfNew(goldenVersion string) {
	if m.checkpointStore == nil || goldenVersion == "" {
		return
	}
	baseImage := filepath.Join(m.cfg.ImagesDir, "default.ext4")
	if !fileExists(baseImage) {
		log.Printf("qemu: base image archival skipped: %s not found", baseImage)
		return
	}

	key := fmt.Sprintf("bases/%s/default.ext4", goldenVersion)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exists, err := m.checkpointStore.Exists(ctx, key)
	if err != nil {
		log.Printf("qemu: base image existence check failed: %v", err)
		return
	}
	if exists {
		return
	}

	uploadCtx, uploadCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer uploadCancel()
	if _, err := m.checkpointStore.Upload(uploadCtx, key, baseImage); err != nil {
		log.Printf("qemu: base image upload failed for version %s: %v", goldenVersion, err)
		return
	}
	log.Printf("qemu: base image archived for golden version %s", goldenVersion)
}

// ensureCheckpointRebased checks if a cached checkpoint was created against a
// different golden version and rebases it to the current base if needed.
// Safe to call from the fork hot path — returns immediately if versions match.
func (m *Manager) ensureCheckpointRebased(ctx context.Context, checkpointID string) error {
	if m.checkpointStore == nil {
		return nil
	}

	cacheDir := filepath.Join(m.cfg.DataDir, "checkpoint-snapshots", checkpointID)
	metaPath := filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")

	m.checkpointCacheMu.RLock()
	data, err := os.ReadFile(metaPath)
	m.checkpointCacheMu.RUnlock()
	if err != nil {
		return nil
	}

	var meta SnapshotMeta
	if json.Unmarshal(data, &meta) != nil {
		return nil
	}

	currentVersion := m.GoldenVersion()
	if meta.GoldenVersion == "" {
		return m.checkLegacyCheckpoint(checkpointID, meta)
	}
	if meta.GoldenVersion == currentVersion {
		return nil
	}

	m.checkpointCacheMu.Lock()
	defer m.checkpointCacheMu.Unlock()

	// Re-check under write lock — background goroutine may have already migrated.
	data, err = os.ReadFile(metaPath)
	if err == nil {
		var fresh SnapshotMeta
		if json.Unmarshal(data, &fresh) == nil && fresh.GoldenVersion == currentVersion {
			return nil
		}
	}

	log.Printf("qemu: rebasing checkpoint %s from golden %s to %s", checkpointID, meta.GoldenVersion, currentVersion)
	t0 := time.Now()

	if err := m.rebaseCheckpointToCurrentBase(ctx, cacheDir, meta.GoldenVersion); err != nil {
		return err
	}
	if err := updateCheckpointGoldenVersion(cacheDir, currentVersion); err != nil {
		return err
	}

	log.Printf("qemu: checkpoint %s rebased (%dms)", checkpointID, time.Since(t0).Milliseconds())
	return nil
}

// migrateStaleCheckpoints scans the local checkpoint cache and rebases any
// checkLegacyCheckpoint handles checkpoints that predate goldenVersion tracking.
// If the checkpoint was created after the current base image was installed on
// this worker, it's compatible with the current base and safe to fork.
// If it was created before, we can't verify compatibility — return a clear
// error so the caller knows to recreate the checkpoint rather than hang on
// agent timeout.
func (m *Manager) checkLegacyCheckpoint(checkpointID string, meta SnapshotMeta) error {
	baseImage := filepath.Join(m.cfg.ImagesDir, "default.ext4")
	stat, err := os.Stat(baseImage)
	if err != nil {
		return nil // can't check, let it proceed (best-effort)
	}
	baseInstalled := stat.ModTime()

	if meta.SnapshotedAt.IsZero() || meta.SnapshotedAt.After(baseInstalled) {
		return nil // created after base was installed, safe to fork
	}

	return fmt.Errorf(
		"checkpoint %s predates current base image (checkpoint created %s, "+
			"base installed %s) and has no goldenVersion recorded. "+
			"Cannot migrate automatically — destroy this checkpoint and recreate it",
		checkpointID,
		meta.SnapshotedAt.Format(time.RFC3339),
		baseInstalled.Format(time.RFC3339))
}

// backfillGoldenVersions labels pre-existing cached checkpoints that predate
// goldenVersion tracking but were created against the current base. Uses the
// default.ext4 mtime as the cutoff — any checkpoint created after it was
// necessarily made against the current base.
func (m *Manager) backfillGoldenVersions() {
	if m.checkpointStore == nil {
		return
	}
	currentVersion := m.GoldenVersion()
	if currentVersion == "" {
		return
	}

	baseImage := filepath.Join(m.cfg.ImagesDir, "default.ext4")
	stat, err := os.Stat(baseImage)
	if err != nil {
		return
	}
	baseInstalled := stat.ModTime()

	cacheBase := filepath.Join(m.cfg.DataDir, "checkpoint-snapshots")
	entries, err := os.ReadDir(cacheBase)
	if err != nil {
		return
	}

	var labeled int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cpDir := filepath.Join(cacheBase, e.Name())
		metaPath := filepath.Join(cpDir, "snapshot", "snapshot-meta.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}
		var meta SnapshotMeta
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		if meta.GoldenVersion != "" {
			continue // already labeled
		}
		if meta.SnapshotedAt.IsZero() || !meta.SnapshotedAt.After(baseInstalled) {
			continue // too old to safely label
		}

		meta.GoldenVersion = currentVersion
		updated, err := json.Marshal(meta)
		if err != nil {
			continue
		}
		if err := os.WriteFile(metaPath, updated, 0644); err != nil {
			log.Printf("qemu: backfill: failed to update %s: %v", e.Name(), err)
			continue
		}

		// Re-upload so S3 copy also has the label.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if err := m.reuploadCheckpoint(ctx, e.Name(), cpDir); err != nil {
			log.Printf("qemu: backfill: %s re-upload failed: %v", e.Name(), err)
		}
		cancel()

		labeled++
	}

	if labeled > 0 {
		log.Printf("qemu: backfill: labeled %d checkpoints with goldenVersion=%s", labeled, currentVersion)
	}
}

// checkpoints created against a different golden version. Runs in background
// with bounded concurrency.
// MigrateStaleCheckpoints scans the local cache and rebases stale checkpoints.
func (m *Manager) MigrateStaleCheckpoints() {
	m.migrateStaleCheckpoints()
}

func (m *Manager) migrateStaleCheckpoints() {
	if m.checkpointStore == nil {
		return
	}

	// First pass: label pre-goldenVersion checkpoints that were made against the current base.
	m.backfillGoldenVersions()

	cacheBase := filepath.Join(m.cfg.DataDir, "checkpoint-snapshots")
	entries, err := os.ReadDir(cacheBase)
	if err != nil {
		return
	}

	currentVersion := m.GoldenVersion()
	if currentVersion == "" {
		return
	}

	type staleCP struct {
		id, dir, version string
	}
	var stale []staleCP

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		cpDir := filepath.Join(cacheBase, e.Name())
		data, err := os.ReadFile(filepath.Join(cpDir, "snapshot", "snapshot-meta.json"))
		if err != nil {
			continue
		}
		var meta SnapshotMeta
		if json.Unmarshal(data, &meta) != nil {
			continue
		}
		if meta.GoldenVersion != "" && meta.GoldenVersion != currentVersion {
			stale = append(stale, staleCP{id: e.Name(), dir: cpDir, version: meta.GoldenVersion})
		}
	}

	if len(stale) == 0 {
		return
	}

	log.Printf("qemu: checkpoint migration: %d stale checkpoints to migrate", len(stale))

	// Track old versions so we can evict their bases after migration
	oldVersions := make(map[string]struct{})
	for _, cp := range stale {
		oldVersions[cp.version] = struct{}{}
	}

	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	migrated := int64(0)

	for _, cp := range stale {
		wg.Add(1)
		sem <- struct{}{}
		go func(cp staleCP) {
			defer wg.Done()
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()

			if err := m.ensureCheckpointRebased(ctx, cp.id); err != nil {
				log.Printf("qemu: checkpoint migration: %s failed: %v", cp.id, err)
				return
			}

			// Re-upload flattened checkpoint to S3 so other workers get the migrated version
			if err := m.reuploadCheckpoint(ctx, cp.id, cp.dir); err != nil {
				log.Printf("qemu: checkpoint migration: %s re-upload failed: %v", cp.id, err)
			}

			atomic.AddInt64(&migrated, 1)
			log.Printf("qemu: checkpoint migration: %s complete (rebased + re-uploaded)", cp.id)
		}(cp)
	}

	wg.Wait()
	cleanupOldBases()

	// Note: archived bases in S3 (bases/{version}/default.ext4) are kept forever
	// so month-old checkpoints in S3 that aren't cached on any worker can still
	// be rebased on demand. Storage cost is small (~4GB per Packer build).
	// The `evictOldBase` function exists for future CP-orchestrated cleanup.
	_ = oldVersions

	log.Printf("qemu: checkpoint migration: done (%d/%d migrated)", migrated, len(stale))
}

// reuploadCheckpoint re-archives and re-uploads a migrated checkpoint to S3,
// replacing the old thin overlay (referencing a prior base) with the new thin
// overlay (referencing the current base, including any inter-base diff blocks).
func (m *Manager) reuploadCheckpoint(ctx context.Context, checkpointID, cacheDir string) error {
	if m.checkpointStore == nil {
		return nil
	}

	// Read metadata to get the sandbox ID for the S3 key
	metaPath := filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}

	// Build list of files to archive
	var archiveFiles []string
	archiveFiles = append(archiveFiles, "rootfs.qcow2", "workspace.qcow2")
	if fileExists(filepath.Join(cacheDir, "snapshot-name")) {
		archiveFiles = append(archiveFiles, "snapshot-name")
	}
	if fileExists(filepath.Join(cacheDir, "mem.zst")) {
		archiveFiles = append(archiveFiles, "mem.zst")
	} else if fileExists(filepath.Join(cacheDir, "mem")) {
		archiveFiles = append(archiveFiles, "mem")
	}
	if fileExists(filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")) {
		archiveFiles = append(archiveFiles, filepath.Join("snapshot", "snapshot-meta.json"))
	}

	archivePath := filepath.Join(cacheDir, "migrated.tar.zst")
	if err := createArchive(archivePath, cacheDir, archiveFiles); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer os.Remove(archivePath)

	s3Key := fmt.Sprintf("checkpoints/%s/%s/rootfs.tar.zst", meta.SandboxID, checkpointID)
	if _, err := m.checkpointStore.Upload(ctx, s3Key, archivePath); err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	log.Printf("qemu: checkpoint %s re-uploaded to S3 (flattened)", checkpointID)
	return nil
}

// evictOldBase removes an archived base image from S3 after all local checkpoints
// referencing it have been migrated.
func (m *Manager) evictOldBase(goldenVersion string) {
	if m.checkpointStore == nil || goldenVersion == "" {
		return
	}

	// Verify no local checkpoints still reference this version
	cacheBase := filepath.Join(m.cfg.DataDir, "checkpoint-snapshots")
	entries, _ := os.ReadDir(cacheBase)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cacheBase, e.Name(), "snapshot", "snapshot-meta.json"))
		if err != nil {
			continue
		}
		var meta SnapshotMeta
		if json.Unmarshal(data, &meta) == nil && meta.GoldenVersion == goldenVersion {
			log.Printf("qemu: skipping eviction of base %s — checkpoint %s still references it", goldenVersion, e.Name())
			return
		}
	}

	key := fmt.Sprintf("bases/%s/default.ext4", goldenVersion)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := m.checkpointStore.Delete(ctx, key); err != nil {
		log.Printf("qemu: failed to evict old base %s: %v", goldenVersion, err)
		return
	}
	log.Printf("qemu: evicted old base image %s from S3", goldenVersion)
}
