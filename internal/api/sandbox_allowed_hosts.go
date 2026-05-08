package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
)

// AllowedHostsResponse is the shape returned by GET /api/sandboxes/:id/allowed-hosts.
//
// The egress allowlist on the secret store is the cluster-wide set of hosts a
// sandbox in this store is allowed to reach. Each entry in
// PerSecretAllowedHosts is an additional restriction tied to one specific
// secret — when the sandbox uses that secret value via the secrets proxy, only
// those hosts are reachable for that request (intersected with EgressAllowlist
// if non-empty).
//
// SecretStoreName is empty (and the lists are empty) when the sandbox was
// created without a secretStore — in that case the sandbox has no per-store
// egress restriction (egress is governed by the platform-wide allowlist
// elsewhere, not exposed here).
type AllowedHostsResponse struct {
	SandboxID             string              `json:"sandboxID"`
	SecretStoreName       string              `json:"secretStore,omitempty"`
	EgressAllowlist       []string            `json:"egressAllowlist"`
	PerSecretAllowedHosts map[string][]string `json:"perSecretAllowedHosts"`
}

// getSandboxAllowedHosts handles GET /api/sandboxes/:id/allowed-hosts.
//
// Returns the egress allowlist + per-secret allowed hosts for a sandbox. Asks
// "what hosts can this sandbox reach via the secrets proxy?" — useful for
// debugging "why is my outbound HTTP call being blocked" without having to
// cross-reference the secret store config separately.
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

	// Verify the sandbox exists in this org and resolve its secret_store_id.
	storeID, err := s.store.GetSandboxSecretStoreID(ctx, orgID, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}

	// No store attached — return an empty (but well-formed) response so SDK
	// callers always get the same shape and don't have to special-case nil.
	if storeID == nil {
		return c.JSON(http.StatusOK, AllowedHostsResponse{
			SandboxID:             sandboxID,
			EgressAllowlist:       []string{},
			PerSecretAllowedHosts: map[string][]string{},
		})
	}

	store, err := s.store.GetSecretStore(ctx, orgID, *storeID)
	if err != nil {
		// Sandbox claims a store that's been deleted under it — surface as
		// empty rather than 500 since the data is consistent (sandbox runs
		// against whatever the proxy snapshotted at create time).
		return c.JSON(http.StatusOK, AllowedHostsResponse{
			SandboxID:             sandboxID,
			EgressAllowlist:       []string{},
			PerSecretAllowedHosts: map[string][]string{},
		})
	}

	entries, err := s.store.ListSecretEntries(ctx, *storeID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to list secret entries",
		})
	}

	perSecret := make(map[string][]string, len(entries))
	for _, e := range entries {
		// Only entries with explicit per-secret restrictions are interesting
		// here — entries with nil/empty AllowedHosts inherit the store's
		// egress allowlist, so showing them as empty is misleading.
		if len(e.AllowedHosts) > 0 {
			perSecret[e.Name] = e.AllowedHosts
		}
	}

	allowlist := store.EgressAllowlist
	if allowlist == nil {
		allowlist = []string{}
	}

	return c.JSON(http.StatusOK, AllowedHostsResponse{
		SandboxID:             sandboxID,
		SecretStoreName:       store.Name,
		EgressAllowlist:       allowlist,
		PerSecretAllowedHosts: perSecret,
	})
}
