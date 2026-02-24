package auth

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"

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

	// Verify CSRF state
	stateCookie, err := c.Cookie("oauth_state")
	if err != nil || stateCookie.Value != state {
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

	// Determine org name — use organization ID, or create a personal org per user
	orgName := authResult.User.Email // personal org keyed by email
	if authResult.OrganizationID != "" {
		orgName = authResult.OrganizationID
	}

	// Provision org and user in local database
	localUser, err := h.workos.ProvisionOrgAndUser(ctx, authResult.User.Email, name, orgName)
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

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":    userID,
		"email": email,
		"orgId": orgID,
	})
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
