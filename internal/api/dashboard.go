package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// dashboardMe returns the current authenticated user info.
func (s *Server) dashboardMe(c echo.Context) error {
	userID := c.Get("user_id")
	email := c.Get("user_email")
	orgID, _ := auth.GetOrgID(c)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"id":    userID,
		"email": email,
		"orgId": orgID,
	})
}

// dashboardSessions returns session history for the authenticated org.
func (s *Server) dashboardSessions(c echo.Context) error {
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

	status := c.QueryParam("status")
	sessions, err := s.store.ListSandboxSessions(c.Request().Context(), orgID, status, 100, 0)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, sessions)
}

// dashboardListAPIKeys returns all API keys for the authenticated org.
func (s *Server) dashboardListAPIKeys(c echo.Context) error {
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

	keys, err := s.store.ListAPIKeys(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, keys)
}

// dashboardCreateAPIKey creates a new API key for the authenticated org.
func (s *Server) dashboardCreateAPIKey(c echo.Context) error {
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
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
	}
	if req.Name == "" {
		req.Name = "Untitled"
	}

	// Get user ID if available
	var createdBy *uuid.UUID
	if uid, ok := c.Get("user_id").(uuid.UUID); ok {
		createdBy = &uid
	}

	plainKey, err := auth.GenerateAPIKey()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to generate key",
		})
	}

	hash := db.HashAPIKey(plainKey)
	prefix := plainKey[:8]

	apiKey, err := s.store.CreateAPIKey(c.Request().Context(), orgID, createdBy, hash, prefix, req.Name, []string{"sandbox:*"})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	// Return the key with the plaintext key (only shown once)
	return c.JSON(http.StatusCreated, map[string]interface{}{
		"id":        apiKey.ID,
		"name":      apiKey.Name,
		"key":       plainKey,
		"keyPrefix": apiKey.KeyPrefix,
		"createdAt": apiKey.CreatedAt,
	})
}

// dashboardDeleteAPIKey revokes an API key (scoped to the authenticated org).
func (s *Server) dashboardDeleteAPIKey(c echo.Context) error {
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

	keyID, err := uuid.Parse(c.Param("keyId"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid key ID",
		})
	}

	if err := s.store.DeleteAPIKeyForOrg(c.Request().Context(), keyID, orgID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// dashboardGetOrg returns the authenticated org info.
func (s *Server) dashboardGetOrg(c echo.Context) error {
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

	org, err := s.store.GetOrg(c.Request().Context(), orgID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, org)
}

// dashboardUpdateOrg updates the org name.
func (s *Server) dashboardUpdateOrg(c echo.Context) error {
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
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "name is required",
		})
	}

	org, err := s.store.UpdateOrg(c.Request().Context(), orgID, req.Name)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, org)
}

// dashboardListTemplates returns all templates visible to the authenticated org.
func (s *Server) dashboardListTemplates(c echo.Context) error {
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

	// Strip internal image refs from dashboard response
	type safeTemplate struct {
		ID        uuid.UUID  `json:"id"`
		OrgID     *uuid.UUID `json:"orgId,omitempty"`
		Name      string     `json:"name"`
		Tag       string     `json:"tag"`
		IsPublic  bool       `json:"isPublic"`
		CreatedAt time.Time  `json:"createdAt"`
	}
	safe := make([]safeTemplate, len(templates))
	for i, t := range templates {
		safe[i] = safeTemplate{
			ID: t.ID, OrgID: t.OrgID, Name: t.Name,
			Tag: t.Tag, IsPublic: t.IsPublic, CreatedAt: t.CreatedAt,
		}
	}

	return c.JSON(http.StatusOK, safe)
}

// dashboardBuildTemplate builds a new template for the authenticated org.
func (s *Server) dashboardBuildTemplate(c echo.Context) error {
	// Delegate to the shared buildTemplate handler (uses same auth context)
	return s.buildTemplate(c)
}

// dashboardDeleteTemplate deletes a custom template for the authenticated org.
func (s *Server) dashboardDeleteTemplate(c echo.Context) error {
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

	templateID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid template ID",
		})
	}

	if err := s.store.DeleteTemplateForOrg(c.Request().Context(), templateID, orgID); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.NoContent(http.StatusNoContent)
}

// dashboardGetSession returns detailed info for a single session.
func (s *Server) dashboardGetSession(c echo.Context) error {
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

	sandboxID := c.Param("sandboxId")
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "session not found",
		})
	}

	// Verify session belongs to this org
	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{
			"error": "session does not belong to this organization",
		})
	}

	// Build response
	resp := map[string]interface{}{
		"id":        session.ID,
		"sandboxId": session.SandboxID,
		"template":  session.Template,
		"status":    session.Status,
		"startedAt": session.StartedAt,
	}
	if session.StoppedAt != nil {
		resp["stoppedAt"] = session.StoppedAt
	}
	if session.ErrorMsg != nil {
		resp["errorMsg"] = *session.ErrorMsg
	}

	// Compute domain
	if s.sandboxDomain != "" {
		resp["domain"] = fmt.Sprintf("%s.%s", sandboxID, s.sandboxDomain)
	}

	// Parse config JSON if available
	if len(session.Config) > 0 {
		var cfg map[string]interface{}
		if json.Unmarshal(session.Config, &cfg) == nil {
			resp["config"] = cfg
		}
	}

	// If hibernated, include checkpoint info
	if session.Status == "hibernated" {
		checkpoint, err := s.store.GetActiveCheckpoint(c.Request().Context(), sandboxID)
		if err == nil {
			resp["checkpoint"] = map[string]interface{}{
				"checkpointKey": checkpoint.CheckpointKey,
				"sizeBytes":     checkpoint.SizeBytes,
				"hibernatedAt":  checkpoint.HibernatedAt,
			}
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// dashboardGetSessionStats returns live CPU/memory stats for a running sandbox.
func (s *Server) dashboardGetSessionStats(c echo.Context) error {
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

	sandboxID := c.Param("sandboxId")
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "session not found",
		})
	}

	if session.OrgID != orgID {
		return c.JSON(http.StatusForbidden, map[string]string{
			"error": "session does not belong to this organization",
		})
	}

	if session.Status != "running" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "sandbox is not running",
		})
	}

	// Combined mode: get stats directly from manager
	if s.manager != nil {
		stats, err := s.manager.Stats(c.Request().Context(), sandboxID)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "stats unavailable: " + err.Error(),
			})
		}
		return c.JSON(http.StatusOK, stats)
	}

	// Server mode: dispatch to worker via gRPC
	if s.workerRegistry != nil {
		grpcClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "worker not available: " + err.Error(),
			})
		}

		ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
		defer cancel()

		grpcResp, err := grpcClient.GetSandboxStats(ctx, &pb.GetSandboxStatsRequest{
			SandboxId: sandboxID,
		})
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "stats unavailable: " + err.Error(),
			})
		}

		return c.JSON(http.StatusOK, map[string]interface{}{
			"cpuPercent": grpcResp.CpuPercent,
			"memUsage":   grpcResp.MemUsage,
			"memLimit":   grpcResp.MemLimit,
			"netInput":   grpcResp.NetInput,
			"netOutput":  grpcResp.NetOutput,
			"pids":       grpcResp.Pids,
		})
	}

	return c.JSON(http.StatusServiceUnavailable, map[string]string{
		"error": "no stats provider available",
	})
}

// dashboardCreatePTY creates a PTY session for a sandbox owned by the authenticated org.
func (s *Server) dashboardCreatePTY(c echo.Context) error {
	sandboxID, session, err := s.dashboardResolveSandbox(c)
	if err != nil {
		return err
	}

	var req types.PTYCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
	}

	// Server mode: proxy PTY creation to the worker via gRPC
	if s.ptyManager == nil && s.workerRegistry != nil {
		grpcClient, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
		if err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "worker not available: " + err.Error(),
			})
		}

		ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
		defer cancel()

		grpcResp, err := grpcClient.CreatePTY(ctx, &pb.CreatePTYRequest{
			SandboxId: sandboxID,
			Cols:      int32(req.Cols),
			Rows:      int32(req.Rows),
			Shell:     req.Shell,
		})
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": "failed to create PTY: " + err.Error(),
			})
		}

		return c.JSON(http.StatusCreated, map[string]string{
			"sessionId": grpcResp.SessionId,
			"sandboxId": sandboxID,
		})
	}

	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "terminal not available",
		})
	}

	// Combined/worker mode: create locally
	var ptySession *sandbox.PTYSessionHandle
	routeOp := func(_ context.Context) error {
		var err error
		ptySession, err = s.ptyManager.CreateSession(sandboxID, req)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), sandboxID, "createPTY", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
		}
	}

	return c.JSON(http.StatusCreated, map[string]string{
		"sessionId": ptySession.ID,
		"sandboxId": sandboxID,
	})
}

// dashboardPTYWebSocket upgrades to a WebSocket for an interactive terminal.
func (s *Server) dashboardPTYWebSocket(c echo.Context) error {
	sandboxID, session, err := s.dashboardResolveSandbox(c)
	if err != nil {
		return err
	}

	sessionID := c.Param("sessionId")

	// Server mode: proxy WebSocket to the worker's HTTP API
	if s.ptyManager == nil && s.workerRegistry != nil {
		return s.dashboardPTYWebSocketRemote(c, sandboxID, sessionID, session)
	}

	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "terminal not available",
		})
	}

	// Combined/worker mode: connect directly to local PTY
	ptySession, err := s.ptyManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	if s.router != nil {
		s.router.Touch(sandboxID)
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	// PTY -> WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, readErr := ptySession.PTY.Read(buf)
			if n > 0 {
				if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// WebSocket -> PTY
	go func() {
		for {
			_, msg, readErr := ws.ReadMessage()
			if readErr != nil {
				return
			}
			if _, writeErr := ptySession.PTY.Write(msg); writeErr != nil {
				return
			}
			if s.router != nil {
				s.router.Touch(sandboxID)
			}
		}
	}()

	select {
	case <-done:
	case <-ptySession.Done:
	}

	ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))

	return nil
}

// dashboardPTYWebSocketRemote proxies the dashboard WebSocket to the worker's
// PTY WebSocket endpoint, acting as a transparent bridge.
func (s *Server) dashboardPTYWebSocketRemote(c echo.Context, sandboxID, sessionID string, session *db.SandboxSession) error {
	worker := s.workerRegistry.GetWorker(session.WorkerID)
	if worker == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "worker not available",
		})
	}

	// Issue a short-lived JWT for the worker request
	orgID, _ := auth.GetOrgID(c)
	token, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, session.WorkerID, 5*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to issue token",
		})
	}

	// Build the worker WebSocket URL
	// worker.HTTPAddr is like "http://13.59.48.110:8080"
	workerURL := strings.Replace(worker.HTTPAddr, "http://", "ws://", 1)
	workerURL = strings.Replace(workerURL, "https://", "wss://", 1)
	workerWSURL := fmt.Sprintf("%s/sandboxes/%s/pty/%s", workerURL, sandboxID, sessionID)

	// Add auth token as query param (WebSocket can't set headers easily from browser,
	// but here we're server→worker so we can use headers)
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	workerWS, resp, err := dialer.Dial(workerWSURL, header)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		log.Printf("dashboard: failed to dial worker PTY WebSocket %s (status=%d): %v", workerWSURL, status, err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": "failed to connect to worker terminal",
		})
	}
	defer workerWS.Close()

	// Upgrade the dashboard client connection
	clientWS, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer clientWS.Close()

	// Bidirectional pipe: dashboard ↔ worker
	done := make(chan struct{}, 2)

	// worker → dashboard
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := workerWS.ReadMessage()
			if err != nil {
				return
			}
			if err := clientWS.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	// dashboard → worker
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msgType, msg, err := clientWS.ReadMessage()
			if err != nil {
				return
			}
			if err := workerWS.WriteMessage(msgType, msg); err != nil {
				return
			}
		}
	}()

	// Wait for either direction to close
	<-done

	clientWS.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))
	workerWS.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))

	return nil
}

// dashboardResizePTY resizes a PTY session.
func (s *Server) dashboardResizePTY(c echo.Context) error {
	sandboxID, session, err := s.dashboardResolveSandbox(c)
	if err != nil {
		return err
	}

	sessionID := c.Param("sessionId")

	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
	}

	// Server mode: proxy resize to the worker via HTTP
	// The worker doesn't have a dedicated resize gRPC call, so we proxy the HTTP request.
	if s.ptyManager == nil && s.workerRegistry != nil {
		return s.proxyWorkerHTTP(c, session, "POST",
			fmt.Sprintf("/sandboxes/%s/pty/%s/resize", sandboxID, sessionID),
			req)
	}

	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "terminal not available",
		})
	}

	if err := s.ptyManager.Resize(sessionID, req.Cols, req.Rows); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	return c.NoContent(http.StatusOK)
}

// dashboardKillPTY kills a PTY session.
func (s *Server) dashboardKillPTY(c echo.Context) error {
	sandboxID, session, err := s.dashboardResolveSandbox(c)
	if err != nil {
		return err
	}

	sessionID := c.Param("sessionId")

	// Server mode: proxy kill to the worker via HTTP
	if s.ptyManager == nil && s.workerRegistry != nil {
		return s.proxyWorkerHTTP(c, session, "DELETE",
			fmt.Sprintf("/sandboxes/%s/pty/%s", sandboxID, sessionID), nil)
	}

	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "terminal not available",
		})
	}

	routeOp := func(_ context.Context) error {
		return s.ptyManager.KillSession(sessionID)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), sandboxID, "killPTY", routeOp); err != nil {
			return c.JSON(http.StatusNotFound, map[string]string{
				"error": err.Error(),
			})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusNotFound, map[string]string{
				"error": err.Error(),
			})
		}
	}

	return c.NoContent(http.StatusNoContent)
}

// dashboardResolveSandbox validates the sandbox belongs to the authenticated org and is running.
func (s *Server) dashboardResolveSandbox(c echo.Context) (string, *db.SandboxSession, error) {
	if s.store == nil {
		return "", nil, c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return "", nil, c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	sandboxID := c.Param("sandboxId")
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return "", nil, c.JSON(http.StatusNotFound, map[string]string{
			"error": "session not found",
		})
	}

	if session.OrgID != orgID {
		return "", nil, c.JSON(http.StatusForbidden, map[string]string{
			"error": "session does not belong to this organization",
		})
	}

	if session.Status != "running" {
		return "", nil, c.JSON(http.StatusBadRequest, map[string]string{
			"error": "sandbox is not running",
		})
	}

	return sandboxID, session, nil
}

// proxyWorkerHTTP proxies an HTTP request to the worker's HTTP API.
// Used by dashboard PTY resize/kill in server-only mode.
func (s *Server) proxyWorkerHTTP(c echo.Context, session *db.SandboxSession, method, path string, body interface{}) error {
	worker := s.workerRegistry.GetWorker(session.WorkerID)
	if worker == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "worker not available",
		})
	}

	// Issue a short-lived JWT for the worker request
	orgID, _ := auth.GetOrgID(c)
	sandboxID := c.Param("sandboxId")
	token, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, session.WorkerID, 5*time.Minute)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to issue token",
		})
	}

	// Build the worker URL
	workerBase := strings.TrimRight(worker.HTTPAddr, "/")
	workerURL, _ := url.JoinPath(workerBase, path)

	var bodyReader io.Reader
	if body != nil {
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": "failed to marshal request body",
			})
		}
		bodyReader = bytes.NewReader(bodyJSON)
	}

	req, err := http.NewRequestWithContext(c.Request().Context(), method, workerURL, bodyReader)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to create request",
		})
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("dashboard: failed to proxy to worker %s: %v", workerURL, err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": "worker request failed",
		})
	}
	defer resp.Body.Close()

	// Forward the worker's response back to the dashboard
	respBody, _ := io.ReadAll(resp.Body)
	for k, vals := range resp.Header {
		for _, v := range vals {
			c.Response().Header().Add(k, v)
		}
	}
	return c.Blob(resp.StatusCode, resp.Header.Get("Content-Type"), respBody)
}
