package billing

import (
	"math"
	"testing"
	"time"

	"github.com/opensandbox/opensandbox/internal/db"
)

// IntegrateBucket is the load-bearing math of the unified billing
// pipeline. These tests pin every branch of the spec from
// ws-pricing/work/001 "Per-second integration walk in detail":
//
//   - zero-reservation collapses to legacy reporter totals
//   - full-coverage reservation produces zero overage
//   - partial overage attributes spike across running tiers
//     proportionally to share-of-memory
//   - disk overage accumulates per-second per-MB above 20 GB
//   - mid-bucket scale events split into segments correctly

const (
	bucketStartCanon = 0
	bucketEndCanon   = 900
)

func eventAt(memMB, diskMB, fromSec, toSec int) db.ScaleEvent {
	base := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	from := base.Add(time.Duration(fromSec) * time.Second)
	to := base.Add(time.Duration(toSec) * time.Second)
	return db.ScaleEvent{
		MemoryMB:  memMB,
		DiskMB:    diskMB,
		StartedAt: from,
		EndedAt:   &to,
	}
}

func canonicalBucket() (time.Time, time.Time) {
	base := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	return base, base.Add(15 * time.Minute)
}

func eq(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("%s: got %v, want %v", label, got, want)
	}
}

func TestIntegrateBucket_zeroReservation_replaysLegacy(t *testing.T) {
	bs, be := canonicalBucket()
	// One 8 GB sandbox running the full bucket.
	totals := IntegrateBucket(bs, be, 0, []db.ScaleEvent{
		eventAt(8192, 20480, 0, 900),
	})
	// Zero reservation → all usage is overage. 8 GB × 900 s = 7200 GB-s
	// at tier 8192. Legacy reporter would report tier-seconds=900 at
	// 8192 MB, which corresponds to 8 × 900 = 7200 GB-seconds — exact
	// replay (modulo the legacy per-tick ceil() which we don't apply
	// at the bucket grain).
	eq(t, "tier 8192", totals.OverageGBSecondsByTier[8192], 7200)
	eq(t, "disk overage", totals.DiskOverageGBSeconds, 0)
	if len(totals.OverageGBSecondsByTier) != 1 {
		t.Errorf("expected only tier 8192, got %v", totals.OverageGBSecondsByTier)
	}
}

func TestIntegrateBucket_fullCoverage_noOverage(t *testing.T) {
	bs, be := canonicalBucket()
	// One 8 GB sandbox, reservation covers it.
	totals := IntegrateBucket(bs, be, 8, []db.ScaleEvent{
		eventAt(8192, 20480, 0, 900),
	})
	if len(totals.OverageGBSecondsByTier) != 0 {
		t.Errorf("expected no overage tiers, got %v", totals.OverageGBSecondsByTier)
	}
	eq(t, "reserved floor", totals.ReservedFloorGBSeconds, 8*900)
}

func TestIntegrateBucket_partialOverage_proportionalSplit(t *testing.T) {
	bs, be := canonicalBucket()
	// 8 GB + 16 GB running concurrently for the full bucket; reserved=8.
	// usage=24, spike=16. Tier 8192 share=8/24, tier 16384 share=16/24.
	totals := IntegrateBucket(bs, be, 8, []db.ScaleEvent{
		eventAt(8192, 20480, 0, 900),
		eventAt(16384, 20480, 0, 900),
	})
	eq(t, "tier 8192 overage", totals.OverageGBSecondsByTier[8192], 16.0*(8.0/24.0)*900.0)
	eq(t, "tier 16384 overage", totals.OverageGBSecondsByTier[16384], 16.0*(16.0/24.0)*900.0)
	// Sum of shares is 1.0: total overage = 16 GB × 900 s = 14400.
	total := totals.OverageGBSecondsByTier[8192] + totals.OverageGBSecondsByTier[16384]
	eq(t, "sum-of-shares", total, 14400)
}

func TestIntegrateBucket_sameTierAccumulates(t *testing.T) {
	bs, be := canonicalBucket()
	// Two 8 GB sandboxes, reserved=8. usage=16, spike=8, all on tier 8192.
	// share=16/16=1.0 → overage[8192] = 8 × 900 = 7200.
	totals := IntegrateBucket(bs, be, 8, []db.ScaleEvent{
		eventAt(8192, 20480, 0, 900),
		eventAt(8192, 20480, 0, 900),
	})
	eq(t, "tier 8192", totals.OverageGBSecondsByTier[8192], 7200)
	if len(totals.OverageGBSecondsByTier) != 1 {
		t.Errorf("expected only tier 8192, got %v", totals.OverageGBSecondsByTier)
	}
}

func TestIntegrateBucket_diskOverage(t *testing.T) {
	bs, be := canonicalBucket()
	// 1 sandbox at 8 GB memory, 30 GB disk (10 GB over the 20 GB
	// allowance) running the full bucket.
	totals := IntegrateBucket(bs, be, 0, []db.ScaleEvent{
		eventAt(8192, 30720, 0, 900),
	})
	// 10 GB × 900 s = 9000 GB-seconds disk overage.
	eq(t, "disk overage", totals.DiskOverageGBSeconds, 9000)
}

func TestIntegrateBucket_segmentedTimeline(t *testing.T) {
	bs, be := canonicalBucket()
	// Mid-bucket scaling:
	//   [000s, 300s)  one 8 GB sandbox running
	//   [300s, 600s)  empty
	//   [600s, 900s)  one 4 GB sandbox running
	// reserved=0.
	totals := IntegrateBucket(bs, be, 0, []db.ScaleEvent{
		eventAt(8192, 20480, 0, 300),
		eventAt(4096, 20480, 600, 900),
	})
	// Segment 1: 8 GB × 300 s = 2400 at tier 8192.
	// Segment 3: 4 GB × 300 s = 1200 at tier 4096.
	eq(t, "tier 8192", totals.OverageGBSecondsByTier[8192], 2400)
	eq(t, "tier 4096", totals.OverageGBSecondsByTier[4096], 1200)
}

func TestIntegrateBucket_eventClippedToBucket(t *testing.T) {
	bs, be := canonicalBucket()
	// Event spans 60 s before bucket through 60 s after. Should clip
	// to the full bucket (900 s).
	base := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	preStart := base.Add(-60 * time.Second)
	postEnd := base.Add(960 * time.Second)
	totals := IntegrateBucket(bs, be, 0, []db.ScaleEvent{
		{MemoryMB: 8192, DiskMB: 20480, StartedAt: preStart, EndedAt: &postEnd},
	})
	eq(t, "tier 8192 (clipped to bucket)", totals.OverageGBSecondsByTier[8192], 7200)
}

func TestIntegrateBucket_openEvent(t *testing.T) {
	bs, be := canonicalBucket()
	// Event with EndedAt=nil (still open) starting mid-bucket. Should
	// run until bucket_end.
	base := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	totals := IntegrateBucket(bs, be, 0, []db.ScaleEvent{
		{MemoryMB: 8192, DiskMB: 20480, StartedAt: base.Add(450 * time.Second), EndedAt: nil},
	})
	// 8 GB × 450 s (00:07:30 → 00:15:00) = 3600.
	eq(t, "tier 8192 open", totals.OverageGBSecondsByTier[8192], 3600)
}

func TestIntegrateBucket_emptyBucket(t *testing.T) {
	bs, be := canonicalBucket()
	totals := IntegrateBucket(bs, be, 0, nil)
	if len(totals.OverageGBSecondsByTier) != 0 {
		t.Errorf("expected empty overage, got %v", totals.OverageGBSecondsByTier)
	}
	eq(t, "disk", totals.DiskOverageGBSeconds, 0)
	eq(t, "floor", totals.ReservedFloorGBSeconds, 0)
}

func TestIntegrateBucket_reservationOnly_bucketEmittedFromReservedFloor(t *testing.T) {
	// Reservation with no scale events: emitter still must emit the
	// reserved_usage row from the bucket-level call site, but the walk
	// itself produces no overage and no floor (no usage to account for).
	bs, be := canonicalBucket()
	totals := IntegrateBucket(bs, be, 8, nil)
	if len(totals.OverageGBSecondsByTier) != 0 {
		t.Errorf("expected no overage, got %v", totals.OverageGBSecondsByTier)
	}
	eq(t, "disk", totals.DiskOverageGBSeconds, 0)
	// The walk itself doesn't manufacture floor when there's no usage —
	// the emitter charges the full reservedGb × 900 separately. This
	// behaviour is what lets the reserved row stay independent of
	// whether the customer actually used the capacity.
	eq(t, "floor", totals.ReservedFloorGBSeconds, 0)
}

func TestIntegrateBucket_sumOfSharesAlwaysOne(t *testing.T) {
	// Property: at any segment with usage > reservedGb, the per-tier
	// overage shares add up to the total spike × secs exactly. This is
	// the invariant that lets the unified pipeline reproduce the legacy
	// per-tier totals when reservedGb=0.
	bs, be := canonicalBucket()
	cases := []struct {
		name     string
		reserved int
		events   []db.ScaleEvent
	}{
		{"three tiers concurrent", 4, []db.ScaleEvent{
			eventAt(4096, 20480, 0, 900),
			eventAt(8192, 20480, 0, 900),
			eventAt(16384, 20480, 0, 900),
		}},
		{"two tiers staggered", 8, []db.ScaleEvent{
			eventAt(8192, 20480, 0, 900),
			eventAt(16384, 20480, 300, 900),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			totals := IntegrateBucket(bs, be, tc.reserved, tc.events)
			// Brute force: integrate the spike second-by-second and
			// confirm the sum matches.
			expected := 0.0
			for sec := 0; sec < 900; sec++ {
				usageGB := 0
				for _, e := range tc.events {
					base := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
					t0 := e.StartedAt
					t1 := *e.EndedAt
					tNow := base.Add(time.Duration(sec) * time.Second)
					if !tNow.Before(t0) && tNow.Before(t1) {
						usageGB += e.MemoryMB / 1024
					}
				}
				if usageGB > tc.reserved {
					expected += float64(usageGB - tc.reserved)
				}
			}
			actual := 0.0
			for _, gbs := range totals.OverageGBSecondsByTier {
				actual += gbs
			}
			eq(t, "sum-of-shares == brute-force spike", actual, expected)
		})
	}
}
