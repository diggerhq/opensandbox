package blobstore

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// AzureConfig configures an Azure Blob backend.
type AzureConfig struct {
	Name        string // logging label, e.g. "azure-blob"
	AccountName string
	AccountKey  string

	// Bucket (Azure container), if set, overrides any bucket argument passed
	// at runtime. Use this to give the Azure backend its own container name
	// — e.g. caller uses Tigris bucket "opencomputer-prod" but the Azure
	// fallback should read from container "checkpoints".
	Bucket string
}

// azureStore implements Store against Azure Blob Storage. We dial the SDK
// lazily on first use so a worker that never touches the fallback never
// opens an Azure connection (and never logs Azure 401s if creds are wrong).
type azureStore struct {
	name    string
	bucket  string // container override; empty means use caller's runtime bucket
	connStr string

	mu     sync.Mutex
	client *azblob.Client
}

// NewAzure constructs an Azure Blob backed Store. Returns nil, nil when
// account + key are both empty (caller treats nil as "this backend
// disabled, fall through to next").
func NewAzure(cfg AzureConfig) (Store, error) {
	if cfg.AccountName == "" && cfg.AccountKey == "" {
		return nil, nil
	}
	if cfg.AccountName == "" || cfg.AccountKey == "" {
		return nil, fmt.Errorf("blobstore: Azure requires both AccountName and AccountKey")
	}
	if cfg.Name == "" {
		cfg.Name = "azure-blob"
	}
	connStr := fmt.Sprintf(
		"DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;EndpointSuffix=core.windows.net",
		cfg.AccountName, cfg.AccountKey,
	)
	return &azureStore{name: cfg.Name, bucket: cfg.Bucket, connStr: connStr}, nil
}

func (a *azureStore) Name() string { return a.name }

// resolveBucket returns the configured container if set, else the caller's bucket.
func (a *azureStore) resolveBucket(bucket string) string {
	if a.bucket != "" {
		return a.bucket
	}
	return bucket
}

func (a *azureStore) ensureClient() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		return nil
	}
	c, err := azblob.NewClientFromConnectionString(a.connStr, nil)
	if err != nil {
		return fmt.Errorf("%s connect: %w", a.name, err)
	}
	a.client = c
	return nil
}

// normalizeKey strips a leading "<bucket>/" if present. S3-style keys often
// embed the bucket name (e.g. "checkpoints/sb-xxx/..."); Azure addresses
// container + blob name separately, so we drop the redundant prefix.
func normalizeKey(bucket, key string) string {
	if trimmed, ok := strings.CutPrefix(key, bucket+"/"); ok {
		return trimmed
	}
	return key
}

func (a *azureStore) wrapNotFound(err error, bucket, key, op string) error {
	if err == nil {
		return nil
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound) || bloberror.HasCode(err, bloberror.ContainerNotFound) {
		return fmt.Errorf("%s://%s/%s: %w", a.name, bucket, key, ErrNotFound)
	}
	return fmt.Errorf("%s %s %s/%s: %w", a.name, op, bucket, key, err)
}

func (a *azureStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	if err := a.ensureClient(); err != nil {
		return nil, err
	}
	c := a.resolveBucket(bucket)
	resp, err := a.client.DownloadStream(ctx, c, normalizeKey(c, key), nil)
	if err != nil {
		return nil, a.wrapNotFound(err, c, key, "DownloadStream")
	}
	return resp.Body, nil
}

func (a *azureStore) GetRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error) {
	if err := a.ensureClient(); err != nil {
		return nil, err
	}
	c := a.resolveBucket(bucket)
	resp, err := a.client.DownloadStream(ctx, c, normalizeKey(c, key), &azblob.DownloadStreamOptions{
		Range: azblob.HTTPRange{Offset: offset, Count: length},
	})
	if err != nil {
		return nil, a.wrapNotFound(err, c, key, "DownloadStream(range)")
	}
	return resp.Body, nil
}

// uploadStreamOpts is shared by every Put. The Azure SDK's nil-default is
// BlockSize=1MB / Concurrency=1, a serial chain of small PutBlock calls that's
// HTTP-overhead bound at 30–60 MB/s per blob — that turned a 10 GB checkpoint
// into a 5+ minute upload. The values below are sized for a 64-vCPU worker
// (D-series, ~30 Gbps NIC, Premium SSD ~700 MB/s read):
//
//   - BlockSize 8 MB amortizes PutBlock HTTP overhead (~10x fewer round-trips
//     than the SDK default); per-blob in-flight memory stays bounded under
//     concurrent uploads.
//   - Concurrency 8 keeps the worker NIC fed when HTTP latency dominates a
//     single block. Per-upload buffer is 8 × 8 MB = 64 MB, negligible vs RAM.
//
// Practical ceiling per blob is ~600–1000 MB/s — already above the disk-read
// rate at which bytes can be fed from the on-disk archive. Bumping further
// just wastes memory.
var azureUploadStreamOpts = &azblob.UploadStreamOptions{
	BlockSize:   8 * 1024 * 1024,
	Concurrency: 8,
}

func (a *azureStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentLength int64) error {
	if err := a.ensureClient(); err != nil {
		return err
	}
	c := a.resolveBucket(bucket)
	_, err := a.client.UploadStream(ctx, c, normalizeKey(c, key), body, azureUploadStreamOpts)
	if err != nil {
		return fmt.Errorf("%s UploadStream %s/%s: %w", a.name, c, key, err)
	}
	return nil
}

func (a *azureStore) Head(ctx context.Context, bucket, key string) (int64, error) {
	if err := a.ensureClient(); err != nil {
		return 0, err
	}
	c := a.resolveBucket(bucket)
	resp, err := a.client.ServiceClient().
		NewContainerClient(c).
		NewBlobClient(normalizeKey(c, key)).
		GetProperties(ctx, nil)
	if err != nil {
		return 0, a.wrapNotFound(err, c, key, "GetProperties")
	}
	if resp.ContentLength == nil {
		return 0, nil
	}
	return *resp.ContentLength, nil
}

func (a *azureStore) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := a.Head(ctx, bucket, key)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (a *azureStore) Delete(ctx context.Context, bucket, key string) error {
	if err := a.ensureClient(); err != nil {
		return err
	}
	c := a.resolveBucket(bucket)
	_, err := a.client.DeleteBlob(ctx, c, normalizeKey(c, key), nil)
	if err != nil {
		// Idempotent: not-found is success.
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil
		}
		return fmt.Errorf("%s DeleteBlob %s/%s: %w", a.name, c, key, err)
	}
	return nil
}
