package billing

import (
	"context"
	"log"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
)

// UsageReporter periodically (a) reports sandbox usage for Pro orgs to Stripe
// via Billing Meter Events, and (b) deducts the same usage from free-tier
// trial credits on the same tick; when a free org's balance reaches zero it
// force-hibernates their running sandboxes via the enforcer.
//
// workers may be nil in tests or in deployments without a worker registry
// (free-tier enforcement will then skip the hibernation step and log).
type UsageReporter struct {
	store    *db.Store
	stripe   *StripeClient
	workers  WorkerClientSource
	interval time.Duration
	stop     chan struct{}
	stopped  chan struct{}
}

func NewUsageReporter(store *db.Store, stripe *StripeClient, workers WorkerClientSource) *UsageReporter {
	return &UsageReporter{
		store:    store,
		stripe:   stripe,
		workers:  workers,
		interval: 5 * time.Minute,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

func (r *UsageReporter) Start() { go r.loop() }
func (r *UsageReporter) Stop()  { close(r.stop); <-r.stopped }

func (r *UsageReporter) loop() {
	defer close(r.stopped)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.safeReportAll()
		case <-r.stop:
			return
		}
	}
}

// safeReportAll wraps reportAll with a recover so a panic in one tick
// (e.g. nil-pointer in a gRPC call) doesn't kill the reporter goroutine
// and silently stop all future billing/credit ticks.
func (r *UsageReporter) safeReportAll() {
	defer func() {
		if v := recover(); v != nil {
			log.Printf("usage-reporter: recovered from panic: %v", v)
		}
	}()
	r.reportAll()
}

func (r *UsageReporter) reportAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Pro pass: ship usage to Stripe meters.
	orgIDs, err := r.store.ListBillableOrgIDs(ctx)
	if err != nil {
		log.Printf("usage-reporter: failed to list billable orgs: %v", err)
	} else if len(orgIDs) > 0 {
		log.Printf("usage-reporter: reporting for %d pro org(s)", len(orgIDs))
		for _, orgID := range orgIDs {
			if err := r.reportOrg(ctx, orgID); err != nil {
				log.Printf("usage-reporter: org %s: %v", orgID, err)
			}
		}
	}

	// Free-tier pass: deduct the same usage from trial credits; hibernate on empty.
	freeIDs, err := r.store.ListFreeOrgIDsWithOpenUsage(ctx)
	if err != nil {
		log.Printf("usage-reporter: failed to list free orgs: %v", err)
		return
	}
	if len(freeIDs) == 0 {
		return
	}
	log.Printf("usage-reporter: deducting credits for %d free org(s)", len(freeIDs))
	for _, orgID := range freeIDs {
		if err := r.deductFreeOrg(ctx, orgID); err != nil {
			log.Printf("usage-reporter: free org %s: %v", orgID, err)
		}
	}
}

func (r *UsageReporter) reportOrg(ctx context.Context, orgID uuid.UUID) error {
	org, err := r.store.GetOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if org.StripeCustomerID == nil {
		return nil
	}

	now := time.Now()
	from := org.LastUsageReportedAt
	to := now

	usage, err := r.store.GetOrgUsage(ctx, orgID.String(), from, to)
	if err != nil {
		return err
	}

	reported := 0
	var totalDiskGBSeconds float64
	for _, u := range usage {
		seconds := int64(math.Ceil(u.TotalSeconds))
		if seconds >= 1 {
			if err := r.stripe.ReportUsage(*org.StripeCustomerID, u.MemoryMB, seconds, now.Unix()); err != nil {
				log.Printf("usage-reporter: org %s tier %dMB: %v", orgID, u.MemoryMB, err)
			} else {
				reported++
			}
		}
		totalDiskGBSeconds += DiskOverageGBSeconds(u)
	}

	// Report disk overage as a single aggregated meter event for the window.
	if diskGBSec := int64(math.Ceil(totalDiskGBSeconds)); diskGBSec >= 1 {
		if err := r.stripe.ReportDiskOverageUsage(*org.StripeCustomerID, diskGBSec, now.Unix()); err != nil {
			log.Printf("usage-reporter: org %s disk overage: %v", orgID, err)
		} else {
			reported++
		}
	}

	if err := r.store.UpdateLastUsageReportedAt(ctx, orgID, to); err != nil {
		log.Printf("usage-reporter: org %s: failed to update watermark: %v", orgID, err)
	}

	if reported > 0 {
		log.Printf("usage-reporter: org %s — reported %d tier(s) to Stripe", orgID, reported)
	}
	return nil
}

// deductFreeOrg computes compute+disk cost for a free-tier org since its last
// report watermark, decrements its trial balance, advances the watermark, and
// force-hibernates running sandboxes when the balance hits zero.
func (r *UsageReporter) deductFreeOrg(ctx context.Context, orgID uuid.UUID) error {
	org, err := r.store.GetOrg(ctx, orgID)
	if err != nil {
		return err
	}
	// Defensive: an org that flipped to pro between ListFreeOrgIDs and now
	// will be picked up by the pro pass on the next tick.
	if org.Plan != "free" {
		return nil
	}

	now := time.Now()
	from := org.LastUsageReportedAt
	to := now

	usage, err := r.store.GetOrgUsage(ctx, orgID.String(), from, to)
	if err != nil {
		return err
	}

	// Round up to the nearest whole cent so sub-cent usage still accumulates
	// over ticks; max overcharge per tick is <1 cent, negligible for a trial.
	costCents := int64(math.Ceil(CalculateUsageCostCents(usage)))

	// Always advance the watermark — even for a zero-cent window, otherwise
	// we'd re-scan the same interval next tick without making progress.
	defer func() {
		if err := r.store.UpdateLastUsageReportedAt(ctx, orgID, to); err != nil {
			log.Printf("usage-reporter: free org %s: failed to update watermark: %v", orgID, err)
		}
	}()

	if costCents <= 0 {
		return nil
	}

	newBalance, err := r.store.DeductFreeCredits(ctx, orgID, costCents)
	if err != nil {
		return err
	}

	if newBalance > 0 {
		log.Printf("usage-reporter: free org %s — deducted %d cents, balance=%d", orgID, costCents, newBalance)
		return nil
	}

	log.Printf("usage-reporter: free org %s — credits exhausted (deducted %d, balance=%d), force-hibernating", orgID, costCents, newBalance)
	if r.workers == nil {
		log.Printf("usage-reporter: free org %s: no worker client source configured, skipping hibernation", orgID)
		return nil
	}
	if _, err := EnforceCreditExhaustion(ctx, r.store, r.workers, orgID); err != nil {
		return err
	}
	return nil
}
