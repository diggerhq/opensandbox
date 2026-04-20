package storage

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// S3Config holds the configuration for the S3 storage backend.
type S3Config struct {
	Endpoint        string
	Bucket          string
	Region          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
}

// CheckpointStore manages checkpoint archives in S3-compatible object storage,
// with an optional local NVMe cache for fast same-machine wake.
//
// S3 is always the source of truth. Local NVMe is a hot cache.
// On hibernate: CRIU checkpoint + workspace both uploaded to S3, kept locally.
// On wake: check NVMe first, fall back to S3 download.
// Eviction: LRU based on real filesystem pressure (keep 20% free for active sandboxes).
type CheckpointStore struct {
	blob     BlobClient
	bucket   string
	cacheDir string // local NVMe cache for CRIU checkpoints (empty = disabled)
	cacheMu  sync.Mutex
}

// NewCheckpointStoreFromClient creates a CheckpointStore using an existing
// BlobClient. Useful for testing with a mock backend.
func NewCheckpointStoreFromClient(blob BlobClient, bucket string) *CheckpointStore {
	return &CheckpointStore{blob: blob, bucket: bucket}
}

// NewCheckpointStore creates a new checkpoint store.
// Automatically selects Azure Blob or AWS S3 based on the endpoint URL.
func NewCheckpointStore(cfg S3Config) (*CheckpointStore, error) {
	blob, err := NewBlobClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create blob client: %w", err)
	}

	log.Printf("storage: using %s backend (endpoint=%s, bucket=%s)", blob.BackendName(), cfg.Endpoint, cfg.Bucket)

	return &CheckpointStore{
		blob:   blob,
		bucket: cfg.Bucket,
	}, nil
}

// SetCacheDir enables local NVMe checkpoint caching at the given directory.
func (s *CheckpointStore) SetCacheDir(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create checkpoint cache dir: %w", err)
	}
	s.cacheDir = dir
	entries, totalMB, availMB := s.CacheStats()
	log.Printf("checkpoint-cache: enabled at %s (%d cached entries, %d MB used, %d MB available)",
		dir, entries, totalMB, availMB)
	return nil
}

// HibernationKey returns the S3 key for a hibernation archive.
func HibernationKey(sandboxID string) string {
	return fmt.Sprintf("checkpoints/%s/%d.tar.zst", sandboxID, time.Now().UnixNano())
}

// cacheFilename returns a stable filename for a given S3 key.
func cacheFilename(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x.tar.zst", h[:16])
}

// CachePath returns the local NVMe cache path for a checkpoint key, or empty if caching is disabled.
func (s *CheckpointStore) CachePath(key string) string {
	if s.cacheDir == "" {
		return ""
	}
	return filepath.Join(s.cacheDir, cacheFilename(key))
}

// CacheHit returns true if the checkpoint exists in the local cache.
func (s *CheckpointStore) CacheHit(key string) bool {
	if s.cacheDir == "" {
		return false
	}
	_, err := os.Stat(s.CachePath(key))
	return err == nil
}

// Upload uploads a checkpoint archive from a local file to S3.
// If caching is enabled, the file is also copied into the local NVMe cache.
// Exists checks whether an object exists in S3/blob storage.
func (s *CheckpointStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.blob.Head(ctx, s.bucket, key)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *CheckpointStore) Upload(ctx context.Context, key, localPath string) (int64, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open checkpoint file: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("failed to stat checkpoint file: %w", err)
	}

	if err := s.blob.Upload(ctx, s.bucket, key, f, stat.Size()); err != nil {
		return 0, fmt.Errorf("failed to upload checkpoint: %w", err)
	}

	// Cache locally for fast same-machine wake
	if s.cacheDir != "" {
		s.cacheFromFile(localPath, key, stat.Size())
	}

	return stat.Size(), nil
}

// cacheFromFile copies or hard-links a checkpoint into the local NVMe cache.
func (s *CheckpointStore) cacheFromFile(localPath, key string, size int64) {
	s.evictIfNeeded()

	cachePath := s.CachePath(key)

	// Try hard link first (instant, zero copy)
	if err := os.Link(localPath, cachePath); err == nil {
		log.Printf("checkpoint-cache: stored %s (%d MB, hard-link)", key, size>>20)
		return
	}

	// Hard link failed (cross-device, e.g. /tmp → /data) — file copy
	src, err := os.Open(localPath)
	if err != nil {
		log.Printf("checkpoint-cache: failed to cache %s: %v", key, err)
		return
	}
	defer src.Close()

	dst, err := os.CreateTemp(s.cacheDir, ".cache-tmp-*")
	if err != nil {
		log.Printf("checkpoint-cache: failed to create temp: %v", err)
		return
	}
	tmpPath := dst.Name()

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		log.Printf("checkpoint-cache: copy failed for %s: %v", key, err)
		return
	}
	dst.Close()

	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("checkpoint-cache: rename failed for %s: %v", key, err)
		return
	}

	log.Printf("checkpoint-cache: stored %s (%d MB, file-copy)", key, size>>20)
}

// Download returns an io.ReadCloser for the checkpoint.
// Checks local NVMe cache first, falls back to S3.
// On S3 download, caches the result locally for next time.
func (s *CheckpointStore) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.cacheDir != "" {
		if reader, err := s.openCached(key); err == nil {
			return reader, nil
		}
		// Cache miss — download from S3 and cache
		return s.downloadAndCache(ctx, key)
	}
	return s.downloadFromS3(ctx, key)
}

// openCached opens a checkpoint from the local NVMe cache.
func (s *CheckpointStore) openCached(key string) (io.ReadCloser, error) {
	cachePath := s.CachePath(key)
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	// Touch mtime for LRU tracking
	now := time.Now()
	_ = os.Chtimes(cachePath, now, now)

	stat, _ := f.Stat()
	log.Printf("checkpoint-cache: HIT %s (%d MB from NVMe)", key, stat.Size()>>20)
	return f, nil
}

// downloadAndCache downloads from S3 using parallel byte-range requests,
// writes to NVMe cache, returns the cached file.
func (s *CheckpointStore) downloadAndCache(ctx context.Context, key string) (io.ReadCloser, error) {
	s.evictIfNeeded()

	cachePath := s.CachePath(key)
	tmpFile, err := os.CreateTemp(s.cacheDir, ".dl-tmp-*")
	if err != nil {
		// Can't write to cache — fall back to single-stream
		log.Printf("checkpoint-cache: can't create temp, falling back to single-stream: %v", err)
		return s.downloadFromS3(ctx, key)
	}
	tmpPath := tmpFile.Name()

	// Get object size
	totalSize, err := s.blob.Head(ctx, s.bucket, key)
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to head checkpoint: %w", err)
	}
	if totalSize <= 0 {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("checkpoint has zero size in S3: %s", key)
	}

	// Pre-allocate the file
	if err := tmpFile.Truncate(totalSize); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to preallocate temp file: %w", err)
	}

	const chunkSize = 64 * 1024 * 1024 // 64 MB chunks
	const maxParallel = 16

	numChunks := int((totalSize + chunkSize - 1) / chunkSize)
	t0 := time.Now()
	log.Printf("checkpoint-cache: downloading %s (%.1f MB, %d chunks, %d parallel)",
		key, float64(totalSize)/(1024*1024), numChunks, maxParallel)

	// Download chunks in parallel
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)
	errs := make([]error, numChunks)

	for i := 0; i < numChunks; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()

			start := int64(idx) * chunkSize
			end := start + chunkSize - 1
			if end >= totalSize {
				end = totalSize - 1
			}
			chunkLen := end - start + 1

			body, err := s.blob.DownloadRange(ctx, s.bucket, key, start, chunkLen)
			if err != nil {
				errs[idx] = fmt.Errorf("chunk %d: %w", idx, err)
				return
			}
			defer body.Close()

			buf, err := io.ReadAll(body)
			if err != nil {
				errs[idx] = fmt.Errorf("chunk %d read: %w", idx, err)
				return
			}

			if _, err := tmpFile.WriteAt(buf, start); err != nil {
				errs[idx] = fmt.Errorf("chunk %d write: %w", idx, err)
				return
			}
		}(i)
	}
	wg.Wait()

	tmpFile.Close()

	// Check for errors
	for _, e := range errs {
		if e != nil {
			os.Remove(tmpPath)
			return nil, fmt.Errorf("parallel download failed: %w", e)
		}
	}

	elapsed := time.Since(t0)
	mbPerSec := float64(totalSize) / (1024 * 1024) / elapsed.Seconds()

	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("checkpoint-cache: rename failed: %v", err)
	} else {
		log.Printf("checkpoint-cache: MISS %s (%.1f MB downloaded from S3 in %dms, %.0f MB/s, now cached)",
			key, float64(totalSize)/(1024*1024), elapsed.Milliseconds(), mbPerSec)
	}

	f, err := os.Open(cachePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open cached checkpoint: %w", err)
	}
	return f, nil
}

// downloadFromS3 streams directly from S3 (no caching).
func (s *CheckpointStore) downloadFromS3(ctx context.Context, key string) (io.ReadCloser, error) {
	body, err := s.blob.Download(ctx, s.bucket, key)
	if err != nil {
		return nil, fmt.Errorf("failed to download checkpoint: %w", err)
	}
	return body, nil
}

// evictIfNeeded removes the oldest cached checkpoints when the filesystem
// is low on space. Policy: keep 20% of total filesystem space free for
// active sandboxes (workspaces, container layers, temp files).
func (s *CheckpointStore) evictIfNeeded() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	var stat unix.Statfs_t
	if err := unix.Statfs(s.cacheDir, &stat); err != nil {
		log.Printf("checkpoint-cache: statfs failed: %v", err)
		return
	}

	totalBytes := stat.Blocks * uint64(stat.Bsize)
	availBytes := stat.Bavail * uint64(stat.Bsize)
	reserveBytes := totalBytes / 5 // 20% reserve

	if availBytes > reserveBytes {
		return
	}

	entries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		log.Printf("checkpoint-cache: readdir failed: %v", err)
		return
	}

	type cacheEntry struct {
		path  string
		size  int64
		mtime time.Time
	}
	var files []cacheEntry
	for _, e := range entries {
		if e.IsDir() || (len(e.Name()) > 0 && e.Name()[0] == '.') {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, cacheEntry{
			path:  filepath.Join(s.cacheDir, e.Name()),
			size:  info.Size(),
			mtime: info.ModTime(),
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime.Before(files[j].mtime)
	})

	needToFree := int64(reserveBytes - availBytes)
	var freed int64
	var evicted int
	for _, f := range files {
		if freed >= needToFree {
			break
		}
		if err := os.Remove(f.path); err != nil {
			continue
		}
		freed += f.size
		evicted++
	}

	if evicted > 0 {
		log.Printf("checkpoint-cache: evicted %d entries, freed %d MB (avail was %d MB, reserve %d MB)",
			evicted, freed>>20, availBytes>>20, reserveBytes>>20)
	}
}

// Delete removes a checkpoint from S3 and from the local cache.
func (s *CheckpointStore) Delete(ctx context.Context, key string) error {
	if err := s.blob.Delete(ctx, s.bucket, key); err != nil {
		return fmt.Errorf("failed to delete checkpoint: %w", err)
	}

	if s.cacheDir != "" {
		os.Remove(s.CachePath(key))
	}

	return nil
}

// CacheStats returns current cache statistics.
func (s *CheckpointStore) CacheStats() (entries int, totalSizeMB int64, availMB int64) {
	if s.cacheDir == "" {
		return 0, 0, 0
	}

	dirEntries, err := os.ReadDir(s.cacheDir)
	if err != nil {
		return 0, 0, 0
	}

	var totalSize int64
	for _, e := range dirEntries {
		if e.IsDir() || (len(e.Name()) > 0 && e.Name()[0] == '.') {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		entries++
		totalSize += info.Size()
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(s.cacheDir, &stat); err == nil {
		availMB = int64(stat.Bavail*uint64(stat.Bsize)) >> 20
	}

	return entries, totalSize >> 20, availMB
}
