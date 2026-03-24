package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// SignedURLParams holds the parameters for generating or verifying a signed URL.
type SignedURLParams struct {
	SandboxID string
	Path      string
	Operation string // "download" or "upload"
	ExpiresAt time.Time
}

// SignURL produces an HMAC-SHA256 hex signature for the given parameters.
// The signing payload uses a domain-separated canonical format to prevent
// parameter substitution attacks.
func SignURL(secret []byte, params SignedURLParams) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signPayload(params)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyURL checks the HMAC signature and expiration.
func VerifyURL(secret []byte, params SignedURLParams, signature string) error {
	if time.Now().After(params.ExpiresAt) {
		return fmt.Errorf("signed URL expired")
	}

	expected := SignURL(secret, params)
	sigBytes, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("invalid signature encoding")
	}
	expectedBytes, _ := hex.DecodeString(expected)

	if !hmac.Equal(sigBytes, expectedBytes) {
		return fmt.Errorf("invalid signature")
	}

	return nil
}

// ExpiresAtUnix returns the expiration as a Unix timestamp string.
func (p SignedURLParams) ExpiresAtUnix() string {
	return strconv.FormatInt(p.ExpiresAt.Unix(), 10)
}

// ParseExpiresUnix parses a Unix timestamp string into a time.Time.
func ParseExpiresUnix(s string) (time.Time, error) {
	unix, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid expiration: %w", err)
	}
	return time.Unix(unix, 0), nil
}

func signPayload(params SignedURLParams) string {
	return "opensandbox-signed-url\n" +
		params.SandboxID + "\n" +
		params.Path + "\n" +
		params.Operation + "\n" +
		params.ExpiresAtUnix()
}
