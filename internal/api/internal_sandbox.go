package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// capClaimsKey is the echo.Context key under which capTokenMiddleware stashes
// the validated capability claims for downstream handlers.
const capClaimsKey = "cap_claims"

// capTokenMiddleware authenticates requests on the /internal/* group with a
// capability token (Authorization: Bearer <jwt>, HMAC-signed by the api-edge
// Worker with the shared session-JWT secret). It verifies the signature, the
// expiry, the issuer, and that the token's cell_id matches this control
// plane's cell — so a token minted for another cell can't be replayed here.
func (s *Server) capTokenMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		authHdr := c.Request().Header.Get("Authorization")
		if !strings.HasPrefix(authHdr, "Bearer ") {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing capability token"})
		}
		token := strings.TrimPrefix(authHdr, "Bearer ")
		claims, err := s.capTokenIssuer.ValidateCapabilityToken(token)
		if err != nil {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "invalid capability token: " + err.Error()})
		}
		if claims.CellID != s.cellID {
			return c.JSON(http.StatusForbidden, map[string]string{
				"error": "capability token is for cell " + claims.CellID + ", this is " + s.cellID,
			})
		}
		c.Set(capClaimsKey, claims)
		return next(c)
	}
}

// internalCreateSandbox is the edge→CP create path: the api-edge Worker has
// already authenticated the caller (API key or session JWT against D1) and
// chosen this cell; it hands us a capability token carrying the org identity.
// We trust org_id from the token, run the normal worker-dispatch path, and
// return the same body as POST /api/sandboxes. The edge records the resulting
// sandbox in D1's sandboxes_index — we don't touch any global tables.
func (s *Server) internalCreateSandbox(c echo.Context) error {
	claims, _ := c.Get(capClaimsKey).(*auth.CapabilityClaims)
	if claims == nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "missing capability claims"})
	}
	orgID, err := uuid.Parse(claims.OrgID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "capability token org_id is not a UUID"})
	}
	auth.SetOrgID(c, orgID)
	if claims.UserID != nil {
		if uid, uerr := uuid.Parse(*claims.UserID); uerr == nil {
			auth.SetUserID(c, uid)
		}
	}

	var cfg types.SandboxConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
	}

	// Same defaults the public POST /api/sandboxes applies.
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = 4096
		cfg.CpuCount = 1
	}
	if cfg.DiskMB == 0 {
		cfg.DiskMB = 20480
	}
	if err := types.ValidateResourceTier(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	// Reuse the normal remote-create path — picks a worker, dispatches via
	// gRPC, persists the session, writes the {sandboxID, token, status, ...}
	// response body. secretStoreID is nil here — cap-token callers don't
	// reference an org-uploaded secret store (that's a /api/sandboxes feature).
	return s.createSandboxRemote(c, c.Request().Context(), cfg, orgID, true, nil)
}
