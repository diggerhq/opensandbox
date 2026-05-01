package db

import (
	"context"
	"fmt"
	"time"
)

// AutoscaleSandbox is the per-sandbox row the autoscaler loop reads each
// tick. Defined here (not in internal/controlplane) so the db package owns
// its own row type and the autoscaler imports from db rather than the
// other way around — avoids the controlplane↔db import cycle.
//
// CurrentMB is intentionally NOT set from sandbox_sessions.config: that
// column is the original creation memory and never updates after a scale
// event (manual or autoscale). The autoscaler populates CurrentMB from the
// live worker heartbeat (registry.SandboxStats.MemLimit) before deciding
// scale targets — that's the only authoritative current size.
type AutoscaleSandbox struct {
	SandboxID   string
	WorkerID    string
	OrgID       string
	CurrentMB   int       // populated by autoscaler from registry stats, NOT from DB
	MinMB       int       // user-configured floor (0 = unset)
	MaxMB       int       // user-configured ceiling (0 = unset)
	LastEventAt time.Time // zero if never scaled
}

// SetSandboxAutoscale enables/disables autoscale for a sandbox and writes
// the min/max bounds. Caller is responsible for validating min/max are
// allowed memory tiers (see types.AllowedResourceTiers).
func (s *Store) SetSandboxAutoscale(ctx context.Context, sandboxID string, enabled bool, minMB, maxMB int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sandbox_sessions
		SET autoscale_enabled = $1,
		    autoscale_min_mb  = NULLIF($2, 0),
		    autoscale_max_mb  = NULLIF($3, 0)
		WHERE sandbox_id = $4
	`, enabled, minMB, maxMB, sandboxID)
	if err != nil {
		return fmt.Errorf("set autoscale on %s: %w", sandboxID, err)
	}
	return nil
}

// GetSandboxAutoscale returns the autoscale config for a sandbox.
func (s *Store) GetSandboxAutoscale(ctx context.Context, sandboxID string) (enabled bool, minMB, maxMB int, err error) {
	var minVal, maxVal *int
	err = s.pool.QueryRow(ctx, `
		SELECT autoscale_enabled, autoscale_min_mb, autoscale_max_mb
		FROM sandbox_sessions WHERE sandbox_id = $1
	`, sandboxID).Scan(&enabled, &minVal, &maxVal)
	if err != nil {
		return false, 0, 0, err
	}
	if minVal != nil {
		minMB = *minVal
	}
	if maxVal != nil {
		maxMB = *maxVal
	}
	return enabled, minMB, maxMB, nil
}

// ListAutoscaleEnabled returns running sandboxes with autoscale_enabled=true.
// The partial index added in migration 032 keeps this scan cheap regardless
// of total session count.
//
// CurrentMB is left at zero — populated by the caller from live heartbeat
// stats. See the AutoscaleSandbox doc comment.
func (s *Store) ListAutoscaleEnabled(ctx context.Context) ([]AutoscaleSandbox, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sandbox_id, worker_id, org_id::text,
		       COALESCE(autoscale_min_mb, 0)            AS min_mb,
		       COALESCE(autoscale_max_mb, 0)            AS max_mb,
		       COALESCE(autoscale_last_event_at, '1970-01-01'::timestamptz) AS last_event_at
		FROM sandbox_sessions
		WHERE autoscale_enabled = TRUE AND status = 'running'
	`)
	if err != nil {
		return nil, fmt.Errorf("list autoscale-enabled: %w", err)
	}
	defer rows.Close()

	var out []AutoscaleSandbox
	for rows.Next() {
		var sb AutoscaleSandbox
		if err := rows.Scan(&sb.SandboxID, &sb.WorkerID, &sb.OrgID,
			&sb.MinMB, &sb.MaxMB, &sb.LastEventAt); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// ClaimAutoscaleEvent atomically claims the right to apply a scale event for
// this sandbox. Performs a CAS on (autoscale_enabled, autoscale_last_event_at):
//
//   - autoscale_enabled must still be TRUE — guards against a manual /scale
//     call (which sets enabled=false) racing with an in-flight autoscaler tick.
//   - autoscale_last_event_at must equal the value the caller observed —
//     guards against two control planes both deciding to scale on the same
//     pre-event row.
//
// If either guard fails, RowsAffected is 0 and the caller MUST NOT proceed
// with the scale: another writer either disabled autoscale or already
// claimed the event. On success the cooldown anchor is updated to `now`,
// so a subsequent gRPC failure simply means we'll retry on the next cooldown
// boundary rather than retry-storming.
//
// `prev` is the LastEventAt value the caller read from ListAutoscaleEnabled
// (zero-valued for never-scaled sandboxes — handled via IS NOT DISTINCT FROM).
func (s *Store) ClaimAutoscaleEvent(ctx context.Context, sandboxID string, prev, now time.Time) (bool, error) {
	// Treat the seeded sentinel from ListAutoscaleEnabled (1970-01-01) as
	// "never scaled" so the CAS matches the actual NULL stored on the row.
	var prevArg interface{} = prev
	if prev.Year() <= 1970 {
		prevArg = nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE sandbox_sessions
		SET autoscale_last_event_at = $1
		WHERE sandbox_id = $2
		  AND autoscale_enabled = TRUE
		  AND autoscale_last_event_at IS NOT DISTINCT FROM $3
	`, now, sandboxID, prevArg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
