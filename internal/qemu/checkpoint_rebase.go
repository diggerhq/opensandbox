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
	"time"
)

// Pin-to-base: checkpoints stay tied to the exact goldenVersion they were
// created against. On fork/restore we ensure that base is available locally
// (either as the current default.ext4, a retained previous base on disk, or
// an on-demand blob download) and do a metadata-only qemu-img rebase -u to
// point the overlay's backing_file field at it. No block copying ever.
//
// Earlier attempts to rebase overlays across goldens (Variants A, B, C) all
// produced subtle corruption: memory dumps reference disk content from the
// old base, so swapping in new-base content under them breaks consistency.

// ensureCheckpointRebased ensures the checkpoint's rootfs.qcow2 backing file
// points at the correct base for its pinned goldenVersion. Name kept for
// call-site stability.
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

	if meta.GoldenVersion == "" {
		return m.checkLegacyCheckpoint(checkpointID, meta)
	}

	basePath, err := m.resolveBaseForVersion(ctx, meta.GoldenVersion)
	if err != nil {
		return fmt.Errorf("resolve base %s: %w", meta.GoldenVersion, err)
	}

	rootfs := filepath.Join(cacheDir, "rootfs.qcow2")
	if !fileExists(rootfs) {
		return nil
	}

	m.checkpointCacheMu.Lock()
	defer m.checkpointCacheMu.Unlock()

	return rebaseMetadataOnly(ctx, rootfs, basePath)
}

// rebaseMetadataOnly runs qemu-img rebase -u to repoint an overlay's backing
// file without touching data clusters.
func rebaseMetadataOnly(ctx context.Context, overlayPath, newBasePath string) error {
	cmd := exec.CommandContext(ctx, "qemu-img", "rebase", "-u", "-b", newBasePath, "-F", "raw", overlayPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img rebase -u: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveBaseForVersion returns a local path to the base image matching the
// given goldenVersion, downloading from blob storage if needed. Downloaded
// bases are cached persistently at ImagesDir/bases/{version}/default.ext4.
func (m *Manager) resolveBaseForVersion(ctx context.Context, goldenVersion string) (string, error) {
	if goldenVersion == "" {
		return "", fmt.Errorf("empty goldenVersion")
	}
	if goldenVersion == m.GoldenVersion() {
		return filepath.Join(m.cfg.ImagesDir, "default.ext4"), nil
	}

	retained := filepath.Join(m.cfg.ImagesDir, "bases", goldenVersion, "default.ext4")
	if fileExists(retained) {
		return retained, nil
	}

	if err := m.downloadBaseToLocal(ctx, goldenVersion, retained); err != nil {
		return "", err
	}
	return retained, nil
}

// downloadBaseToLocal fetches bases/{goldenVersion}/default.ext4 from blob
// storage. Concurrent callers share one download through an in-flight map.
func (m *Manager) downloadBaseToLocal(ctx context.Context, goldenVersion, destPath string) error {
	flightMu.Lock()
	if ch, downloading := downloadFlight[goldenVersion]; downloading {
		flightMu.Unlock()
		select {
		case <-ch:
			if fileExists(destPath) {
				return nil
			}
			return m.downloadBaseToLocal(ctx, goldenVersion, destPath)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ch := make(chan struct{})
	downloadFlight[goldenVersion] = ch
	flightMu.Unlock()
	defer func() {
		flightMu.Lock()
		delete(downloadFlight, goldenVersion)
		flightMu.Unlock()
		close(ch)
	}()

	if fileExists(destPath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("mkdir base cache: %w", err)
	}

	log.Printf("qemu: downloading base %s from blob storage", goldenVersion)
	t0 := time.Now()

	key := fmt.Sprintf("bases/%s/default.ext4", goldenVersion)
	reader, err := m.checkpointStore.Download(ctx, key)
	if err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	defer reader.Close()

	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), "default-dl-*.ext4")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write base image: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}

	log.Printf("qemu: base %s cached at %s (%dms)", goldenVersion, destPath, time.Since(t0).Milliseconds())
	return nil
}

var (
	flightMu       sync.Mutex
	downloadFlight = map[string]chan struct{}{}
)

// UploadBaseImageIfNew archives the current base to blob storage if this
// golden version hasn't been stored yet. Lets workers rolled up later pull
// back checkpoints pinned to earlier goldens.
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

// checkLegacyCheckpoint handles checkpoints that predate goldenVersion
// tracking. If snapshot-at time is after the current base install, we trust
// the current base is compatible. Otherwise we can't prove compatibility.
func (m *Manager) checkLegacyCheckpoint(checkpointID string, meta SnapshotMeta) error {
	baseImage := filepath.Join(m.cfg.ImagesDir, "default.ext4")
	stat, err := os.Stat(baseImage)
	if err != nil {
		return nil
	}
	baseInstalled := stat.ModTime()

	if meta.SnapshotedAt.IsZero() || meta.SnapshotedAt.After(baseInstalled) {
		return nil
	}
	return fmt.Errorf(
		"checkpoint %s predates current base image (checkpoint created %s, "+
			"base installed %s) and has no goldenVersion recorded. "+
			"Destroy this checkpoint and recreate it",
		checkpointID,
		meta.SnapshotedAt.Format(time.RFC3339),
		baseInstalled.Format(time.RFC3339))
}

