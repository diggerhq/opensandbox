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
		// Set the standard auth context fields too — dashboard handlers (and
		// many others) call auth.GetOrgID(c) / c.Get("user_id") rather than
		// digging out the cap-claims directly. internalCreateSandbox sets
		// these itself for back-compat (the original capTokenMiddleware did
		// not set them), but every other capTokenMiddleware-gated route
		// would 401 without this — including the /internal/dashboard/*
		// routes the api-edge proxies to.
		if orgID, perr := uuid.Parse(claims.OrgID); perr == nil {
			auth.SetOrgID(c, orgID)
		}
		if claims.UserID != nil {
			if uid, perr := uuid.Parse(*claims.UserID); perr == nil {
				auth.SetUserID(c, uid)
			}
		}
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

	// Ensure the cell-local orgs row exists. The edge is authoritative on
	// org identity (it just minted this cap-token), so we trust the claims
	// and lazily materialize the row here. Plan comes from the cap-token so
	// the worker's event resolver can tag usage_tick events correctly.
	if s.store != nil {
		if upErr := s.store.UpsertOrgFromCapToken(c.Request().Context(), orgID, claims.Plan); upErr != nil {
			// Non-fatal — the create may still succeed; downstream gates fall through
			// when the row is missing. Logged so ops can investigate persistent failures.
			c.Logger().Errorf("upsert org from cap-token: %v", upErr)
		}
		// Also upsert the user. sandbox_sessions.user_id has an FK to users(id),
		// so without this the session insert in createSandboxRemote silently
		// fails (the errors are discarded with `_, _ =`) and the sandbox ends
		// up only in D1 sandboxes_index — never in cell PG, so subsequent
		// exec/wake/etc requests return "sandbox not found".
		if claims.UserID != nil {
			if uid, err := uuid.Parse(*claims.UserID); err == nil {
				if upErr := s.store.UpsertUserFromCapToken(c.Request().Context(), uid, orgID); upErr != nil {
					c.Logger().Errorf("upsert user from cap-token: %v", upErr)
				}
			}
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

	// Resolve secret store binding (if cfg.SecretStore is set). The edge-first
	// path puts the user's POST /api/sandboxes body through this handler
	// verbatim, so any caller passing {"secretStore": "<name>"} expects it to
	// be resolved here. resolveSecretStoreInto consults the edge over HMAC
	// when s.edge is configured, decrypts entries with the shared key, and
	// populates cfg.SecretEnvs + cfg.EgressAllowlist. Pre-fix this was nil,
	// silently dropping the binding and leaving sandboxes without their
	// requested secrets.
	secretStoreID, err := s.resolveSecretStoreInto(c.Request().Context(), orgID, &cfg)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	// Reuse the normal remote-create path — picks a worker, dispatches via
	// gRPC, persists the session, writes the {sandboxID, token, status, ...}
	// response body.
	return s.createSandboxRemote(c, c.Request().Context(), cfg, orgID, true, secretStoreID)
}
