package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
)

// azureBlobClient implements BlobClient using the native Azure Blob SDK.
type azureBlobClient struct {
	client  *azblob.Client
	connStr string
}

// uploadStreamOpts is shared by every Upload call. The Azure SDK's nil-default
// is BlockSize=1MB / Concurrency=1, i.e. a serial chain of small PutBlock
// calls that's HTTP-overhead bound at 30–60 MB/s per blob — that turned a
// 10 GB checkpoint into a 5+ minute upload. The values below are sized for a
// 64-vCPU worker (D-series, ~30 Gbps NIC, Premium SSD ~700 MB/s read):
//
//   - BlockSize 8 MB: large enough that PutBlock HTTP overhead is amortized
//     (~10x fewer round-trips than the SDK default), small enough that under
//     concurrent uploads the per-blob in-flight memory stays bounded.
//   - Concurrency 8: enough parallelism to keep the worker NIC fed even when
//     HTTP latency dominates a single block. Per-upload in-flight buffer is
//     8 × 8 MB = 64 MB, negligible against worker RAM.
//
// Practical ceiling per blob with these settings is ~600–1000 MB/s, which is
// already above the disk-read rate at which we can feed bytes into the
// uploader from the on-disk archive — meaning disk read is now the bottleneck,
// not HTTP. Bumping further would just waste memory.
//
// Concurrency on the worker level: 5 simultaneous CreateCheckpoint uploads ≈
// 320 MB total in-flight buffer + ~40 connections to blob — comfortably under
// the worker's NIC and Azure's per-account ingress limits.
var uploadStreamOpts = &azblob.UploadStreamOptions{
	BlockSize:   8 * 1024 * 1024,
	Concurrency: 8,
}

func (c *azureBlobClient) BackendName() string { return "Azure Blob" }

// normalizeKey strips the container name prefix from the key if present.
// S3 keys often include the bucket name (e.g., "checkpoints/sb-xxx/...") because
// S3 treats bucket and key as separate. Azure Blob uses container + blob name,
// so we strip the redundant prefix to avoid "container/container/..." paths.
func normalizeKey(container, key string) string {
	prefix := container + "/"
	if strings.HasPrefix(key, prefix) {
		return strings.TrimPrefix(key, prefix)
	}
	return key
}

func (c *azureBlobClient) ensureClient() error {
	if c.client != nil {
		return nil
	}
	client, err := azblob.NewClientFromConnectionString(c.connStr, nil)
	if err != nil {
		return fmt.Errorf("azure blob: %w", err)
	}
	c.client = client
	return nil
}

func (c *azureBlobClient) Upload(ctx context.Context, container, key string, body io.ReadSeeker, contentLength int64) error {
	if err := c.ensureClient(); err != nil {
		return err
	}
	_, err := c.client.UploadStream(ctx, container, normalizeKey(container, key), body, uploadStreamOpts)
	return err
}

func (c *azureBlobClient) Download(ctx context.Context, container, key string) (io.ReadCloser, error) {
	if err := c.ensureClient(); err != nil {
		return nil, err
	}
	resp, err := c.client.DownloadStream(ctx, container, normalizeKey(container, key), nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *azureBlobClient) DownloadRange(ctx context.Context, container, key string, offset, length int64) (io.ReadCloser, error) {
	if err := c.ensureClient(); err != nil {
		return nil, err
	}
	resp, err := c.client.DownloadStream(ctx, container, normalizeKey(container, key), &azblob.DownloadStreamOptions{
		Range: azblob.HTTPRange{Offset: offset, Count: length},
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *azureBlobClient) Head(ctx context.Context, container, key string) (int64, error) {
	if err := c.ensureClient(); err != nil {
		return 0, err
	}
	resp, err := c.client.ServiceClient().NewContainerClient(container).NewBlobClient(normalizeKey(container, key)).GetProperties(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if resp.ContentLength == nil {
		return 0, nil
	}
	return *resp.ContentLength, nil
}

func (c *azureBlobClient) Delete(ctx context.Context, container, key string) error {
	if err := c.ensureClient(); err != nil {
		return err
	}
	_, err := c.client.DeleteBlob(ctx, container, normalizeKey(container, key), nil)
	return err
}
