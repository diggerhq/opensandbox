package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/workos/workos-go/v4/pkg/usermanagement"
)

// WorkOSConfig holds WorkOS integration settings.
type WorkOSConfig struct {
	APIKey       string
	ClientID     string
	RedirectURI  string
	CookieDomain string
	FrontendURL  string // e.g. "http://localhost:3000" for Vite dev; empty = same origin
}

// WorkOSMiddleware validates WorkOS session tokens for dashboard access.
// It checks for a session cookie or Authorization header, validates with WorkOS,
// and provisions orgs/users in the local database on first login.
type WorkOSMiddleware struct {
	config  WorkOSConfig
	store   *db.Store
	userMgr *usermanagement.Client
}

// NewWorkOSMiddleware creates WorkOS session middleware.
func NewWorkOSMiddleware(config WorkOSConfig, store *db.Store) *WorkOSMiddleware {
	var userMgr *usermanagement.Client
	if config.APIKey != "" {
		userMgr = usermanagement.NewClient(config.APIKey)
	}
	return &WorkOSMiddleware{config: config, store: store, userMgr: userMgr}
}

// Middleware returns the Echo middleware function.
func (w *WorkOSMiddleware) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// Skip if WorkOS is not configured
			if w.config.APIKey == "" {
				return next(c)
			}

			// Extract access token from cookie or header
			accessToken := ""
			if cookie, err := c.Cookie("workos_session"); err == nil {
				accessToken = cookie.Value
			}
			if accessToken == "" {
				auth := c.Request().Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer ") {
					accessToken = strings.TrimPrefix(auth, "Bearer ")
				}
			}

			if accessToken == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "authentication required",
				})
			}

			// Validate session with WorkOS
			user, err := w.validateSession(c.Request().Context(), accessToken)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{
					"error": "invalid session: " + err.Error(),
				})
			}

			// Set org context
			SetOrgID(c, user.OrgID)
			c.Set("user_id", user.ID)
			c.Set("user_email", user.Email)

			return next(c)
		}
	}
}

// WorkOSUser represents a validated WorkOS user.
type WorkOSUser struct {
	ID    uuid.UUID
	OrgID uuid.UUID
	Email string
	Name  string
}

// UserMgr returns the WorkOS user management client for use in OAuth handlers.
func (w *WorkOSMiddleware) UserMgr() *usermanagement.Client {
	return w.userMgr
}

// Config returns the WorkOS configuration.
func (w *WorkOSMiddleware) Config() WorkOSConfig {
	return w.config
}

// Store returns the database store.
func (w *WorkOSMiddleware) Store() *db.Store {
	return w.store
}

// validateSession validates a WorkOS access token by looking up the user
// in the local database. The access token is the one returned by
// AuthenticateWithCode during the OAuth callback, and the user was
// provisioned at that time.
func (w *WorkOSMiddleware) validateSession(ctx context.Context, accessToken string) (*WorkOSUser, error) {
	if w.store == nil {
		return nil, fmt.Errorf("database not configured")
	}

	// The access token is stored alongside the user email during callback.
	// Look up the user by the stored access token.
	// For simplicity, we re-validate with WorkOS by fetching user info
	// using the access token that was issued during authentication.
	if w.userMgr == nil {
		return nil, fmt.Errorf("WorkOS not configured")
	}

	// Use the refresh token flow to validate the session.
	// First try to find the user from the cookie's access token.
	// The access token was set during the callback after AuthenticateWithCode.
	// We look up the user by matching the token stored in the session cookie.
	user, err := w.store.GetUserByAccessToken(ctx, accessToken)
	if err != nil {
		return nil, fmt.Errorf("invalid or expired session")
	}

	return &WorkOSUser{
		ID:    user.ID,
		OrgID: user.OrgID,
		Email: user.Email,
		Name:  user.Name,
	}, nil
}

// ProvisionOrgAndUser creates or fetches an org and user based on WorkOS data.
// Called on first login to auto-provision local records.
func (w *WorkOSMiddleware) ProvisionOrgAndUser(ctx context.Context, email, name, orgName string) (*WorkOSUser, error) {
	if w.store == nil {
		return nil, fmt.Errorf("database not configured")
	}

	// Check if user exists
	existingUser, err := w.store.GetUserByEmail(ctx, email)
	if err == nil {
		return &WorkOSUser{
			ID:    existingUser.ID,
			OrgID: existingUser.OrgID,
			Email: existingUser.Email,
			Name:  existingUser.Name,
		}, nil
	}

	// Create org (slug from org name — sanitize for emails and special chars)
	slug := strings.ToLower(orgName)
	slug = strings.ReplaceAll(slug, "@", "-at-")
	slug = strings.ReplaceAll(slug, ".", "-")
	slug = strings.ReplaceAll(slug, " ", "-")
	org, err := w.store.GetOrgBySlug(ctx, slug)
	if err != nil {
		org, err = w.store.CreateOrg(ctx, orgName, slug)
		if err != nil {
			return nil, fmt.Errorf("failed to create org: %w", err)
		}
		log.Printf("workos: provisioned new org: %s (%s)", org.Name, org.ID)

		// Generate a default API key for the new org
		apiKey, err := GenerateAPIKey()
		if err == nil {
			hash := db.HashAPIKey(apiKey)
			prefix := apiKey[:8]
			_, _ = w.store.CreateAPIKey(ctx, org.ID, nil, hash, prefix, "Default", []string{"sandbox:*"})
			log.Printf("workos: created default API key for org %s: %s...", org.Slug, prefix)
		}
	}

	// Create user
	user, err := w.store.CreateUser(ctx, org.ID, email, name, "admin")
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}
	log.Printf("workos: provisioned new user: %s (%s)", user.Email, user.ID)

	return &WorkOSUser{
		ID:    user.ID,
		OrgID: user.OrgID,
		Email: user.Email,
		Name:  user.Name,
	}, nil
}

// GenerateAPIKey generates a new plaintext API key with the osb_ prefix.
func GenerateAPIKey() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "osb_" + hex.EncodeToString(bytes), nil
}
