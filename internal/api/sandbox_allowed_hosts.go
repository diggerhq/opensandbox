package api

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
)

// AllowedHostsResponse is the shape returned by GET /api/sandboxes/:id/allowed-hosts.
//
// EgressAllowlist + PerSecretAllowedHosts represent the runtime view — the
// union of every layered store's allowlist, exactly as the secrets proxy
// enforces it. Most sandboxes have a single store and the union is trivially
// that one store's allowlist; forks that layer an additional store on top of
// an inherited one have BOTH stores' allowlists merged here.
//
// SecretStoreName is the "primary" (last/winning) store on the sandbox row —
// the one whose secrets shadow the base store on env-name collisions.
// BaseSecretStoreName is the inherited parent store from the fork chain;
// populated only when there's actual layering. Both empty = sandbox created
// without a secretStore (no per-store egress restriction enforced).
type AllowedHostsResponse struct {
	SandboxID             string              `json:"sandboxID"`
	SecretStoreName       string              `json:"secretStore,omitempty"`
	BaseSecretStoreName   string              `json:"baseSecretStore,omitempty"`
	EgressAllowlist       []string            `json:"egressAllowlist"`
	PerSecretAllowedHosts map[string][]string `json:"perSecretAllowedHosts"`
}

// getSandboxAllowedHosts handles GET /api/sandboxes/:id/allowed-hosts.
//
// Returns the egress allowlist + per-secret allowed hosts the sandbox's
// secrets proxy enforces. Useful for debugging "why is my outbound HTTP call
// being blocked" without having to cross-reference store config separately.
//
// For forks that layered a new secretStore on top of an inherited one, the
// response merges both stores' allowlists — the runtime proxy enforces the
// union, so this matches actual behavior. The primary store's secrets shadow
// the base store on env-name collisions.
//
// Auth: same as other /api/sandboxes/:id routes — PGAPIKeyMiddleware sets
// orgID on the context. Sandbox lookup is org-scoped to prevent cross-tenant
// reads via guessed sandbox IDs.
func (s *Server) getSandboxAllowedHosts(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	orgID, hasOrg := auth.GetOrgID(c)
	if !hasOrg {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	sandboxID := c.Param("id")
	ctx := c.Request().Context()

	primaryID, baseStoreName, err := s.store.GetSandboxStoreRefs(ctx, orgID, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}

	// Sandbox has neither a primary store nor an inherited base. Return an
	// empty (well-formed) response so callers always see the same shape.
	if primaryID == nil && baseStoreName == "" {
		return c.JSON(http.StatusOK, AllowedHostsResponse{
			SandboxID:             sandboxID,
			EgressAllowlist:       []string{},
			PerSecretAllowedHosts: map[string][]string{},
		})
	}

	resp := AllowedHostsResponse{
		SandboxID:             sandboxID,
		EgressAllowlist:       []string{},
		PerSecretAllowedHosts: map[string][]string{},
	}

	// Fetch base store first so primary's per-secret entries can shadow on
	// name collision (matches the runtime proxy: later layer wins for envs).
	if baseStoreName != "" {
		base, err := s.store.GetSecretStoreByName(ctx, orgID, baseStoreName)
		if err == nil {
			resp.BaseSecretStoreName = base.Name
			mergeStoreInto(ctx, s.store, base, &resp)
		}
		// Base store missing (deleted under us) is treated as a soft no-op
		// rather than 500 — proxy already snapshotted whatever it needs.
	}

	if primaryID != nil {
		primary, err := s.store.GetSecretStore(ctx, orgID, *primaryID)
		if err == nil {
			resp.SecretStoreName = primary.Name
			mergeStoreInto(ctx, s.store, primary, &resp)
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// mergeStoreInto folds one store's allowlist + per-secret restrictions into
// the running response. Egress hosts dedupe (preserving insertion order so
// base-store hosts appear before primary's additions). Per-secret entries
// with empty AllowedHosts are skipped — empty means "inherits store
// allowlist," and surfacing them as [] would falsely imply "no hosts allowed
// for this secret." Per-secret name collisions are last-write-wins, so the
// primary store (called second) shadows the base.
func mergeStoreInto(ctx context.Context, store *db.Store, ss *db.SecretStore, resp *AllowedHostsResponse) {
	// Build a set view of existing egress hosts so we can dedupe in O(1).
	existing := make(map[string]bool, len(resp.EgressAllowlist))
	for _, h := range resp.EgressAllowlist {
		existing[h] = true
	}
	for _, h := range ss.EgressAllowlist {
		if !existing[h] {
			existing[h] = true
			resp.EgressAllowlist = append(resp.EgressAllowlist, h)
		}
	}

	entries, err := store.ListSecretEntries(ctx, ss.ID)
	if err != nil {
		return
	}
	for _, e := range entries {
		if len(e.AllowedHosts) == 0 {
			continue
		}
		resp.PerSecretAllowedHosts[e.Name] = e.AllowedHosts
	}
}

