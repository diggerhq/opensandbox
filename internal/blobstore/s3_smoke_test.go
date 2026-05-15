package blobstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/opensandbox/opensandbox/internal/blobstore"
)

// TestS3_Smoke exercises Put + Get + Exists against a real S3-compatible
// endpoint. Skipped unless these env vars are set:
//
//	BLOBSTORE_TEST_ENDPOINT   - e.g. https://t3.storage.dev
//	BLOBSTORE_TEST_ACCESS_KEY - access key id
//	BLOBSTORE_TEST_SECRET_KEY - secret access key
//	BLOBSTORE_TEST_BUCKET     - bucket name (must exist)
//	BLOBSTORE_TEST_REGION     - optional, defaults to "auto"
//
// Designed to run against Tigris but works with any S3-compat backend.
func TestS3_Smoke(t *testing.T) {
	endpoint := os.Getenv("BLOBSTORE_TEST_ENDPOINT")
	accessKey := os.Getenv("BLOBSTORE_TEST_ACCESS_KEY")
	secretKey := os.Getenv("BLOBSTORE_TEST_SECRET_KEY")
	bucket := os.Getenv("BLOBSTORE_TEST_BUCKET")
	if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
		t.Skip("BLOBSTORE_TEST_* env not set; skipping live smoke")
	}
	region := os.Getenv("BLOBSTORE_TEST_REGION")
	if region == "" {
		region = "auto"
	}

	store, err := blobstore.NewS3(blobstore.S3Config{
		Name:            "smoke-test",
		Endpoint:        endpoint,
		Region:          region,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		UsePathStyle:    true,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if store == nil {
		t.Fatal("NewS3 returned nil with no error")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := "blobstore-smoke/" + time.Now().Format("20060102-150405.000")
	payload := []byte("hello tigris from blobstore smoke test\n")

	// Put
	if err := store.Put(ctx, bucket, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Logf("Put %s/%s (%d bytes)", bucket, key, len(payload))

	// Exists
	ok, err := store.Exists(ctx, bucket, key)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Fatalf("Exists: want true immediately after Put, got false")
	}

	// Get
	r, err := store.Get(ctx, bucket, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get bytes mismatch: want %q, got %q", payload, got)
	}

	// Exists on missing key
	ok, err = store.Exists(ctx, bucket, key+".does-not-exist")
	if err != nil {
		t.Fatalf("Exists(missing): %v", err)
	}
	if ok {
		t.Errorf("Exists(missing): want false, got true")
	}

	// Get on missing key returns ErrNotFound
	_, err = store.Get(ctx, bucket, key+".does-not-exist")
	if !errors.Is(err, blobstore.ErrNotFound) {
		t.Errorf("Get(missing): want ErrNotFound, got %v", err)
	}

	t.Logf("smoke: backend=%s endpoint=%s bucket=%s key=%s all ops OK", store.Name(), endpoint, bucket, key)
}
