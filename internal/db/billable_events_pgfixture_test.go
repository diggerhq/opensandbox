//go:build pgfixture

// Integration tests for the billable_events outbox. Run only under
// `go test -tags=pgfixture` against a real Postgres pointed at by
// TEST_DATABASE_URL.
//
//	TEST_DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable \
//	  go test -tags=pgfixture ./internal/db/ -run BillableEvents -v
package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// makeBucketEvent builds a valid BillableEvent for a 15-min bucket
// starting at base + (m × 15 min).
func makeBucketEvent(orgID uuid.UUID, eventType string, memoryMB int, gbSeconds float64, base time.Time, m int) BillableEvent {
	start := base.Truncate(15 * time.Minute).Add(time.Duration(m) * 15 * time.Minute)
	return BillableEvent{
		OrgID:       orgID,
		EventType:   eventType,
		MemoryMB:    memoryMB,
		GBSeconds:   gbSeconds,
		BucketStart: start,
		BucketEnd:   start.Add(15 * time.Minute),
	}
}

func TestBillableEvents_upsertIdempotent_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	base := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	ev := makeBucketEvent(orgID, BillableEventReservedUsage, 0, 14400, base, 0) // 16 GB × 900s

	inserted, err := store.UpsertBillableEvent(ctx, ev)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !inserted {
		t.Error("expected first upsert to insert, got conflict no-op")
	}

	// Same key — must no-op.
	inserted, err = store.UpsertBillableEvent(ctx, ev)
	if err != nil {
		t.Fatalf("replay upsert: %v", err)
	}
	if inserted {
		t.Error("expected replay to no-op, got fresh insert")
	}

	// Different gb_seconds, same key — should still no-op (UNIQUE wins
	// over the new value; allocator must not be able to "correct" a
	// historical row by reinserting).
	ev2 := ev
	ev2.GBSeconds = 99999
	inserted, err = store.UpsertBillableEvent(ctx, ev2)
	if err != nil {
		t.Fatalf("differing-value upsert: %v", err)
	}
	if inserted {
		t.Error("expected differing-value replay to no-op, got fresh insert")
	}

	// Verify the persisted row still has the original gb_seconds.
	rows, err := store.ListBillableEventsForBucket(ctx, orgID, ev.BucketStart)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].GBSeconds != 14400 {
		t.Errorf("gb_seconds: got %v, want 14400 (replay must not overwrite)", rows[0].GBSeconds)
	}
}

func TestBillableEvents_distinctKeys_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	base := time.Date(2030, 7, 1, 0, 0, 0, 0, time.UTC)

	// All four of these must be distinct rows: same org, same bucket,
	// but different (event_type, memory_mb) tuples.
	cases := []BillableEvent{
		makeBucketEvent(orgID, BillableEventReservedUsage, 0, 7200, base, 0),
		makeBucketEvent(orgID, BillableEventOverageUsage, 4096, 1800, base, 0),
		makeBucketEvent(orgID, BillableEventOverageUsage, 8192, 900, base, 0),
		makeBucketEvent(orgID, BillableEventDiskOverageUsage, 0, 18000, base, 0),
	}
	for i, ev := range cases {
		ins, err := store.UpsertBillableEvent(ctx, ev)
		if err != nil {
			t.Fatalf("upsert[%d]: %v", i, err)
		}
		if !ins {
			t.Errorf("upsert[%d] expected insert, got conflict no-op", i)
		}
	}

	rows, err := store.ListBillableEventsForBucket(ctx, orgID, cases[0].BucketStart)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
}

func TestBillableEvents_listPendingOrder_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	base := time.Date(2030, 8, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		ev := makeBucketEvent(orgID, BillableEventReservedUsage, 0, 900, base, i)
		if _, err := store.UpsertBillableEvent(ctx, ev); err != nil {
			t.Fatalf("upsert[%d]: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond) // distinct created_at ordering
	}

	pending, err := store.ListPendingBillableEvents(ctx, 100)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	mine := 0
	var prev time.Time
	for _, e := range pending {
		if e.OrgID != orgID {
			continue // other tests may have left rows around
		}
		mine++
		if !prev.IsZero() && e.CreatedAt.Before(prev) {
			t.Errorf("pending list out of created_at order")
		}
		prev = e.CreatedAt
		if e.DeliveryState != BillableDeliveryPending {
			t.Errorf("expected pending, got %q", e.DeliveryState)
		}
	}
	if mine != 5 {
		t.Errorf("expected 5 of our rows in pending list, got %d", mine)
	}
}

func TestBillableEvents_allocatorCandidates_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	// Use a far-future window so we don't intersect with state from
	// other tests in the same DB.
	bucket := time.Date(2031, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := bucket.Add(15 * time.Minute) // bucket end = candidate window end
	lookbackStart := bucket.Add(-1 * time.Hour)

	// Seed one scale event covering the bucket and one reservation
	// for the same bucket.
	end := bucket.Add(15 * time.Minute)
	if _, err := store.pool.Exec(ctx,
		`INSERT INTO sandbox_scale_events (sandbox_id, org_id, memory_mb, cpu_percent, disk_mb, started_at, ended_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		"sbx-alloc", orgID, 8192, 200, 20480, bucket, &end); err != nil {
		t.Fatalf("seed scale event: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `
		INSERT INTO capacity_reservation_intervals
		    (reservation_id, org_id, resource, starts_at, ends_at, capacity_gb)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, uuid.New(), orgID, ResourceMemoryGB, bucket, end, 4); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}

	cands, err := store.ListAllocatorCandidates(ctx, lookbackStart, cutoff, 100)
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	found := false
	for _, c := range cands {
		if c.OrgID == orgID && c.BucketStart.Equal(bucket) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected candidate (orgID=%s, bucket=%s) in result, got %d candidates", orgID, bucket, len(cands))
	}

	// Now emit a row for that bucket and confirm the candidate
	// disappears (NOT EXISTS clause).
	ev := makeBucketEvent(orgID, BillableEventReservedUsage, 0, 4*900, bucket, 0)
	if _, err := store.UpsertBillableEvent(ctx, ev); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	cands, err = store.ListAllocatorCandidates(ctx, lookbackStart, cutoff, 100)
	if err != nil {
		t.Fatalf("list candidates 2: %v", err)
	}
	for _, c := range cands {
		if c.OrgID == orgID && c.BucketStart.Equal(bucket) {
			t.Fatalf("expected candidate to be excluded after emission, got it back")
		}
	}
}

func TestBillableEvents_reservedAndScaleHelpers_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	bucket := time.Date(2031, 2, 1, 12, 0, 0, 0, time.UTC)
	end := bucket.Add(15 * time.Minute)

	// Two reservations (4 GB + 8 GB) on the same bucket → SUM = 12.
	for _, gb := range []int{4, 8} {
		if _, err := store.pool.Exec(ctx, `
			INSERT INTO capacity_reservation_intervals
			    (reservation_id, org_id, resource, starts_at, ends_at, capacity_gb)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, uuid.New(), orgID, ResourceMemoryGB, bucket, end, gb); err != nil {
			t.Fatalf("seed reservation %dGB: %v", gb, err)
		}
	}
	got, err := store.GetReservedGBForBucket(ctx, orgID, bucket)
	if err != nil {
		t.Fatalf("get reserved gb: %v", err)
	}
	if got != 12 {
		t.Errorf("reserved gb: got %d, want 12", got)
	}

	// Scale events: one fully inside, one straddling the start, one
	// straddling the end, and one completely outside (must not be
	// returned).
	insertSE := func(sandboxID string, memMB, diskMB int, started time.Time, ended *time.Time) {
		if _, err := store.pool.Exec(ctx,
			`INSERT INTO sandbox_scale_events (sandbox_id, org_id, memory_mb, cpu_percent, disk_mb, started_at, ended_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			sandboxID, orgID, memMB, memMB/40, diskMB, started, ended); err != nil {
			t.Fatalf("seed scale event %s: %v", sandboxID, err)
		}
	}
	preEnd := bucket.Add(5 * time.Minute)
	postStart := bucket.Add(10 * time.Minute)
	farPast := bucket.Add(-2 * time.Hour)
	farPastEnd := bucket.Add(-1 * time.Hour)
	insertSE("sbx-inside", 4096, 20480, bucket.Add(2*time.Minute), &preEnd)
	insertSE("sbx-pre", 8192, 20480, bucket.Add(-3*time.Minute), &postStart)
	insertSE("sbx-post", 16384, 20480, postStart, nil) // open
	insertSE("sbx-outside", 1024, 20480, farPast, &farPastEnd)

	events, err := store.GetScaleEventsForBucket(ctx, orgID, bucket, end)
	if err != nil {
		t.Fatalf("get scale events: %v", err)
	}
	if len(events) != 3 {
		t.Errorf("expected 3 in-window events, got %d", len(events))
	}
	for _, e := range events {
		if e.SandboxID == "sbx-outside" {
			t.Errorf("expected sbx-outside excluded, got it")
		}
	}
}

func TestBillableEvents_validation_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)
	orgID := seedOrgWithCap(t, store, 64)

	base := time.Date(2030, 9, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		mutate  func(*BillableEvent)
		wantSub string
	}{
		{
			name:    "zero gb_seconds rejected by handler",
			mutate:  func(e *BillableEvent) { e.GBSeconds = 0 },
			wantSub: "gb_seconds must be > 0",
		},
		{
			name:    "non-15-minute bucket rejected by handler",
			mutate:  func(e *BillableEvent) { e.BucketEnd = e.BucketStart.Add(20 * time.Minute) },
			wantSub: "bucket must span exactly 15 minutes",
		},
		{
			name: "unknown event_type rejected by schema CHECK",
			mutate: func(e *BillableEvent) {
				e.EventType = "totally_made_up"
			},
			wantSub: "billable_events_event_type_check",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := makeBucketEvent(orgID, BillableEventReservedUsage, 0, 900, base, 0)
			tc.mutate(&ev)
			_, err := store.UpsertBillableEvent(ctx, ev)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
