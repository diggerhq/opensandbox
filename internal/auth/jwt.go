package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// SandboxClaims are the JWT claims for sandbox-scoped access tokens.
type SandboxClaims struct {
	jwt.RegisteredClaims
	OrgID     uuid.UUID `json:"org_id"`
	SandboxID string    `json:"sandbox_id"`
	WorkerID  string    `json:"worker_id"`
}

// JWTIssuer creates sandbox-scoped JWTs.
type JWTIssuer struct {
	secret []byte
}

// NewJWTIssuer creates a new JWT issuer with the given shared secret.
func NewJWTIssuer(secret string) *JWTIssuer {
	return &JWTIssuer{secret: []byte(secret)}
}

// IssueSandboxToken creates a JWT for direct worker access.
func (j *JWTIssuer) IssueSandboxToken(orgID uuid.UUID, sandboxID, workerID string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := SandboxClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   orgID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "opensandbox",
		},
		OrgID:     orgID,
		SandboxID: sandboxID,
		WorkerID:  workerID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// Audience constants for identity tokens. Each consumer requires its own
// audience so that a token minted for one surface cannot be replayed against
// another (e.g. a token meant for sessions-api can't be used to call OC's
// sandbox API directly).
const (
	AudSessionsAPI     = "sessions-api"
	AudOpenComputerAPI = "opencomputer-api"
)

// IdentityClaims are the JWT claims for identity tokens issued to downstream
// services (e.g. sessions-api, OC's own /api/sandboxes when called by
// sessions-api). They carry the caller's org and user identity so the
// downstream can authorize and attribute analytics events without calling
// back to the control plane.
type IdentityClaims struct {
	jwt.RegisteredClaims
	OrgID        string  `json:"org_id"`
	UserID       *string `json:"user_id,omitempty"`
	Email        *string `json:"email,omitempty"`
	WorkOSUserID *string `json:"workos_user_id,omitempty"`
}

// IdentityTokenInput bundles the identity fields embedded in an identity JWT.
// Nil pointers are omitted from the resulting token. Audience is required —
// see the Aud* constants.
type IdentityTokenInput struct {
	OrgID        string
	UserID       *string
	Email        *string
	WorkOSUserID *string
	Audience     string
}

// IssueIdentityToken creates a short-lived JWT carrying the caller's identity.
func (j *JWTIssuer) IssueIdentityToken(in IdentityTokenInput, ttl time.Duration) (string, error) {
	if in.Audience == "" {
		return "", fmt.Errorf("audience is required")
	}
	now := time.Now()
	claims := IdentityClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   in.OrgID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "opensandbox",
			Audience:  jwt.ClaimStrings{in.Audience},
		},
		OrgID:        in.OrgID,
		UserID:       in.UserID,
		Email:        in.Email,
		WorkOSUserID: in.WorkOSUserID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// ValidateIdentityToken parses and validates an identity JWT, requiring the
// expected audience. Pass one of the Aud* constants.
func (j *JWTIssuer) ValidateIdentityToken(tokenStr, expectedAudience string) (*IdentityClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &IdentityClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.secret, nil
	}, jwt.WithIssuer("opensandbox"), jwt.WithAudience(expectedAudience))
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*IdentityClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}

// CapabilityClaims are the claims for a capability token: the short-lived JWT
// the api-edge Worker mints and hands to a regional control plane when it
// proxies POST /api/sandboxes. The CP trusts org_id from this token (the edge
// is the authority on org identity now) and rejects the token if cell_id
// doesn't match its own OPENSANDBOX_CELL_ID — so a token minted for cell A
// can't be replayed against cell B.
//
// Signed with the shared session-JWT secret (SESSION_JWT_SECRET / the CP's
// OPENSANDBOX_SESSION_JWT_SECRET). Distinguished from session JWTs by Issuer.
type CapabilityClaims struct {
	jwt.RegisteredClaims
	OrgID  string  `json:"org_id"`
	CellID string  `json:"cell_id"`
	UserID *string `json:"user_id,omitempty"`
	// Plan is the org's billing plan ("free" | "pro") at token-mint time.
	// The CP forwards this to the worker so it can tag usage_tick events
	// without a per-event PG lookup. Plan may go stale (org upgrades while
	// a sandbox is running) — that's fine: the events-ingest fan-out is
	// idempotent, and the DO knows the current plan from D1 so it no-ops
	// debits for a now-pro org.
	Plan string `json:"plan,omitempty"`
}

// CapabilityIssuer is the Issuer string on capability tokens — lets the CP
// reject a session JWT presented where a capability token is expected.
const CapabilityIssuer = "opensandbox-edge"

// IssueCapabilityToken mints a capability token for (orgID, cellID, plan).
// userID may be nil (API-key-authenticated callers have no user). plan may
// be empty when the caller doesn't have it handy — the cell will then
// fall back to a PG lookup, which is a small performance hit but never
// incorrect.
func (j *JWTIssuer) IssueCapabilityToken(orgID, cellID, plan string, userID *string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := CapabilityClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   orgID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    CapabilityIssuer,
		},
		OrgID:  orgID,
		CellID: cellID,
		UserID: userID,
		Plan:   plan,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.secret)
}

// ValidateCapabilityToken parses and validates a capability token. It checks
// the signature, expiry, and Issuer; the caller must additionally verify that
// claims.CellID matches the local cell.
func (j *JWTIssuer) ValidateCapabilityToken(tokenStr string) (*CapabilityClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &CapabilityClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.secret, nil
	}, jwt.WithIssuer(CapabilityIssuer))
	if err != nil {
		return nil, fmt.Errorf("invalid capability token: %w", err)
	}
	claims, ok := token.Claims.(*CapabilityClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid capability token claims")
	}
	if claims.OrgID == "" || claims.CellID == "" {
		return nil, fmt.Errorf("capability token missing org_id or cell_id")
	}
	return claims, nil
}

// SigningSecret returns the raw HMAC secret for use by other signing functions (e.g. signed URLs).
func (j *JWTIssuer) SigningSecret() []byte { return j.secret }

// ValidateSandboxToken parses and validates a sandbox-scoped JWT.
func (j *JWTIssuer) ValidateSandboxToken(tokenStr string) (*SandboxClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &SandboxClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return j.secret, nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(*SandboxClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}
