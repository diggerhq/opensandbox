package api

import (
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
)

const identityTokenTTL = 10 * time.Minute

// createAuthToken exchanges the current API key for a short-lived identity JWT.
// Downstream services (sessions-api) use this to resolve key → owner without
// calling back on every request. The token carries email + workos_user_id so
// downstreams can emit analytics events consistently with opencomputer.
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

	in := auth.IdentityTokenInput{OrgID: orgID.String(), Audience: auth.AudSessionsAPI}
	if userID := auth.GetUserID(c); userID != nil {
		userStr := userID.String()
		in.UserID = &userStr

		// Look up email + workos_user_id for downstream analytics identity.
		// Soft failure: if the lookup fails, still issue a token (without
		// those fields) rather than block the caller.
		if s.store != nil {
			user, err := s.store.GetUserByID(c.Request().Context(), *userID)
			if err != nil {
				log.Printf("identity: failed to load user %s: %v", userStr, err)
			} else {
				if user.Email != "" {
					email := user.Email
					in.Email = &email
				}
				if user.WorkOSUserID != nil && *user.WorkOSUserID != "" {
					in.WorkOSUserID = user.WorkOSUserID
				}
			}
		}
	}

	token, err := s.jwtIssuer.IssueIdentityToken(in, identityTokenTTL)
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
