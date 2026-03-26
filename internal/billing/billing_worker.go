package billing

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/db"
)

// BillingWorker periodically deducts credits based on sandbox usage
// and triggers auto top-ups when the balance drops below the threshold.
type BillingWorker struct {
	store    *db.Store
	stripe   *StripeClient // nil if Stripe not configured (auto top-up disabled)
	interval time.Duration
	stop     chan struct{}
	stopped  chan struct{}
}

// NewBillingWorker creates a new billing worker.
func NewBillingWorker(store *db.Store, stripe *StripeClient) *BillingWorker {
	return &BillingWorker{
		store:    store,
		stripe:   stripe,
		interval: 60 * time.Second,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start begins the billing loop in a background goroutine.
func (b *BillingWorker) Start() {
	go b.loop()
}

// Stop signals the billing loop to stop and waits for it to finish.
func (b *BillingWorker) Stop() {
	close(b.stop)
	<-b.stopped
}

func (b *BillingWorker) loop() {
	defer close(b.stopped)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.billAllOrgs()
		case <-b.stop:
			return
		}
	}
}

func (b *BillingWorker) billAllOrgs() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	orgIDs, err := b.store.ListBillableOrgIDs(ctx)
	if err != nil {
		log.Printf("billing: failed to list billable orgs: %v", err)
		return
	}

	if len(orgIDs) == 0 {
		log.Printf("billing: tick — no billable orgs")
		return
	}

	log.Printf("billing: tick — processing %d org(s)", len(orgIDs))
	for _, orgID := range orgIDs {
		if err := b.billOrg(ctx, orgID); err != nil {
			log.Printf("billing: failed to bill org %s: %v", orgID, err)
		}
	}
}

func (b *BillingWorker) billOrg(ctx context.Context, orgID uuid.UUID) error {
	info, err := b.store.GetOrgBillingInfo(ctx, orgID)
	if err != nil {
		return fmt.Errorf("get billing info: %w", err)
	}

	now := time.Now()
	from := info.LastBilledAt
	to := now

	// Get usage since last billing
	usage, err := b.store.GetOrgUsage(ctx, orgID.String(), from, to)
	if err != nil {
		return fmt.Errorf("get usage: %w", err)
	}

	// Calculate cost
	costCents := CalculateUsageCostCents(usage)
	if costCents == 0 {
		// Still update the watermark even if no cost
		b.store.DeductCredits(ctx, info.ID, 0, info.UnbilledUsageCents, to, info.LastBilledAt)
		return nil
	}

	// Accumulate with existing unbilled amount
	totalUnbilled := info.UnbilledUsageCents + costCents
	wholeCents := int(math.Floor(totalUnbilled))
	remainder := totalUnbilled - float64(wholeCents)

	// Deduct with optimistic lock
	ok, err := b.store.DeductCredits(ctx, info.ID, wholeCents, remainder, to, info.LastBilledAt)
	if err != nil {
		return fmt.Errorf("deduct credits: %w", err)
	}
	if !ok {
		// Another instance already billed this org — skip
		log.Printf("billing: org %s skipped (already billed by another instance)", orgID)
		return nil
	}

	log.Printf("billing: org %s — deducted %d cents (unbilled: %.4f cents, balance: %d → %d)",
		orgID, wholeCents, remainder, info.CreditBalanceCents, info.CreditBalanceCents-wholeCents)

	// Record the deduction in the ledger if there were whole cents
	if wholeCents > 0 {
		newBalance := info.CreditBalanceCents - wholeCents
		if err := b.store.RecordUsageDeduction(ctx, info.ID, wholeCents, newBalance); err != nil {
			log.Printf("billing: failed to record usage deduction for org %s: %v", orgID, err)
		}
	}

	// Check auto top-up
	newBalance := info.CreditBalanceCents - wholeCents
	if b.stripe != nil && info.AutoTopupEnabled && info.StripeCustomerID != nil && newBalance <= info.AutoTopupThresholdCents {
		b.tryAutoTopup(ctx, info)
	}

	return nil
}

func (b *BillingWorker) tryAutoTopup(ctx context.Context, info *db.OrgBillingInfo) {
	// Check monthly spend cap
	if info.MonthlySpendCapCents != nil {
		now := time.Now()
		currentSpend, err := b.store.GetOrUpdateMonthlySpend(ctx, info.ID, now, 0)
		if err != nil {
			log.Printf("billing: failed to get monthly spend for org %s: %v", info.ID, err)
			return
		}
		if currentSpend+info.AutoTopupAmountCents > *info.MonthlySpendCapCents {
			log.Printf("billing: auto-topup skipped for org %s — monthly cap reached (%d/%d cents)",
				info.ID, currentSpend, *info.MonthlySpendCapCents)
			return
		}
	}

	// Charge the customer
	amountCents := int64(info.AutoTopupAmountCents)
	description := fmt.Sprintf("OpenSandbox auto top-up ($%.2f)", float64(amountCents)/100.0)

	piID, err := b.stripe.ChargeCustomer(*info.StripeCustomerID, amountCents, description)
	if err != nil {
		log.Printf("billing: auto-topup charge failed for org %s: %v", info.ID, err)
		return
	}

	// Add credits
	err = b.store.AddCredits(ctx, info.ID, info.AutoTopupAmountCents, "auto_topup",
		description, piID, "")
	if err != nil {
		log.Printf("billing: failed to add auto-topup credits for org %s: %v", info.ID, err)
		return
	}

	// Update monthly spend
	now := time.Now()
	_, err = b.store.GetOrUpdateMonthlySpend(ctx, info.ID, now, info.AutoTopupAmountCents)
	if err != nil {
		log.Printf("billing: failed to update monthly spend for org %s: %v", info.ID, err)
	}

	log.Printf("billing: auto-topup %d cents for org %s via PI %s", info.AutoTopupAmountCents, info.ID, piID)
}
