// Package blobstore is the abstraction over the global object store that
// holds canonical golden rootfs blobs, template blobs, and the events
// archive.
//
// We currently use Tigris (Region Earth — automatic global replication),
// but every implementation talks S3-compatible APIs, so swapping providers
// (R2, AWS S3, Azure Blob via S3 compat, GCS via interop, MinIO) is a
// config change — point at a different endpoint, supply different
// credentials. Multi-backend failover composes Stores via FallbackStore.
//
// The package is intentionally small: just enough surface for downloading
// goldens on cache miss, uploading from CI/Packer, and archiving events.
// Anything richer (presigned URLs, multipart uploads >5GB, server-side
// copy) belongs in a higher layer if/when we need it.
package blobstore

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get/Exists/Download when the object doesn't
// exist. Distinguishable from transient errors so callers can fall back
// to other behaviors (rebuild locally, try a different store, etc.).
var ErrNotFound = errors.New("blobstore: object not found")

// Store is the abstract interface every backend implements.
//
// Implementations should be safe for concurrent use — a single Store is
// typically shared across goroutines (worker pool, async upload, etc.).
type Store interface {
	// Get streams the object at key from bucket. Caller closes the reader.
	// Returns ErrNotFound if the object isn't present.
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)

	// Put writes body of length contentLength bytes to bucket/key.
	// Streams from the reader — no in-memory buffering of the full body.
	Put(ctx context.Context, bucket, key string, body io.Reader, contentLength int64) error

	// Exists returns true if the object is present at bucket/key.
	// Returns (false, nil) on NotFound; (false, err) on other errors.
	Exists(ctx context.Context, bucket, key string) (bool, error)

	// Name returns a short identifier for logging ("tigris", "r2",
	// "azure-blob"). Never empty.
	Name() string
}

// Download is a convenience wrapper that streams an object to a local file
// atomically (writes to dest+".tmp" and renames on success). Most callers
// want this rather than the streaming Get.
func Download(ctx context.Context, s Store, bucket, key, destPath string) error {
	r, err := s.Get(ctx, bucket, key)
	if err != nil {
		return err
	}
	defer r.Close()
	return writeAtomic(destPath, r)
}

// Upload is a convenience wrapper that streams a local file to bucket/key.
func Upload(ctx context.Context, s Store, bucket, key, srcPath string) error {
	f, length, err := openSized(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.Put(ctx, bucket, key, f, length)
}
