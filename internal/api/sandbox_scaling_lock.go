package api

import (
	"log"
	"net/http"

	"github.com/labstack/echo/v4"
)

// PUT /api/sandboxes/:id/scaling-lock
//
// Locks (or unlocks) a sandbox against any size change. When locked:
//
//   - POST /scale returns 403 with code "scaling_locked"
//   - PUT /autoscale {enabled:true} returns 403 with code "scaling_locked"
//   - The per-sandbox autoscaler skips the sandbox (filter in
//     ListAutoscaleEnabled, defense in depth)
//
// Locking ALSO disables autoscale on the row — a user who doesn't want
// their sandbox to scale almost certainly doesn't want autoscale either,
// and leaving it half-on would be confusing the next time they unlock.
// Unlocking does NOT auto-re-enable autoscale; the user re-enables
// explicitly via PUT /autoscale.
func (s *Server) setScalingLock(c echo.Context) error {
	sandboxID := c.Param("id")
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	var req struct {
		Locked bool `json:"locked"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid body"})
	}

	if err := s.store.SetScalingLock(c.Request().Context(), sandboxID, req.Locked); err != nil {
		// Distinguish "not found" from internal errors.
		if err.Error() == "sandbox "+sandboxID+" not found" {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	if req.Locked {
		// Disable autoscale alongside the lock — single user-visible state
		// instead of two overlapping toggles. Best-effort, logged on failure.
		if err := s.store.SetSandboxAutoscale(c.Request().Context(), sandboxID, false, 0, 0); err != nil {
			log.Printf("scaling-lock: failed to disable autoscale on %s after lock: %v", sandboxID, err)
		}
	}

	return c.JSON(http.StatusOK, map[string]any{
		"sandboxID": sandboxID,
		"locked":    req.Locked,
	})
}

// GET /api/sandboxes/:id/scaling-lock
func (s *Server) getScalingLock(c echo.Context) error {
	sandboxID := c.Param("id")
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	locked, err := s.store.GetScalingLock(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"sandboxID": sandboxID,
		"locked":    locked,
	})
}
