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

	// Declarative image or named snapshot → resolve to checkpoint and use createFromCheckpoint flow
	if len(cfg.ImageManifest) > 0 || cfg.Snapshot != "" {
		if !hasOrg {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required for image/snapshot creation"})
		}

		// Check if the client wants build log streaming (SSE)
		wantsSSE := c.Request().Header.Get("Accept") == "text/event-stream"

		if wantsSSE {
			return s.createSandboxWithSSE(c, ctx, orgID, cfg)
		}

		// Non-streaming path
		var checkpointID uuid.UUID
		var err error

		if cfg.Snapshot != "" {
			checkpointID, err = s.resolveSnapshot(ctx, orgID, cfg.Snapshot)
		} else {
			checkpointID, err = s.resolveImageManifest(ctx, orgID, cfg.ImageManifest, nil)
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}

		c.SetParamNames("checkpointId")
		c.SetParamValues(checkpointID.String())
		return s.createFromCheckpoint(c)
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
			template = "default"
		}
		_, _ = s.store.CreateSandboxSession(ctx, sb.ID, orgID, nil, template, region, workerID, cfgJSON, metadataJSON)
	}

	return c.JSON(http.StatusCreated, sb)
}

// createSandboxWithSSE handles sandbox creation with SSE build log streaming.
// Streams build_log events during image build, then emits the final result event.
func (s *Server) createSandboxWithSSE(c echo.Context, ctx context.Context, orgID uuid.UUID, cfg types.SandboxConfig) error {
	flusher, ok := c.Response().Writer.(http.Flusher)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
	}

	// Set SSE headers
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)
	flusher.Flush()

	// Helper to emit SSE events
	emit := func(eventType string, payload interface{}) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(c.Response(), "event: %s\ndata: %s\n\n", eventType, data)
		flusher.Flush()
	}

	// Build log callback — emits SSE events during image build
	logFn := BuildLogFunc(func(step int, stepType string, message string) {
		emit("build_log", map[string]interface{}{
			"step":    step,
			"type":    stepType,
			"message": message,
		})
	})

	// Resolve image or snapshot to checkpoint ID
	var checkpointID uuid.UUID
	var err error

	if cfg.Snapshot != "" {
		emit("build_log", map[string]interface{}{"step": 0, "type": "resolve", "message": "Resolving snapshot '" + cfg.Snapshot + "'..."})
		checkpointID, err = s.resolveSnapshot(ctx, orgID, cfg.Snapshot)
	} else {
		checkpointID, err = s.resolveImageManifest(ctx, orgID, cfg.ImageManifest, logFn)
	}
	if err != nil {
		emit("error", map[string]string{"error": err.Error()})
		return nil
	}

	// Create sandbox from checkpoint
	emit("build_log", map[string]interface{}{"step": 0, "type": "create", "message": "Creating sandbox from image..."})

	c.SetParamNames("checkpointId")
	c.SetParamValues(checkpointID.String())
	result, _, cpErr := s.createFromCheckpointCore(c)
	if cpErr != nil {
		emit("error", map[string]string{"error": cpErr.Error()})
		return nil
	}

	emit("result", result)
	return nil
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

	// Resolve template from DB (org-scoped lookup with public fallback)
	var templateRootfsKey, templateWorkspaceKey string
	var templateID *uuid.UUID
	if s.store != nil && hasOrg {
		tmpl, err := s.store.GetTemplateByName(ctx, orgID, cfg.Template)
		if err == nil {
			templateID = &tmpl.ID
			log.Printf("sandbox: resolved template %q (type=%s, id=%s)", cfg.Template, tmpl.TemplateType, tmpl.ID)
			// Sandbox-type templates provide S3 drive keys instead of ECR image refs
			if tmpl.TemplateType == "sandbox" && tmpl.RootfsS3Key != nil && tmpl.WorkspaceS3Key != nil {
				templateRootfsKey = *tmpl.RootfsS3Key
				templateWorkspaceKey = *tmpl.WorkspaceS3Key
				log.Printf("sandbox: using snapshot template drives: rootfs=%s, workspace=%s", templateRootfsKey, templateWorkspaceKey)
			}
		} else {
			log.Printf("sandbox: template %q lookup failed: %v", cfg.Template, err)
		}
	}

	// Dispatch via persistent gRPC connection.
	// Template-based creation needs more time for S3 download + decompression of drives.
	grpcTimeout := 30 * time.Second
	if templateRootfsKey != "" {
		grpcTimeout = 120 * time.Second
	}
	grpcCtx, cancel := context.WithTimeout(ctx, grpcTimeout)
	defer cancel()

	grpcResp, err := grpcClient.CreateSandbox(grpcCtx, &pb.CreateSandboxRequest{
		Template:             cfg.Template,
		Timeout:              int32(cfg.Timeout),
		Envs:                 cfg.Envs,
		MemoryMb:             int32(cfg.MemoryMB),
		CpuCount:             int32(cfg.CpuCount),
		NetworkEnabled:       cfg.NetworkEnabled,
		Port:                 int32(cfg.Port),
		TemplateRootfsKey:    templateRootfsKey,
		TemplateWorkspaceKey: templateWorkspaceKey,
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
			template = "default"
		}
		cfgJSON, _ := json.Marshal(cfg)
		metadataJSON, _ := json.Marshal(cfg.Metadata)
		_, _ = s.store.CreateSandboxSession(ctx, grpcResp.SandboxId, orgID, nil, template, region, worker.ID, cfgJSON, metadataJSON)
		if templateID != nil {
			_ = s.store.UpdateSandboxSessionTemplate(ctx, grpcResp.SandboxId, *templateID)
		}
	}

	resp := map[string]interface{}{
		"sandboxID": grpcResp.SandboxId,
		"token":     token,
		"status":    grpcResp.Status,
		"region":    region,
		"workerID":  worker.ID,
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

	// Issue a fresh token
	var token string
	if s.jwtIssuer != nil {
		t, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, session.WorkerID, 24*time.Hour)
		if err == nil {
			token = t
		}
	}

	resp := map[string]interface{}{
		"sandboxID": sandboxID,
		"token":     token,
		"status":    session.Status,
		"region":    session.Region,
		"workerID":  session.WorkerID,
		"startedAt": session.StartedAt,
		"template":  session.Template,
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
		_, _ = s.store.CreateHibernation(ctx, id, orgID, result.HibernationKey, result.SizeBytes, region, template, cfg)
		_ = s.store.UpdateSandboxSessionStatus(ctx, id, "hibernated", nil)
	}

	if s.sandboxDBs != nil {
		_ = s.sandboxDBs.Remove(id)
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"sandboxID":      id,
		"status":         "hibernated",
		"hibernationKey": result.HibernationKey,
		"sizeBytes":      result.SizeBytes,
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

	// Record hibernation in PG
	orgID, _ := auth.GetOrgID(c)
	_, _ = s.store.CreateHibernation(c.Request().Context(), sandboxID, orgID,
		grpcResp.CheckpointKey, grpcResp.SizeBytes,
		session.Region, session.Template, session.Config)
	_ = s.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "hibernated", nil)

	resp := map[string]interface{}{
		"sandboxID":      sandboxID,
		"status":         "hibernated",
		"hibernationKey": grpcResp.CheckpointKey,
		"sizeBytes":      grpcResp.SizeBytes,
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

	hibernation, err := s.store.GetActiveHibernation(ctx, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active hibernation found"})
	}

	sb, err := s.manager.Wake(ctx, id, hibernation.HibernationKey, s.checkpointStore, req.Timeout)
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

	_ = s.store.MarkHibernationRestored(ctx, id)
	_ = s.store.UpdateSandboxSessionForWake(ctx, id, s.workerID)

	// Apply pending checkpoint patches in background
	go s.applyPendingPatches(id, s.workerID)

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

	hibernation, err := s.store.GetActiveHibernation(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "no active hibernation found"})
	}

	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox session not found"})
	}
	if session.Status != "hibernated" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox is not hibernated"})
	}

	// Pick ANY worker in the same region
	region := hibernation.Region
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
		CheckpointKey: hibernation.HibernationKey,
		Timeout:       int32(req.Timeout),
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "wake failed: " + err.Error(),
		})
	}

	// Mark hibernation as restored, update session
	_ = s.store.MarkHibernationRestored(c.Request().Context(), sandboxID)
	_ = s.store.UpdateSandboxSessionForWake(c.Request().Context(), sandboxID, worker.ID)

	// Apply pending checkpoint patches in background
	go s.applyPendingPatches(sandboxID, worker.ID)

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

// --- Checkpoint handlers ---

// createCheckpoint creates a named checkpoint of a running sandbox.
func (s *Server) createCheckpoint(c echo.Context) error {
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

	// Verify sandbox is running and belongs to org
	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "sandbox does not belong to this organization"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox must be running to create a checkpoint"})
	}

	// Parse request body
	var req struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}

	// Enforce max 10 checkpoints per sandbox
	count, err := s.store.CountCheckpoints(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to count checkpoints"})
	}
	if count >= 10 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "maximum 10 checkpoints per sandbox"})
	}

	// Reserve a checkpoint UUID
	checkpointID := uuid.New()

	// Create DB record (status='processing')
	cp := &db.Checkpoint{
		ID:            checkpointID,
		SandboxID:     sandboxID,
		OrgID:         orgID,
		Name:          req.Name,
		Status:        "processing",
		SandboxConfig: session.Config,
	}
	if err := s.store.CreateCheckpoint(ctx, cp); err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			return c.JSON(http.StatusConflict, map[string]string{"error": "checkpoint name already exists for this sandbox"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create checkpoint: " + err.Error()})
	}

	// Dispatch checkpoint creation in background — return immediately with status "processing".
	// The heavy work (SyncFS + pause + memory snapshot + drive copy + resume + S3 upload)
	// runs async. Clients poll listCheckpoints for status=ready.
	if s.workerRegistry != nil {
		grpcClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker not available: " + err.Error()})
		}

		go func() {
			grpcCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			grpcResp, err := grpcClient.CreateCheckpoint(grpcCtx, &pb.CreateCheckpointRequest{
				SandboxId:    sandboxID,
				CheckpointId: checkpointID.String(),
			})
			if err != nil {
				log.Printf("api: async checkpoint %s failed: %v", checkpointID, err)
				_ = s.store.SetCheckpointFailed(context.Background(), checkpointID, err.Error())
				return
			}
			_ = s.store.SetCheckpointReady(context.Background(), checkpointID, grpcResp.RootfsS3Key, grpcResp.WorkspaceS3Key, 0)
			log.Printf("api: checkpoint %s ready", checkpointID)
		}()
	} else if s.manager != nil {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			rootfsKey, workspaceKey, err := s.manager.CreateCheckpoint(bgCtx, sandboxID, checkpointID.String(), s.checkpointStore, func() {})
			if err != nil {
				log.Printf("api: async checkpoint %s failed: %v", checkpointID, err)
				_ = s.store.SetCheckpointFailed(context.Background(), checkpointID, err.Error())
				return
			}
			_ = s.store.SetCheckpointReady(context.Background(), checkpointID, rootfsKey, workspaceKey, 0)
			log.Printf("api: checkpoint %s ready", checkpointID)
		}()
	} else {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	return c.JSON(http.StatusCreated, cp)
}

// listCheckpoints returns all checkpoints for a sandbox.
func (s *Server) listCheckpoints(c echo.Context) error {
	sandboxID := c.Param("id")

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	// Verify sandbox belongs to org
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "sandbox does not belong to this organization"})
	}

	checkpoints, err := s.store.ListCheckpoints(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, checkpoints)
}

// restoreCheckpoint restores a sandbox to a checkpoint (in-place revert).
func (s *Server) restoreCheckpoint(c echo.Context) error {
	sandboxID := c.Param("id")
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify sandbox belongs to org and is running
	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "sandbox does not belong to this organization"})
	}
	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "sandbox must be running to restore a checkpoint"})
	}

	// Verify checkpoint exists, belongs to this sandbox, and is ready
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.SandboxID != sandboxID {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint does not belong to this sandbox"})
	}
	if cp.Status != "ready" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint is not ready (status: " + cp.Status + ")"})
	}

	// Dispatch restore in background — return immediately.
	// Commands will block until restore completes.
	pending := &pendingCreate{ready: make(chan struct{})}
	s.pendingCreates.Store(sandboxID, pending)

	if s.workerRegistry != nil {
		grpcClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
		if err != nil {
			s.pendingCreates.Delete(sandboxID)
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "worker not available: " + err.Error()})
		}

		go func() {
			grpcCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			_, restoreErr := grpcClient.RestoreCheckpoint(grpcCtx, &pb.RestoreCheckpointRequest{
				SandboxId:    sandboxID,
				CheckpointId: checkpointID.String(),
			})
			if restoreErr != nil {
				log.Printf("api: async restore %s/%s failed: %v", sandboxID, checkpointID, restoreErr)
			}
			pending.err = restoreErr
			close(pending.ready)
		}()
	} else if s.manager != nil {
		go func() {
			restoreErr := s.manager.RestoreFromCheckpoint(context.Background(), sandboxID, checkpointID.String())
			if restoreErr != nil {
				log.Printf("api: async restore %s/%s failed: %v", sandboxID, checkpointID, restoreErr)
			}
			pending.err = restoreErr
			close(pending.ready)
		}()
	} else {
		s.pendingCreates.Delete(sandboxID)
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	return c.JSON(http.StatusOK, map[string]string{
		"sandboxId":    sandboxID,
		"checkpointId": checkpointID.String(),
		"status":       "restoring",
	})
}

// createFromCheckpoint creates a new sandbox from an existing checkpoint (fork).
func (s *Server) createFromCheckpoint(c echo.Context) error {
	result, httpStatus, err := s.createFromCheckpointCore(c)
	if err != nil {
		return c.JSON(httpStatus, map[string]string{"error": err.Error()})
	}
	return c.JSON(httpStatus, result)
}

// createFromCheckpointCore contains the core logic for creating a sandbox from a checkpoint.
// Returns the result map, HTTP status, or an error.
func (s *Server) createFromCheckpointCore(c echo.Context) (map[string]interface{}, int, error) {
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("database not configured")
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return nil, http.StatusUnauthorized, fmt.Errorf("org context required")
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid checkpoint ID")
	}

	// Verify checkpoint exists, belongs to org, and is ready
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("checkpoint not found")
	}
	if cp.OrgID != orgID {
		return nil, http.StatusForbidden, fmt.Errorf("checkpoint does not belong to this organization")
	}
	if cp.Status != "ready" {
		return nil, http.StatusBadRequest, fmt.Errorf("checkpoint is not ready (status: %s)", cp.Status)
	}

	// Parse optional overrides from request body
	var req struct {
		Timeout int `json:"timeout"`
	}
	_ = c.Bind(&req)

	// Get S3 keys from the checkpoint
	if cp.RootfsS3Key == nil || cp.WorkspaceS3Key == nil {
		return nil, http.StatusBadRequest, fmt.Errorf("checkpoint S3 keys not available")
	}

	// Parse the original sandbox config to reuse settings
	var originalCfg types.SandboxConfig
	_ = json.Unmarshal(cp.SandboxConfig, &originalCfg)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = originalCfg.Timeout
	}
	if timeout <= 0 {
		timeout = 300
	}

	// Unified async fork: return immediately, boot VM in background.
	// First command from SDK will block until VM is ready.

	// Pre-generate sandbox ID
	sandboxID := "sb-" + uuid.New().String()[:8]

	// Determine execution target
	region := s.region
	if region == "" {
		region = "iad"
	}
	var workerID string
	var grpcClient pb.SandboxWorkerClient

	if s.workerRegistry != nil {
		// Server mode: pick a worker
		worker, client, wErr := s.workerRegistry.GetLeastLoadedWorker(region)
		if wErr != nil {
			return nil, http.StatusServiceUnavailable, fmt.Errorf("no workers available: %w", wErr)
		}
		workerID = worker.ID
		grpcClient = client
	} else if s.manager != nil {
		// Combined mode: local execution
		workerID = s.workerID
	} else {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("sandbox execution not available in server-only mode")
	}

	// Register pending create — commands will wait until ready
	pending := &pendingCreate{ready: make(chan struct{})}
	s.pendingCreates.Store(sandboxID, pending)

	// Also register with sandbox router if available (combined mode)
	if s.router != nil {
		s.router.RegisterCreating(sandboxID, time.Duration(timeout)*time.Second)
	}

	// Issue JWT immediately
	var token string
	if s.jwtIssuer != nil {
		t, jwtErr := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, workerID, 24*time.Hour)
		if jwtErr == nil {
			token = t
		}
	}

	// Record session immediately
	if s.store != nil {
		template := originalCfg.Template
		if template == "" {
			template = "default"
		}
		cfgJSON, _ := json.Marshal(originalCfg)
		metadataJSON, _ := json.Marshal(originalCfg.Metadata)
		_, _ = s.store.CreateSandboxSession(ctx, sandboxID, orgID, nil, template, region, workerID, cfgJSON, metadataJSON)
		// Track checkpoint lineage for patch system
		_ = s.store.SetSandboxCheckpointID(ctx, sandboxID, checkpointID)
	}

	// Boot VM in background
	go func() {
		createCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()

		var createErr error

		if grpcClient != nil {
			// Server mode: dispatch to worker via gRPC
			_, createErr = grpcClient.CreateSandbox(createCtx, &pb.CreateSandboxRequest{
				Template:             originalCfg.Template,
				Timeout:              int32(timeout),
				Envs:                 originalCfg.Envs,
				MemoryMb:             int32(originalCfg.MemoryMB),
				CpuCount:             int32(originalCfg.CpuCount),
				NetworkEnabled:       originalCfg.NetworkEnabled,
				Port:                 int32(originalCfg.Port),
				TemplateRootfsKey:    *cp.RootfsS3Key,
				TemplateWorkspaceKey: *cp.WorkspaceS3Key,
				CheckpointId:         checkpointID.String(),
				SandboxId:            sandboxID,
			})
		} else {
			// Combined mode: create locally
			cfg := originalCfg
			cfg.Timeout = timeout
			cfg.TemplateRootfsKey = *cp.RootfsS3Key
			cfg.TemplateWorkspaceKey = *cp.WorkspaceS3Key
			cfg.SandboxID = sandboxID

			forkMgr, hasFork := s.manager.(interface {
				ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error)
			})
			if hasFork {
				_, createErr = forkMgr.ForkFromCheckpoint(createCtx, checkpointID.String(), cfg)
			} else {
				_, createErr = s.manager.Create(createCtx, cfg)
			}
		}

		if createErr != nil {
			log.Printf("api: async fork %s failed: %v", sandboxID, createErr)
		}

		// Signal completion
		pending.err = createErr
		close(pending.ready)

		// Also signal router if available (combined mode)
		if s.router != nil {
			s.router.MarkCreated(sandboxID, createErr)
		}

		// Apply any existing patches for this checkpoint after boot
		if createErr == nil {
			s.applyPendingPatches(sandboxID, workerID)
		}
	}()

	return map[string]interface{}{
		"sandboxID":        sandboxID,
		"status":           "creating",
		"token":            token,
		"region":           region,
		"workerID":         workerID,
		"fromCheckpointId": checkpointID.String(),
	}, http.StatusCreated, nil
}

// deleteCheckpoint deletes a checkpoint.
func (s *Server) deleteCheckpoint(c echo.Context) error {
	sandboxID := c.Param("id")
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify checkpoint exists and belongs to this sandbox and org
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.SandboxID != sandboxID {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint does not belong to this sandbox"})
	}

	// Delete from DB (enforces org ownership)
	if err := s.store.DeleteCheckpoint(ctx, orgID, checkpointID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Best-effort: delete S3 objects if checkpoint store is configured
	if s.checkpointStore != nil && cp.RootfsS3Key != nil && cp.WorkspaceS3Key != nil {
		go func() {
			bgCtx := context.Background()
			if err := s.checkpointStore.Delete(bgCtx, *cp.RootfsS3Key); err != nil {
				log.Printf("checkpoint: failed to delete S3 rootfs %s: %v", *cp.RootfsS3Key, err)
			}
			if err := s.checkpointStore.Delete(bgCtx, *cp.WorkspaceS3Key); err != nil {
				log.Printf("checkpoint: failed to delete S3 workspace %s: %v", *cp.WorkspaceS3Key, err)
			}
		}()
	}

	return c.NoContent(http.StatusNoContent)
}

// --- Checkpoint Patch handlers ---

// createCheckpointPatch creates a patch for a checkpoint and fans out to running sandboxes.
func (s *Server) createCheckpointPatch(c echo.Context) error {
	checkpointIDStr := c.Param("checkpointId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify checkpoint exists and belongs to org
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}
	if cp.Status != "ready" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "checkpoint is not ready"})
	}

	var req struct {
		Script      string `json:"script"`
		Description string `json:"description"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Script == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "script is required"})
	}
	// Create the patch record (patches apply on next wake/boot)
	patch := &db.CheckpointPatch{
		ID:           uuid.New(),
		CheckpointID: checkpointID,
		Script:       req.Script,
		Description:  req.Description,
		Strategy:     "on_wake",
	}
	if err := s.store.CreateCheckpointPatch(ctx, patch); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create patch: " + err.Error()})
	}

	return c.JSON(http.StatusCreated, map[string]interface{}{
		"patch": patch,
	})
}

// execPatchOnSandbox runs a patch script on a running sandbox via gRPC exec.
func (s *Server) execPatchOnSandbox(ctx context.Context, sandboxID, workerID string, patch *db.CheckpointPatch) error {
	execCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if s.workerRegistry != nil {
		client, err := s.workerRegistry.GetWorkerClient(workerID)
		if err != nil {
			return fmt.Errorf("worker %s unreachable: %w", workerID, err)
		}
		resp, err := client.ExecCommand(execCtx, &pb.ExecCommandRequest{
			SandboxId: sandboxID,
			Command:   "bash",
			Args:      []string{"-c", patch.Script},
			Timeout:   300,
		})
		if err != nil {
			return fmt.Errorf("exec failed: %w", err)
		}
		if resp.ExitCode != 0 {
			return fmt.Errorf("patch exited with code %d: %s", resp.ExitCode, resp.Stderr)
		}
		return nil
	}

	// Combined mode: exec locally
	if s.manager != nil {
		result, err := s.manager.Exec(ctx, sandboxID, types.ProcessConfig{
			Command: "bash",
			Args:    []string{"-c", patch.Script},
			Timeout: 300,
		})
		if err != nil {
			return fmt.Errorf("exec failed: %w", err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("patch exited with code %d: %s", result.ExitCode, result.Stderr)
		}
		return nil
	}

	return fmt.Errorf("no execution backend available")
}

// applyPendingPatches checks for and applies any pending patches after a sandbox wakes.
func (s *Server) applyPendingPatches(sandboxID, workerID string) {
	if s.store == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	session, err := s.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		log.Printf("patches: %s: failed to get session: %v", sandboxID, err)
		return
	}
	if session.BasedOnCheckpointID == nil {
		return // Not based on a checkpoint, nothing to patch
	}

	patches, err := s.store.GetPendingPatches(ctx, *session.BasedOnCheckpointID, session.LastPatchSequence)
	if err != nil {
		log.Printf("patches: %s: failed to get pending patches: %v", sandboxID, err)
		return
	}
	if len(patches) == 0 {
		return
	}

	log.Printf("patches: %s: applying %d pending patches (from seq %d)", sandboxID, len(patches), session.LastPatchSequence+1)

	for _, patch := range patches {
		if err := s.execPatchOnSandbox(ctx, sandboxID, workerID, &patch); err != nil {
			log.Printf("patches: %s: patch seq %d failed: %v (stopping)", sandboxID, patch.Sequence, err)
			return // Stop on first failure
		}
		_ = s.store.UpdateSandboxPatchSequence(ctx, sandboxID, patch.Sequence)
		log.Printf("patches: %s: patch seq %d applied successfully", sandboxID, patch.Sequence)
	}
}

// listCheckpointPatches returns all patches for a checkpoint.
func (s *Server) listCheckpointPatches(c echo.Context) error {
	checkpointIDStr := c.Param("checkpointId")

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	// Verify checkpoint belongs to org
	cp, err := s.store.GetCheckpoint(c.Request().Context(), checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}

	patches, err := s.store.ListCheckpointPatches(c.Request().Context(), checkpointID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if patches == nil {
		patches = []db.CheckpointPatch{}
	}

	return c.JSON(http.StatusOK, patches)
}

// deleteCheckpointPatch removes a patch from a checkpoint.
func (s *Server) deleteCheckpointPatch(c echo.Context) error {
	checkpointIDStr := c.Param("checkpointId")
	patchIDStr := c.Param("patchId")
	ctx := c.Request().Context()

	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "database not configured"})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "org context required"})
	}

	checkpointID, err := uuid.Parse(checkpointIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid checkpoint ID"})
	}

	patchID, err := uuid.Parse(patchIDStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid patch ID"})
	}

	// Verify checkpoint belongs to org
	cp, err := s.store.GetCheckpoint(ctx, checkpointID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "checkpoint not found"})
	}
	if cp.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "checkpoint does not belong to this organization"})
	}

	if err := s.store.DeleteCheckpointPatch(ctx, checkpointID, patchID); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "patch not found"})
	}

	return c.NoContent(http.StatusNoContent)
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
