package db

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Usage aggregation — one query builder, two call sites:
// GET /usage?groupBy=sandbox and GET /usage?groupBy=tag:<key>.
// The untagged bucket for the tag variant is queried separately
// (UntaggedBucket below) to keep cursor pagination clean; the design's
// response shape puts untagged in a sibling field, not in items.
//
// GB-second math mirrors GetOrgUsage and DiskOverageGBSeconds bit-for-bit
// so sums reconcile (see the reconciliation test in the design). Any
// change here requires an audit against those two, or billing will drift.

// UsageFilter is a single tag-filter clause.
// Empty Values means "tag key is absent on the sandbox."
type UsageFilter struct {
	TagKey string
	Values []string
}

// UsageSort selects the primary sort dimension. Secondary (tiebreaker)
// is always the grouping key ASC for cursor stability.
type UsageSort string

const (
	UsageSortByMemoryDesc       UsageSort = "-memoryGbSeconds"
	UsageSortByDiskOverageDesc  UsageSort = "-diskOverageGbSeconds"
)

// UsageQuery is the full input to the aggregator.
type UsageQuery struct {
	OrgID     uuid.UUID
	From      time.Time
	To        time.Time
	GroupBy   string // "sandbox" or "tag:<key>"
	Filters   []UsageFilter
	Sort      UsageSort
	Limit     int
	Cursor    string // opaque, empty for first page
}

// UsageRow is one aggregated bucket. Exactly one of SandboxID or TagValue
// is populated, depending on GroupBy.
type UsageRow struct {
	SandboxID            string  // groupBy=sandbox
	TagValue             string  // groupBy=tag:<key>
	MemoryGbSeconds      float64
	DiskOverageGbSeconds float64
	SandboxCount         int // always 1 for groupBy=sandbox (emitted for parity)
}

// UsageTotals reduces a page's rows to scalar totals for the response
// envelope. Computed by the handler across all rows (pre-pagination),
// using a separate query.
type UsageTotals struct {
	MemoryGbSeconds      float64
	DiskOverageGbSeconds float64
}

// parseGroupBy returns ("sandbox", "") or ("tag", key). Tag keys may
// contain `:` — we split on the first `:` only, per the design.
func parseGroupBy(groupBy string) (kind, tagKey string, err error) {
	if groupBy == "sandbox" {
		return "sandbox", "", nil
	}
	if strings.HasPrefix(groupBy, "tag:") {
		key := strings.TrimPrefix(groupBy, "tag:")
		if key == "" {
			return "", "", errors.New("groupBy tag:<key> requires a key")
		}
		return "tag", key, nil
	}
	return "", "", fmt.Errorf("unsupported groupBy %q", groupBy)
}

// cursorPayload is what we base64-JSON into the opaque cursor.
type cursorPayload struct {
	V float64 `json:"v"` // last sort value
	T string  `json:"t"` // last tiebreaker value (sandbox_id or tag value)
}

func encodeCursor(v float64, t string) string {
	b, _ := json.Marshal(cursorPayload{V: v, T: t})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursorPayload, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursorPayload{}, fmt.Errorf("invalid cursor: %w", err)
	}
	var p cursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return cursorPayload{}, fmt.Errorf("invalid cursor payload: %w", err)
	}
	return p, nil
}

// builderState tracks pgx parameter positions as we assemble SQL.
type builderState struct {
	args []any
}

func (b *builderState) arg(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// durationExpr mirrors the same idiom GetOrgUsage uses. Note: when an
// event's ended_at is *greater* than the query `to`, the COALESCE
// branch doesn't fire and duration overshoots the window by
// (ended_at - to). This is an inherited quirk in the existing billing
// pipeline; reproducing it exactly is load-bearing for the
// reconciliation invariant.
func (b *builderState) durationExpr(fromArg, toArg string) string {
	return fmt.Sprintf(
		`EXTRACT(EPOCH FROM (COALESCE(e.ended_at, LEAST(now(), %s)) - GREATEST(e.started_at, %s)))`,
		toArg, fromArg)
}

func (b *builderState) windowWhere(orgArg, fromArg, toArg string) string {
	return fmt.Sprintf(
		`e.org_id = %s AND e.started_at < %s AND (e.ended_at IS NULL OR e.ended_at > %s)`,
		orgArg, toArg, fromArg)
}

// filterJoins emits JOIN clauses that restrict `e.sandbox_id` to the
// filter set. All joins scope on (org_id, sandbox_id) — sandbox_tags
// is keyed on both (see migration 026) because sandbox IDs are not
// schema-unique across orgs.
//
// Non-empty value slice → INNER JOIN on matching rows.
// Empty slice → LEFT JOIN + IS NULL, meaning "key absent."
func (b *builderState) filterJoins(filters []UsageFilter) string {
	if len(filters) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, f := range filters {
		alias := fmt.Sprintf("ft%d", i)
		keyArg := b.arg(f.TagKey)
		if len(f.Values) == 0 {
			sb.WriteString(fmt.Sprintf(
				" LEFT JOIN sandbox_tags %s ON %s.org_id = e.org_id AND %s.sandbox_id = e.sandbox_id AND %s.key = %s",
				alias, alias, alias, alias, keyArg))
		} else {
			valArg := b.arg(f.Values)
			sb.WriteString(fmt.Sprintf(
				" INNER JOIN sandbox_tags %s ON %s.org_id = e.org_id AND %s.sandbox_id = e.sandbox_id AND %s.key = %s AND %s.value = ANY(%s)",
				alias, alias, alias, alias, keyArg, alias, valArg))
		}
	}
	return sb.String()
}

// filterExtraWhere adds the "IS NULL" clauses for key-absent filters.
func filterExtraWhere(filters []UsageFilter) string {
	var parts []string
	for i, f := range filters {
		if len(f.Values) == 0 {
			alias := fmt.Sprintf("ft%d", i)
			parts = append(parts, fmt.Sprintf("%s.sandbox_id IS NULL", alias))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " AND " + strings.Join(parts, " AND ")
}

// sortSQL returns the ORDER BY fragment and the SQL expression that
// yields the primary sort value (used for cursor encoding).
func sortSQL(sort UsageSort, tiebreakCol string) (orderBy, sortExpr string) {
	switch sort {
	case UsageSortByDiskOverageDesc:
		return fmt.Sprintf("disk_overage_gb_seconds DESC, %s ASC", tiebreakCol), "disk_overage_gb_seconds"
	default:
		// default: memory desc
		return fmt.Sprintf("memory_gb_seconds DESC, %s ASC", tiebreakCol), "memory_gb_seconds"
	}
}

// BuildUsageQuery is the pure entry point — returns SQL + args for the
// primary items query. Callers decode the rows as UsageRow values.
// untagged-bucket totals are a separate call; see BuildUntaggedTotals.
func BuildUsageQuery(q UsageQuery) (sqlText string, args []any, err error) {
	kind, tagKey, err := parseGroupBy(q.GroupBy)
	if err != nil {
		return "", nil, err
	}
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		return "", nil, errors.New("limit must be <= 500")
	}
	if q.To.Sub(q.From) > 90*24*time.Hour {
		return "", nil, errors.New("query window must be <= 90 days")
	}
	if !q.To.After(q.From) {
		return "", nil, errors.New("`to` must be after `from`")
	}

	b := &builderState{}
	orgArg := b.arg(q.OrgID)
	fromArg := b.arg(q.From)
	toArg := b.arg(q.To)

	dur := b.durationExpr(fromArg, toArg)
	window := b.windowWhere(orgArg, fromArg, toArg)
	joins := b.filterJoins(q.Filters)
	extra := filterExtraWhere(q.Filters)

	var groupKeyCol, groupKeyAlias, tiebreakCol string
	var groupJoin string

	switch kind {
	case "sandbox":
		groupKeyCol = "e.sandbox_id"
		groupKeyAlias = "sandbox_id"
		tiebreakCol = "sandbox_id"
	case "tag":
		tagKeyArg := b.arg(tagKey)
		groupJoin = fmt.Sprintf(
			" LEFT JOIN sandbox_tags gt ON gt.org_id = e.org_id AND gt.sandbox_id = e.sandbox_id AND gt.key = %s",
			tagKeyArg)
		// Only rows with a value go in items; untagged is queried separately.
		extra += " AND gt.value IS NOT NULL"
		groupKeyCol = "gt.value"
		groupKeyAlias = "tag_value"
		tiebreakCol = "tag_value"
	}

	orderBy, sortExpr := sortSQL(q.Sort, tiebreakCol)

	// Cursor filter goes in the outer WHERE — Postgres disallows output
	// aliases in HAVING, so we wrap the aggregating SELECT in a subquery
	// and apply the keyset predicate above it.
	var outerWhere string
	if q.Cursor != "" {
		cp, err := decodeCursor(q.Cursor)
		if err != nil {
			return "", nil, err
		}
		cvArg := b.arg(cp.V)
		ctArg := b.arg(cp.T)
		outerWhere = fmt.Sprintf(
			" WHERE (%s < %s) OR (%s = %s AND %s > %s)",
			sortExpr, cvArg, sortExpr, cvArg, tiebreakCol, ctArg)
	}

	limitArg := b.arg(q.Limit + 1) // +1 to detect next-page existence

	// 20480 == billing.DiskFreeAllowanceMB — can't import that here
	// (billing → db dependency would cycle). Kept in sync by convention.
	sqlText = fmt.Sprintf(`
SELECT %s, memory_gb_seconds, disk_overage_gb_seconds, sandbox_count
FROM (
  SELECT
    %s AS %s,
    SUM(e.memory_mb::float / 1024.0 * %s) AS memory_gb_seconds,
    SUM(GREATEST(e.disk_mb - %d, 0)::float / 1024.0 * %s) AS disk_overage_gb_seconds,
    COUNT(DISTINCT e.sandbox_id) AS sandbox_count
  FROM sandbox_scale_events e%s%s
  WHERE %s%s
  GROUP BY %s
) sub%s
ORDER BY %s
LIMIT %s`,
		tiebreakCol,
		groupKeyCol, groupKeyAlias,
		dur,
		20480, dur,
		joins, groupJoin,
		window, extra,
		groupKeyCol,
		outerWhere,
		orderBy,
		limitArg,
	)
	return sqlText, b.args, nil
}

// BuildUntaggedTotals returns SQL + args for the untagged sibling bucket
// on groupBy=tag:<key>. Sandboxes that lack the grouping key; their
// usage is summed into a single scalar result plus a sandbox count.
func BuildUntaggedTotals(q UsageQuery) (sqlText string, args []any, err error) {
	kind, tagKey, err := parseGroupBy(q.GroupBy)
	if err != nil {
		return "", nil, err
	}
	if kind != "tag" {
		return "", nil, errors.New("untagged totals only apply to groupBy=tag:<key>")
	}
	if q.To.Sub(q.From) > 90*24*time.Hour {
		return "", nil, errors.New("query window must be <= 90 days")
	}

	b := &builderState{}
	orgArg := b.arg(q.OrgID)
	fromArg := b.arg(q.From)
	toArg := b.arg(q.To)
	tagKeyArg := b.arg(tagKey)

	dur := b.durationExpr(fromArg, toArg)
	window := b.windowWhere(orgArg, fromArg, toArg)
	joins := b.filterJoins(q.Filters)
	extra := filterExtraWhere(q.Filters)

	sqlText = fmt.Sprintf(`
SELECT
  COALESCE(SUM(e.memory_mb::float / 1024.0 * %s), 0) AS memory_gb_seconds,
  COALESCE(SUM(GREATEST(e.disk_mb - %d, 0)::float / 1024.0 * %s), 0) AS disk_overage_gb_seconds,
  COUNT(DISTINCT e.sandbox_id) AS sandbox_count
FROM sandbox_scale_events e%s
LEFT JOIN sandbox_tags gt ON gt.org_id = e.org_id AND gt.sandbox_id = e.sandbox_id AND gt.key = %s
WHERE %s%s AND gt.sandbox_id IS NULL`,
		dur,
		20480, dur,
		joins,
		tagKeyArg,
		window, extra,
	)
	return sqlText, b.args, nil
}

// BuildOrgTotals returns SQL + args for the envelope `total` field —
// all scale events matching the filters in the window, summed as one
// row.
func BuildOrgTotals(q UsageQuery) (sqlText string, args []any, err error) {
	if q.To.Sub(q.From) > 90*24*time.Hour {
		return "", nil, errors.New("query window must be <= 90 days")
	}
	b := &builderState{}
	orgArg := b.arg(q.OrgID)
	fromArg := b.arg(q.From)
	toArg := b.arg(q.To)

	dur := b.durationExpr(fromArg, toArg)
	window := b.windowWhere(orgArg, fromArg, toArg)
	joins := b.filterJoins(q.Filters)
	extra := filterExtraWhere(q.Filters)

	sqlText = fmt.Sprintf(`
SELECT
  COALESCE(SUM(e.memory_mb::float / 1024.0 * %s), 0),
  COALESCE(SUM(GREATEST(e.disk_mb - %d, 0)::float / 1024.0 * %s), 0)
FROM sandbox_scale_events e%s
WHERE %s%s`,
		dur,
		20480, dur,
		joins,
		window, extra,
	)
	return sqlText, b.args, nil
}

// ExecuteUsageQuery runs the items query and returns rows + optional
// next-page cursor. GroupBy determines which fields on UsageRow are
// populated.
func (s *Store) ExecuteUsageQuery(ctx context.Context, q UsageQuery) (rows []UsageRow, nextCursor string, err error) {
	sqlText, args, err := BuildUsageQuery(q)
	if err != nil {
		return nil, "", err
	}
	pgRows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, "", fmt.Errorf("exec usage query: %w", err)
	}
	defer pgRows.Close()

	kind, _, _ := parseGroupBy(q.GroupBy)

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	var out []UsageRow
	for pgRows.Next() {
		var r UsageRow
		var groupKey string
		if err := pgRows.Scan(&groupKey, &r.MemoryGbSeconds, &r.DiskOverageGbSeconds, &r.SandboxCount); err != nil {
			return nil, "", err
		}
		if kind == "sandbox" {
			r.SandboxID = groupKey
		} else {
			r.TagValue = groupKey
		}
		out = append(out, r)
	}
	if err := pgRows.Err(); err != nil {
		return nil, "", err
	}

	if len(out) > limit {
		// We fetched limit+1; last row signals a next page exists.
		last := out[limit-1]
		out = out[:limit]
		var tb string
		var sortVal float64
		if kind == "sandbox" {
			tb = last.SandboxID
		} else {
			tb = last.TagValue
		}
		if q.Sort == UsageSortByDiskOverageDesc {
			sortVal = last.DiskOverageGbSeconds
		} else {
			sortVal = last.MemoryGbSeconds
		}
		nextCursor = encodeCursor(sortVal, tb)
	}
	return out, nextCursor, nil
}

// ExecuteUntaggedTotals returns the sibling untagged bucket for
// groupBy=tag:<key>. Returns zero-values when no sandboxes are untagged.
func (s *Store) ExecuteUntaggedTotals(ctx context.Context, q UsageQuery) (UsageRow, error) {
	sqlText, args, err := BuildUntaggedTotals(q)
	if err != nil {
		return UsageRow{}, err
	}
	var r UsageRow
	if err := s.pool.QueryRow(ctx, sqlText, args...).
		Scan(&r.MemoryGbSeconds, &r.DiskOverageGbSeconds, &r.SandboxCount); err != nil {
		return UsageRow{}, fmt.Errorf("exec untagged totals: %w", err)
	}
	return r, nil
}

// ExecuteOrgTotals returns the `total` envelope scalars.
func (s *Store) ExecuteOrgTotals(ctx context.Context, q UsageQuery) (UsageTotals, error) {
	sqlText, args, err := BuildOrgTotals(q)
	if err != nil {
		return UsageTotals{}, err
	}
	var t UsageTotals
	if err := s.pool.QueryRow(ctx, sqlText, args...).
		Scan(&t.MemoryGbSeconds, &t.DiskOverageGbSeconds); err != nil {
		return UsageTotals{}, fmt.Errorf("exec org totals: %w", err)
	}
	return t, nil
}

// SandboxUsageWindow returns the single-sandbox drilldown values for
// GET /sandboxes/{id}/usage: GB-second numbers plus first/last session
// timestamps clamped to the query window. Tag hydration is left to
// the handler (which calls GetSandboxTags).
//
// Timestamps are explicitly clamped into [from, to] (design F9):
// without the clamp, long-lived sandboxes leak endpoints outside the
// requested window. The SQL uses GREATEST(MIN(started_at), from) and
// LEAST(MAX(...), to).
func (s *Store) SandboxUsageWindow(ctx context.Context, orgID uuid.UUID, sandboxID string, from, to time.Time) (mem, disk float64, firstStartedAt *time.Time, lastEndedAt *time.Time, err error) {
	// Usage numbers from scale events, identical math to GetOrgUsage.
	err = s.pool.QueryRow(ctx, `
SELECT
  COALESCE(SUM(e.memory_mb::float / 1024.0 * EXTRACT(EPOCH FROM (COALESCE(e.ended_at, LEAST(now(), $3)) - GREATEST(e.started_at, $2)))), 0),
  COALESCE(SUM(GREATEST(e.disk_mb - 20480, 0)::float / 1024.0 * EXTRACT(EPOCH FROM (COALESCE(e.ended_at, LEAST(now(), $3)) - GREATEST(e.started_at, $2)))), 0)
FROM sandbox_scale_events e
WHERE e.org_id = $1 AND e.sandbox_id = $4
  AND e.started_at < $3 AND (e.ended_at IS NULL OR e.ended_at > $2)`,
		orgID, from, to, sandboxID).Scan(&mem, &disk)
	if err != nil {
		return 0, 0, nil, nil, fmt.Errorf("sandbox usage query: %w", err)
	}

	// Lifetime bounds from sandbox_sessions, clamped into [from, to].
	// Session-level MIN/MAX is more intuitive than scale-event edges
	// for "when did this sandbox exist" and is stable across scale
	// churn — see design D1.
	var minStart *time.Time
	var anyOpen bool
	var maxEnd *time.Time
	row := s.pool.QueryRow(ctx, `
SELECT
  GREATEST(MIN(started_at), $3),
  BOOL_OR(stopped_at IS NULL AND status IN ('running', 'hibernated')),
  LEAST(MAX(COALESCE(stopped_at, now())), $4)
FROM sandbox_sessions
WHERE org_id = $1 AND sandbox_id = $2
  AND started_at < $4
  AND (stopped_at IS NULL OR stopped_at > $3)`,
		orgID, sandboxID, from, to)
	if err := row.Scan(&minStart, &anyOpen, &maxEnd); err != nil {
		// No session in the window is OK — the sandbox may have been
		// created then torn down before `from`. Leave first/last nil.
		return mem, disk, nil, nil, nil
	}
	firstStartedAt = minStart
	if !anyOpen {
		lastEndedAt = maxEnd
	}
	return mem, disk, firstStartedAt, lastEndedAt, nil
}
