package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

// azureBlobClient implements BlobClient using the native Azure Blob SDK.
type azureBlobClient struct {
	client  *azblob.Client
	connStr string
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
	_, err := c.client.UploadStream(ctx, container, normalizeKey(container, key), body, nil)
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
