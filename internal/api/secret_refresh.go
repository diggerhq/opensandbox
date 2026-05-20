package api

import (
	"encoding/base64"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// internalSecretRefresh is the edge-side fan-in for secret-store updates.
//
// When a user PUTs a new value via /api/secret-stores/{id}/secrets/{name} on
// the edge, the edge writes the encrypted blob to D1 (single source of truth)
// and then HMAC-POSTs this endpoint on every active cell. Each cell asks its
// LOCAL Postgres which running sandboxes bind that store
// (sandbox_sessions.secret_store_id), then runs the existing in-cell gRPC
// fan-out (fanoutSecretRefresh) to push the new value to each affected
// worker. So no sandbox restart needed for a secret change.
//
// Wire format mirrors /internal/secret-stores/by-name on the edge: the value
// arrives as base64(nonce || ciphertext+tag), encrypted with the shared
// SECRET_ENCRYPTION_KEY. The CP decrypts locally before gRPC'ing to workers.
// Plaintext never crosses the cell boundary.
//
// HMAC: AdminAuth middleware in router.go verifies "{X-Timestamp}.{body}"
// signed with CFEventSecret (shared with edge). The endpoint is idempotent
// at the worker level (UpdateSandboxSecret on a sandbox with no matching
// session returns updated=false, not an error) so duplicate deliveries are
// safe.
func (s *Server) internalSecretRefresh(c echo.Context) error {
	var req struct {
		StoreID           string `json:"storeId"`
		Name              string `json:"name"`
		EncryptedValueB64 string `json:"encryptedValueB64"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.StoreID == "" || req.Name == "" || req.EncryptedValueB64 == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "storeId, name, encryptedValueB64 required"})
	}
	storeID, err := uuid.Parse(req.StoreID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid storeId"})
	}
	if s.store == nil || s.store.Encryptor() == nil {
		// No encryptor means this cell can't decrypt. Surface as 503 so the
		// edge marks this cell as a failure but doesn't mistake it for a
		// permanent rejection.
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "encryptor not configured"})
	}

	ct, err := base64.StdEncoding.DecodeString(req.EncryptedValueB64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid base64"})
	}
	pt, err := s.store.Encryptor().Decrypt(ct)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "decrypt failed (key mismatch?)"})
	}

	refreshed, failures := s.fanoutSecretRefresh(c.Request().Context(), storeID, req.Name, string(pt))
	return c.JSON(http.StatusOK, map[string]any{
		"refreshed": refreshed,
		"failures":  failures,
	})
}
