package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/pkg/types"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now; tighten in production
	},
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func (s *Server) createPTY(c echo.Context) error {
	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")

	var req types.PTYCreateRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	var session *sandbox.PTYSessionHandle

	routeOp := func(_ context.Context) error {
		var err error
		session, err = s.ptyManager.CreateSession(id, req)
		return err
	}

	// Route through sandbox router (handles auto-wake, rolling timeout reset)
	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "createPTY", routeOp); err != nil {
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

	return c.JSON(http.StatusCreated, types.PTYSession{
		SessionID: session.ID,
		SandboxID: id,
	})
}

func (s *Server) ptyWebSocket(c echo.Context) error {
	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sessionID")

	// Touch the router on initial WebSocket connection to reset timeout
	if s.router != nil {
		s.router.Touch(id)
	}

	session, err := s.ptyManager.GetSession(sessionID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	if session.SandboxID != id {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
	}

	ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
	if err != nil {
		return err
	}
	defer ws.Close()

	// Read from PTY -> send to WebSocket
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := session.PTY.Read(buf)
			if n > 0 {
				if writeErr := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Read from WebSocket -> write to PTY (with router touch on activity)
	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if _, err := session.PTY.Write(msg); err != nil {
				return
			}
			// Touch on every WebSocket message to keep sandbox alive
			if s.router != nil {
				s.router.Touch(id)
			}
		}
	}()

	// Wait for PTY process to end or connection to close
	select {
	case <-done:
	case <-session.Done:
	}

	// Give the reader goroutine a moment to flush remaining output
	ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second))

	return nil
}

func (s *Server) resizePTY(c echo.Context) error {
	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	sessionID := c.Param("sessionID")

	var req struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
	}

	if err := s.ptyManager.Resize(sessionID, req.Cols, req.Rows); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": err.Error(),
		})
	}

	return c.NoContent(http.StatusOK)
}

func (s *Server) killPTY(c echo.Context) error {
	if s.ptyManager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	sessionID := c.Param("sessionID")

	routeOp := func(_ context.Context) error {
		return s.ptyManager.KillSession(sessionID)
	}

	// Route through sandbox router
	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "killPTY", routeOp); err != nil {
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
