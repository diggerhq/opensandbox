// Package storage provides blob storage backends for checkpoint archives.
//
// BlobClient is the abstraction — implementations exist for Azure Blob Storage
// (default) and AWS S3. The CheckpointStore uses a BlobClient for all remote
// operations (upload, download, head, delete).
//
// Detection is automatic: if the endpoint contains ".blob.core.windows.net",
// the Azure client is used. Otherwise, the AWS S3 client is used.
package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// BlobClient abstracts remote object storage operations.
// Implementations: azureBlobClient (Azure Blob Storage), s3BlobClient (AWS S3).
type BlobClient interface {
	// Upload writes data from reader to the given key. Returns bytes written.
	Upload(ctx context.Context, bucket, key string, body io.ReadSeeker, contentLength int64) error
	// Download returns a reader for the object at the given key.
	Download(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	// DownloadRange returns a reader for a byte range of the object.
	DownloadRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error)
	// Head returns the content length of the object, or error if not found.
	Head(ctx context.Context, bucket, key string) (int64, error)
	// Delete removes the object at the given key.
	Delete(ctx context.Context, bucket, key string) error
	// BackendName returns a human-readable name for the storage backend (e.g. "S3", "Azure Blob").
	BackendName() string
}

// NewBlobClient creates the appropriate blob client based on the endpoint.
// Azure Blob Storage endpoints (*.blob.core.windows.net) use the native Azure SDK.
// All other endpoints use the AWS S3 SDK.
func NewBlobClient(cfg S3Config) (BlobClient, error) {
	if strings.Contains(cfg.Endpoint, ".blob.core.windows.net") {
		return newAzureBlobClient(cfg)
	}
	return newS3BlobClient(cfg)
}

// newAzureBlobClient creates an Azure Blob Storage client.
func newAzureBlobClient(cfg S3Config) (BlobClient, error) {
	// Azure Blob Storage uses the storage account name as the access key ID
	// and the account key as the secret. The endpoint is the full URL.
	connStr := fmt.Sprintf(
		"DefaultEndpointsProtocol=https;AccountName=%s;AccountKey=%s;EndpointSuffix=core.windows.net",
		cfg.AccessKeyID, cfg.SecretAccessKey,
	)
	return &azureBlobClient{connStr: connStr}, nil
}

// newS3BlobClient creates an AWS S3-compatible client.
func newS3BlobClient(cfg S3Config) (BlobClient, error) {
	// Reuse the existing S3 client setup
	client, err := buildS3Client(cfg)
	if err != nil {
		return nil, err
	}
	return &s3BlobClient{client: client}, nil
}
