package sandbox

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// HibernateResult holds the result of a hibernate operation.
type HibernateResult struct {
	SandboxID     string `json:"sandboxId"`
	CheckpointKey string `json:"checkpointKey"`
	SizeBytes     int64  `json:"sizeBytes"`
}

// Hibernate checkpoints a running sandbox, uploads to S3, and removes the container.
// The CRIU checkpoint (process memory) is cached on local NVMe for fast same-machine wake.
// The workspace directory stays on local NVMe and is archived to S3 async for cross-machine wake.
func (m *Manager) Hibernate(ctx context.Context, sandboxID string, checkpointStore *storage.CheckpointStore) (*HibernateResult, error) {
	name := m.ContainerName(sandboxID)

	// 1. Trim memory before checkpoint (best-effort)
	m.trimBeforeCheckpoint(ctx, name)

	// 2. Generate S3 key upfront so we can use it for cache path
	s3Key := storage.CheckpointKey(sandboxID)

	// 3. Checkpoint to local file
	//    If NVMe cache is enabled, write directly to cache dir (stays after upload).
	//    Otherwise use a temp dir (cleaned up after upload).
	var localPath string
	var cleanup func()

	cachePath := checkpointStore.CachePath(s3Key)
	if cachePath != "" && !m.podman.UseSSH() {
		// Linux with NVMe cache: checkpoint directly into cache dir
		localPath = cachePath
		cleanup = func() {} // keep the file — it's the cache
	} else {
		// macOS or no cache: use temp dir
		tmpDir, err := os.MkdirTemp("", "osb-checkpoint-")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp dir: %w", err)
		}
		cleanup = func() { os.RemoveAll(tmpDir) }
		localPath = filepath.Join(tmpDir, "checkpoint.tar.zst")
	}
	defer cleanup()

	if m.podman.UseSSH() {
		// On macOS: checkpoint inside VM, then copy archive to host
		vmPath := fmt.Sprintf("/tmp/osb-checkpoint-%s.tar.zst", sandboxID)
		if err := m.podman.CheckpointContainer(ctx, name, vmPath); err != nil {
			return nil, fmt.Errorf("checkpoint failed for sandbox %s: %w", sandboxID, err)
		}
		defer m.podman.RemoveVMFile(ctx, vmPath)

		if err := m.podman.CopyFromVM(ctx, vmPath, localPath); err != nil {
			return nil, fmt.Errorf("failed to copy checkpoint from VM for sandbox %s: %w", sandboxID, err)
		}
	} else {
		// On Linux: checkpoint directly to local path (NVMe cache or temp)
		if err := m.podman.CheckpointContainer(ctx, name, localPath); err != nil {
			return nil, fmt.Errorf("checkpoint failed for sandbox %s: %w", sandboxID, err)
		}
	}

	// 4. Upload CRIU checkpoint to S3 (source of truth)
	sizeBytes, err := checkpointStore.Upload(ctx, s3Key, localPath)
	if err != nil {
		return nil, fmt.Errorf("failed to upload checkpoint for sandbox %s: %w", sandboxID, err)
	}

	// 5. Remove the container (checkpoint --export already stopped it)
	_ = m.podman.RemoveContainer(ctx, name, true)

	// 6. Archive workspace to S3 in background (for cross-machine wake).
	// The workspace directory stays on local NVMe for fast same-machine wake.
	if m.dataDir != "" {
		workspaceDir := filepath.Join(m.dataDir, sandboxID, "workspace")
		if dirExists(workspaceDir) {
			go func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				if err := m.archiveWorkspaceToS3(bgCtx, sandboxID, workspaceDir, checkpointStore); err != nil {
					log.Printf("hibernate: workspace backup failed for %s: %v (workspace still on local disk)", sandboxID, err)
				} else {
					log.Printf("hibernate: workspace backed up to S3 for %s", sandboxID)
				}
			}()
		}
	}

	return &HibernateResult{
		SandboxID:     sandboxID,
		CheckpointKey: s3Key,
		SizeBytes:     sizeBytes,
	}, nil
}

// Wake restores a hibernated sandbox.
// Fast path: if CRIU checkpoint + workspace are both on local NVMe, no S3 calls at all.
// Slow path: downloads missing data from S3 (and caches it locally for next time).
func (m *Manager) Wake(ctx context.Context, sandboxID string, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	name := m.ContainerName(sandboxID)

	// 1. Ensure workspace is available
	if m.dataDir != "" {
		workspaceDir := filepath.Join(m.dataDir, sandboxID, "workspace")
		if !dirExists(workspaceDir) {
			log.Printf("wake: workspace not found locally for %s, downloading from S3", sandboxID)
			if err := m.restoreWorkspaceFromS3(ctx, sandboxID, workspaceDir, checkpointStore); err != nil {
				log.Printf("wake: workspace restore failed for %s: %v (creating empty workspace)", sandboxID, err)
				if mkErr := os.MkdirAll(workspaceDir, 0755); mkErr != nil {
					return nil, fmt.Errorf("failed to create workspace dir for sandbox %s: %w", sandboxID, mkErr)
				}
			}
		} else {
			log.Printf("wake: workspace found locally for %s", sandboxID)
		}
	}

	// 2. Restore CRIU checkpoint — local NVMe fast path or S3 fallback
	if checkpointStore.CacheHit(checkpointKey) {
		// Fast path: restore directly from NVMe file — no S3, no temp file copy
		cachePath := checkpointStore.CachePath(checkpointKey)
		log.Printf("wake: restoring %s from local NVMe cache", sandboxID)
		if err := m.podman.RestoreContainer(ctx, cachePath, name); err != nil {
			return nil, fmt.Errorf("failed to restore sandbox %s from cache: %w", sandboxID, err)
		}
	} else {
		// Slow path: download from S3 (Download() will cache it for next time)
		log.Printf("wake: checkpoint not cached locally for %s, downloading from S3", sandboxID)
		reader, err := checkpointStore.Download(ctx, checkpointKey)
		if err != nil {
			return nil, fmt.Errorf("failed to download checkpoint for sandbox %s: %w", sandboxID, err)
		}
		defer reader.Close()

		if err := m.podman.RestoreContainerFromStream(ctx, reader, name); err != nil {
			return nil, fmt.Errorf("failed to restore sandbox %s: %w", sandboxID, err)
		}
	}

	// 3. Get sandbox info
	info, err := m.podman.InspectContainer(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect restored sandbox %s: %w", sandboxID, err)
	}

	return containerInfoToSandbox(info), nil
}

// trimBeforeCheckpoint reduces the container's memory footprint before checkpointing.
func (m *Manager) trimBeforeCheckpoint(ctx context.Context, containerName string) {
	trimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	commands := [][]string{
		{"/bin/sh", "-c", "sync"},
		{"/bin/sh", "-c", "echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || true"},
	}

	for _, cmd := range commands {
		_, _ = m.podman.ExecInContainer(trimCtx, podman.ExecConfig{
			Container: containerName,
			Command:   cmd,
		})
	}
}

// WorkspaceS3Key returns the S3 key for a sandbox's workspace archive.
func WorkspaceS3Key(sandboxID string) string {
	return fmt.Sprintf("workspaces/%s/workspace.tar.zst", sandboxID)
}

// archiveWorkspaceToS3 creates a tar.zst archive of the workspace directory and uploads it to S3.
func (m *Manager) archiveWorkspaceToS3(ctx context.Context, sandboxID, workspaceDir string, store *storage.CheckpointStore) error {
	archivePath, err := createTarZstd(workspaceDir)
	if err != nil {
		return fmt.Errorf("failed to archive workspace: %w", err)
	}
	defer os.Remove(archivePath)

	s3Key := WorkspaceS3Key(sandboxID)
	if _, err := store.Upload(ctx, s3Key, archivePath); err != nil {
		return fmt.Errorf("failed to upload workspace archive: %w", err)
	}
	return nil
}

// restoreWorkspaceFromS3 downloads a workspace archive from S3 and extracts it.
func (m *Manager) restoreWorkspaceFromS3(ctx context.Context, sandboxID, workspaceDir string, store *storage.CheckpointStore) error {
	s3Key := WorkspaceS3Key(sandboxID)
	reader, err := store.Download(ctx, s3Key)
	if err != nil {
		return fmt.Errorf("workspace archive not found in S3: %w", err)
	}
	defer reader.Close()

	if err := os.MkdirAll(workspaceDir, 0755); err != nil {
		return fmt.Errorf("failed to create workspace dir: %w", err)
	}

	return extractTarZstd(reader, workspaceDir)
}

// createTarZstd creates a zstd-compressed tar archive of a directory.
// Returns the path to the temp file containing the archive.
func createTarZstd(srcDir string) (string, error) {
	tmpFile, err := os.CreateTemp("", "osb-workspace-*.tar.zst")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()

	zw, err := zstd.NewWriter(tmpFile, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return "", err
	}

	tw := tar.NewWriter(zw)

	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = relPath

		// Handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = link
		}

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})

	// Close in reverse order: tar → zstd → file
	tw.Close()
	zw.Close()
	tmpFile.Close()

	if err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	return tmpPath, nil
}

// extractTarZstd extracts a zstd-compressed tar archive from a reader into destDir.
func extractTarZstd(reader io.Reader, destDir string) error {
	zr, err := zstd.NewReader(reader)
	if err != nil {
		return fmt.Errorf("failed to create zstd reader: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read error: %w", err)
		}

		// Sanitize the path to prevent directory traversal
		target := filepath.Join(destDir, header.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)) {
			return fmt.Errorf("tar entry %q attempts path traversal", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		}
	}

	return nil
}

// dirExists returns true if the path exists and is a directory.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
