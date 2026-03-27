package auth

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/workos/workos-go/v4/pkg/usermanagement"
)

// OAuthHandlers provides HTTP handlers for WorkOS OAuth flow.
type OAuthHandlers struct {
	workos *WorkOSMiddleware
}

// NewOAuthHandlers creates new OAuth handlers.
func NewOAuthHandlers(workos *WorkOSMiddleware) *OAuthHandlers {
	return &OAuthHandlers{workos: workos}
}

// HandleLogin redirects the user to WorkOS AuthKit for authentication.
func (h *OAuthHandlers) HandleLogin(c echo.Context) error {
	cfg := h.workos.Config()

	state, err := generateState()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to generate state",
		})
	}

	// Store state in cookie for CSRF protection
	c.SetCookie(&http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	authURL, err := h.workos.UserMgr().GetAuthorizationURL(usermanagement.GetAuthorizationURLOpts{
		ClientID:    cfg.ClientID,
		RedirectURI: cfg.RedirectURI,
		Provider:    "authkit",
		State:       state,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to generate authorization URL: " + err.Error(),
		})
	}

	return c.Redirect(http.StatusFound, authURL.String())
}

// HandleCallback exchanges the authorization code for user info and sets session cookie.
func (h *OAuthHandlers) HandleCallback(c echo.Context) error {
	code := c.QueryParam("code")
	state := c.QueryParam("state")

	if code == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "missing authorization code",
		})
	}

	// Verify CSRF state — only when the user started from /auth/login (which sets the cookie).
	// Invitation flows bypass login, so the user arrives with a code but no state/cookie.
	stateCookie, err := c.Cookie("oauth_state")
	if err == nil {
		// Cookie exists — verify it matches (normal login flow)
		if stateCookie.Value != state {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": "invalid state parameter",
			})
		}
		// Clear state cookie
		c.SetCookie(&http.Cookie{
			Name:   "oauth_state",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})
	}
	// No cookie = invitation flow — skip CSRF check (code is single-use and client-bound)

	cfg := h.workos.Config()
	ctx := c.Request().Context()

	// Exchange code for user info
	authResult, err := h.workos.UserMgr().AuthenticateWithCode(ctx, usermanagement.AuthenticateWithCodeOpts{
		ClientID: cfg.ClientID,
		Code:     code,
	})
	if err != nil {
		log.Printf("workos: callback authentication failed: %v", err)
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "authentication failed",
		})
	}

	// Build user display name
	name := authResult.User.FirstName
	if authResult.User.LastName != "" {
		name += " " + authResult.User.LastName
	}
	if name == "" {
		name = authResult.User.Email
	}

	// Provision org and user in local database
	// orgName is used for slug generation; workosOrgID is set if user was invited to an org
	orgName := authResult.User.Email
	localUser, err := h.workos.ProvisionOrgAndUser(ctx, authResult.User.Email, name, orgName, authResult.User.ID, authResult.OrganizationID)
	if err != nil {
		log.Printf("workos: provisioning failed: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to provision user",
		})
	}

	// Store the access token mapped to user for session validation
	if h.workos.Store() != nil {
		_ = h.workos.Store().StoreAccessToken(ctx, localUser.ID, authResult.AccessToken)
	}

	// Set session cookie with the access token
	cookieDomain := cfg.CookieDomain
	c.SetCookie(&http.Cookie{
		Name:     "workos_session",
		Value:    authResult.AccessToken,
		Path:     "/",
		Domain:   cookieDomain,
		MaxAge:   86400 * 7, // 7 days
		HttpOnly: true,
		Secure:   isSecureRequest(c),
		SameSite: http.SameSiteLaxMode,
	})

	// Redirect to dashboard after login
	return c.Redirect(http.StatusFound, "/")
}

// HandleLogout clears the session cookie and invalidates the server-side session.
func (h *OAuthHandlers) HandleLogout(c echo.Context) error {
	// Invalidate server-side session if we can identify the user
	if cookie, err := c.Cookie("workos_session"); err == nil && cookie.Value != "" {
		store := h.workos.Store()
		if store != nil {
			ctx := c.Request().Context()
			user, err := store.GetUserByAccessToken(ctx, cookie.Value)
			if err == nil {
				_ = store.DeleteAccessTokensForUser(ctx, user.ID)
			}
		}
	}

	// Clear all auth cookies
	ClearAllCookies(c)

	return c.JSON(http.StatusOK, map[string]string{
		"message": "logged out",
	})
}

// HandleMe returns the current user info from the authenticated context.
func (h *OAuthHandlers) HandleMe(c echo.Context) error {
	userID := c.Get("user_id")
	email := c.Get("user_email")
	orgID, _ := GetOrgID(c)

	resp := map[string]interface{}{
		"id":    userID,
		"email": email,
		"orgId": orgID,
	}

	// If we have a store and WorkOS org manager, include the user's org list
	store := h.workos.Store()
	orgMgr := h.workos.OrgMgr()
	if store != nil && orgMgr != nil {
		if uid, ok := userID.(uuid.UUID); ok {
			user, err := store.GetUserByEmail(c.Request().Context(), email.(string))
			if err == nil && user.WorkOSUserID != nil {
				memberships, err := orgMgr.ListUserMemberships(c.Request().Context(), *user.WorkOSUserID)
				if err == nil {
					type orgInfo struct {
						ID         uuid.UUID `json:"id"`
						Name       string    `json:"name"`
						IsPersonal bool      `json:"isPersonal"`
						IsActive   bool      `json:"isActive"`
					}
					var orgs []orgInfo
					for _, m := range memberships {
						localOrg, err := store.GetOrgByWorkOSID(c.Request().Context(), m.OrganizationID)
						if err == nil {
							orgs = append(orgs, orgInfo{
								ID:         localOrg.ID,
								Name:       localOrg.Name,
								IsPersonal: localOrg.IsPersonal,
								IsActive:   localOrg.ID == user.OrgID,
							})
						}
					}
					resp["orgs"] = orgs
				}
			}
			_ = uid // suppress unused
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// isSecureRequest returns true if the request is over HTTPS,
// either directly or via a TLS-terminating proxy (e.g. Caddy, ALB).
func isSecureRequest(c echo.Context) bool {
	if c.Request().TLS != nil {
		return true
	}
	return c.Request().Header.Get("X-Forwarded-Proto") == "https"
}

func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// SetRefreshCookie sets a refresh token cookie (used for token renewal).
func SetRefreshCookie(c echo.Context, refreshToken, domain string) {
	c.SetCookie(&http.Cookie{
		Name:     "workos_refresh",
		Value:    refreshToken,
		Path:     "/",
		Domain:   domain,
		MaxAge:   86400 * 30, // 30 days
		HttpOnly: true,
		Secure:   isSecureRequest(c),
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearAllCookies helper to clear all auth cookies (used for force-logout).
func ClearAllCookies(c echo.Context) {
	for _, name := range []string{"workos_session", "workos_refresh", "oauth_state"} {
		c.SetCookie(&http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0),
			HttpOnly: true,
		})
	}
}
