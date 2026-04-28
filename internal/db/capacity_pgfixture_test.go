//go:build pgfixture

// Integration tests for the reserved-capacity Store methods. Run only
// under `go test -tags=pgfixture` against a real Postgres pointed at by
// TEST_DATABASE_URL — the advisory-lock and idempotency-key paths cannot
// be exercised in pure-Go because they depend on Postgres semantics
// (transaction-scoped locks, row-level conflict, JSONB aggregation).
//
// Run locally:
//
//	TEST_DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable \
//	  go test -tags=pgfixture ./internal/db/ -run Capacity -v
package db

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// seedOrgWithCap inserts a fresh org with a known max_memory_gb so the
// capacity tests can assert against a deterministic ceiling.
func seedOrgWithCap(t *testing.T, store *Store, maxMemoryGB int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	orgID := uuid.New()
	_, err := store.pool.Exec(ctx, `
		INSERT INTO orgs (id, name, slug, plan, max_concurrent_sandboxes, max_sandbox_timeout_sec, max_disk_mb, max_memory_gb)
		VALUES ($1, $2, $3, 'pro', 100, 86400, 20480, $4)
	`, orgID, "cap-"+orgID.String()[:8], "cap-"+orgID.String()[:8], maxMemoryGB)
	if err != nil {
		t.Fatalf("seed org: %v", err)
	}
	return orgID
}

func aligned(base time.Time, m int) time.Time {
	return base.Truncate(15 * time.Minute).Add(time.Duration(m) * 15 * time.Minute)
}

func TestCapacityCalendar_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 16)

	// Anchor far in the future so nothing else collides.
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	from := base
	to := base.Add(2 * time.Hour) // 8 buckets

	// Two reservations, partially overlapping at bucket 2 (sum should be 12).
	r1 := uuid.New()
	r2 := uuid.New()
	ins := func(reservationID uuid.UUID, m int, gb int) {
		_, err := store.pool.Exec(ctx, `
			INSERT INTO capacity_reservation_intervals
				(reservation_id, org_id, resource, starts_at, ends_at, capacity_gb)
			VALUES ($1, $2, 'memory_gb', $3, $4, $5)
		`, reservationID, orgID, aligned(base, m), aligned(base, m+1), gb)
		if err != nil {
			t.Fatalf("seed interval: %v", err)
		}
	}
	ins(r1, 1, 4)
	ins(r1, 2, 8)
	ins(r2, 2, 4)
	ins(r2, 3, 12)

	buckets, err := store.GetCapacityCalendar(ctx, orgID, from, to)
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	if len(buckets) != 8 {
		t.Fatalf("expected 8 buckets, got %d", len(buckets))
	}
	want := map[int]int{0: 0, 1: 4, 2: 12, 3: 12, 4: 0, 5: 0, 6: 0, 7: 0}
	for i, b := range buckets {
		if b.ReservedGB != want[i] {
			t.Errorf("bucket[%d] = %d, want %d (startsAt=%s)", i, b.ReservedGB, want[i], b.StartsAt)
		}
		if !b.EndsAt.Equal(b.StartsAt.Add(15 * time.Minute)) {
			t.Errorf("bucket[%d] endsAt mismatch", i)
		}
	}
}

func TestCreateReservation_capExceeded_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 8) // cap = 8 GB

	base := time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC)
	bodyHash := sha256.Sum256([]byte(`{"oversize":true}`))

	_, _, err := store.CreateReservation(ctx, CreateReservationRequest{
		OrgID:           orgID,
		RequestBodyHash: bodyHash[:],
		Intervals: []CapacityInterval{{
			StartsAt:   aligned(base, 0),
			EndsAt:     aligned(base, 1),
			CapacityGB: 12, // 12 > cap of 8
		}},
	})
	var shortfall *CapacityShortfallError
	if !errors.As(err, &shortfall) {
		t.Fatalf("expected CapacityShortfallError, got %v", err)
	}
	if len(shortfall.Intervals) != 1 || shortfall.Intervals[0].ReservableGB != 8 {
		t.Errorf("unexpected shortfall: %+v", shortfall.Intervals)
	}
}

func TestCreateReservation_idempotencyReplay_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 16)

	base := time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC)
	body := []byte(`{"intervals":[{"startsAt":"2030-03-01T00:00:00Z","endsAt":"2030-03-01T00:15:00Z","capacityGb":4}]}`)
	bodyHash := sha256.Sum256(body)

	req := CreateReservationRequest{
		OrgID:            orgID,
		IdempotencyKey:   "test-key-1",
		IdempotencyEndpt: "POST /api/capacity/reservations",
		RequestBodyHash:  bodyHash[:],
		Intervals: []CapacityInterval{{
			StartsAt:   aligned(base, 0),
			EndsAt:     aligned(base, 1),
			CapacityGB: 4,
		}},
	}

	// First call: writes the reservation. Cache the (synthetic) success body.
	rid, _, err := store.CreateReservation(ctx, req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if rid == uuid.Nil {
		t.Fatal("expected non-nil reservationId")
	}
	if err := store.SaveIdempotencyResult(ctx, orgID, req.IdempotencyEndpt, req.IdempotencyKey, bodyHash[:], 200, []byte(`{"reservationId":"`+rid.String()+`"}`)); err != nil {
		t.Fatalf("save idempotency: %v", err)
	}

	// Second call: same body, same key — should replay.
	_, _, err = store.CreateReservation(ctx, req)
	var replay *IdempotencyReplay
	if !errors.As(err, &replay) {
		t.Fatalf("expected IdempotencyReplay, got %v", err)
	}
	if replay.StatusCode != 200 {
		t.Errorf("replay status: got %d, want 200", replay.StatusCode)
	}

	// Third call: same key, different body — should conflict.
	conflictReq := req
	otherHash := sha256.Sum256([]byte(`{"different":true}`))
	conflictReq.RequestBodyHash = otherHash[:]
	_, _, err = store.CreateReservation(ctx, conflictReq)
	var conflict *IdempotencyConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected IdempotencyConflictError, got %v", err)
	}
}

// TestCreateReservation_concurrentSerializes proves the advisory-lock
// path. Two goroutines try to reserve 10 GB on the same bucket of an
// 16-GB-cap org. The first succeeds; the second must see the post-lock
// state and return a shortfall — not a duplicate insert, not a 500.
func TestCreateReservation_concurrentSerializes_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 16)

	base := time.Date(2030, 4, 1, 0, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte(`{}`))

	makeReq := func() CreateReservationRequest {
		return CreateReservationRequest{
			OrgID:           orgID,
			RequestBodyHash: hash[:],
			Intervals: []CapacityInterval{{
				StartsAt:   aligned(base, 0),
				EndsAt:     aligned(base, 1),
				CapacityGB: 12,
			}},
		}
	}

	type result struct {
		rid uuid.UUID
		err error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			rid, _, err := store.CreateReservation(ctx, makeReq())
			results <- result{rid: rid, err: err}
		}()
	}
	wg.Wait()
	close(results)

	successes := 0
	shortfalls := 0
	for r := range results {
		if r.err == nil {
			successes++
			continue
		}
		var s *CapacityShortfallError
		if errors.As(r.err, &s) {
			shortfalls++
			continue
		}
		t.Errorf("unexpected error: %v", r.err)
	}
	if successes != 1 || shortfalls != 1 {
		t.Errorf("expected 1 success and 1 shortfall, got successes=%d shortfalls=%d", successes, shortfalls)
	}
}

func TestListReservations_cursor_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	base := time.Date(2030, 5, 1, 0, 0, 0, 0, time.UTC)
	hash := sha256.Sum256([]byte(`{}`))

	// Five distinct reservations on five distinct buckets.
	for i := 0; i < 5; i++ {
		_, _, err := store.CreateReservation(ctx, CreateReservationRequest{
			OrgID:           orgID,
			RequestBodyHash: hash[:],
			Intervals: []CapacityInterval{{
				StartsAt:   aligned(base, i),
				EndsAt:     aligned(base, i+1),
				CapacityGB: 4,
			}},
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		// Avoid timestamp ties — pagination ordering uses (created_at,
		// reservation_id) as the cursor key, and tied created_at would make
		// the assertion non-deterministic without also pinning IDs.
		time.Sleep(2 * time.Millisecond)
	}

	from := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)

	first, next, err := store.ListReservations(ctx, ListReservationsRequest{
		OrgID: orgID, From: from, To: to, Limit: 2,
	})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("page 1 size: got %d, want 2", len(first))
	}
	if next == nil {
		t.Fatal("expected non-nil cursor after page 1")
	}

	second, next2, err := store.ListReservations(ctx, ListReservationsRequest{
		OrgID: orgID, From: from, To: to, Limit: 2, Cursor: next,
	})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(second) != 2 {
		t.Fatalf("page 2 size: got %d, want 2", len(second))
	}
	if next2 == nil {
		t.Fatal("expected non-nil cursor after page 2")
	}

	third, next3, err := store.ListReservations(ctx, ListReservationsRequest{
		OrgID: orgID, From: from, To: to, Limit: 2, Cursor: next2,
	})
	if err != nil {
		t.Fatalf("list page 3: %v", err)
	}
	if len(third) != 1 {
		t.Fatalf("page 3 size: got %d, want 1", len(third))
	}
	if next3 != nil {
		t.Errorf("expected nil cursor on final page, got %+v", next3)
	}

	// No reservation_id should appear twice across the three pages.
	seen := map[uuid.UUID]struct{}{}
	for _, r := range append(append(first, second...), third...) {
		if _, dup := seen[r.ReservationID]; dup {
			t.Errorf("reservation %s appeared on multiple pages", r.ReservationID)
		}
		seen[r.ReservationID] = struct{}{}
	}
}
