package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
)

// Tag hydration helpers for the GET /sandboxes* response additions.
// All four read paths (local/remote × get/list) need to surface
// `tags` + `tagsLastUpdatedAt` — these helpers factor out the
// marshal-to-map + merge step so each handler stays one line.
//
// Fail-soft: if the store isn't configured or the tag query errors,
// the sandbox response is returned as-is rather than 500. Tags are
// purely additive and should never break the primary sandbox read.
//
// Every call takes orgID so the (org_id, sandbox_id) lookup scope
// from migration 026 is preserved end-to-end.

// mergeTagsInto mutates a response map, adding `tags` (always, even
// if empty) and `tagsLastUpdatedAt` (nil when the sandbox has no
// tags). Intended for the remote paths, which already build maps.
func (s *Server) mergeTagsInto(ctx context.Context, orgID uuid.UUID, resp map[string]interface{}, sandboxID string) {
	if s.store == nil {
		return
	}
	set, err := s.store.GetSandboxTags(ctx, orgID, sandboxID)
	if err != nil {
		return
	}
	if set.Tags == nil {
		set.Tags = map[string]string{}
	}
	resp["tags"] = set.Tags
	if set.LastUpdatedAt != nil {
		resp["tagsLastUpdatedAt"] = set.LastUpdatedAt.UTC().Format(time.RFC3339)
	} else {
		resp["tagsLastUpdatedAt"] = nil
	}
}

// withTagsHydrated takes an arbitrary sandbox value (as returned by
// the manager in local mode) and returns either the original value
// unchanged (no store, no tags) or a generic map with tag fields
// merged in. Marshalling through JSON preserves whatever fields the
// sandbox type emits without binding this package to its shape.
func (s *Server) withTagsHydrated(c echo.Context, sb interface{}, sandboxID string) interface{} {
	if s.store == nil {
		return sb
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return sb
	}
	buf, err := json.Marshal(sb)
	if err != nil {
		return sb
	}
	m := map[string]interface{}{}
	if err := json.Unmarshal(buf, &m); err != nil {
		return sb
	}
	s.mergeTagsInto(c.Request().Context(), orgID, m, sandboxID)
	return m
}

// withTagsHydratedList is the list-response analogue. One batched
// tag fetch for the whole page; per-item lookup inside the loop.
func (s *Server) withTagsHydratedList(c echo.Context, sandboxes interface{}) interface{} {
	if s.store == nil {
		return sandboxes
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return sandboxes
	}
	buf, err := json.Marshal(sandboxes)
	if err != nil {
		return sandboxes
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(buf, &arr); err != nil {
		return sandboxes
	}

	ids := make([]string, 0, len(arr))
	for _, m := range arr {
		if id, ok := m["sandboxID"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	sets, err := s.store.GetSandboxTagsMulti(c.Request().Context(), orgID, ids)
	if err != nil {
		return sandboxes
	}
	for _, m := range arr {
		sid, _ := m["sandboxID"].(string)
		set := sets[sid]
		if set.Tags == nil {
			set.Tags = map[string]string{}
		}
		m["tags"] = set.Tags
		if set.LastUpdatedAt != nil {
			m["tagsLastUpdatedAt"] = set.LastUpdatedAt.UTC().Format(time.RFC3339)
		} else {
			m["tagsLastUpdatedAt"] = nil
		}
	}
	return arr
}

