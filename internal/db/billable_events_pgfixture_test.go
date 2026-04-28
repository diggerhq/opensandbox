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
