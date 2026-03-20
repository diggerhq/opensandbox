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
