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
func PGAPIKeyMiddleware(store *db.Store, staticKey string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Try static API key first (backward compat for combined mode)
			if staticKey != "" && store == nil {
				return APIKeyMiddleware(staticKey)(next)(c)
			}

			// Get API key from header or query
			key := c.Request().Header.Get("X-API-Key")
			if key == "" {
				key = c.QueryParam("api_key")
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
