package db

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// ScaleEvent represents a period at a specific resource tier.
type ScaleEvent struct {
	ID        string     `json:"id"`
	SandboxID string     `json:"sandboxId"`
	OrgID     string     `json:"orgId"`
	MemoryMB  int        `json:"memoryMB"`
	CPUPct    int        `json:"cpuPercent"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// UsageSample is a point-in-time resource usage measurement.
type UsageSample struct {
	SandboxID   string    `json:"sandboxId"`
	OrgID       string    `json:"orgId"`
	SampledAt   time.Time `json:"sampledAt"`
	MemoryMB    int       `json:"memoryMB"`
	CPUUsec     int64     `json:"cpuUsec"`
	MemoryBytes int64     `json:"memoryBytes"`
	PIDs        int       `json:"pids"`
}

// RecordScaleEvent ends the current scale event (if any) and starts a new one.
func (s *Store) RecordScaleEvent(ctx context.Context, sandboxID, orgID string, memoryMB, cpuPct int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// End the current open event
	_, err = tx.Exec(ctx,
		`UPDATE sandbox_scale_events SET ended_at = now()
		 WHERE sandbox_id = $1 AND ended_at IS NULL`, sandboxID)
	if err != nil {
		return err
	}

	// Start a new event
	_, err = tx.Exec(ctx,
		`INSERT INTO sandbox_scale_events (sandbox_id, org_id, memory_mb, cpu_percent)
		 VALUES ($1, $2, $3, $4)`,
		sandboxID, orgID, memoryMB, cpuPct)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// EndScaleEvent marks the current scale event as ended (sandbox stopped/hibernated).
func (s *Store) EndScaleEvent(ctx context.Context, sandboxID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE sandbox_scale_events SET ended_at = now()
		 WHERE sandbox_id = $1 AND ended_at IS NULL`, sandboxID)
	return err
}

// InsertUsageSamples batch-inserts usage samples.
func (s *Store) InsertUsageSamples(ctx context.Context, samples []UsageSample) error {
	if len(samples) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for _, sample := range samples {
		_, err := tx.Exec(ctx,
			`INSERT INTO sandbox_usage_samples (sandbox_id, org_id, sampled_at, memory_mb, cpu_usec, memory_bytes, pids)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (sandbox_id, sampled_at) DO NOTHING`,
			sample.SandboxID, sample.OrgID, sample.SampledAt, sample.MemoryMB, sample.CPUUsec, sample.MemoryBytes, sample.PIDs)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// OrgUsageSummary returns total billed seconds per memory tier for an org in a time range.
type OrgUsageSummary struct {
	MemoryMB     int     `json:"memoryMB"`
	CPUPercent   int     `json:"cpuPercent"`
	TotalSeconds float64 `json:"totalSeconds"`
}

// GetOrgUsage returns billing summary for an org.
func (s *Store) GetOrgUsage(ctx context.Context, orgID string, from, to time.Time) ([]OrgUsageSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT memory_mb, cpu_percent,
		       SUM(EXTRACT(EPOCH FROM (COALESCE(ended_at, LEAST(now(), $3)) - GREATEST(started_at, $2)))) as total_seconds
		FROM sandbox_scale_events
		WHERE org_id = $1
		  AND started_at < $3
		  AND (ended_at IS NULL OR ended_at > $2)
		GROUP BY memory_mb, cpu_percent
		ORDER BY memory_mb`,
		orgID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []OrgUsageSummary
	for rows.Next() {
		var s OrgUsageSummary
		if err := rows.Scan(&s.MemoryMB, &s.CPUPercent, &s.TotalSeconds); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// --- Billing methods ---

// OrgBillingInfo contains the billing-relevant fields for an org.
type OrgBillingInfo struct {
	ID                      uuid.UUID
	CreditBalanceCents      int
	LastBilledAt            time.Time
	UnbilledUsageCents      float64
	StripeCustomerID        *string
	AutoTopupEnabled        bool
	AutoTopupThresholdCents int
	AutoTopupAmountCents    int
	MonthlySpendCapCents    *int
}

// ListBillableOrgIDs returns org IDs that have at least one open scale event.
func (s *Store) ListBillableOrgIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT org_id FROM sandbox_scale_events WHERE ended_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetOrgBillingInfo returns billing fields for an org.
func (s *Store) GetOrgBillingInfo(ctx context.Context, orgID uuid.UUID) (*OrgBillingInfo, error) {
	info := &OrgBillingInfo{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, credit_balance_cents, last_billed_at, unbilled_usage_cents,
		        stripe_customer_id, auto_topup_enabled, auto_topup_threshold_cents,
		        auto_topup_amount_cents, monthly_spend_cap_cents
		 FROM orgs WHERE id = $1`, orgID,
	).Scan(&info.ID, &info.CreditBalanceCents, &info.LastBilledAt, &info.UnbilledUsageCents,
		&info.StripeCustomerID, &info.AutoTopupEnabled, &info.AutoTopupThresholdCents,
		&info.AutoTopupAmountCents, &info.MonthlySpendCapCents)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// DeductCredits atomically deducts whole cents from credit_balance_cents and updates
// unbilled_usage_cents and last_billed_at. Uses optimistic locking on last_billed_at
// to prevent double-billing across server instances.
func (s *Store) DeductCredits(ctx context.Context, orgID uuid.UUID, wholeCents int, unbilledRemainder float64, billedAt, expectedLastBilledAt time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE orgs
		 SET credit_balance_cents = credit_balance_cents - $2,
		     unbilled_usage_cents = $3,
		     last_billed_at = $4,
		     updated_at = now()
		 WHERE id = $1 AND last_billed_at = $5`,
		orgID, wholeCents, unbilledRemainder, billedAt, expectedLastBilledAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AddCredits adds credits to an org and records the transaction in the ledger.
func (s *Store) AddCredits(ctx context.Context, orgID uuid.UUID, amountCents int, txnType, description, stripePaymentIntentID, stripeCheckoutSessionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Update balance
	var newBalance int
	err = tx.QueryRow(ctx,
		`UPDATE orgs SET credit_balance_cents = credit_balance_cents + $2, updated_at = now()
		 WHERE id = $1 RETURNING credit_balance_cents`,
		orgID, amountCents).Scan(&newBalance)
	if err != nil {
		return fmt.Errorf("update credit balance: %w", err)
	}

	// Record transaction
	_, err = tx.Exec(ctx,
		`INSERT INTO credit_transactions (org_id, amount_cents, balance_after_cents, type, description, stripe_payment_intent_id, stripe_checkout_session_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		orgID, amountCents, newBalance, txnType, description, nilIfEmpty(stripePaymentIntentID), nilIfEmpty(stripeCheckoutSessionID))
	if err != nil {
		return fmt.Errorf("insert credit transaction: %w", err)
	}

	return tx.Commit(ctx)
}

// RecordUsageDeduction records a usage deduction in the credit_transactions ledger.
func (s *Store) RecordUsageDeduction(ctx context.Context, orgID uuid.UUID, amountCents int, balanceAfter int) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO credit_transactions (org_id, amount_cents, balance_after_cents, type, description)
		 VALUES ($1, $2, $3, 'usage_deduction', 'Periodic usage billing')`,
		orgID, -int(math.Abs(float64(amountCents))), balanceAfter)
	return err
}

// CreditTransaction represents a single entry in the credit ledger.
type CreditTransaction struct {
	ID                       string    `json:"id"`
	OrgID                    string    `json:"orgId"`
	AmountCents              int       `json:"amountCents"`
	BalanceAfterCents        int       `json:"balanceAfterCents"`
	Type                     string    `json:"type"`
	Description              *string   `json:"description,omitempty"`
	StripePaymentIntentID    *string   `json:"stripePaymentIntentId,omitempty"`
	StripeCheckoutSessionID  *string   `json:"stripeCheckoutSessionId,omitempty"`
	CreatedAt                time.Time `json:"createdAt"`
}

// GetCreditTransactions returns paginated credit transaction history for an org.
func (s *Store) GetCreditTransactions(ctx context.Context, orgID uuid.UUID, limit, offset int) ([]CreditTransaction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, org_id, amount_cents, balance_after_cents, type, description,
		        stripe_payment_intent_id, stripe_checkout_session_id, created_at
		 FROM credit_transactions
		 WHERE org_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2 OFFSET $3`,
		orgID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var txns []CreditTransaction
	for rows.Next() {
		var t CreditTransaction
		if err := rows.Scan(&t.ID, &t.OrgID, &t.AmountCents, &t.BalanceAfterCents,
			&t.Type, &t.Description, &t.StripePaymentIntentID, &t.StripeCheckoutSessionID,
			&t.CreatedAt); err != nil {
			return nil, err
		}
		txns = append(txns, t)
	}
	return txns, rows.Err()
}

// GetOrUpdateMonthlySpend returns the current monthly spend for an org.
// If addCents > 0, it atomically adds to the total. Month should be the first day of the month.
func (s *Store) GetOrUpdateMonthlySpend(ctx context.Context, orgID uuid.UUID, month time.Time, addCents int) (int, error) {
	// Normalize to first of month
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)

	var total int
	if addCents > 0 {
		err := s.pool.QueryRow(ctx,
			`INSERT INTO monthly_spend (org_id, month, total_charged_cents)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (org_id, month)
			 DO UPDATE SET total_charged_cents = monthly_spend.total_charged_cents + $3
			 RETURNING total_charged_cents`,
			orgID, month, addCents).Scan(&total)
		return total, err
	}

	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT total_charged_cents FROM monthly_spend WHERE org_id = $1 AND month = $2), 0)`,
		orgID, month).Scan(&total)
	return total, err
}

// UpdateOrgBillingSettings updates auto top-up and spending cap settings.
func (s *Store) UpdateOrgBillingSettings(ctx context.Context, orgID uuid.UUID, autoTopup bool, thresholdCents, amountCents int, capCents *int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET auto_topup_enabled = $2, auto_topup_threshold_cents = $3,
		        auto_topup_amount_cents = $4, monthly_spend_cap_cents = $5, updated_at = now()
		 WHERE id = $1`,
		orgID, autoTopup, thresholdCents, amountCents, capCents)
	return err
}

// SetStripeCustomerID sets the Stripe customer ID for an org.
func (s *Store) SetStripeCustomerID(ctx context.Context, orgID uuid.UUID, customerID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET stripe_customer_id = $2, updated_at = now() WHERE id = $1`,
		orgID, customerID)
	return err
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
