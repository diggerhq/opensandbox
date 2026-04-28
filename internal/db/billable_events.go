package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Outbox of metered events for the unified billing pipeline. Phase 2
// (capacity allocator) writes rows here after each 15-min bucket
// settles. Phase 3 (Stripe sender) reads pending rows and ships them.
//
// The (org_id, event_type, memory_mb, bucket_start) UNIQUE constraint
// makes allocator reruns DB-level no-ops, so a crashed or restarted
// allocator can replay any bucket safely.

// Event types written to billable_events.event_type. Mirror the schema
// CHECK constraint in migration 030.
const (
	BillableEventReservedUsage     = "reserved_usage"
	BillableEventOverageUsage      = "overage_usage"
	BillableEventDiskOverageUsage  = "disk_overage_usage"
)

// Delivery states. Mirror the schema CHECK constraint in migration 030.
const (
	BillableDeliveryPending = "pending"
	BillableDeliverySent    = "sent"
	BillableDeliveryFailed  = "failed"
)

// BillableEvent is one outbox row.
//
// `MemoryMB` is 0 for `reserved_usage` and `disk_overage_usage` (sentinel
// for "not a sandbox tier"), and the running sandbox tier for
// `overage_usage` (one row per tier per bucket via the proportional split
// rule — see ws-pricing/work/001 "Per-second integration walk").
type BillableEvent struct {
	ID             uuid.UUID  `json:"id"`
	OrgID          uuid.UUID  `json:"orgId"`
	EventType      string     `json:"eventType"`
	MemoryMB       int        `json:"memoryMB"`
	GBSeconds      float64    `json:"gbSeconds"`
	BucketStart    time.Time  `json:"bucketStart"`
	BucketEnd      time.Time  `json:"bucketEnd"`
	DeliveryState  string     `json:"deliveryState"`
	StripeEventID  *string    `json:"stripeEventId,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
}

// UpsertBillableEvent inserts a new outbox row, or no-ops if a row with
// the same (org_id, event_type, memory_mb, bucket_start) already exists.
// Returns true when a fresh row was written, false when the conflict
// path was taken. Either outcome is success — the allocator is allowed
// to replay buckets without coordination.
func (s *Store) UpsertBillableEvent(ctx context.Context, ev BillableEvent) (inserted bool, err error) {
	if ev.GBSeconds <= 0 {
		return false, fmt.Errorf("billable event gb_seconds must be > 0, got %v", ev.GBSeconds)
	}
	if ev.BucketEnd.Sub(ev.BucketStart) != 15*time.Minute {
		return false, fmt.Errorf("billable event bucket must span exactly 15 minutes")
	}

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO billable_events
			(org_id, event_type, memory_mb, gb_seconds, bucket_start, bucket_end)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (org_id, event_type, memory_mb, bucket_start) DO NOTHING
	`, ev.OrgID, ev.EventType, ev.MemoryMB, ev.GBSeconds, ev.BucketStart, ev.BucketEnd)
	if err != nil {
		return false, fmt.Errorf("upsert billable event: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListPendingBillableEvents returns up to `limit` rows in
// `delivery_state = 'pending'` ordered by `(created_at, id)`. The
// ordering is stable across the partial index, so the Stripe sender can
// resume from the oldest pending row without missing or repeating
// anything.
func (s *Store) ListPendingBillableEvents(ctx context.Context, limit int) ([]BillableEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, event_type, memory_mb, gb_seconds,
		       bucket_start, bucket_end, delivery_state,
		       stripe_event_id, created_at, delivered_at
		FROM billable_events
		WHERE delivery_state = 'pending'
		ORDER BY created_at, id
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending billable events: %w", err)
	}
	defer rows.Close()

	out := make([]BillableEvent, 0, limit)
	for rows.Next() {
		var e BillableEvent
		if err := rows.Scan(
			&e.ID, &e.OrgID, &e.EventType, &e.MemoryMB, &e.GBSeconds,
			&e.BucketStart, &e.BucketEnd, &e.DeliveryState,
			&e.StripeEventID, &e.CreatedAt, &e.DeliveredAt,
		); err != nil {
			return nil, fmt.Errorf("scan billable event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billable events rows: %w", err)
	}
	return out, nil
}

// ListBillableEventsForBucket returns every emitted row for a single
// (org, bucket) pair across all event types. Used by the shadow-verify
// script and the dashboard breakdown view to reconcile against
// UsageReporter output.
func (s *Store) ListBillableEventsForBucket(ctx context.Context, orgID uuid.UUID, bucketStart time.Time) ([]BillableEvent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, org_id, event_type, memory_mb, gb_seconds,
		       bucket_start, bucket_end, delivery_state,
		       stripe_event_id, created_at, delivered_at
		FROM billable_events
		WHERE org_id = $1 AND bucket_start = $2
		ORDER BY event_type, memory_mb
	`, orgID, bucketStart)
	if err != nil {
		return nil, fmt.Errorf("list bucket billable events: %w", err)
	}
	defer rows.Close()

	out := make([]BillableEvent, 0, 8)
	for rows.Next() {
		var e BillableEvent
		if err := rows.Scan(
			&e.ID, &e.OrgID, &e.EventType, &e.MemoryMB, &e.GBSeconds,
			&e.BucketStart, &e.BucketEnd, &e.DeliveryState,
			&e.StripeEventID, &e.CreatedAt, &e.DeliveredAt,
		); err != nil {
			return nil, fmt.Errorf("scan billable event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billable events rows: %w", err)
	}
	return out, nil
}
