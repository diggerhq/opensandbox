package db

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func baseQuery() UsageQuery {
	return UsageQuery{
		OrgID:   uuid.New(),
		From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		GroupBy: "sandbox",
		Limit:   50,
	}
}

func TestBuildUsageQuery_GroupBySandbox(t *testing.T) {
	sql, args, err := BuildUsageQuery(baseQuery())
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	// Grouping col + aggregates.
	if !strings.Contains(sql, "e.sandbox_id AS sandbox_id") {
		t.Errorf("missing sandbox group key:\n%s", sql)
	}
	if !strings.Contains(sql, "SUM(e.memory_mb::float / 1024.0") {
		t.Errorf("missing memory-seconds SUM:\n%s", sql)
	}
	if !strings.Contains(sql, "GREATEST(e.disk_mb - 20480, 0)") {
		t.Errorf("missing disk overage formula:\n%s", sql)
	}
	if !strings.Contains(sql, "COUNT(DISTINCT e.sandbox_id)") {
		t.Errorf("missing sandbox count:\n%s", sql)
	}
	// Duration expression must mirror GetOrgUsage verbatim.
	if !strings.Contains(sql, "COALESCE(e.ended_at, LEAST(now(),") {
		t.Errorf("duration expression diverges from GetOrgUsage:\n%s", sql)
	}
	// Default sort is memory DESC.
	if !strings.Contains(sql, "ORDER BY memory_gb_seconds DESC") {
		t.Errorf("expected default sort memory DESC:\n%s", sql)
	}
	// Limit is arg, value is limit+1 so we can detect next page.
	if got := args[len(args)-1]; got != 51 {
		t.Errorf("expected limit+1=51 as last arg, got %v", got)
	}
}

func TestBuildUsageQuery_GroupByTag(t *testing.T) {
	q := baseQuery()
	q.GroupBy = "tag:team"
	sql, args, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	if !strings.Contains(sql, "LEFT JOIN sandbox_tags gt") {
		t.Errorf("missing tag-group join:\n%s", sql)
	}
	if !strings.Contains(sql, "gt.key =") {
		t.Errorf("missing tag-group key filter:\n%s", sql)
	}
	// Untagged rows excluded from items query.
	if !strings.Contains(sql, "gt.value IS NOT NULL") {
		t.Errorf("untagged should be excluded from items:\n%s", sql)
	}
	if !strings.Contains(sql, "gt.value AS tag_value") {
		t.Errorf("missing tag_value output column:\n%s", sql)
	}
	// tag_key bound as a pgx parameter, not inlined.
	var found bool
	for _, a := range args {
		if a == "team" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("tag key 'team' not bound as arg; got %v", args)
	}
}

func TestBuildUsageQuery_TagKeyWithColon(t *testing.T) {
	// Design rule: SplitN on first ':'. Key may contain ':'.
	q := baseQuery()
	q.GroupBy = "tag:team:payments"
	_, args, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	var found bool
	for _, a := range args {
		if a == "team:payments" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected literal 'team:payments' bound as tag key arg; got %v", args)
	}
}

func TestBuildUsageQuery_FilterValuePresent(t *testing.T) {
	q := baseQuery()
	q.Filters = []UsageFilter{{TagKey: "env", Values: []string{"prod"}}}
	sql, _, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	if !strings.Contains(sql, "INNER JOIN sandbox_tags ft0") {
		t.Errorf("expected INNER JOIN for value-present filter:\n%s", sql)
	}
	if !strings.Contains(sql, "ft0.value = ANY(") {
		t.Errorf("expected ANY() value match:\n%s", sql)
	}
}

func TestBuildUsageQuery_FilterKeyAbsent(t *testing.T) {
	q := baseQuery()
	q.Filters = []UsageFilter{{TagKey: "env", Values: nil}}
	sql, _, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	if !strings.Contains(sql, "LEFT JOIN sandbox_tags ft0") {
		t.Errorf("expected LEFT JOIN for key-absent filter:\n%s", sql)
	}
	if !strings.Contains(sql, "ft0.sandbox_id IS NULL") {
		t.Errorf("expected IS NULL guard:\n%s", sql)
	}
}

func TestBuildUsageQuery_SortDiskDesc(t *testing.T) {
	q := baseQuery()
	q.Sort = UsageSortByDiskOverageDesc
	sql, _, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	if !strings.Contains(sql, "ORDER BY disk_overage_gb_seconds DESC") {
		t.Errorf("expected disk sort:\n%s", sql)
	}
}

func TestBuildUsageQuery_CursorEmitsOuterWhere(t *testing.T) {
	q := baseQuery()
	q.Cursor = encodeCursor(1234.5, "sbx_abc")
	sql, _, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	if !strings.Contains(sql, ") sub WHERE") {
		t.Errorf("expected keyset predicate wrapped around subquery:\n%s", sql)
	}
	if !strings.Contains(sql, "memory_gb_seconds <") {
		t.Errorf("expected keyset predicate on memory_gb_seconds:\n%s", sql)
	}
}

func TestBuildUsageQuery_RejectsInvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*UsageQuery)
	}{
		{"bad groupBy", func(q *UsageQuery) { q.GroupBy = "region" }},
		{"empty tag key", func(q *UsageQuery) { q.GroupBy = "tag:" }},
		{"window too large", func(q *UsageQuery) { q.To = q.From.Add(91 * 24 * time.Hour) }},
		{"to before from", func(q *UsageQuery) { q.To = q.From.Add(-time.Hour) }},
		{"limit too big", func(q *UsageQuery) { q.Limit = 501 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := baseQuery()
			tc.mut(&q)
			if _, _, err := BuildUsageQuery(q); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := encodeCursor(42.125, "sbx_abc")
	if c == "" {
		t.Fatal("empty cursor")
	}
	p, err := decodeCursor(c)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.V != 42.125 || p.T != "sbx_abc" {
		t.Errorf("cursor round-trip mismatch: %+v", p)
	}
	if _, err := decodeCursor("not-base64!"); err == nil {
		t.Error("expected error on malformed cursor")
	}
}

// Pins the tenancy boundary: every sandbox_tags join must scope on
// org_id, not just sandbox_id. Sandbox IDs are not schema-unique
// across orgs, so a sandbox_id-only join could alias tag state across
// tenants on collision. Migration 026 reflects the same decision in
// the PK.
func TestBuildUsageQuery_JoinsIncludeOrgID(t *testing.T) {
	q := baseQuery()
	q.GroupBy = "tag:team"
	q.Filters = []UsageFilter{
		{TagKey: "env", Values: []string{"prod"}},
		{TagKey: "region", Values: nil}, // key-absent
	}
	sql, _, err := BuildUsageQuery(q)
	if err != nil {
		t.Fatalf("BuildUsageQuery: %v", err)
	}
	// Every sandbox_tags alias — gt, ft0, ft1 — must constrain
	// ON org_id.
	expects := []string{
		"gt.org_id = e.org_id",
		"ft0.org_id = e.org_id",
		"ft1.org_id = e.org_id",
	}
	for _, want := range expects {
		if !strings.Contains(sql, want) {
			t.Errorf("missing org-scoped join predicate %q:\n%s", want, sql)
		}
	}
}

func TestBuildUntaggedTotals(t *testing.T) {
	q := baseQuery()
	q.GroupBy = "tag:env"
	sql, _, err := BuildUntaggedTotals(q)
	if err != nil {
		t.Fatalf("BuildUntaggedTotals: %v", err)
	}
	if !strings.Contains(sql, "LEFT JOIN sandbox_tags gt") {
		t.Errorf("expected grouping join:\n%s", sql)
	}
	if !strings.Contains(sql, "gt.sandbox_id IS NULL") {
		t.Errorf("expected untagged predicate:\n%s", sql)
	}

	// untagged is only defined for tag:<key>.
	q2 := baseQuery() // groupBy=sandbox
	if _, _, err := BuildUntaggedTotals(q2); err == nil {
		t.Error("expected error when GroupBy is not tag:<key>")
	}
}

func TestBuildOrgTotals(t *testing.T) {
	sql, _, err := BuildOrgTotals(baseQuery())
	if err != nil {
		t.Fatalf("BuildOrgTotals: %v", err)
	}
	if !strings.Contains(sql, "COALESCE(SUM(e.memory_mb::float") {
		t.Errorf("expected memory total:\n%s", sql)
	}
	if !strings.Contains(sql, "GREATEST(e.disk_mb - 20480, 0)") {
		t.Errorf("expected disk overage total:\n%s", sql)
	}
}

func TestParseGroupBy(t *testing.T) {
	cases := []struct {
		in       string
		wantKind string
		wantKey  string
		wantErr  bool
	}{
		{"sandbox", "sandbox", "", false},
		{"tag:team", "tag", "team", false},
		{"tag:team:payments", "tag", "team:payments", false},
		{"tag:", "", "", true},
		{"region", "", "", true},
		{"", "", "", true},
	}
	for _, tc := range cases {
		kind, key, err := parseGroupBy(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseGroupBy(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if kind != tc.wantKind || key != tc.wantKey {
			t.Errorf("parseGroupBy(%q) = (%q,%q), want (%q,%q)", tc.in, kind, key, tc.wantKind, tc.wantKey)
		}
	}
}
