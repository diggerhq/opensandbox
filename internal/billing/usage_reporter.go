package billing

import (
	"context"
	"log"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
)

// UsageReporter periodically reports sandbox usage to Stripe via Billing Meter Events.
type UsageReporter struct {
	store    *db.Store
	stripe   *StripeClient
	interval time.Duration
	stop     chan struct{}
	stopped  chan struct{}
}

func NewUsageReporter(store *db.Store, stripe *StripeClient) *UsageReporter {
	return &UsageReporter{
		store:    store,
		stripe:   stripe,
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
			r.reportAll()
		case <-r.stop:
			return
		}
	}
}

func (r *UsageReporter) reportAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	orgIDs, err := r.store.ListBillableOrgIDs(ctx)
	if err != nil {
		log.Printf("usage-reporter: failed to list billable orgs: %v", err)
		return
	}
	if len(orgIDs) == 0 {
		return
	}

	log.Printf("usage-reporter: reporting for %d org(s)", len(orgIDs))
	for _, orgID := range orgIDs {
		if err := r.reportOrg(ctx, orgID); err != nil {
			log.Printf("usage-reporter: org %s: %v", orgID, err)
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
	for _, u := range usage {
		seconds := int64(math.Ceil(u.TotalSeconds))
		if seconds < 1 {
			continue
		}
		if err := r.stripe.ReportUsage(*org.StripeCustomerID, u.MemoryMB, seconds, now.Unix()); err != nil {
			log.Printf("usage-reporter: org %s tier %dMB: %v", orgID, u.MemoryMB, err)
			continue
		}
		reported++
	}

	if err := r.store.UpdateLastUsageReportedAt(ctx, orgID, to); err != nil {
		log.Printf("usage-reporter: org %s: failed to update watermark: %v", orgID, err)
	}

	if reported > 0 {
		log.Printf("usage-reporter: org %s — reported %d tier(s) to Stripe", orgID, reported)
	}
	return nil
}
