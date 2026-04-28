package billing

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/opensandbox/opensandbox/internal/db"
)

// BillableEventsSender is the phase-3 Stripe shipper. Every `interval`,
// it pulls pending rows from `billable_events` for orgs in
// `billing_mode='unified'` and submits them to Stripe meter events
// using `billable_events.id` as the idempotency key. On success the
// row's `delivery_state` flips to `'sent'`. On error the row stays
// `'pending'` and gets retried next tick — Stripe meter events are
// idempotent by identifier within 24h, so retrying is safe.
//
// New orgs default to `billing_mode='unified'` (per migration 031);
// existing orgs are pinned to `'legacy'`. So on deploy this goroutine
// has zero work to do until the first new pro signup or until an org
// is manually flipped.
type BillableEventsSender struct {
	store    *db.Store
	stripe   *StripeClient
	interval time.Duration
	batch    int
	stop     chan struct{}
	stopped  chan struct{}
}

// BillableEventsSenderOpts allows the server bootstrap to override the
// defaults via env. Zero values fall back to defaults.
type BillableEventsSenderOpts struct {
	Interval time.Duration // default 5m — same cadence as UsageReporter
	Batch    int           // pending rows per tick, default 200
}

func NewBillableEventsSender(store *db.Store, stripe *StripeClient, opts BillableEventsSenderOpts) *BillableEventsSender {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.Batch <= 0 {
		opts.Batch = 200
	}
	return &BillableEventsSender{
		store:    store,
		stripe:   stripe,
		interval: opts.Interval,
		batch:    opts.Batch,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

func (s *BillableEventsSender) Start() { go s.loop() }
func (s *BillableEventsSender) Stop()  { close(s.stop); <-s.stopped }

func (s *BillableEventsSender) loop() {
	defer close(s.stopped)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.tickSafe()
		case <-s.stop:
			return
		}
	}
}

func (s *BillableEventsSender) tickSafe() {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("billable-events-sender: recovered from panic: %v", v)
		}
	}()
	s.tick()
}

func (s *BillableEventsSender) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pending, err := s.store.ListPendingBillableEventsForSender(ctx, s.batch)
	if err != nil {
		log.Printf("billable-events-sender: list pending: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	log.Printf("billable-events-sender: shipping %d pending event(s)", len(pending))

	sent, failed := 0, 0
	for _, p := range pending {
		if err := s.shipOne(ctx, p); err != nil {
			log.Printf("billable-events-sender: event=%s org=%s type=%s tier=%d: %v",
				p.Event.ID, p.Event.OrgID, p.Event.EventType, p.Event.MemoryMB, err)
			failed++
			continue
		}
		sent++
	}
	log.Printf("billable-events-sender: sent=%d failed=%d (failed rows stay 'pending' for retry)", sent, failed)
}

// shipOne maps one outbox row to its Stripe meter event_name, submits
// the meter event with `Identifier = billable_events.id` for at-least-
// once safety, and marks the row 'sent' on success.
func (s *BillableEventsSender) shipOne(ctx context.Context, p db.PendingBillableEventForSender) error {
	eventName, err := s.meterEventNameFor(p.Event.EventType, p.Event.MemoryMB)
	if err != nil {
		return err
	}

	// Stripe wants a unix timestamp for the meter event. Use the
	// bucket end — that's the moment the usage was "incurred" from
	// Stripe's perspective and is stable across retries.
	timestamp := p.Event.BucketEnd.Unix()
	identifier := p.Event.ID.String()

	stripeID, err := s.stripe.ReportMeterEvent(eventName, p.StripeCustomerID, p.Event.GBSeconds, identifier, timestamp)
	if err != nil {
		return fmt.Errorf("submit meter event: %w", err)
	}

	if err := s.store.MarkBillableEventSent(ctx, p.Event.ID, stripeID); err != nil {
		// Stripe accepted the event but we couldn't update the row.
		// Next tick will resubmit; the Identifier dedup makes this
		// safe — Stripe will return the same event ID without
		// double-counting.
		return fmt.Errorf("mark sent (will retry): %w", err)
	}
	return nil
}

// meterEventNameFor maps an outbox event_type → Stripe meter event_name.
// All three event types route to a single meter each — overage is flat
// (the per-tier memory_mb on the outbox row is preserved for analytics
// but ignored when shipping; Stripe sums GB-seconds across rows).
func (s *BillableEventsSender) meterEventNameFor(eventType string, memoryMB int) (string, error) {
	switch eventType {
	case db.BillableEventReservedUsage:
		if s.stripe.ReservedMeterEventName == "" {
			return "", fmt.Errorf("reserved meter not provisioned (run EnsureProducts)")
		}
		return s.stripe.ReservedMeterEventName, nil
	case db.BillableEventOverageUsage:
		if s.stripe.OverageMeterEventName == "" {
			return "", fmt.Errorf("overage meter not provisioned (run EnsureProducts)")
		}
		return s.stripe.OverageMeterEventName, nil
	case db.BillableEventDiskOverageUsage:
		if s.stripe.DiskOverageMeterEventName == "" {
			return "", fmt.Errorf("disk overage meter not provisioned")
		}
		return s.stripe.DiskOverageMeterEventName, nil
	default:
		return "", fmt.Errorf("unknown event_type %q", eventType)
	}
}
