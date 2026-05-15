// capacity-shadow-verify compares the phase-2 outbox against the legacy
// UsageReporter pipeline for orgs that have zero capacity reservations
// in the window. With reservedGb=0 the integration walk's per-tier
// overage equals legacy `tier_GB × total_seconds` — any divergence
// surfaces an allocator bug before phase 3 starts shipping outbox rows
// to Stripe.
//
// Usage:
//
//	DATABASE_URL=postgres://... \
//	    go run ./cmd/capacity-shadow-verify \
//	        --from=2026-04-26T00:00:00Z \
//	        --to=2026-04-27T00:00:00Z \
//	        [--org-id=<uuid>] [--epsilon=0.01]
//
// Both bounds must be UTC and 15-minute aligned. Org filter is optional
// — when omitted, every org with at least one outbox row in the window
// is checked.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	fromS := flag.String("from", "", "window start (RFC3339, UTC, 15-min aligned)")
	toS := flag.String("to", "", "window end (RFC3339, UTC, 15-min aligned)")
	orgFilter := flag.String("org-id", "", "limit comparison to one org id (optional)")
	epsilon := flag.Float64("epsilon", 0.01, "fractional delta tolerated before flagging a mismatch")
	flag.Parse()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	if *fromS == "" || *toS == "" {
		log.Fatal("--from and --to are required")
	}
	from, err := time.Parse(time.RFC3339, *fromS)
	if err != nil {
		log.Fatalf("parse --from: %v", err)
	}
	to, err := time.Parse(time.RFC3339, *toS)
	if err != nil {
		log.Fatalf("parse --to: %v", err)
	}
	if from.Truncate(15*time.Minute) != from.UTC() || to.Truncate(15*time.Minute) != to.UTC() {
		log.Fatal("--from and --to must be UTC and 15-minute aligned")
	}
	if !to.After(from) {
		log.Fatal("--to must be after --from")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	orgs, err := orgsToCheck(ctx, pool, from, to, *orgFilter)
	if err != nil {
		log.Fatalf("list orgs: %v", err)
	}
	if len(orgs) == 0 {
		log.Println("no orgs with outbox rows in window — nothing to verify")
		return
	}
	log.Printf("comparing %d org(s) over [%s, %s)", len(orgs), from.Format(time.RFC3339), to.Format(time.RFC3339))

	var mismatches int
	for _, orgID := range orgs {
		ok, err := checkOrg(ctx, pool, orgID, from, to, *epsilon)
		if err != nil {
			log.Printf("org %s: error: %v", orgID, err)
			mismatches++
			continue
		}
		if !ok {
			mismatches++
		}
	}
	if mismatches > 0 {
		log.Printf("RESULT: %d/%d org(s) had mismatches", mismatches, len(orgs))
		os.Exit(1)
	}
	log.Printf("RESULT: all %d org(s) match within epsilon=%.4f", len(orgs), *epsilon)
}

// orgsToCheck returns the set of orgs that have either outbox or legacy
// usage activity in the window, AND zero reservations in the window
// (so the comparison is apples-to-apples — outbox overage should equal
// legacy tier-GB-seconds).
func orgsToCheck(ctx context.Context, pool *pgxpool.Pool, from, to time.Time, orgFilter string) ([]uuid.UUID, error) {
	args := []any{from, to}
	filter := ""
	if orgFilter != "" {
		filter = `AND org_id = $3::uuid`
		args = append(args, orgFilter)
	}
	rows, err := pool.Query(ctx, `
		WITH activity AS (
			SELECT DISTINCT org_id FROM billable_events
			WHERE bucket_start >= $1 AND bucket_start < $2
			UNION
			SELECT DISTINCT org_id FROM sandbox_scale_events
			WHERE started_at < $2 AND (ended_at IS NULL OR ended_at > $1)
		),
		reserved AS (
			SELECT DISTINCT org_id FROM capacity_reservation_intervals
			WHERE starts_at >= $1 AND starts_at < $2
		)
		SELECT a.org_id
		FROM activity a
		WHERE a.org_id NOT IN (SELECT org_id FROM reserved)
		`+filter+`
		ORDER BY a.org_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func checkOrg(ctx context.Context, pool *pgxpool.Pool, orgID uuid.UUID, from, to time.Time, epsilon float64) (bool, error) {
	outbox, err := outboxTotals(ctx, pool, orgID, from, to)
	if err != nil {
		return false, fmt.Errorf("outbox: %w", err)
	}
	legacy, err := legacyTotals(ctx, pool, orgID, from, to)
	if err != nil {
		return false, fmt.Errorf("legacy: %w", err)
	}

	// Compare per tier and disk overage. With zero reservations, every
	// outbox overage row should match legacy tier-GB-seconds.
	allTiers := map[int]struct{}{}
	for tier := range outbox.tierGBSec {
		allTiers[tier] = struct{}{}
	}
	for tier := range legacy.tierGBSec {
		allTiers[tier] = struct{}{}
	}

	ok := true
	for tier := range allTiers {
		o := outbox.tierGBSec[tier]
		l := legacy.tierGBSec[tier]
		if !approxEq(o, l, epsilon) {
			fmt.Printf("MISMATCH org=%s tier=%dMB outbox=%.2f legacy=%.2f delta=%.2f (%.2f%%)\n",
				orgID, tier, o, l, o-l, percentDelta(o, l))
			ok = false
		}
	}
	if !approxEq(outbox.diskGBSec, legacy.diskGBSec, epsilon) {
		fmt.Printf("MISMATCH org=%s disk_overage outbox=%.2f legacy=%.2f delta=%.2f (%.2f%%)\n",
			orgID, outbox.diskGBSec, legacy.diskGBSec, outbox.diskGBSec-legacy.diskGBSec,
			percentDelta(outbox.diskGBSec, legacy.diskGBSec))
		ok = false
	}
	if ok {
		log.Printf("OK org=%s (%d tier(s), disk=%.2f)", orgID, len(allTiers), outbox.diskGBSec)
	}
	return ok, nil
}

type totals struct {
	tierGBSec map[int]float64 // memory_mb → GB-seconds (overage only)
	diskGBSec float64
}

func outboxTotals(ctx context.Context, pool *pgxpool.Pool, orgID uuid.UUID, from, to time.Time) (totals, error) {
	out := totals{tierGBSec: map[int]float64{}}
	rows, err := pool.Query(ctx, `
		SELECT event_type, memory_mb, COALESCE(SUM(gb_seconds), 0)::float8
		FROM billable_events
		WHERE org_id = $1 AND bucket_start >= $2 AND bucket_start < $3
		GROUP BY event_type, memory_mb
	`, orgID, from, to)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var mem int
		var gbs float64
		if err := rows.Scan(&typ, &mem, &gbs); err != nil {
			return out, err
		}
		switch typ {
		case "overage_usage":
			out.tierGBSec[mem] += gbs
		case "disk_overage_usage":
			out.diskGBSec += gbs
		}
		// reserved_usage rows are skipped: the comparison restricts to
		// orgs with zero reservations in the window.
	}
	return out, rows.Err()
}

// legacyTotals derives the same shape from sandbox_scale_events the way
// UsageReporter does: per-tier total seconds × tier_GB, disk overage as
// (disk_mb − 20480) × seconds / 1024. Window-clipped via GREATEST/LEAST
// to mirror GetOrgUsage.
func legacyTotals(ctx context.Context, pool *pgxpool.Pool, orgID uuid.UUID, from, to time.Time) (totals, error) {
	out := totals{tierGBSec: map[int]float64{}}
	rows, err := pool.Query(ctx, `
		SELECT memory_mb, disk_mb,
		       SUM(EXTRACT(EPOCH FROM (LEAST(COALESCE(ended_at, $3), $3) - GREATEST(started_at, $2))))::float8 AS secs
		FROM sandbox_scale_events
		WHERE org_id = $1 AND started_at < $3 AND (ended_at IS NULL OR ended_at > $2)
		GROUP BY memory_mb, disk_mb
	`, orgID, from, to)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var mem, disk int
		var secs float64
		if err := rows.Scan(&mem, &disk, &secs); err != nil {
			return out, err
		}
		gbs := float64(mem/1024) * secs
		out.tierGBSec[mem] += gbs
		if disk > 20480 {
			out.diskGBSec += float64(disk-20480) / 1024.0 * secs
		}
	}
	return out, rows.Err()
}

func approxEq(a, b, epsilon float64) bool {
	if a == 0 && b == 0 {
		return true
	}
	denom := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b)/denom <= epsilon
}

func percentDelta(a, b float64) float64 {
	if b == 0 {
		if a == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return (a - b) / b * 100.0
}
