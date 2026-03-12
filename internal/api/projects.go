package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
)

// ── Secret Stores ─────────────────────────────────────────────────────────────

func (s *Server) createSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	var req struct {
		Name            string   `json:"name"`
		EgressAllowlist []string `json:"egressAllowlist"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	store, err := s.store.CreateSecretStore(c.Request().Context(), orgID, req.Name, req.EgressAllowlist)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, store)
}

func (s *Server) listSecretStores(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	stores, err := s.store.ListSecretStores(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, stores)
}

func (s *Server) getSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	store, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}
	return c.JSON(http.StatusOK, store)
}

func (s *Server) updateSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	var req struct {
		Name            *string  `json:"name"`
		EgressAllowlist []string `json:"egressAllowlist"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	existing, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	name := existing.Name
	allowlist := existing.EgressAllowlist

	if req.Name != nil {
		name = *req.Name
	}
	if req.EgressAllowlist != nil {
		allowlist = req.EgressAllowlist
	}

	store, err := s.store.UpdateSecretStore(c.Request().Context(), orgID, storeID, name, allowlist)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, store)
}

func (s *Server) deleteSecretStore(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	if err := s.store.DeleteSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// ── Secret Store Entries ──────────────────────────────────────────────────────

func (s *Server) setSecretEntry(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}
	name := c.Param("name")
	if name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "secret name required"})
	}

	if _, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	var req struct {
		Value        string   `json:"value"`
		AllowedHosts []string `json:"allowedHosts"`
	}
	if err := c.Bind(&req); err != nil || req.Value == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "secret value required: {\"value\": \"...\", \"allowedHosts\": [...]}"})
	}

	for _, h := range req.AllowedHosts {
		h = strings.TrimSpace(h)
		if h == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "allowedHosts entries cannot be empty"})
		}
	}

	if err := s.store.SetSecretEntry(c.Request().Context(), storeID, name, []byte(req.Value), req.AllowedHosts); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"name": name, "status": "set"})
}

func (s *Server) deleteSecretEntry(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}
	name := c.Param("name")

	if _, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	if err := s.store.DeleteSecretEntry(c.Request().Context(), storeID, name); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listSecretEntries(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	storeID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid store ID"})
	}

	if _, err := s.store.GetSecretStore(c.Request().Context(), orgID, storeID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "secret store not found"})
	}

	entries, err := s.store.ListSecretEntries(c.Request().Context(), storeID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, entries)
}
