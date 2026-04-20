package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
)

const identityTokenTTL = 10 * time.Minute

// createAuthToken exchanges the current API key for a short-lived identity JWT.
// Downstream services (sessions-api) use this to resolve key → owner without
// calling back on every request.
func (s *Server) createAuthToken(c echo.Context) error {
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	if s.jwtIssuer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "token issuance not configured",
		})
	}

	orgStr := orgID.String()
	var userStr *string
	if userID := auth.GetUserID(c); userID != nil {
		s := userID.String()
		userStr = &s
	}

	token, err := s.jwtIssuer.IssueIdentityToken(orgStr, userStr, identityTokenTTL)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to issue token",
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"token":      token,
		"expires_in": int(identityTokenTTL.Seconds()),
	})
}
