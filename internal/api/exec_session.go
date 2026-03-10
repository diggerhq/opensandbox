package api

import (
	"context"
	"encoding/binary"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
)

func (s *Server) createExecSession(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")

	var req types.ExecSessionCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	if req.Command == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "cmd is required",
		})
	}

	var session *sandbox.ExecSessionHandle

	routeOp := func(_ context.Context) error {
		var err error
		session, err = s.execSessionManager.CreateSession(id, req)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "execSessionCreate", routeOp); err != nil {
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

	return c.JSON(http.StatusCreated, types.ExecSessionInfo{
		SessionID: session.ID,
		SandboxID: id,
		Command:   session.Command,
		Args:      session.Args,
		Running:   true,
		StartedAt: session.StartedAt.Format(time.RFC3339),
	})
}

func (s *Server) listExecSessions(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessions := s.execSessionManager.ListSessions(id)

	if sessions == nil {
		sessions = []types.ExecSessionInfo{}
	}

	return c.JSON(http.StatusOK, sessions)
}

func (s *Server) execSessionWebSocket(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sessionID")

	session, err := s.execSessionManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	if session.SandboxID != id {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
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
		// No scrollback (shouldn't happen with Firecracker sessions, but handle gracefully)
		ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "no scrollback"),
			time.Now().Add(time.Second))
		return nil
	}

	// Send scrollback snapshot
	snapshot := session.Scrollback.Snapshot()
	for _, chunk := range snapshot {
		msg := make([]byte, 1+len(chunk.Data))
		msg[0] = chunk.Stream // 1=stdout, 2=stderr
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

	// Read stdin from WebSocket (0x00 prefix)
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

	// Send live output and exit code
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
			// Send exit code: 0x03 + 4-byte big-endian exit code
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

func (s *Server) execRun(c echo.Context) error {
	id := c.Param("id")

	var req types.ProcessConfig
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	if req.Command == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "cmd is required",
		})
	}

	var result *types.ProcessResult

	routeOp := func(ctx context.Context) error {
		var err error
		result, err = s.manager.Exec(ctx, id, req)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "execRun", routeOp); err != nil {
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

	return c.JSON(http.StatusOK, result)
}

func (s *Server) killExecSession(c echo.Context) error {
	if s.execSessionManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sessionID")

	var body struct {
		Signal int `json:"signal"`
	}
	_ = c.Bind(&body) // optional body

	if body.Signal == 0 {
		body.Signal = 9 // SIGKILL default
	}

	routeOp := func(_ context.Context) error {
		return s.execSessionManager.KillSession(sessionID, body.Signal)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "execSessionKill", routeOp); err != nil {
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

	return c.NoContent(http.StatusNoContent)
}
