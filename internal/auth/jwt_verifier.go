package auth

import (
	"context"
	"errors"

	"github.com/labstack/echo/v4"
)

// SessionContext carries claims extracted from a session JWT minted by the
// api-edge Worker. Stored in echo.Context under the key "session".
type SessionContext struct {
	UserID      string
	OrgID       string
	Plan        string // "free" | "pro"
	Permissions []string
	JTI         string // unique token id (used for revocation)
	ExpiresAt   int64  // unix sec
}

// ErrSessionInvalid means the JWT is malformed, expired, or its signature
// does not verify against the configured SessionJWTSecret.
var ErrSessionInvalid = errors.New("session invalid")

// SessionJWTVerifier validates session JWTs minted by the api-edge Worker.
// HMAC-SHA256 against a shared secret rotated across the fleet.
type SessionJWTVerifier struct {
	secret []byte

	// TODO: Redis client for revocation check (optional, V2)
	// revocationKey = "session:revoked:{jti}"
}

// NewSessionJWTVerifier constructs a verifier. Empty secret returns a
// verifier that always errs — callers should only register the middleware
// when SessionJWTSecret is configured.
func NewSessionJWTVerifier(secret string) *SessionJWTVerifier {
	return &SessionJWTVerifier{secret: []byte(secret)}
}

// Verify parses and validates a token string. Returns the claims or an error.
func (v *SessionJWTVerifier) Verify(ctx context.Context, token string) (*SessionContext, error) {
	// TODO:
	// 1. Parse JWT (use github.com/golang-jwt/jwt/v5 — already a dep)
	// 2. Verify HMAC-SHA256 signature against v.secret
	// 3. Verify exp > now
	// 4. (V2) Check JTI against Redis revocation set
	// 5. Build and return SessionContext
	_ = ctx
	_ = token
	return nil, ErrSessionInvalid
}

// Middleware extracts and verifies the Authorization: Bearer <jwt> header,
// stashing the SessionContext on echo.Context. Returns 401 on any failure.
func (v *SessionJWTVerifier) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// TODO:
			// 1. Read Authorization header, strip "Bearer "
			// 2. Verify token
			// 3. c.Set("session", ctx)
			// 4. Continue or 401
			return next(c)
		}
	}
}

// SessionFrom retrieves the SessionContext from an echo.Context.
// Returns nil if no session was attached (e.g. an unauthenticated route).
func SessionFrom(c echo.Context) *SessionContext {
	if v, ok := c.Get("session").(*SessionContext); ok {
		return v
	}
	return nil
}
