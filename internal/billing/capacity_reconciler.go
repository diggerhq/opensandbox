package billing

import (
	"context"
	"log"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
)

// CapacityReconciler is the phase-2 outbox allocator. Every `interval`,
// it scans for closed 15-min buckets that are at least `settle` past
// their end and haven't been processed yet, runs the per-second
// integration walk for each (org, bucket), and emits the resulting
// reserved_usage / overage_usage / disk_overage_usage rows to the
// `billable_events` outbox.
//
// Phase 2 runs in shadow: the rows are written but not delivered to
// Stripe. UsageReporter continues to drive real billing. Shadow-verify
// (cmd/capacity-shadow-verify) compares the two for orgs with zero
// reservations to validate the new pipeline before phase 3 cuts over.
//
// The outbox unique constraint
// `(org_id, event_type, memory_mb, bucket_start)` makes the allocator
// idempotent at the DB level — a crashed or restarted reconciler can
// replay any bucket safely.
type CapacityReconciler struct {
	store    *db.Store
	interval time.Duration
	settle   time.Duration
	lookback time.Duration
	limit    int
	stop     chan struct{}
	stopped  chan struct{}
}

// CapacityReconcilerOpts allows the server bootstrap to override the
// defaults via env (CAPACITY_ALLOCATOR_INTERVAL, _SETTLE_MINUTES,
// _LOOKBACK_HOURS, _BATCH_LIMIT). Zero values fall back to defaults.
type CapacityReconcilerOpts struct {
	Interval time.Duration // default 5m
	Settle   time.Duration // default 30m
	Lookback time.Duration // default 24h
	Limit    int           // candidates per tick, default 500
}

func NewCapacityReconciler(store *db.Store, opts CapacityReconcilerOpts) *CapacityReconciler {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.Settle <= 0 {
		opts.Settle = 30 * time.Minute
	}
	if opts.Lookback <= 0 {
		opts.Lookback = 24 * time.Hour
	}
	if opts.Limit <= 0 {
		opts.Limit = 500
	}
	return &CapacityReconciler{
		store:    store,
		interval: opts.Interval,
		settle:   opts.Settle,
		lookback: opts.Lookback,
		limit:    opts.Limit,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

func (r *CapacityReconciler) Start() { go r.loop() }
func (r *CapacityReconciler) Stop()  { close(r.stop); <-r.stopped }

func (r *CapacityReconciler) loop() {
	defer close(r.stopped)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.tickSafe()
		case <-r.stop:
			return
		}
	}
}

func (r *CapacityReconciler) tickSafe() {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("capacity-reconciler: recovered from panic: %v", v)
		}
	}()
	r.tick()
}

func (r *CapacityReconciler) tick() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Cutoff: only buckets whose end is at least `settle` ago are eligible.
	// Truncate to a 15-min boundary so we don't accidentally include a
	// half-finished bucket if `settle` happens to land mid-bucket.
	cutoff := time.Now().UTC().Add(-r.settle).Truncate(15 * time.Minute)
	lookbackStart := cutoff.Add(-r.lookback)

	candidates, err := r.store.ListAllocatorCandidates(ctx, lookbackStart, cutoff, r.limit)
	if err != nil {
		log.Printf("capacity-reconciler: list candidates: %v", err)
		return
	}
	if len(candidates) == 0 {
		return
	}
	log.Printf("capacity-reconciler: processing %d candidate bucket(s)", len(candidates))

	processed := 0
	for _, c := range candidates {
		if err := r.processBucket(ctx, c.OrgID, c.BucketStart); err != nil {
			log.Printf("capacity-reconciler: org=%s bucket=%s: %v", c.OrgID, c.BucketStart.Format(time.RFC3339), err)
			continue
		}
		processed++
	}
	log.Printf("capacity-reconciler: emitted outbox rows for %d/%d bucket(s)", processed, len(candidates))
}

// processBucket runs the per-second integration walk for one (org,
// bucket) and emits the resulting outbox rows. The outbox UNIQUE
// constraint absorbs replays.
func (r *CapacityReconciler) processBucket(ctx context.Context, orgID uuid.UUID, bucketStart time.Time) error {
	bucketEnd := bucketStart.Add(15 * time.Minute)

	reservedGB, err := r.store.GetReservedGBForBucket(ctx, orgID, bucketStart)
	if err != nil {
		return err
	}

	events, err := r.store.GetScaleEventsForBucket(ctx, orgID, bucketStart, bucketEnd)
	if err != nil {
		return err
	}

	totals := IntegrateBucket(bucketStart, bucketEnd, reservedGB, events)
	return emitBucket(ctx, r.store, orgID, bucketStart, bucketEnd, reservedGB, totals)
}

// BucketTotals is the output of the per-bucket integration walk.
// Memory tier keys are `memory_mb` (1024 / 4096 / ...). DiskOverageGBSeconds
// is org-level, summed across all running events at each segment.
type BucketTotals struct {
	OverageGBSecondsByTier map[int]float64
	DiskOverageGBSeconds   float64
	ReservedFloorGBSeconds float64 // reservedGb × secs accumulated across segments — for shadow validation only
}

// IntegrateBucket runs the full clip → segments → integrate pipeline
// for one (org, bucket). Pure-Go, deterministic, unit-testable.
func IntegrateBucket(bucketStart, bucketEnd time.Time, reservedGB int, events []db.ScaleEvent) BucketTotals {
	clipped := make([]clippedEvent, 0, len(events))
	for _, e := range events {
		c, ok := clipEvent(e, bucketStart, bucketEnd)
		if !ok {
			continue
		}
		clipped = append(clipped, c)
	}
	boundaries := collectBoundaries(clipped, bucketStart, bucketEnd)
	segs := walkSegments(boundaries, clipped)
	return integrateSegments(segs, reservedGB)
}

type clippedEvent struct {
	From, To time.Time
	TierMB   int
	DiskMB   int
}

// clipEvent restricts a ScaleEvent's lifetime to the bucket window.
// Returns ok=false if the event's clipped span is empty.
func clipEvent(e db.ScaleEvent, bucketStart, bucketEnd time.Time) (clippedEvent, bool) {
	from := e.StartedAt
	if from.Before(bucketStart) {
		from = bucketStart
	}
	to := bucketEnd
	if e.EndedAt != nil && e.EndedAt.Before(bucketEnd) {
		to = *e.EndedAt
	}
	if !to.After(from) {
		return clippedEvent{}, false
	}
	return clippedEvent{From: from, To: to, TierMB: e.MemoryMB, DiskMB: e.DiskMB}, true
}

func collectBoundaries(events []clippedEvent, bucketStart, bucketEnd time.Time) []time.Time {
	seen := map[int64]struct{}{
		bucketStart.UnixNano(): {},
		bucketEnd.UnixNano():   {},
	}
	out := []time.Time{bucketStart, bucketEnd}
	for _, e := range events {
		if _, ok := seen[e.From.UnixNano()]; !ok {
			seen[e.From.UnixNano()] = struct{}{}
			out = append(out, e.From)
		}
		if _, ok := seen[e.To.UnixNano()]; !ok {
			seen[e.To.UnixNano()] = struct{}{}
			out = append(out, e.To)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

type segment struct {
	From, To       time.Time
	RunningByTier  map[int]int // tier_mb → running GB at this tier
	DiskOverageMB  int         // sum of (disk_mb − 20480) over running events
}

func walkSegments(boundaries []time.Time, events []clippedEvent) []segment {
	segs := make([]segment, 0, len(boundaries)-1)
	for i := 0; i < len(boundaries)-1; i++ {
		from, to := boundaries[i], boundaries[i+1]
		if !to.After(from) {
			continue
		}
		tiers := map[int]int{}
		diskOver := 0
		for _, e := range events {
			// Event is "running" in [from, to) if it covers the segment
			// fully — i.e. e.From <= from AND e.To >= to. Since the
			// boundary set includes every event endpoint, every segment
			// is fully contained in any event whose span covers `from`.
			if !e.From.After(from) && !e.To.Before(to) {
				tiers[e.TierMB] += e.TierMB / 1024
				if e.DiskMB > 20480 {
					diskOver += e.DiskMB - 20480
				}
			}
		}
		segs = append(segs, segment{From: from, To: to, RunningByTier: tiers, DiskOverageMB: diskOver})
	}
	return segs
}

func integrateSegments(segs []segment, reservedGB int) BucketTotals {
	out := BucketTotals{OverageGBSecondsByTier: map[int]float64{}}
	for _, s := range segs {
		secs := s.To.Sub(s.From).Seconds()
		usage := 0
		for _, gb := range s.RunningByTier {
			usage += gb
		}

		if usage > reservedGB {
			spike := float64(usage - reservedGB)
			for tier, gb := range s.RunningByTier {
				share := float64(gb) / float64(usage)
				out.OverageGBSecondsByTier[tier] += spike * share * secs
			}
			out.ReservedFloorGBSeconds += float64(reservedGB) * secs
		} else {
			out.ReservedFloorGBSeconds += float64(usage) * secs
		}

		if s.DiskOverageMB > 0 {
			out.DiskOverageGBSeconds += float64(s.DiskOverageMB) / 1024.0 * secs
		}
	}
	return out
}

// emitBucket writes the outbox rows for one settled bucket.
//
//   - reserved_usage  — one row per (org, bucket) when reservedGB > 0.
//     `gb_seconds = reservedGB × 900` (full bucket charge regardless of
//     actual usage — the customer paid for the floor whether or not
//     they used it).
//   - overage_usage   — one row per (org, sandbox_tier, bucket) where
//     the tier's overage contribution is non-zero.
//   - disk_overage_usage — one row per (org, bucket) when any sandbox
//     in the bucket exceeded the 20 GB allowance.
//
// `INSERT ... ON CONFLICT DO NOTHING` (inside `UpsertBillableEvent`)
// makes each emission idempotent at the unique constraint.
func emitBucket(ctx context.Context, store *db.Store, orgID uuid.UUID, bucketStart, bucketEnd time.Time, reservedGB int, totals BucketTotals) error {
	if reservedGB > 0 {
		ev := db.BillableEvent{
			OrgID:       orgID,
			EventType:   db.BillableEventReservedUsage,
			MemoryMB:    0,
			GBSeconds:   float64(reservedGB) * 900.0,
			BucketStart: bucketStart,
			BucketEnd:   bucketEnd,
		}
		if _, err := store.UpsertBillableEvent(ctx, ev); err != nil {
			return err
		}
	}

	for tier, gbs := range totals.OverageGBSecondsByTier {
		if gbs <= 0 {
			continue
		}
		ev := db.BillableEvent{
			OrgID:       orgID,
			EventType:   db.BillableEventOverageUsage,
			MemoryMB:    tier,
			GBSeconds:   gbs,
			BucketStart: bucketStart,
			BucketEnd:   bucketEnd,
		}
		if _, err := store.UpsertBillableEvent(ctx, ev); err != nil {
			return err
		}
	}

	if totals.DiskOverageGBSeconds > 0 {
		ev := db.BillableEvent{
			OrgID:       orgID,
			EventType:   db.BillableEventDiskOverageUsage,
			MemoryMB:    0,
			GBSeconds:   totals.DiskOverageGBSeconds,
			BucketStart: bucketStart,
			BucketEnd:   bucketEnd,
		}
		if _, err := store.UpsertBillableEvent(ctx, ev); err != nil {
			return err
		}
	}

	return nil
}
