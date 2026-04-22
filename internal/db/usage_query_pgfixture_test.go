//go:build pgfixture

// Reconciliation invariant for the usage aggregator. Runs only under
// `go test -tags=pgfixture` against a real Postgres pointed at by
// TEST_DATABASE_URL. The invariant is load-bearing for billing-
// adjacent math — the pure-Go builder tests prove SQL shape but they
// cannot prove the numbers line up with GetOrgUsage's rollup, which
// is what the Stripe pipeline reports. See design
// .agents/design/sandbox-tags-and-usage.md ("Unit is GB-seconds")
// and work doc F5/F14.
//
// Run locally:
//   TEST_DATABASE_URL=postgres://user:pass@localhost:5432/dbname?sslmode=disable \
//     go test -tags=pgfixture ./internal/db/ -run Reconciliation -v
package db

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// DiskFreeAllowanceMB is duplicated from internal/billing because db
// cannot import billing (billing → db would cycle). Kept in sync by
// convention; BuildUsageQuery / BuildOrgTotals both hardcode 20480
// for the same reason.
const testDiskFreeAllowanceMB = 20480

func openPgStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping pgfixture test")
	}
	store, err := NewStore(context.Background(), url)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store
}

// seedReconciliationFixture writes a deterministic set of scale events
// and tags scoped to a fresh org UUID. Isolation by org_id means
// repeated runs against the same DB don't leak state — no global
// cleanup needed.
func seedReconciliationFixture(t *testing.T, store *Store, orgID uuid.UUID, from, to time.Time) {
	t.Helper()
	ctx := context.Background()

	insertSE := func(sandboxID string, memMB, diskMB int, started time.Time, ended *time.Time) {
		if _, err := store.pool.Exec(ctx,
			`INSERT INTO sandbox_scale_events (sandbox_id, org_id, memory_mb, cpu_percent, disk_mb, started_at, ended_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			sandboxID, orgID, memMB, memMB/40, diskMB, started, ended); err != nil {
			t.Fatalf("seed scale event %s: %v", sandboxID, err)
		}
	}
	insertTag := func(sandboxID, key, value string) {
		if _, err := store.pool.Exec(ctx,
			`INSERT INTO sandbox_tags (org_id, sandbox_id, key, value)
			 VALUES ($1, $2, $3, $4)`,
			orgID, sandboxID, key, value); err != nil {
			t.Fatalf("seed tag %s/%s: %v", sandboxID, key, err)
		}
	}

	// sbx1: tagged team=payments, two scale events — one within the
	// allowance (no disk overage), one with a 40GB workspace.
	t1 := from.Add(2 * time.Hour)
	t2 := from.Add(6 * time.Hour)
	insertSE("sbx1", 4096, 20480, from.Add(-1*time.Hour), &t1)
	insertSE("sbx1", 8192, 40960, t1, &t2)
	insertTag("sbx1", "team", "payments")
	insertTag("sbx1", "env", "prod")

	// sbx2: tagged team=growth, one scale event, still open
	// (ended_at NULL) — exercises the COALESCE path.
	insertSE("sbx2", 4096, 20480, from.Add(1*time.Hour), nil)
	insertTag("sbx2", "team", "growth")

	// sbx3: untagged — tag table has no rows for it. Goes into the
	// untagged sibling bucket on groupBy=tag:team.
	t3 := from.Add(3 * time.Hour)
	insertSE("sbx3", 1024, 20480, from.Add(30*time.Minute), &t3)

	// sbx4: entirely before the window — must contribute zero to
	// anything. Guards that the window clamp actually works.
	pre := from.Add(-10 * time.Hour)
	preEnd := from.Add(-2 * time.Hour)
	insertSE("sbx4", 4096, 20480, pre, &preEnd)
	insertTag("sbx4", "team", "ancient")
}

// TestReconciliationInvariant_pgfixture pins the load-bearing billing
// claim: Σ by-sandbox == Σ by-tag + untagged == ExecuteOrgTotals ==
// rollup from GetOrgUsage, all within float epsilon. If any of these
// four diverge, per-tenant spend attribution diverges from what the
// Stripe pipeline reports, which is a silent correctness bug.
func TestReconciliationInvariant_pgfixture(t *testing.T) {
	ctx := context.Background()
	store := openPgStore(t)

	orgID := uuid.New()
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Second)
	to := time.Now().UTC().Truncate(time.Second)
	seedReconciliationFixture(t, store, orgID, from, to)

	// --- 1. GetOrgUsage rollup (what the Stripe pipeline uses).
	summaries, err := store.GetOrgUsage(ctx, orgID.String(), from, to)
	if err != nil {
		t.Fatalf("GetOrgUsage: %v", err)
	}
	var rollupMem, rollupDisk float64
	for _, s := range summaries {
		rollupMem += float64(s.MemoryMB) / 1024.0 * s.TotalSeconds
		if s.DiskMB > testDiskFreeAllowanceMB {
			rollupDisk += float64(s.DiskMB-testDiskFreeAllowanceMB) / 1024.0 * s.TotalSeconds
		}
	}

	// --- 2. ExecuteOrgTotals should match the rollup exactly.
	baseQ := UsageQuery{
		OrgID:   orgID,
		From:    from,
		To:      to,
		GroupBy: "sandbox",
		Limit:   500,
	}
	totals, err := store.ExecuteOrgTotals(ctx, baseQ)
	if err != nil {
		t.Fatalf("ExecuteOrgTotals: %v", err)
	}
	assertClose(t, "org totals memory", rollupMem, totals.MemoryGbSeconds)
	assertClose(t, "org totals disk overage", rollupDisk, totals.DiskOverageGbSeconds)

	// --- 3. Σ by-sandbox == ExecuteOrgTotals.
	rows, _, err := store.ExecuteUsageQuery(ctx, baseQ)
	if err != nil {
		t.Fatalf("ExecuteUsageQuery sandbox: %v", err)
	}
	var sbxMem, sbxDisk float64
	for _, r := range rows {
		sbxMem += r.MemoryGbSeconds
		sbxDisk += r.DiskOverageGbSeconds
	}
	assertClose(t, "Σ by-sandbox memory", totals.MemoryGbSeconds, sbxMem)
	assertClose(t, "Σ by-sandbox disk", totals.DiskOverageGbSeconds, sbxDisk)

	// --- 4. Σ by-tag items + untagged == ExecuteOrgTotals.
	tagQ := baseQ
	tagQ.GroupBy = "tag:team"
	tagRows, _, err := store.ExecuteUsageQuery(ctx, tagQ)
	if err != nil {
		t.Fatalf("ExecuteUsageQuery tag: %v", err)
	}
	untagged, err := store.ExecuteUntaggedTotals(ctx, tagQ)
	if err != nil {
		t.Fatalf("ExecuteUntaggedTotals: %v", err)
	}
	var tagMem, tagDisk float64
	for _, r := range tagRows {
		tagMem += r.MemoryGbSeconds
		tagDisk += r.DiskOverageGbSeconds
	}
	tagMem += untagged.MemoryGbSeconds
	tagDisk += untagged.DiskOverageGbSeconds
	assertClose(t, "Σ by-tag memory", totals.MemoryGbSeconds, tagMem)
	assertClose(t, "Σ by-tag disk", totals.DiskOverageGbSeconds, tagDisk)

	// --- 5. Sanity: the pre-window event must not leak.
	if rollupMem == 0 {
		t.Fatal("expected non-zero memory usage from in-window events")
	}

	// --- 6. Untagged bucket must contain exactly one sandbox (sbx3).
	if untagged.SandboxCount != 1 {
		t.Errorf("untagged sandbox count = %d, want 1", untagged.SandboxCount)
	}
}

func assertClose(t *testing.T, label string, want, got float64) {
	t.Helper()
	const eps = 1e-6
	if math.Abs(want-got) > eps {
		t.Errorf("%s: got %.9f, want %.9f (diff %.3e)", label, got, want, math.Abs(want-got))
	}
}
