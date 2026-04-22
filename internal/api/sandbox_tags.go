package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
)

// Sandbox tag endpoints. All ownership checks read sandbox_sessions —
// the sandbox_tags PK is (sandbox_id, key) without org_id, so the
// handler is the enforcement point that prevents cross-tenant tag
// writes. Never bypass that lookup.

const (
	maxTagsPerSandbox = 50
	maxTagKeyLen      = 128
	maxTagValueLen    = 256
	reservedKeyPrefix = "oc:"
)

// Key charset: alphanumerics, underscore, period, hyphen, colon (for
// user namespacing like "team:payments"). `:` parses unambiguously on
// the filter/groupBy side via SplitN on the first `:`; see
// internal/db/usage_query.go.
var tagKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.\-:]+$`)

func validateTags(tags map[string]string) error {
	if len(tags) > maxTagsPerSandbox {
		return fmt.Errorf("max %d tags per sandbox, got %d", maxTagsPerSandbox, len(tags))
	}
	for k, v := range tags {
		if len(k) == 0 || len(k) > maxTagKeyLen {
			return fmt.Errorf("tag key %q: length must be 1..%d", k, maxTagKeyLen)
		}
		if !tagKeyRe.MatchString(k) {
			return fmt.Errorf("tag key %q contains invalid characters", k)
		}
		if strings.HasPrefix(k, reservedKeyPrefix) {
			return fmt.Errorf("tag key prefix %q is reserved", reservedKeyPrefix)
		}
		if len(v) > maxTagValueLen {
			return fmt.Errorf("tag value for %q: length must be <= %d", k, maxTagValueLen)
		}
	}
	return nil
}

// ownsSandbox returns a typed 404 when the sandbox is missing and a
// typed 403 when it belongs to another org. It does not leak the
// existence of sandboxes across orgs — both cases read 404.
func (s *Server) ownsSandbox(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}
	sess, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil || sess.OrgID != orgID {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	return nil
}

type tagsResponse struct {
	Tags              map[string]string `json:"tags"`
	TagsLastUpdatedAt *string           `json:"tagsLastUpdatedAt"`
}

func tagsResponseFromSet(tags map[string]string, lastUpdated *string) tagsResponse {
	if tags == nil {
		tags = map[string]string{}
	}
	return tagsResponse{Tags: tags, TagsLastUpdatedAt: lastUpdated}
}

// getSandboxTags → GET /api/sandboxes/:id/tags
func (s *Server) getSandboxTags(c echo.Context) error {
	sandboxID := c.Param("id")
	if err := s.ownsSandbox(c, sandboxID); err != nil {
		return err
	}
	set, err := s.store.GetSandboxTags(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	var ts *string
	if set.LastUpdatedAt != nil {
		v := set.LastUpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		ts = &v
	}
	return c.JSON(http.StatusOK, tagsResponseFromSet(set.Tags, ts))
}

// putSandboxTags → PUT /api/sandboxes/:id/tags — full replace.
func (s *Server) putSandboxTags(c echo.Context) error {
	sandboxID := c.Param("id")
	if err := s.ownsSandbox(c, sandboxID); err != nil {
		return err
	}

	// Bind as flat map; reject nested values explicitly by decoding
	// into map[string]json.RawMessage and asserting each is a JSON
	// string.
	var raw map[string]json.RawMessage
	if err := c.Bind(&raw); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "request body must be a flat { key: string } map",
		})
	}
	tags := map[string]string{}
	for k, rv := range raw {
		var v string
		if err := json.Unmarshal(rv, &v); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("tag %q: value must be a string", k),
			})
		}
		tags[k] = v
	}
	if err := validateTags(tags); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	orgID, _ := auth.GetOrgID(c)
	if err := s.store.ReplaceSandboxTags(c.Request().Context(), orgID, sandboxID, tags); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	// Re-read to return the canonical state + fresh tagsLastUpdatedAt.
	set, err := s.store.GetSandboxTags(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	var ts *string
	if set.LastUpdatedAt != nil {
		v := set.LastUpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00")
		ts = &v
	}
	return c.JSON(http.StatusOK, tagsResponseFromSet(set.Tags, ts))
}
