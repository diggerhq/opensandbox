package db

import (
	"context"
	"time"
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
