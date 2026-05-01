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
type AutoscaleSandbox struct {
	SandboxID   string
	WorkerID    string
	OrgID       string
	CurrentMB   int       // current memoryMB, parsed from sandbox_sessions.config
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
// CurrentMB is parsed from sandbox_sessions.config — the canonical source-
// of-truth for sandbox sizing. We don't track it as a separate column to
// avoid drift between the autoscale "what we think it is" and the worker's
// actual virtio-mem state.
func (s *Store) ListAutoscaleEnabled(ctx context.Context) ([]AutoscaleSandbox, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT sandbox_id, worker_id, org_id::text,
		       COALESCE((config->>'memoryMB')::int, 0) AS current_mb,
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
			&sb.CurrentMB, &sb.MinMB, &sb.MaxMB, &sb.LastEventAt); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// UpdateAutoscaleLastEvent stamps the cooldown anchor after a scale event
// (either direction). The autoscaler reads this before deciding whether
// the per-direction cooldown has elapsed.
func (s *Store) UpdateAutoscaleLastEvent(ctx context.Context, sandboxID string, t time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE sandbox_sessions SET autoscale_last_event_at = $1 WHERE sandbox_id = $2
	`, t, sandboxID)
	return err
}
