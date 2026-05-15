package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
)

// FallbackStore wraps a primary Store with one or more fallback Stores.
// Reads (Get / GetRange / Head / Exists) try the primary first; on transient
// errors (network, 5xx) they cascade through fallbacks. NotFound is normally
// authoritative on primary — we don't try fallbacks for missing objects —
// because resurrecting deleted state from a stale mirror is surprising.
//
// During a backend migration (e.g. lazy Tigris cutover with Azure as the
// safety net), set TryFallbacksOnNotFound=true so a miss on primary falls
// through to fallbacks. Flip back to false once the soak window closes.
//
// Writes (Put / Delete) go to the primary only — dual-write would create
// consistency challenges and is overkill for the migration use case.
//
// Use case: Tigris primary, Azure fallback during cutover. While running
// FallbackStore in migration mode, any cold key that hasn't yet been
// rclone-synced into Tigris transparently fetches from Azure.
type FallbackStore struct {
	primary               Store
	fallbacks             []Store
	TryFallbacksOnNotFound bool
}

// NewFallback wraps primary with fallback Stores in HA mode: fallbacks are
// consulted only on transient errors from primary. NotFound on primary is
// authoritative. Nil primary is an error; nil entries in fallbacks are
// ignored (so callers can pass disabled backends without filtering).
func NewFallback(primary Store, fallbacks ...Store) (Store, error) {
	return newFallback(false, primary, fallbacks...)
}

// NewMigrationFallback wraps primary with fallback Stores in migration mode:
// NotFound on primary cascades through fallbacks too. Use during a backend
// cutover where the primary is being lazily populated (e.g. Tigris primary,
// Azure fallback during the soak window). Switch back to NewFallback (or
// remove the fallback entirely) once the migration completes.
func NewMigrationFallback(primary Store, fallbacks ...Store) (Store, error) {
	return newFallback(true, primary, fallbacks...)
}

func newFallback(migrationMode bool, primary Store, fallbacks ...Store) (Store, error) {
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
	return &FallbackStore{
		primary:                primary,
		fallbacks:              cleaned,
		TryFallbacksOnNotFound: migrationMode,
	}, nil
}

func (f *FallbackStore) Name() string {
	names := f.primary.Name()
	for _, b := range f.fallbacks {
		names += "+" + b.Name()
	}
	return names
}

// shouldFallThrough is the shared policy across read methods. Transient
// errors always cascade. NotFound only cascades when migration mode is on.
func (f *FallbackStore) shouldFallThrough(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return f.TryFallbacksOnNotFound
	}
	return true
}

func (f *FallbackStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	r, err := f.primary.Get(ctx, bucket, key)
	if err == nil {
		return r, nil
	}
	if !f.shouldFallThrough(err) {
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
			return nil, err2
		}
		err = err2
	}
	return nil, fmt.Errorf("blobstore: all backends failed: %w", err)
}

func (f *FallbackStore) GetRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error) {
	r, err := f.primary.GetRange(ctx, bucket, key, offset, length)
	if err == nil {
		return r, nil
	}
	if !f.shouldFallThrough(err) {
		return nil, err
	}
	for i, fb := range f.fallbacks {
		log.Printf("blobstore: primary %s GetRange failed (%v); trying fallback %s [%d/%d]",
			f.primary.Name(), err, fb.Name(), i+1, len(f.fallbacks))
		r2, err2 := fb.GetRange(ctx, bucket, key, offset, length)
		if err2 == nil {
			return r2, nil
		}
		if errors.Is(err2, ErrNotFound) {
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

func (f *FallbackStore) Head(ctx context.Context, bucket, key string) (int64, error) {
	n, err := f.primary.Head(ctx, bucket, key)
	if err == nil {
		return n, nil
	}
	if !f.shouldFallThrough(err) {
		return 0, err
	}
	for _, fb := range f.fallbacks {
		n2, err2 := fb.Head(ctx, bucket, key)
		if err2 == nil {
			return n2, nil
		}
		if errors.Is(err2, ErrNotFound) {
			return 0, err2
		}
		err = err2
	}
	return 0, err
}

func (f *FallbackStore) Exists(ctx context.Context, bucket, key string) (bool, error) {
	ok, err := f.primary.Exists(ctx, bucket, key)
	if err == nil {
		if ok || !f.TryFallbacksOnNotFound {
			return ok, nil
		}
		// Primary returned (false, nil) — "not found" but no error. In migration
		// mode, treat that the same as ErrNotFound and ask fallbacks too.
	}
	if err != nil && !f.shouldFallThrough(err) {
		return false, err
	}
	for _, fb := range f.fallbacks {
		ok2, err2 := fb.Exists(ctx, bucket, key)
		if err2 == nil {
			if ok2 {
				return true, nil
			}
			continue
		}
		err = err2
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (f *FallbackStore) Delete(ctx context.Context, bucket, key string) error {
	// Deletes go only to primary. Migration mode does not propagate deletes
	// to the fallback — that would defeat lazy migration by removing the
	// authoritative copy from the old backend before its data is fully
	// mirrored.
	return f.primary.Delete(ctx, bucket, key)
}
