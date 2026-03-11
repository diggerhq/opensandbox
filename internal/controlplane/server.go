package controlplane

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// Server is the control plane API server.
type Server struct {
	echo      *echo.Echo
	store     *db.Store
	jwtIssuer *auth.JWTIssuer
	registry  *WorkerRegistry
}

// NewServer creates a new control plane server.
func NewServer(store *db.Store, jwtIssuer *auth.JWTIssuer, registry *WorkerRegistry, apiKey string) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &Server{
		echo:      e,
		store:     store,
		jwtIssuer: jwtIssuer,
		registry:  registry,
	}

	// Global middleware
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORS())
	e.Use(middleware.RequestID())

	// Health check
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok", "role": "controlplane"})
	})

	// Auth middleware
	api := e.Group("")
	api.Use(auth.PGAPIKeyMiddleware(store, apiKey))

	// Sandbox lifecycle (control plane only handles create/destroy/discover)
	api.POST("/sandboxes", s.createSandbox)
	api.GET("/sandboxes/:id", s.discoverSandbox)
	api.DELETE("/sandboxes/:id", s.destroySandbox)

	// Session history (global queries from PG)
	api.GET("/sessions", s.listSessions)

	// Workers
	api.GET("/workers", s.listWorkers)

	// Projects
	api.POST("/projects", s.createProject)
	api.GET("/projects", s.listProjects)
	api.GET("/projects/:id", s.getProject)
	api.PUT("/projects/:id", s.updateProject)
	api.DELETE("/projects/:id", s.deleteProject)

	// Project secrets
	api.PUT("/projects/:id/secrets/:name", s.setProjectSecret)
	api.DELETE("/projects/:id/secrets/:name", s.deleteProjectSecret)
	api.GET("/projects/:id/secrets", s.listProjectSecrets)

	return s
}

// Start starts the HTTP server.
func (s *Server) Start(addr string) error {
	return s.echo.Start(addr)
}

// Close shuts down the server.
func (s *Server) Close() error {
	return s.echo.Close()
}

func (s *Server) createSandbox(c echo.Context) error {
	var req struct {
		TemplateID string            `json:"templateID"`
		Timeout    int               `json:"timeout"`
		Region     string            `json:"region"`
		Envs       map[string]string `json:"envs"`
		MemoryMB   int               `json:"memoryMB"`
		CpuCount   int               `json:"cpuCount"`
		Metadata   map[string]string `json:"metadata"`
		NetworkEnabled bool          `json:"networkEnabled"`
		Project    string            `json:"project"` // project name — resolves config + secrets
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	// Check org quota
	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "org not found"})
	}
	count, err := s.store.CountActiveSandboxes(c.Request().Context(), orgID)
	if err == nil && count >= org.MaxConcurrentSandboxes {
		return c.JSON(http.StatusTooManyRequests, map[string]string{"error": "concurrent sandbox limit reached"})
	}

	// Resolve project: inherit config defaults + decrypt and merge secrets
	var projectID *string
	if req.Project != "" {
		project, err := s.store.GetProjectByName(c.Request().Context(), orgID, req.Project)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "project not found: " + req.Project})
		}
		pid := project.ID.String()
		projectID = &pid

		// Project config serves as defaults — request fields override
		if req.TemplateID == "" {
			req.TemplateID = project.Template
		}
		if req.CpuCount == 0 {
			req.CpuCount = project.CpuCount
		}
		if req.MemoryMB == 0 {
			req.MemoryMB = project.MemoryMB
		}
		if req.Timeout == 0 {
			req.Timeout = project.TimeoutSec
		}

		// Decrypt project secrets and merge into envs (request envs override project secrets)
		secrets, err := s.store.DecryptProjectSecrets(c.Request().Context(), project.ID)
		if err != nil {
			log.Printf("controlplane: decrypt project secrets failed for %s: %v", req.Project, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to decrypt project secrets"})
		}
		if len(secrets) > 0 {
			if req.Envs == nil {
				req.Envs = make(map[string]string)
			}
			for k, v := range secrets {
				if _, exists := req.Envs[k]; !exists {
					req.Envs[k] = v
				}
			}
		}
	}
	_ = projectID // TODO: store on sandbox_sessions.project_id

	// Select region (explicit, or from Fly-Region header, or default)
	region := req.Region
	if region == "" {
		region = c.Request().Header.Get("Fly-Region")
	}
	if region == "" {
		region = "iad" // default
	}

	// Select least-loaded worker in region
	worker := s.registry.GetLeastLoadedWorker(region)
	if worker == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "no workers available in region " + region})
	}

	// Call gRPC CreateSandbox on the worker
	// Firecracker VM boot + agent readiness can take up to ~35s
	ctx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(worker.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to connect to worker"})
	}
	defer conn.Close()

	client := pb.NewSandboxWorkerClient(conn)
	grpcResp, err := client.CreateSandbox(ctx, &pb.CreateSandboxRequest{
		Template:       req.TemplateID,
		Timeout:        int32(req.Timeout),
		Envs:           req.Envs,
		MemoryMb:       int32(req.MemoryMB),
		CpuCount:       int32(req.CpuCount),
		NetworkEnabled: req.NetworkEnabled,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "worker create failed: " + err.Error()})
	}

	// Insert session record into PG
	template := req.TemplateID
	if template == "" {
		template = "base"
	}
	cfgJSON, _ := json.Marshal(req)
	metadataJSON, _ := json.Marshal(req.Metadata)
	_, _ = s.store.CreateSandboxSession(ctx, grpcResp.SandboxId, orgID, nil, template, region, worker.ID, cfgJSON, metadataJSON)

	// Issue sandbox-scoped JWT (24h TTL — independent of sandbox idle timeout)
	token, err := s.jwtIssuer.IssueSandboxToken(orgID, grpcResp.SandboxId, worker.ID, 24*time.Hour)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to issue token"})
	}

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"sandboxID":  grpcResp.SandboxId,
		"connectURL": worker.HTTPAddr,
		"token":      token,
		"status":     grpcResp.Status,
		"region":     region,
		"workerID":   worker.ID,
	})
}

func (s *Server) discoverSandbox(c echo.Context) error {
	sandboxID := c.Param("id")

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}

	// Look up worker address
	worker := s.registry.GetWorker(session.WorkerID)
	connectURL := ""
	if worker != nil {
		connectURL = worker.HTTPAddr
	}

	orgID, _ := auth.GetOrgID(c)

	// Issue a new token for reconnection
	token := ""
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, session.WorkerID, 24*time.Hour)
		if err == nil {
			token = t
		}
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":  sandboxID,
		"connectURL": connectURL,
		"token":      token,
		"status":     session.Status,
		"region":     session.Region,
		"workerID":   session.WorkerID,
		"startedAt":  session.StartedAt,
		"template":   session.Template,
	})
}

func (s *Server) destroySandbox(c echo.Context) error {
	sandboxID := c.Param("id")

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}

	// Get worker gRPC address
	worker := s.registry.GetWorker(session.WorkerID)
	if worker == nil {
		// Worker is down, just mark as error
		_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "error", strPtr("worker unreachable"))
		return c.NoContent(http.StatusNoContent)
	}

	// Call gRPC DestroySandbox
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(worker.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to connect to worker"})
	}
	defer conn.Close()

	client := pb.NewSandboxWorkerClient(conn)
	if _, err := client.DestroySandbox(ctx, &pb.DestroySandboxRequest{SandboxId: sandboxID}); err != nil {
		log.Printf("controlplane: gRPC destroy failed: %v", err)
	}

	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "stopped", nil)

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listSessions(c echo.Context) error {
	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "auth required"})
	}

	status := c.QueryParam("status")
	sessions, err := s.store.ListSandboxSessions(c.Request().Context(), orgID, status, 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, sessions)
}

func (s *Server) listWorkers(c echo.Context) error {
	workers := s.registry.GetAllWorkers()
	return c.JSON(http.StatusOK, workers)
}

func strPtr(s string) *string {
	return &s
}
