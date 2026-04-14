package db

import (
	"context"
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
	DiskMB    int        `json:"diskMB"`
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
// diskMB is the workspace disk size at this point — pass 0 to inherit from the
// most recent scale event (disk doesn't change at runtime).
func (s *Store) RecordScaleEvent(ctx context.Context, sandboxID, orgID string, memoryMB, cpuPct, diskMB int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if diskMB <= 0 {
		// Inherit from the most recent open or closed event for this sandbox.
		var prev int
		err = tx.QueryRow(ctx,
			`SELECT disk_mb FROM sandbox_scale_events
			 WHERE sandbox_id = $1
			 ORDER BY started_at DESC LIMIT 1`, sandboxID).Scan(&prev)
		if err == nil && prev > 0 {
			diskMB = prev
		} else {
			diskMB = 20480 // fall back to default 20GB
		}
	}

	// End the current open event
	_, err = tx.Exec(ctx,
		`UPDATE sandbox_scale_events SET ended_at = now()
		 WHERE sandbox_id = $1 AND ended_at IS NULL`, sandboxID)
	if err != nil {
		return err
	}

	// Start a new event
	_, err = tx.Exec(ctx,
		`INSERT INTO sandbox_scale_events (sandbox_id, org_id, memory_mb, cpu_percent, disk_mb)
		 VALUES ($1, $2, $3, $4, $5)`,
		sandboxID, orgID, memoryMB, cpuPct, diskMB)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// GetSandboxOrgID looks up the org ID for a sandbox from the sessions table.
func (s *Store) GetSandboxOrgID(ctx context.Context, sandboxID string) (string, error) {
	var orgID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT org_id FROM sandbox_sessions WHERE sandbox_id = $1 ORDER BY started_at DESC LIMIT 1`,
		sandboxID).Scan(&orgID)
	if err != nil {
		return "", err
	}
	return orgID.String(), nil
}

// SandboxOwner holds the org and user details for a sandbox session.
type SandboxOwner struct {
	OrgID        string
	UserID       string
	UserEmail    string
	WorkosUserID string
	WorkosOrgID  string
}

// GetSandboxOwner returns the org and user details for a sandbox. User fields
// may be empty if the session has no associated user (e.g. created with an
// org-level API key that isn't tied to a user).
func (s *Store) GetSandboxOwner(ctx context.Context, sandboxID string) (SandboxOwner, error) {
	var orgUUID uuid.UUID
	var userUUID *uuid.UUID
	var email, workosUserID, workosOrgID *string
	err := s.pool.QueryRow(ctx,
		`SELECT s.org_id, s.user_id, u.email, u.workos_user_id, o.workos_org_id
		   FROM sandbox_sessions s
		   LEFT JOIN users u ON u.id = s.user_id
		   LEFT JOIN orgs  o ON o.id = s.org_id
		  WHERE s.sandbox_id = $1
		  ORDER BY s.started_at DESC LIMIT 1`,
		sandboxID).Scan(&orgUUID, &userUUID, &email, &workosUserID, &workosOrgID)
	if err != nil {
		return SandboxOwner{}, err
	}
	owner := SandboxOwner{OrgID: orgUUID.String()}
	if userUUID != nil {
		owner.UserID = userUUID.String()
	}
	if email != nil {
		owner.UserEmail = *email
	}
	if workosUserID != nil {
		owner.WorkosUserID = *workosUserID
	}
	if workosOrgID != nil {
		owner.WorkosOrgID = *workosOrgID
	}
	return owner, nil
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

// OrgUsageSummary returns total billed seconds per (memory tier, disk size) for an org in a time range.
type OrgUsageSummary struct {
	MemoryMB     int     `json:"memoryMB"`
	CPUPercent   int     `json:"cpuPercent"`
	DiskMB       int     `json:"diskMB"`
	TotalSeconds float64 `json:"totalSeconds"`
}

// GetOrgUsage returns billing summary for an org.
func (s *Store) GetOrgUsage(ctx context.Context, orgID string, from, to time.Time) ([]OrgUsageSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT memory_mb, cpu_percent, disk_mb,
		       SUM(EXTRACT(EPOCH FROM (COALESCE(ended_at, LEAST(now(), $3)) - GREATEST(started_at, $2)))) as total_seconds
		FROM sandbox_scale_events
		WHERE org_id = $1
		  AND started_at < $3
		  AND (ended_at IS NULL OR ended_at > $2)
		GROUP BY memory_mb, cpu_percent, disk_mb
		ORDER BY memory_mb, disk_mb`,
		orgID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []OrgUsageSummary
	for rows.Next() {
		var s OrgUsageSummary
		if err := rows.Scan(&s.MemoryMB, &s.CPUPercent, &s.DiskMB, &s.TotalSeconds); err != nil {
			return nil, err
		}
		results = append(results, s)
	}
	return results, rows.Err()
}

// --- Stripe billing methods ---

// SetStripeCustomerID sets the Stripe customer ID for an org.
func (s *Store) SetStripeCustomerID(ctx context.Context, orgID uuid.UUID, customerID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET stripe_customer_id = $2, updated_at = now() WHERE id = $1`,
		orgID, customerID)
	return err
}

// SetStripeSubscriptionID sets the Stripe subscription ID for an org.
func (s *Store) SetStripeSubscriptionID(ctx context.Context, orgID uuid.UUID, subscriptionID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET stripe_subscription_id = $2, updated_at = now() WHERE id = $1`,
		orgID, subscriptionID)
	return err
}

// UpdateOrgPlan changes the org plan (e.g. "free" -> "pro").
func (s *Store) UpdateOrgPlan(ctx context.Context, orgID uuid.UUID, plan string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET plan = $2, updated_at = now() WHERE id = $1`,
		orgID, plan)
	return err
}

// UpdateLastUsageReportedAt updates the usage reporting watermark.
func (s *Store) UpdateLastUsageReportedAt(ctx context.Context, orgID uuid.UUID, t time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET last_usage_reported_at = $2 WHERE id = $1`,
		orgID, t)
	return err
}

// ListBillableOrgIDs returns org IDs with plan="pro" that have unreported usage:
// either a currently-running sandbox or a scale event that ended after the last report.
func (s *Store) ListBillableOrgIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT se.org_id
		 FROM sandbox_scale_events se
		 JOIN orgs o ON o.id = se.org_id
		 WHERE o.plan = 'pro'
		   AND (se.ended_at IS NULL OR se.ended_at > o.last_usage_reported_at)`)
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

// SubscriptionItem maps a memory tier to a Stripe subscription item ID.
type SubscriptionItem struct {
	OrgID                    uuid.UUID `json:"orgId"`
	MemoryMB                 int       `json:"memoryMB"`
	StripeSubscriptionItemID string    `json:"stripeSubscriptionItemId"`
}

// SaveSubscriptionItems inserts or updates the org's subscription item mapping.
func (s *Store) SaveSubscriptionItems(ctx context.Context, orgID uuid.UUID, items map[int]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	for memoryMB, itemID := range items {
		_, err := tx.Exec(ctx,
			`INSERT INTO org_subscription_items (org_id, memory_mb, stripe_subscription_item_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (org_id, memory_mb) DO UPDATE SET stripe_subscription_item_id = $3`,
			orgID, memoryMB, itemID)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// DeductCredits subtracts cents from an org's credit balance.
func (s *Store) DeductCredits(ctx context.Context, orgID uuid.UUID, amountCents int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET credit_balance_cents = credit_balance_cents - $2, updated_at = now() WHERE id = $1`,
		orgID, amountCents)
	return err
}

// AddCredits adds cents to an org's credit balance.
func (s *Store) AddCredits(ctx context.Context, orgID uuid.UUID, amountCents int) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE orgs SET credit_balance_cents = credit_balance_cents + $2, updated_at = now() WHERE id = $1`,
		orgID, amountCents)
	return err
}

// GetSubscriptionItems returns the subscription item mapping for an org.
func (s *Store) GetSubscriptionItems(ctx context.Context, orgID uuid.UUID) (map[int]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT memory_mb, stripe_subscription_item_id FROM org_subscription_items WHERE org_id = $1`,
		orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make(map[int]string)
	for rows.Next() {
		var memMB int
		var itemID string
		if err := rows.Scan(&memMB, &itemID); err != nil {
			return nil, err
		}
		items[memMB] = itemID
	}
	return items, rows.Err()
}
