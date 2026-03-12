package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
)

func (s *Server) createAgentSession(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")

	var req types.AgentSessionCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	execReq := types.ExecSessionCreateRequest{
		Command: "claude-agent-wrapper",
	}

	var session *sandbox.ExecSessionHandle

	routeOp := func(_ context.Context) error {
		var err error
		session, err = s.execSessionManager.CreateSession(id, execReq)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "agentSessionCreate", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	// Send configure command if any config options provided
	hasConfig := req.Model != "" || req.SystemPrompt != "" || len(req.AllowedTools) > 0 ||
		req.PermissionMode != "" || req.MaxTurns > 0 || req.Cwd != "" || len(req.McpServers) > 0 ||
		req.Resume != ""
	if hasConfig && session.StdinWriter != nil {
		configCmd := map[string]interface{}{"type": "configure"}
		if req.Model != "" {
			configCmd["model"] = req.Model
		}
		if req.SystemPrompt != "" {
			configCmd["systemPrompt"] = req.SystemPrompt
		}
		if len(req.AllowedTools) > 0 {
			configCmd["allowedTools"] = req.AllowedTools
		}
		if req.PermissionMode != "" {
			configCmd["permissionMode"] = req.PermissionMode
		}
		if req.MaxTurns > 0 {
			configCmd["maxTurns"] = req.MaxTurns
		}
		if req.Cwd != "" {
			configCmd["cwd"] = req.Cwd
		}
		if len(req.McpServers) > 0 {
			configCmd["mcpServers"] = req.McpServers
		}
		if req.Resume != "" {
			configCmd["resume"] = req.Resume
		}
		configJSON, _ := json.Marshal(configCmd)
		session.StdinWriter.Write(append(configJSON, '\n'))
	}

	// Send initial prompt if provided
	if req.Prompt != "" && session.StdinWriter != nil {
		promptCmd := map[string]interface{}{
			"type": "prompt",
			"text": req.Prompt,
		}
		if req.Resume != "" {
			promptCmd["resume"] = req.Resume
		}
		promptJSON, _ := json.Marshal(promptCmd)
		session.StdinWriter.Write(append(promptJSON, '\n'))
	}

	return c.JSON(http.StatusCreated, types.AgentSessionInfo{
		SessionID: session.ID,
		SandboxID: id,
		Running:   true,
		StartedAt: session.StartedAt.Format(time.RFC3339),
	})
}

func (s *Server) listAgentSessions(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	allSessions := s.execSessionManager.ListSessions(id)

	var agentSessions []types.AgentSessionInfo
	for _, sess := range allSessions {
		if sess.Command == "claude-agent-wrapper" {
			agentSessions = append(agentSessions, types.AgentSessionInfo{
				SessionID: sess.SessionID,
				SandboxID: sess.SandboxID,
				Running:   sess.Running,
				StartedAt: sess.StartedAt,
			})
		}
	}

	if agentSessions == nil {
		agentSessions = []types.AgentSessionInfo{}
	}

	return c.JSON(http.StatusOK, agentSessions)
}

func (s *Server) sendAgentPrompt(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sid")

	var req types.AgentPromptRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
	}
	if req.Text == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "text is required"})
	}

	session, err := s.execSessionManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	if session.SandboxID != id {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
	}

	if session.StdinWriter == nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "session stdin not available"})
	}

	promptCmd := map[string]interface{}{
		"type": "prompt",
		"text": req.Text,
	}
	promptJSON, _ := json.Marshal(promptCmd)
	session.StdinWriter.Write(append(promptJSON, '\n'))

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) interruptAgent(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sid")

	session, err := s.execSessionManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	if session.SandboxID != id {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
	}

	if session.StdinWriter == nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "session stdin not available"})
	}

	interruptCmd := map[string]interface{}{"type": "interrupt"}
	interruptJSON, _ := json.Marshal(interruptCmd)
	session.StdinWriter.Write(append(interruptJSON, '\n'))

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) killAgentSession(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sid")

	var body struct {
		Signal int `json:"signal"`
	}
	_ = c.Bind(&body)

	if body.Signal == 0 {
		body.Signal = 9
	}

	routeOp := func(_ context.Context) error {
		return s.execSessionManager.KillSession(sessionID, body.Signal)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "agentSessionKill", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusNoContent)
}
