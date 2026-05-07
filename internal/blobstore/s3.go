package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config configures an S3-compatible backend. Works for Tigris, AWS S3,
// CF R2, Azure Blob (via S3 compat), GCS (via interop), MinIO.
type S3Config struct {
	Name            string // logging label: "tigris", "r2", "azure-blob"
	Endpoint        string // e.g. "https://t3.storage.dev". Empty = AWS default.
	Region          string // "auto" works for Tigris/R2; AWS needs a real region.
	AccessKeyID     string
	SecretAccessKey string
	UsePathStyle    bool // true for R2/Tigris/MinIO; false for AWS S3.
}

// s3Store is an S3-compatible backend.
type s3Store struct {
	name string
	s3   *s3.Client
}

// NewS3 constructs an S3-compatible Store. Returns nil, nil if Endpoint
// and AccessKeyID are both empty (caller treats nil as "this backend
// disabled, fall through to next").
func NewS3(cfg S3Config) (Store, error) {
	if cfg.Endpoint == "" && cfg.AccessKeyID == "" {
		return nil, nil
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	if cfg.Name == "" {
		cfg.Name = "s3"
	}
	awsCfg := aws.Config{
		Region: cfg.Region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretAccessKey, "",
		),
	}
	endpoint := cfg.Endpoint
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = cfg.UsePathStyle
	})
	return &s3Store{name: cfg.Name, s3: client}, nil
}

func (s *s3Store) Name() string { return s.name }

func (s *s3Store) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	out, err := s.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%s://%s/%s: %w", s.name, bucket, key, ErrNotFound)
		}
		return nil, fmt.Errorf("%s GetObject %s/%s: %w", s.name, bucket, key, err)
	}
	return out.Body, nil
}

func (s *s3Store) Put(ctx context.Context, bucket, key string, body io.Reader, contentLength int64) error {
	_, err := s.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &bucket,
		Key:           &key,
		Body:          body,
		ContentLength: &contentLength,
	})
	if err != nil {
		return fmt.Errorf("%s PutObject %s/%s: %w", s.name, bucket, key, err)
	}
	return nil
}

func (s *s3Store) Exists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.s3.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("%s HeadObject %s/%s: %w", s.name, bucket, key, err)
}

// isNotFound matches the various ways the v2 SDK signals 404. The typed
// errors are nested in operation-specific wrapper types (NoSuchKey for
// GetObject, NotFound for HeadObject) — match by code substring rather
// than chase the type tree.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "NoSuchKey") ||
		strings.Contains(msg, "NotFound") ||
		strings.Contains(msg, "status code: 404")
}
