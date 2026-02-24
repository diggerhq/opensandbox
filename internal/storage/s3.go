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

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
	client   *s3.Client
	bucket   string
	cacheDir string // local NVMe cache for CRIU checkpoints (empty = disabled)
	cacheMu  sync.Mutex
}

// NewCheckpointStore creates a new S3 checkpoint store.
// If AccessKeyID is empty, uses the default AWS credential chain (IAM instance profile on EC2).
func NewCheckpointStore(cfg S3Config) (*CheckpointStore, error) {
	var client *s3.Client

	if cfg.AccessKeyID != "" {
		opts := []func(*s3.Options){
			func(o *s3.Options) {
				o.Region = cfg.Region
				o.Credentials = credentials.NewStaticCredentialsProvider(
					cfg.AccessKeyID, cfg.SecretAccessKey, "",
				)
				if cfg.ForcePathStyle {
					o.UsePathStyle = true
				}
				if cfg.Endpoint != "" {
					o.BaseEndpoint = aws.String(cfg.Endpoint)
				}
			},
		}
		client = s3.New(s3.Options{}, opts...)
	} else {
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(cfg.Region),
		)
		if err != nil {
			return nil, fmt.Errorf("failed to load AWS config for S3: %w", err)
		}
		var s3Opts []func(*s3.Options)
		if cfg.ForcePathStyle {
			s3Opts = append(s3Opts, func(o *s3.Options) { o.UsePathStyle = true })
		}
		if cfg.Endpoint != "" {
			s3Opts = append(s3Opts, func(o *s3.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) })
		}
		client = s3.NewFromConfig(awsCfg, s3Opts...)
	}

	return &CheckpointStore{
		client: client,
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

// CheckpointKey returns the S3 key for a checkpoint archive.
func CheckpointKey(sandboxID string) string {
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

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentLength: aws.Int64(stat.Size()),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to upload checkpoint to S3: %w", err)
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

// downloadAndCache downloads from S3, writes to NVMe cache, returns the cached file.
func (s *CheckpointStore) downloadAndCache(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download checkpoint from S3: %w", err)
	}

	s.evictIfNeeded()

	cachePath := s.CachePath(key)
	tmpFile, err := os.CreateTemp(s.cacheDir, ".dl-tmp-*")
	if err != nil {
		// Can't write to cache — fall back to direct S3 stream
		log.Printf("checkpoint-cache: can't create temp, streaming from S3: %v", err)
		return resp.Body, nil
	}
	tmpPath := tmpFile.Name()

	written, err := io.Copy(tmpFile, resp.Body)
	resp.Body.Close()
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("failed to download checkpoint from S3: %w", err)
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		log.Printf("checkpoint-cache: rename failed: %v", err)
	} else {
		log.Printf("checkpoint-cache: MISS %s (%d MB downloaded from S3, now cached)", key, written>>20)
	}

	f, err := os.Open(cachePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open cached checkpoint: %w", err)
	}
	return f, nil
}

// downloadFromS3 streams directly from S3 (no caching).
func (s *CheckpointStore) downloadFromS3(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download checkpoint from S3: %w", err)
	}
	return resp.Body, nil
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
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete checkpoint from S3: %w", err)
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
