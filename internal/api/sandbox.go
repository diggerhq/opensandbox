package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)


func (s *Server) createSandbox(c echo.Context) error {
	var cfg types.SandboxConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	// Validate and clamp CPU/memory limits
	if cfg.CpuCount < 0 {
		cfg.CpuCount = 0
	}
	if cfg.CpuCount > 4 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "cpuCount must not exceed 4",
		})
	}
	if cfg.MemoryMB > 2048 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "memoryMB must not exceed 2048",
		})
	}
	if cfg.MemoryMB < 0 {
		cfg.MemoryMB = 0
	}

	ctx := c.Request().Context()

	// Check org quota if PG is available
	orgID, hasOrg := auth.GetOrgID(c)
	if hasOrg && s.store != nil {
		org, err := s.store.GetOrg(ctx, orgID)
		if err == nil {
			count, err := s.store.CountActiveSandboxes(ctx, orgID)
			if err == nil && count >= org.MaxConcurrentSandboxes {
				return c.JSON(http.StatusTooManyRequests, map[string]string{
					"error": "concurrent sandbox limit reached",
				})
			}
		}
	}

	// Server mode with worker registry: dispatch to remote worker via gRPC
	if s.workerRegistry != nil {
		return s.createSandboxRemote(c, ctx, cfg, orgID, hasOrg)
	}

	// Combined/worker mode: create locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sb, err := s.manager.Create(ctx, cfg)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Register with sandbox router for rolling timeout tracking
	if s.router != nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 300
		}
		s.router.Register(sb.ID, time.Duration(timeout)*time.Second)
	}

	// Initialize per-sandbox SQLite if available
	if s.sandboxDBs != nil {
		sdb, err := s.sandboxDBs.Get(sb.ID)
		if err == nil {
			_ = sdb.LogEvent("created", map[string]string{
				"sandbox_id": sb.ID,
				"template":   cfg.Template,
			})
		}
	}

	// Issue sandbox-scoped JWT for combined mode (24h TTL — independent of sandbox idle timeout)
	if s.jwtIssuer != nil {
		token, err := s.jwtIssuer.IssueSandboxToken(orgID, sb.ID, s.workerID, 24*time.Hour)
		if err == nil {
			sb.Token = token
		}
	}

	// Write session record to PG if available
	if s.store != nil && hasOrg {
		cfgJSON, _ := json.Marshal(cfg)
		metadataJSON, _ := json.Marshal(cfg.Metadata)
		region := s.region
		if region == "" {
			region = "local"
		}
		workerID := s.workerID
		if workerID == "" {
			workerID = "w-local-1"
		}
		template := cfg.Template
		if template == "" {
			template = "base"
		}
		_, _ = s.store.CreateSandboxSession(ctx, sb.ID, orgID, nil, template, region, workerID, cfgJSON, metadataJSON)
	}

	return c.JSON(http.StatusCreated, sb)
}

// createSandboxRemote dispatches sandbox creation to a remote worker via gRPC.
func (s *Server) createSandboxRemote(c echo.Context, ctx context.Context, cfg types.SandboxConfig, orgID [16]byte, hasOrg bool) error {
	// Select region (explicit header, or default to server's region)
	region := c.Request().Header.Get("Fly-Region")
	if region == "" {
		region = s.region
	}
	if region == "" {
		region = "iad"
	}

	worker, grpcClient, err := s.workerRegistry.GetLeastLoadedWorker(region)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "no workers available: " + err.Error(),
		})
	}

	// Resolve template image from DB (org-scoped lookup with public fallback)
	var imageRef string
	if s.store != nil && hasOrg {
		tmpl, err := s.store.GetTemplateByName(ctx, orgID, cfg.Template)
		if err == nil {
			imageRef = tmpl.ImageRef
		}
	}

	// Dispatch via persistent gRPC connection
	grpcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	grpcResp, err := grpcClient.CreateSandbox(grpcCtx, &pb.CreateSandboxRequest{
		Template:       cfg.Template,
		Timeout:        int32(cfg.Timeout),
		Envs:           cfg.Envs,
		MemoryMb:       int32(cfg.MemoryMB),
		CpuCount:       int32(cfg.CpuCount),
		NetworkEnabled: cfg.NetworkEnabled,
		ImageRef:       imageRef,
		Port:           int32(cfg.Port),
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "worker create failed: " + err.Error(),
		})
	}

	// Issue sandbox-scoped JWT (24h TTL — independent of sandbox idle timeout)
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, grpcResp.SandboxId, worker.ID, 24*time.Hour)
		if err != nil {
			log.Printf("sandbox: failed to issue JWT: %v", err)
		} else {
			token = t
		}
	}

	// Record session in PG
	if s.store != nil && hasOrg {
		template := cfg.Template
		if template == "" {
			template = "base"
		}
		cfgJSON, _ := json.Marshal(cfg)
		metadataJSON, _ := json.Marshal(cfg.Metadata)
		_, _ = s.store.CreateSandboxSession(ctx, grpcResp.SandboxId, orgID, nil, template, region, worker.ID, cfgJSON, metadataJSON)
	}

	resp := map[string]interface{}{
		"sandboxID":  grpcResp.SandboxId,
		"connectURL": worker.HTTPAddr,
		"token":      token,
		"status":     grpcResp.Status,
		"region":     region,
		"workerID":   worker.ID,
	}

	return c.JSON(http.StatusCreated, resp)
}

func (s *Server) getSandbox(c echo.Context) error {
	id := c.Param("id")

	// Server mode with worker registry: look up from PG and issue fresh token
	if s.workerRegistry != nil {
		return s.getSandboxRemote(c, id)
	}

	// Combined/worker mode: look up locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sb, err := s.manager.Get(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	orgID, hasOrg := auth.GetOrgID(c)
	if s.jwtIssuer != nil {
		if hasOrg {
			token, err := s.jwtIssuer.IssueSandboxToken(orgID, id, s.workerID, 24*time.Hour)
			if err == nil {
				sb.Token = token
			}
		}
	}

	return c.JSON(http.StatusOK, sb)
}

// getSandboxRemote looks up a sandbox via the PG session record and returns
// the worker's connectURL + a fresh JWT.
func (s *Server) getSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "sandbox not found",
		})
	}

	orgID, _ := auth.GetOrgID(c)

	// Hibernated sandboxes have no worker
	if session.Status == "hibernated" {
		resp := map[string]interface{}{
			"sandboxID": sandboxID,
			"status":    "hibernated",
			"region":    session.Region,
			"template":  session.Template,
			"startedAt": session.StartedAt,
		}
		return c.JSON(http.StatusOK, resp)
	}

	// Look up worker address
	worker := s.workerRegistry.GetWorker(session.WorkerID)
	connectURL := ""
	if worker != nil {
		connectURL = worker.HTTPAddr
	}

	// Issue a fresh token
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, session.WorkerID, 24*time.Hour)
		if err == nil {
			token = t
		}
	}

	resp := map[string]interface{}{
		"sandboxID":  sandboxID,
		"connectURL": connectURL,
		"token":      token,
		"status":     session.Status,
		"region":     session.Region,
		"workerID":   session.WorkerID,
		"startedAt":  session.StartedAt,
		"template":   session.Template,
	}

	return c.JSON(http.StatusOK, resp)
}

func (s *Server) killSandbox(c echo.Context) error {
	id := c.Param("id")

	// Server mode with worker registry: dispatch destroy via gRPC
	if s.workerRegistry != nil {
		return s.killSandboxRemote(c, id)
	}

	// Combined/worker mode: kill locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	if err := s.manager.Kill(c.Request().Context(), id); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Unregister from sandbox router
	if s.router != nil {
		s.router.Unregister(id)
	}

	if s.store != nil {
		_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), id, "stopped", nil)
		s.cleanupPreviewURLs(c.Request().Context(), id)
	}

	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(id)
	}

	return c.NoContent(http.StatusNoContent)
}

// killSandboxRemote dispatches sandbox destruction to the appropriate worker via gRPC.
func (s *Server) killSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "sandbox not found",
		})
	}

	// Attempt gRPC destroy (best-effort)
	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		// Worker is unreachable — mark as error in PG
		log.Printf("sandbox: worker %s unreachable for destroy: %v", session.WorkerID, err)
		errMsg := "worker unreachable"
		_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "error", &errMsg)
		return c.NoContent(http.StatusNoContent)
	}

	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()

	if _, err := client.DestroySandbox(grpcCtx, &pb.DestroySandboxRequest{SandboxId: sandboxID}); err != nil {
		log.Printf("sandbox: gRPC destroy failed for %s: %v", sandboxID, err)
	}

	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "stopped", nil)
	s.cleanupPreviewURLs(c.Request().Context(), sandboxID)

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listSandboxes(c echo.Context) error {
	// Server mode with worker registry: query PG for org's running sandboxes
	if s.workerRegistry != nil {
		return s.listSandboxesRemote(c)
	}

	// Combined/worker mode: list locally
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sandboxes, err := s.manager.List(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, sandboxes)
}

// listSandboxesRemote queries PG for the org's running sandboxes and returns
// connectURL + fresh JWT for each.
func (s *Server) listSandboxesRemote(c echo.Context) error {
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

	sessions, err := s.store.ListSandboxSessions(c.Request().Context(), orgID, "running", 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	result := make([]map[string]interface{}, 0, len(sessions))
	for _, sess := range sessions {
		entry := map[string]interface{}{
			"sandboxID": sess.SandboxID,
			"status":    sess.Status,
			"region":    sess.Region,
			"workerID":  sess.WorkerID,
			"template":  sess.Template,
			"startedAt": sess.StartedAt,
		}

		// Attach connectURL from registry
		worker := s.workerRegistry.GetWorker(sess.WorkerID)
		if worker != nil {
			entry["connectURL"] = worker.HTTPAddr
		}

		// Issue fresh JWT
		if s.jwtIssuer != nil {
			token, err := s.jwtIssuer.IssueSandboxToken(orgID, sess.SandboxID, sess.WorkerID, 24*time.Hour)
			if err == nil {
				entry["token"] = token
			}
		}

		result = append(result, entry)
	}

	return c.JSON(http.StatusOK, result)
}

func (s *Server) setTimeout(c echo.Context) error {
	// In server mode, timeout must be set directly on the worker via connectURL
	if s.workerRegistry != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "timeout must be set directly on the worker via connectURL",
		})
	}

	if s.router == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")

	var req types.TimeoutRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	if req.Timeout <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "timeout must be positive",
		})
	}

	s.router.SetTimeout(id, time.Duration(req.Timeout)*time.Second)

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) hibernateSandbox(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	// Server mode: dispatch to worker via gRPC
	if s.workerRegistry != nil {
		return s.hibernateSandboxRemote(c, id)
	}

	// Combined mode: hibernate locally
	if s.manager == nil || s.checkpointStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "hibernation not available",
		})
	}

	result, err := s.manager.Hibernate(ctx, id, s.checkpointStore)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Mark hibernated in sandbox router
	if s.router != nil {
		timeout := 600 // default for explicit hibernate
		s.router.MarkHibernated(id, time.Duration(timeout)*time.Second)
	}

	// Record checkpoint in PG
	orgID, hasOrg := auth.GetOrgID(c)
	if s.store != nil && hasOrg {
		session, _ := s.store.GetSandboxSession(ctx, id)
		cfg := json.RawMessage("{}")
		if session != nil {
			cfg = session.Config
		}
		template := "base"
		region := s.region
		if session != nil {
			template = session.Template
			region = session.Region
		}
		_, _ = s.store.CreateCheckpoint(ctx, id, orgID, result.CheckpointKey, result.SizeBytes, region, template, cfg)
		_ = s.store.UpdateSandboxSessionStatus(ctx, id, "hibernated", nil)
	}

	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(id)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":     id,
		"status":        "hibernated",
		"checkpointKey": result.CheckpointKey,
		"sizeBytes":     result.SizeBytes,
	})
}

func (s *Server) hibernateSandboxRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox is not running"})
	}

	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker unreachable"})
	}

	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()

	grpcResp, err := client.HibernateSandbox(grpcCtx, &pb.HibernateSandboxRequest{
		SandboxId: sandboxID,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "hibernate failed: " + err.Error(),
		})
	}

	// Record checkpoint in PG
	orgID, _ := auth.GetOrgID(c)
	_, _ = s.store.CreateCheckpoint(c.Request().Context(), sandboxID, orgID,
		grpcResp.CheckpointKey, grpcResp.SizeBytes,
		session.Region, session.Template, session.Config)
	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "hibernated", nil)

	resp := map[string]interface{}{
		"sandboxID":     sandboxID,
		"status":        "hibernated",
		"checkpointKey": grpcResp.CheckpointKey,
		"sizeBytes":     grpcResp.SizeBytes,
	}

	return c.JSON(http.StatusOK, resp)
}

func (s *Server) wakeSandbox(c echo.Context) error {
	id := c.Param("id")
	ctx := c.Request().Context()

	var req types.WakeRequest
	_ = c.Bind(&req)

	// Server mode: pick any worker, dispatch via gRPC
	if s.workerRegistry != nil {
		return s.wakeSandboxRemote(c, id, req)
	}

	// Combined mode: wake locally
	if s.manager == nil || s.checkpointStore == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "hibernation not available",
		})
	}

	// Get checkpoint key from PG
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	checkpoint, err := s.store.GetActiveCheckpoint(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active checkpoint found"})
	}

	sb, err := s.manager.Wake(ctx, id, checkpoint.CheckpointKey, s.checkpointStore, req.Timeout)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Register with sandbox router after explicit wake
	if s.router != nil {
		timeout := req.Timeout
		if timeout <= 0 {
			timeout = 300
		}
		s.router.Register(id, time.Duration(timeout)*time.Second)
	}

	_ = s.store.MarkCheckpointRestored(ctx, id)
	_ = s.store.UpdateSandboxSessionForWake(ctx, id, s.workerID)

	// Issue fresh JWT
	orgID, _ := auth.GetOrgID(c)
	if s.jwtIssuer != nil {
		token, err := s.jwtIssuer.IssueSandboxToken(orgID, id, s.workerID, 24*time.Hour)
		if err == nil {
			sb.Token = token
		}
	}

	sb.ConnectURL = s.httpAddr

	return c.JSON(http.StatusOK, sb)
}

func (s *Server) wakeSandboxRemote(c echo.Context, sandboxID string, req types.WakeRequest) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	checkpoint, err := s.store.GetActiveCheckpoint(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active checkpoint found"})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox session not found"})
	}
	if session.Status != "hibernated" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox is not hibernated"})
	}

	// Pick ANY worker in the same region
	region := checkpoint.Region
	worker, grpcClient, err := s.workerRegistry.GetLeastLoadedWorker(region)
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "no workers available in region " + region,
		})
	}

	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), 60*time.Second)
	defer cancel()

	grpcResp, err := grpcClient.WakeSandbox(grpcCtx, &pb.WakeSandboxRequest{
		SandboxId:     sandboxID,
		CheckpointKey: checkpoint.CheckpointKey,
		Timeout:       int32(req.Timeout),
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "wake failed: " + err.Error(),
		})
	}

	// Mark checkpoint as restored, update session
	_ = s.store.MarkCheckpointRestored(c.Request().Context(), sandboxID)
	_ = s.store.UpdateSandboxSessionForWake(c.Request().Context(), sandboxID, worker.ID)

	// Issue fresh JWT
	orgID, _ := auth.GetOrgID(c)
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 300
	}
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, worker.ID, 24*time.Hour)
		if err == nil {
			token = t
		}
	}

	resp := map[string]interface{}{
		"sandboxID":  sandboxID,
		"connectURL": worker.HTTPAddr,
		"token":      token,
		"status":     grpcResp.Status,
		"region":     region,
		"workerID":   worker.ID,
	}

	return c.JSON(http.StatusOK, resp)
}

// listSessions returns session history from PostgreSQL.
func (s *Server) listWorkers(c echo.Context) error {
	if s.workerRegistry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "worker registry not available (server mode only)",
		})
	}
	return c.JSON(http.StatusOK, s.workerRegistry.GetAllWorkers())
}

func (s *Server) listSessions(c echo.Context) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "session history requires database configuration",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	status := c.QueryParam("status")
	sessions, err := s.store.ListSandboxSessions(c.Request().Context(), orgID, status, 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, sessions)
}

// --- Preview URL handlers ---

// createPreviewURL creates an on-demand preview URL for a running sandbox
// targeting a specific container port. Hostname format: {sandboxID}-p{port}.{baseDomain}
func (s *Server) createPreviewURL(c echo.Context) error {
	sandboxID := c.Param("id")
	ctx := c.Request().Context()

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

	// Parse request body — port is required
	var req struct {
		Port       int             `json:"port"`
		Domain     string          `json:"domain"`
		AuthConfig json.RawMessage `json:"authConfig"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}
	if req.Port < 1 || req.Port > 65535 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "port must be between 1 and 65535",
		})
	}
	if req.AuthConfig == nil {
		req.AuthConfig = json.RawMessage("{}")
	}

	// Verify sandbox is running
	sandboxRunning := false
	if s.manager != nil {
		if _, err := s.manager.Get(ctx, sandboxID); err == nil {
			sandboxRunning = true
		}
	}
	if !sandboxRunning && s.store != nil {
		session, err := s.store.GetSandboxSession(ctx, sandboxID)
		if err == nil && session.Status == "running" && session.OrgID == orgID {
			sandboxRunning = true
		}
	}
	if !sandboxRunning {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "sandbox is not running or not found",
		})
	}

	// Look up org for custom domain support
	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to look up org",
		})
	}
	var customDomain string
	if org.CustomDomain != nil && *org.CustomDomain != "" {
		customDomain = *org.CustomDomain
	}

	// If preview URL already exists for this port, return it
	existing, err := s.store.GetPreviewURLByPort(ctx, sandboxID, req.Port)
	if err == nil && existing != nil {
		return c.JSON(http.StatusOK, previewURLToMap(*existing, customDomain))
	}

	// Determine hostname based on whether a custom domain was requested
	var hostname string
	var cfHostnameID *string

	if req.Domain != "" {
		// Validate the requested domain matches the org's verified custom domain
		if customDomain == "" {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": "org has no custom domain configured",
			})
		}
		if req.Domain != customDomain {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("domain %q does not match org custom domain %q", req.Domain, customDomain),
			})
		}
		if org.DomainVerificationStatus != "active" {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("custom domain %q is not verified (status: %s)", req.Domain, org.DomainVerificationStatus),
			})
		}

		hostname = fmt.Sprintf("%s-p%d.%s", sandboxID, req.Port, req.Domain)

		// Register with Cloudflare if configured
		if s.cfClient != nil {
			cfResult, err := s.cfClient.CreateCustomHostnameHTTP(hostname)
			if err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{
					"error": "failed to register custom hostname with Cloudflare: " + err.Error(),
				})
			}
			cfHostnameID = &cfResult.ID
		}
	} else {
		// Default: use the platform sandbox domain
		hostname = fmt.Sprintf("%s-p%d.%s", sandboxID, req.Port, s.sandboxDomain)
	}

	previewURL, err := s.store.CreatePreviewURL(ctx, sandboxID, orgID, hostname, req.Port, cfHostnameID, "active", req.AuthConfig)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusCreated, previewURLToMap(*previewURL, customDomain))
}

// listPreviewURLs returns all preview URLs for a sandbox.
func (s *Server) listPreviewURLs(c echo.Context) error {
	sandboxID := c.Param("id")

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, _ := auth.GetOrgID(c)
	customDomain := s.getOrgCustomDomain(c.Request().Context(), orgID)

	urls, err := s.store.ListPreviewURLs(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	result := make([]map[string]interface{}, len(urls))
	for i, u := range urls {
		result[i] = previewURLToMap(u, customDomain)
	}

	return c.JSON(http.StatusOK, result)
}

// deletePreviewURL removes the preview URL for a sandbox on a specific port.
func (s *Server) deletePreviewURL(c echo.Context) error {
	sandboxID := c.Param("id")
	portStr := c.Param("port")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port < 1 || port > 65535 {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid port",
		})
	}

	previewURL, err := s.store.GetPreviewURLByPort(ctx, sandboxID, port)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "no preview URL for this port",
		})
	}

	// Delete from Cloudflare if applicable (for legacy custom domain URLs)
	if s.cfClient != nil && previewURL.CFHostnameID != nil && *previewURL.CFHostnameID != "" {
		if err := s.cfClient.DeleteCustomHostname(*previewURL.CFHostnameID); err != nil {
			log.Printf("preview: failed to delete CF hostname %s: %v", *previewURL.CFHostnameID, err)
		}
	}

	_ = s.store.DeletePreviewURL(ctx, previewURL.ID)

	return c.NoContent(http.StatusNoContent)
}

// previewURLToMap converts a PreviewURL to a response map, including customHostname if provided.
func previewURLToMap(u db.PreviewURL, customDomain string) map[string]interface{} {
	m := map[string]interface{}{
		"id":         u.ID,
		"sandboxId":  u.SandboxID,
		"orgId":      u.OrgID,
		"hostname":   u.Hostname,
		"port":       u.Port,
		"sslStatus":  u.SSLStatus,
		"authConfig": u.AuthConfig,
		"createdAt":  u.CreatedAt,
	}
	if u.CFHostnameID != nil {
		m["cfHostnameId"] = *u.CFHostnameID
	}
	if customDomain != "" {
		if dot := strings.Index(u.Hostname, "."); dot > 0 {
			m["customHostname"] = u.Hostname[:dot+1] + customDomain
		}
	}
	return m
}

// getOrgCustomDomain returns the org's custom domain, or "" if none.
func (s *Server) getOrgCustomDomain(ctx context.Context, orgID uuid.UUID) string {
	if s.store == nil {
		return ""
	}
	org, err := s.store.GetOrg(ctx, orgID)
	if err == nil && org.CustomDomain != nil && *org.CustomDomain != "" {
		return *org.CustomDomain
	}
	return ""
}

// cleanupPreviewURLs removes all preview URLs for a sandbox on kill (best-effort).
func (s *Server) cleanupPreviewURLs(ctx context.Context, sandboxID string) {
	if s.store == nil {
		return
	}
	urls, err := s.store.DeletePreviewURLsBySandbox(ctx, sandboxID)
	if err != nil {
		return
	}
	for _, u := range urls {
		if s.cfClient != nil && u.CFHostnameID != nil && *u.CFHostnameID != "" {
			if err := s.cfClient.DeleteCustomHostname(*u.CFHostnameID); err != nil {
				log.Printf("preview: cleanup failed for CF hostname %s: %v", *u.CFHostnameID, err)
			}
		}
	}
}
