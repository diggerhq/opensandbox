package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
)

// FallbackStore wraps a primary Store with one or more fallback Stores.
// Read operations (Get, Exists) try the primary first; on transient
// errors (network, 5xx) they cascade through fallbacks. NotFound is
// authoritative — we don't try fallbacks for missing objects.
//
// Writes (Put) go to the primary only — multi-write would create
// consistency challenges and is overkill for the use case.
//
// Use case: configure Tigris as primary, R2 as fallback. If Tigris has
// an outage, golden pulls keep working from the R2 mirror (assuming
// you've populated it; that's an upload-side concern out of scope here).
type FallbackStore struct {
	primary   Store
	fallbacks []Store
}

// NewFallback wraps primary with fallback Stores. Nil primary is an error;
// nil entries in fallbacks are ignored (so callers can pass disabled
// backends without filtering).
func NewFallback(primary Store, fallbacks ...Store) (Store, error) {
	if primary == nil {
		return nil, errors.New("blobstore: FallbackStore requires non-nil primary")
	}
	cleaned := make([]Store, 0, len(fallbacks))
	for _, f := range fallbacks {
		if f != nil {
			cleaned = append(cleaned, f)
		}
	}
	if len(cleaned) == 0 {
		return primary, nil // no fallbacks configured — just return primary
	}
	return &FallbackStore{primary: primary, fallbacks: cleaned}, nil
}

func (f *FallbackStore) Name() string {
	names := f.primary.Name()
	for _, b := range f.fallbacks {
		names += "+" + b.Name()
	}
	return names
}

func (f *FallbackStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	r, err := f.primary.Get(ctx, bucket, key)
	if err == nil {
		return r, nil
	}
	if errors.Is(err, ErrNotFound) {
		// Authoritative — primary says it doesn't exist. Don't ask fallbacks.
		return nil, err
	}
	for i, fb := range f.fallbacks {
		log.Printf("blobstore: primary %s failed (%v); trying fallback %s [%d/%d]",
			f.primary.Name(), err, fb.Name(), i+1, len(f.fallbacks))
		r2, err2 := fb.Get(ctx, bucket, key)
		if err2 == nil {
			return r2, nil
		}
		if errors.Is(err2, ErrNotFound) {
			// Fallback also says missing — call it ErrNotFound. Probably means
			// the fallback isn't populated; surface so caller can decide.
			return nil, err2
		}
		err = err2
	}
	return nil, fmt.Errorf("blobstore: all backends failed: %w", err)
}

func (f *FallbackStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentLength int64) error {
	// Writes go only to primary. See package doc for rationale.
	return f.primary.Put(ctx, bucket, key, body, contentLength)
}

func (f *FallbackStore) Exists(ctx context.Context, bucket, key string) (bool, error) {
	ok, err := f.primary.Exists(ctx, bucket, key)
	if err == nil {
		return ok, nil
	}
	for _, fb := range f.fallbacks {
		ok2, err2 := fb.Exists(ctx, bucket, key)
		if err2 == nil {
			return ok2, nil
		}
		err = err2
	}
	return false, err
}
