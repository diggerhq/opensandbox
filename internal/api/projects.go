package api

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
)

func (s *Server) createProject(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	var req struct {
		Name            string   `json:"name"`
		Template        string   `json:"template"`
		CpuCount        int      `json:"cpuCount"`
		MemoryMB        int      `json:"memoryMB"`
		TimeoutSec      int      `json:"timeoutSec"`
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

	project, err := s.store.CreateProject(c.Request().Context(), orgID, req.Name, req.Template, req.CpuCount, req.MemoryMB, req.TimeoutSec, req.EgressAllowlist)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, project)
}

func (s *Server) listProjects(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projects, err := s.store.ListProjects(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, projects)
}

func (s *Server) getProject(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
	}

	project, err := s.store.GetProject(c.Request().Context(), orgID, projectID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project not found"})
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) updateProject(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
	}

	var req struct {
		Name            *string  `json:"name"`
		Template        *string  `json:"template"`
		CpuCount        *int     `json:"cpuCount"`
		MemoryMB        *int     `json:"memoryMB"`
		TimeoutSec      *int     `json:"timeoutSec"`
		EgressAllowlist []string `json:"egressAllowlist"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}

	// Fetch existing project to merge partial updates
	existing, err := s.store.GetProject(c.Request().Context(), orgID, projectID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project not found"})
	}

	name := existing.Name
	template := existing.Template
	cpuCount := existing.CpuCount
	memoryMB := existing.MemoryMB
	timeoutSec := existing.TimeoutSec
	allowlist := existing.EgressAllowlist

	if req.Name != nil { name = *req.Name }
	if req.Template != nil { template = *req.Template }
	if req.CpuCount != nil { cpuCount = *req.CpuCount }
	if req.MemoryMB != nil { memoryMB = *req.MemoryMB }
	if req.TimeoutSec != nil { timeoutSec = *req.TimeoutSec }
	if req.EgressAllowlist != nil { allowlist = req.EgressAllowlist }

	project, err := s.store.UpdateProject(c.Request().Context(), orgID, projectID, name, template, cpuCount, memoryMB, timeoutSec, allowlist)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, project)
}

func (s *Server) deleteProject(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
	}

	if err := s.store.DeleteProject(c.Request().Context(), orgID, projectID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// ── Project Secrets ───────────────────────────────────────────────────────────

func (s *Server) setProjectSecret(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
	}
	name := c.Param("name")
	if name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "secret name required"})
	}

	// Verify project belongs to org
	if _, err := s.store.GetProject(c.Request().Context(), orgID, projectID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project not found"})
	}

	var req struct {
		Value string `json:"value"`
	}
	if err := c.Bind(&req); err != nil || req.Value == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "secret value required: {\"value\": \"...\"}"})
	}

	if err := s.store.SetProjectSecret(c.Request().Context(), projectID, name, []byte(req.Value)); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"name": name, "status": "set"})
}

func (s *Server) deleteProjectSecret(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
	}
	name := c.Param("name")

	if _, err := s.store.GetProject(c.Request().Context(), orgID, projectID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project not found"})
	}

	if err := s.store.DeleteProjectSecret(c.Request().Context(), projectID, name); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listProjectSecrets(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	projectID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid project ID"})
	}

	if _, err := s.store.GetProject(c.Request().Context(), orgID, projectID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "project not found"})
	}

	secrets, err := s.store.ListProjectSecretNames(c.Request().Context(), projectID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	names := make([]string, len(secrets))
	for i, s := range secrets {
		names[i] = s.Name
	}
	return c.JSON(http.StatusOK, names)
}
