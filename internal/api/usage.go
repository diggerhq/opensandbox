package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// Usage endpoints. Dimensions are data (`groupBy=sandbox` or
// `groupBy=tag:<key>`), not URL segments — design choice so adding
// status / template / region later is one string in groupBy, not a
// new route.

const (
	usageDefaultWindow = 30 * 24 * time.Hour
	usageHandlerTimeout = 10 * time.Second
)

// parseUsageQuery reads from / to / groupBy / filter[...] / sort /
// limit / cursor from the echo request. Returns a fully-validated
// UsageQuery suitable for handing to the store.
func parseUsageQuery(c echo.Context) (db.UsageQuery, error) {
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return db.UsageQuery{}, fmt.Errorf("org context required")
	}

	q := db.UsageQuery{OrgID: orgID}

	now := time.Now().UTC()
	if s := c.QueryParam("from"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, fmt.Errorf("`from` must be RFC3339: %w", err)
		}
		q.From = t
	} else {
		q.From = now.Add(-usageDefaultWindow)
	}
	if s := c.QueryParam("to"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, fmt.Errorf("`to` must be RFC3339: %w", err)
		}
		q.To = t
	} else {
		q.To = now
	}

	q.GroupBy = c.QueryParam("groupBy")
	if q.GroupBy == "" {
		return q, fmt.Errorf("`groupBy` is required")
	}

	switch s := c.QueryParam("sort"); s {
	case "", "-memoryGbSeconds":
		q.Sort = db.UsageSortByMemoryDesc
	case "-diskOverageGbSeconds":
		q.Sort = db.UsageSortByDiskOverageDesc
	default:
		return q, fmt.Errorf("unsupported sort %q", s)
	}

	q.Cursor = c.QueryParam("cursor")

	if s := c.QueryParam("limit"); s != "" {
		var lim int
		if _, err := fmt.Sscanf(s, "%d", &lim); err != nil || lim <= 0 {
			return q, fmt.Errorf("`limit` must be a positive integer")
		}
		q.Limit = lim
	} else {
		q.Limit = 50
	}

	// filter[tag:<key>]=v1,v2 — repeatable.  filter[tag:<key>]= (empty)
	// means the sandbox lacks that tag key.
	for rawKey, vals := range c.QueryParams() {
		if !strings.HasPrefix(rawKey, "filter[") || !strings.HasSuffix(rawKey, "]") {
			continue
		}
		dim := strings.TrimSuffix(strings.TrimPrefix(rawKey, "filter["), "]")
		if !strings.HasPrefix(dim, "tag:") {
			return q, fmt.Errorf("filter dimension %q not supported (only tag:<key> in v1)", dim)
		}
		tagKey := strings.TrimPrefix(dim, "tag:")
		if tagKey == "" {
			return q, fmt.Errorf("filter tag key is empty")
		}
		f := db.UsageFilter{TagKey: tagKey}
		raw := ""
		if len(vals) > 0 {
			raw = vals[0]
		}
		if raw == "" {
			f.Values = nil // key-absent
		} else {
			for _, v := range strings.Split(raw, ",") {
				v = strings.TrimSpace(v)
				if v != "" {
					f.Values = append(f.Values, v)
				}
			}
		}
		q.Filters = append(q.Filters, f)
	}

	return q, nil
}

// getUsage → GET /api/usage
func (s *Server) getUsage(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	q, err := parseUsageQuery(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), usageHandlerTimeout)
	defer cancel()

	// Primary items query. Validation (window size, limit) lives in
	// BuildUsageQuery — surface its errors as 400.
	rows, nextCursor, err := s.store.ExecuteUsageQuery(ctx, q)
	if err != nil {
		if isUserInputError(err) {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	totals, err := s.store.ExecuteOrgTotals(ctx, q)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	resp := map[string]interface{}{
		"from":    q.From.UTC().Format(time.RFC3339),
		"to":      q.To.UTC().Format(time.RFC3339),
		"groupBy": q.GroupBy,
		"total": map[string]float64{
			"memoryGbSeconds":      totals.MemoryGbSeconds,
			"diskOverageGbSeconds": totals.DiskOverageGbSeconds,
		},
		"nextCursor": nullableString(nextCursor),
	}

	if strings.HasPrefix(q.GroupBy, "tag:") {
		untagged, err := s.store.ExecuteUntaggedTotals(ctx, q)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		resp["untagged"] = map[string]interface{}{
			"memoryGbSeconds":      untagged.MemoryGbSeconds,
			"diskOverageGbSeconds": untagged.DiskOverageGbSeconds,
			"sandboxCount":         untagged.SandboxCount,
		}

		tagKey := strings.SplitN(q.GroupBy, ":", 2)[1]
		items := make([]map[string]interface{}, 0, len(rows))
		for _, r := range rows {
			items = append(items, map[string]interface{}{
				"tagKey":               tagKey,
				"tagValue":             r.TagValue,
				"memoryGbSeconds":      r.MemoryGbSeconds,
				"diskOverageGbSeconds": r.DiskOverageGbSeconds,
				"sandboxCount":         r.SandboxCount,
			})
		}
		resp["items"] = items
		return c.JSON(http.StatusOK, resp)
	}

	// groupBy=sandbox — hydrate alias/status/tags on each row.
	items, err := s.hydrateSandboxUsageItems(ctx, rows)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	resp["items"] = items
	return c.JSON(http.StatusOK, resp)
}

// hydrateSandboxUsageItems enriches the minimal scale-event rows with
// fields the handler is responsible for: alias (from
// sandbox_sessions.config JSONB — design F1), status, tag set,
// tagsLastUpdatedAt.
func (s *Server) hydrateSandboxUsageItems(ctx context.Context, rows []db.UsageRow) ([]map[string]interface{}, error) {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.SandboxID)
	}
	// Single batched tag fetch.
	tagSets, err := s.store.GetSandboxTagsMulti(ctx, ids)
	if err != nil {
		return nil, err
	}

	// Per-sandbox session lookup for alias + status. The store doesn't
	// yet have a batched-by-id variant; N+1 is tolerable in v1 given
	// the 500-row limit and the 10s handler budget. If this shows up
	// hot, promote to a batched GetSandboxSessionsMulti.
	out := make([]map[string]interface{}, 0, len(rows))
	for _, r := range rows {
		item := map[string]interface{}{
			"sandboxId":            r.SandboxID,
			"memoryGbSeconds":      r.MemoryGbSeconds,
			"diskOverageGbSeconds": r.DiskOverageGbSeconds,
		}
		if sess, err := s.store.GetSandboxSession(ctx, r.SandboxID); err == nil {
			item["status"] = sess.Status
			if alias := aliasFromConfig(sess.Config); alias != "" {
				item["alias"] = alias
			}
		}
		set := tagSets[r.SandboxID]
		if set.Tags == nil {
			set.Tags = map[string]string{}
		}
		item["tags"] = set.Tags
		if set.LastUpdatedAt != nil {
			item["tagsLastUpdatedAt"] = set.LastUpdatedAt.UTC().Format(time.RFC3339)
		} else {
			item["tagsLastUpdatedAt"] = nil
		}
		out = append(out, item)
	}
	return out, nil
}

// aliasFromConfig extracts `alias` from a sandbox session's JSONB
// config. The field is set by the client at create time (see
// pkg/types.SandboxConfig.Alias) and persisted as-is. Returns empty
// when absent — callers render nothing rather than "null."
func aliasFromConfig(cfg json.RawMessage) string {
	if len(cfg) == 0 {
		return ""
	}
	var v struct {
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(cfg, &v); err != nil {
		return ""
	}
	return v.Alias
}

// listTags → GET /api/tags
func (s *Server) listTags(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	stats, err := s.store.ListOrgTagKeys(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	keys := make([]map[string]interface{}, 0, len(stats))
	for _, k := range stats {
		keys = append(keys, map[string]interface{}{
			"key":          k.Key,
			"sandboxCount": k.SandboxCount,
			"valueCount":   k.ValueCount,
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"keys": keys})
}

// getSandboxUsage → GET /api/sandboxes/:id/usage
func (s *Server) getSandboxUsage(c echo.Context) error {
	sandboxID := c.Param("id")
	if err := s.ownsSandbox(c, sandboxID); err != nil {
		return err
	}

	orgID, _ := auth.GetOrgID(c)

	now := time.Now().UTC()
	from, to := now.Add(-usageDefaultWindow), now
	if s := c.QueryParam("from"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "`from` must be RFC3339"})
		}
		from = t
	}
	if s := c.QueryParam("to"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "`to` must be RFC3339"})
		}
		to = t
	}
	if to.Sub(from) > 90*24*time.Hour {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "query window must be <= 90 days"})
	}

	ctx, cancel := context.WithTimeout(c.Request().Context(), usageHandlerTimeout)
	defer cancel()

	mem, disk, firstStarted, lastEnded, err := s.store.SandboxUsageWindow(ctx, orgID, sandboxID, from, to)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	sess, _ := s.store.GetSandboxSession(ctx, sandboxID)
	tagSet, err := s.store.GetSandboxTags(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if tagSet.Tags == nil {
		tagSet.Tags = map[string]string{}
	}

	resp := map[string]interface{}{
		"sandboxId":            sandboxID,
		"from":                 from.UTC().Format(time.RFC3339),
		"to":                   to.UTC().Format(time.RFC3339),
		"memoryGbSeconds":      mem,
		"diskOverageGbSeconds": disk,
		"tags":                 tagSet.Tags,
	}
	if sess != nil {
		resp["status"] = sess.Status
		if alias := aliasFromConfig(sess.Config); alias != "" {
			resp["alias"] = alias
		}
	}
	if tagSet.LastUpdatedAt != nil {
		resp["tagsLastUpdatedAt"] = tagSet.LastUpdatedAt.UTC().Format(time.RFC3339)
	} else {
		resp["tagsLastUpdatedAt"] = nil
	}
	if firstStarted != nil {
		resp["firstStartedAt"] = firstStarted.UTC().Format(time.RFC3339)
	} else {
		resp["firstStartedAt"] = nil
	}
	if lastEnded != nil {
		resp["lastEndedAt"] = lastEnded.UTC().Format(time.RFC3339)
	} else {
		resp["lastEndedAt"] = nil
	}
	return c.JSON(http.StatusOK, resp)
}

// isUserInputError returns true when an error should be surfaced as
// 400 rather than 500. The query builder returns static strings for
// all validation failures.
func isUserInputError(err error) bool {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "limit must be"),
		strings.Contains(msg, "query window must be"),
		strings.Contains(msg, "`to` must be after"),
		strings.Contains(msg, "unsupported groupBy"),
		strings.Contains(msg, "groupBy tag"),
		strings.Contains(msg, "invalid cursor"):
		return true
	}
	return false
}

func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
