package auth

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/db"
)

type contextKey string

const (
	// ContextKeyOrgID is the echo context key for the authenticated org ID.
	ContextKeyOrgID contextKey = "org_id"
	// ContextKeyUserID is the echo context key for the authenticated user ID.
	ContextKeyUserID contextKey = "user_id"
)

// SetOrgID stores the org ID in the echo context.
func SetOrgID(c echo.Context, orgID uuid.UUID) {
	c.Set(string(ContextKeyOrgID), orgID)
}

// GetOrgID retrieves the org ID from the echo context.
func GetOrgID(c echo.Context) (uuid.UUID, bool) {
	v := c.Get(string(ContextKeyOrgID))
	if v == nil {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

// SetUserID stores the user ID in the echo context.
func SetUserID(c echo.Context, userID uuid.UUID) {
	c.Set(string(ContextKeyUserID), userID)
}

// GetUserID retrieves the user ID from the echo context. Returns nil if not set.
func GetUserID(c echo.Context) *uuid.UUID {
	v := c.Get(string(ContextKeyUserID))
	if v == nil {
		return nil
	}
	id, ok := v.(uuid.UUID)
	if !ok {
		return nil
	}
	return &id
}

// PGAPIKeyMiddleware validates API keys against PostgreSQL.
// Falls back to static API key comparison if store is nil (combined/dev mode).
//
// Also accepts an identity JWT (aud=opencomputer-api) when jwtIssuer is non-nil.
// The JWT can arrive either in `Authorization: Bearer <jwt>` or as the X-API-Key
// value (JWTs are detected by the two-dot signature: "header.payload.signature").
// This lets sessions-api act on behalf of a dashboard user without holding a
// long-lived API key — see internal/auth/jwt.go for the audience constants.
func PGAPIKeyMiddleware(store *db.Store, staticKey string, jwtIssuer *JWTIssuer) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Try static API key first (backward compat for combined mode)
			if staticKey != "" && store == nil {
				return APIKeyMiddleware(staticKey)(next)(c)
			}

			// Identity-JWT path: Authorization: Bearer <jwt> with aud=opencomputer-api.
			if jwtIssuer != nil {
				authHeader := c.Request().Header.Get("Authorization")
				if strings.HasPrefix(authHeader, "Bearer ") {
					tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
					if claims, err := jwtIssuer.ValidateIdentityToken(tokenStr, AudOpenComputerAPI); err == nil {
						return applyIdentityClaims(c, claims, next)
					}
				}
			}

			// Get API key from header or query
			key := c.Request().Header.Get("X-API-Key")
			if key == "" {
				key = c.QueryParam("api_key")
			}

			// JWTs may also arrive in the X-API-Key slot (the OC SDK takes a single
			// `apiKey` field and ships it as X-API-Key — no Authorization option).
			// Detect JWT by the two-dot signature; opaque API keys never contain dots.
			if jwtIssuer != nil && key != "" && strings.Count(key, ".") == 2 {
				if claims, err := jwtIssuer.ValidateIdentityToken(key, AudOpenComputerAPI); err == nil {
					return applyIdentityClaims(c, claims, next)
				}
			}

			// If no key and no store, pass through (dev mode)
			if key == "" && store == nil && staticKey == "" {
				return next(c)
			}

			if key == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "missing API key",
				})
			}

			// Validate against PG if store is available
			if store != nil {
				orgID, userID, err := store.ValidateAPIKey(c.Request().Context(), key)
				if err != nil {
					return c.JSON(http.StatusForbidden, map[string]string{
						"error": "invalid API key",
					})
				}
				SetOrgID(c, orgID)
				if userID != nil {
					SetUserID(c, *userID)
				}
				return next(c)
			}

			// Fall back to static key comparison
			return APIKeyMiddleware(staticKey)(next)(c)
		}
	}
}

func applyIdentityClaims(c echo.Context, claims *IdentityClaims, next echo.HandlerFunc) error {
	orgID, err := uuid.Parse(claims.OrgID)
	if err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{
			"error": "identity token: invalid org_id",
		})
	}
	SetOrgID(c, orgID)
	if claims.UserID != nil {
		if userID, err := uuid.Parse(*claims.UserID); err == nil {
			SetUserID(c, userID)
		}
	}
	if claims.Email != nil {
		c.Set("user_email", *claims.Email)
	}
	return next(c)
}

// SandboxJWTMiddleware validates sandbox-scoped JWTs for direct worker access.
// It verifies the token and checks that the sandbox_id in the token matches the :id URL param.
func SandboxJWTMiddleware(jwtIssuer *JWTIssuer) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			authHeader := c.Request().Header.Get("Authorization")
			var tokenStr string
			if authHeader != "" && strings.HasPrefix(authHeader, "Bearer ") {
				tokenStr = strings.TrimPrefix(authHeader, "Bearer ")
			} else if q := c.QueryParam("token"); q != "" {
				// Allow token as query param for WebSocket connections
				// (browsers/Node.js WebSocket API can't set custom headers)
				tokenStr = q
			} else {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "missing or invalid Authorization header",
				})
			}
			claims, err := jwtIssuer.ValidateSandboxToken(tokenStr)
			if err != nil {
				return c.JSON(http.StatusForbidden, map[string]string{
					"error": "invalid token: " + err.Error(),
				})
			}

			// Verify sandbox ID matches URL parameter
			urlSandboxID := c.Param("id")
			if urlSandboxID != "" && claims.SandboxID != urlSandboxID {
				return c.JSON(http.StatusForbidden, map[string]string{
					"error": "token not valid for this sandbox",
				})
			}

			SetOrgID(c, claims.OrgID)
			c.Set("sandbox_id", claims.SandboxID)
			c.Set("worker_id", claims.WorkerID)

			return next(c)
		}
	}
}
