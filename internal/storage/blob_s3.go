package storage

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3BlobClient implements BlobClient using the AWS S3 SDK.
type s3BlobClient struct {
	client *s3.Client
}

func (c *s3BlobClient) BackendName() string { return "S3" }

// buildS3Client creates an S3 client from config.
func buildS3Client(cfg S3Config) (*s3.Client, error) {
	forcePathStyle := cfg.ForcePathStyle
	if !forcePathStyle && strings.Contains(cfg.Endpoint, ".blob.core.windows.net") {
		forcePathStyle = true
	}

	if cfg.AccessKeyID != "" {
		opts := []func(*s3.Options){
			func(o *s3.Options) {
				o.Region = cfg.Region
				o.Credentials = credentials.NewStaticCredentialsProvider(
					cfg.AccessKeyID, cfg.SecretAccessKey, "",
				)
				if forcePathStyle {
					o.UsePathStyle = true
				}
				if cfg.Endpoint != "" {
					o.BaseEndpoint = aws.String(cfg.Endpoint)
				}
			},
		}
		return s3.New(s3.Options{}, opts...), nil
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.Region),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}
	var s3Opts []func(*s3.Options)
	if forcePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) { o.UsePathStyle = true })
	}
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) { o.BaseEndpoint = aws.String(cfg.Endpoint) })
	}
	return s3.NewFromConfig(awsCfg, s3Opts...), nil
}

func (c *s3BlobClient) Upload(ctx context.Context, bucket, key string, body io.ReadSeeker, contentLength int64) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(contentLength),
	})
	return err
}

func (c *s3BlobClient) Download(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *s3BlobClient) DownloadRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error) {
	rangeStr := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	resp, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(rangeStr),
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *s3BlobClient) Head(ctx context.Context, bucket, key string) (int64, error) {
	resp, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, err
	}
	return aws.ToInt64(resp.ContentLength), nil
}

func (c *s3BlobClient) Delete(ctx context.Context, bucket, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err
}
