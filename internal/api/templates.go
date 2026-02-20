package api

import (
	"context"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/ecr"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

func (s *Server) buildTemplate(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	var req struct {
		Name       string `json:"name"`
		Dockerfile string `json:"dockerfile"`
		Tag        string `json:"tag"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}
	if req.Name == "" || req.Dockerfile == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "name and dockerfile are required",
		})
	}
	if req.Tag == "" {
		req.Tag = "latest"
	}

	ctx := c.Request().Context()

	// Compute ECR image reference
	var ecrImageRef string
	if s.ecrConfig != nil && s.ecrConfig.IsConfigured() {
		ecrImageRef = ecr.ImageRef(s.ecrConfig, orgID.String(), req.Name, req.Tag)
	}

	// Pick a worker and dispatch build via gRPC
	if s.workerRegistry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "no workers available for template builds",
		})
	}

	region := s.region
	if region == "" {
		region = "use2"
	}

	_, grpcClient, err := s.workerRegistry.GetLeastLoadedWorker(region)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "no workers available: " + err.Error(),
		})
	}

	grpcCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	grpcResp, err := grpcClient.BuildTemplate(grpcCtx, &pb.BuildTemplateRequest{
		Name:        req.Name,
		Dockerfile:  req.Dockerfile,
		Tag:         req.Tag,
		EcrImageRef: ecrImageRef,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "template build failed: " + err.Error(),
		})
	}

	// Insert template record in DB
	tmpl, err := s.store.CreateTemplate(ctx, &orgID, req.Name, req.Tag, grpcResp.ImageRef, &req.Dockerfile, false)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to save template: " + err.Error(),
		})
	}

	return c.JSON(http.StatusCreated, tmpl)
}

func (s *Server) listTemplates(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	templates, err := s.store.ListTemplates(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, templates)
}

func (s *Server) getTemplate(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	name := c.Param("name")
	tmpl, err := s.store.GetTemplateByName(c.Request().Context(), orgID, name)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, tmpl)
}

func (s *Server) deleteTemplate(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	name := c.Param("name")

	// Look up template first
	tmpl, err := s.store.GetTemplateByName(c.Request().Context(), orgID, name)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	if tmpl.IsPublic {
		return c.JSON(http.StatusForbidden, map[string]string{
			"error": "cannot delete public templates",
		})
	}

	if err := s.store.DeleteTemplateForOrg(c.Request().Context(), tmpl.ID, orgID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.NoContent(http.StatusNoContent)
}
