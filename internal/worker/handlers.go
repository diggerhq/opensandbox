package worker

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
)

func (s *HTTPServer) setTimeout(c echo.Context) error {
	if s.router == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "sandbox router not available",
		})
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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func (s *HTTPServer) getSandbox(c echo.Context) error {
	id := c.Param("id")
	sb, err := s.manager.Get(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, sb)
}

func (s *HTTPServer) createExecSession(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "exec sessions not available"})
	}

	id := c.Param("id")

	var req types.ExecSessionCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
	}
	if req.Command == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "cmd is required"})
	}

	var session *sandbox.ExecSessionHandle

	routeOp := func(_ context.Context) error {
		var err error
		session, err = s.execSessionManager.CreateSession(id, req)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "execSessionCreate", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.JSON(http.StatusCreated, types.ExecSessionInfo{
		SessionID: session.ID,
		SandboxID: id,
		Command:   session.Command,
		Args:      session.Args,
		Running:   true,
		StartedAt: session.StartedAt.Format(time.RFC3339),
	})
}

func (s *HTTPServer) listExecSessions(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "exec sessions not available"})
	}

	id := c.Param("id")
	sessions := s.execSessionManager.ListSessions(id)

	if sessions == nil {
		sessions = []types.ExecSessionInfo{}
	}

	return c.JSON(http.StatusOK, sessions)
}

func (s *HTTPServer) execSessionWebSocket(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "exec sessions not available"})
	}

	id := c.Param("id")
	sessionID := c.Param("sessionID")

	session, err := s.execSessionManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	if s.router != nil {
		s.router.Touch(id)
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	if session.Scrollback == nil {
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "no scrollback"),
			time.Now().Add(time.Second))
		return nil
	}

	// Send scrollback snapshot
	snapshot := session.Scrollback.Snapshot()
	for _, chunk := range snapshot {
		msg := make([]byte, 1+len(chunk.Data))
		msg[0] = chunk.Stream
		copy(msg[1:], chunk.Data)
		if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
			return nil
		}
	}

	// Send scrollback_end marker (0x04)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte{0x04}); err != nil {
		return nil
	}

	// Subscribe for live output
	sub := session.Scrollback.Subscribe()
	defer session.Scrollback.Unsubscribe(sub)

	// Read stdin from WebSocket
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if len(raw) < 1 {
				continue
			}
			if raw[0] == 0x00 && len(raw) > 1 && session.StdinWriter != nil {
				session.StdinWriter.Write(raw[1:])
			}
			if s.router != nil {
				s.router.Touch(id)
			}
		}
	}()

	for {
		select {
		case chunk, ok := <-sub:
			if !ok {
				return nil
			}
			msg := make([]byte, 1+len(chunk.Data))
			msg[0] = chunk.Stream
			copy(msg[1:], chunk.Data)
			if err := ws.WriteMessage(websocket.BinaryMessage, msg); err != nil {
				return nil
			}
			if s.router != nil {
				s.router.Touch(id)
			}

		case <-session.Done:
			// Drain remaining
			for {
				select {
				case chunk := <-sub:
					msg := make([]byte, 1+len(chunk.Data))
					msg[0] = chunk.Stream
					copy(msg[1:], chunk.Data)
					_ = ws.WriteMessage(websocket.BinaryMessage, msg)
				default:
					goto sendExit
				}
			}
		sendExit:
			exitMsg := make([]byte, 5)
			exitMsg[0] = 0x03
			exitCode := 0
			if session.ExitCode != nil {
				exitCode = *session.ExitCode
			}
			binary.BigEndian.PutUint32(exitMsg[1:], uint32(int32(exitCode)))
			_ = ws.WriteMessage(websocket.BinaryMessage, exitMsg)

			ws.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
				time.Now().Add(time.Second))
			return nil

		case <-wsDone:
			return nil
		}
	}
}

func (s *HTTPServer) killExecSession(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "exec sessions not available"})
	}

	id := c.Param("id")
	sessionID := c.Param("sessionID")

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
		if err := s.router.Route(c.Request().Context(), id, "execSessionKill", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPServer) readFile(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
	}

	var content string
	routeOp := func(ctx context.Context) error {
		var err error
		content, err = s.manager.ReadFile(ctx, id, path)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "readFile", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.String(http.StatusOK, content)
}

func (s *HTTPServer) writeFile(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
	}
	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "failed to read request body: " + err.Error()})
	}

	routeOp := func(ctx context.Context) error {
		return s.manager.WriteFile(ctx, id, path, string(body))
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "writeFile", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPServer) listDir(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		path = "/"
	}

	var entries []types.EntryInfo
	routeOp := func(ctx context.Context) error {
		var err error
		entries, err = s.manager.ListDir(ctx, id, path)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "listDir", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.JSON(http.StatusOK, entries)
}

func (s *HTTPServer) makeDir(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
	}

	routeOp := func(ctx context.Context) error {
		return s.manager.MakeDir(ctx, id, path)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "makeDir", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPServer) removeFile(c echo.Context) error {
	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path query parameter is required"})
	}

	routeOp := func(ctx context.Context) error {
		return s.manager.Remove(ctx, id, path)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "removeFile", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPServer) createPTY(c echo.Context) error {
	id := c.Param("id")

	var req types.PTYCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
	}

	var session *sandbox.PTYSessionHandle
	routeOp := func(ctx context.Context) error {
		var err error
		session, err = s.ptyManager.CreateSession(id, req)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "createPTY", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	// Log PTY start to SQLite
	if s.sandboxDBs != nil {
		sdb, dbErr := s.sandboxDBs.Get(id)
		if dbErr == nil {
			_ = sdb.LogPTYStart(session.ID)
		}
	}

	return c.JSON(http.StatusCreated, types.PTYSession{
		SessionID: session.ID,
		SandboxID: id,
	})
}

func (s *HTTPServer) ptyWebSocket(c echo.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionID")

	session, err := s.ptyManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	// Touch to reset rolling timeout when PTY connects
	if s.router != nil {
		s.router.Touch(id)
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := session.PTY.Read(buf)
			if n > 0 {
				// Touch on PTY output to keep sandbox alive
				if s.router != nil {
					s.router.Touch(id)
				}
				if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			// Touch on PTY input to keep sandbox alive
			if s.router != nil {
				s.router.Touch(id)
			}
			if _, err := session.PTY.Write(msg); err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-session.Done:
	}

	ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))

	return nil
}

func (s *HTTPServer) killPTY(c echo.Context) error {
	id := c.Param("id")
	sessionID := c.Param("sessionID")

	routeOp := func(ctx context.Context) error {
		return s.ptyManager.KillSession(sessionID)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "killPTY", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func (s *HTTPServer) resizePTY(c echo.Context) error {
	sessionID := c.Param("sessionID")

	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}

	if err := s.ptyManager.Resize(sessionID, req.Cols, req.Rows); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}

	return c.NoContent(http.StatusOK)
}

// refreshToken issues a fresh 24h JWT for a sandbox.
// The caller must already be authenticated (existing valid JWT via middleware).
func (s *HTTPServer) refreshToken(c echo.Context) error {
	sandboxID := c.Param("id")

	orgIDVal := c.Get(string(auth.ContextKeyOrgID))
	orgID, _ := orgIDVal.(uuid.UUID)
	workerID, _ := c.Get("worker_id").(string)

	if s.jwtIssuer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "JWT issuer not configured",
		})
	}

	token, err := s.jwtIssuer.IssueSandboxToken(orgID, sandboxID, workerID, 24*time.Hour)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to issue token: " + err.Error(),
		})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"token": token,
	})
}
