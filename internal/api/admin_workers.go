package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// adminSetWorkerDraining toggles the in-memory `Draining` flag on a worker so
// the placement filter (RedisWorkerRegistry.GetLeastLoadedWorker and
// findScaleMigrationTargets) stops routing new sandboxes to it. Existing
// sandboxes on the worker are unaffected.
//
// POST /admin/workers/:id/drain          — mark draining (default)
// POST /admin/workers/:id/drain?drain=false — clear draining
//
// The flag is per-controlplane-instance memory: call this on every active
// control plane to drain consistently across replicas. Heartbeats do not
// overwrite the flag.
func (s *Server) adminSetWorkerDraining(c echo.Context) error {
	if s.workerRegistry == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "worker registry not configured (combined/worker mode)",
		})
	}

	workerID := c.Param("id")
	if workerID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "worker id required"})
	}

	drain := c.QueryParam("drain") != "false"

	known := false
	for _, w := range s.workerRegistry.GetAllWorkers() {
		if w.ID == workerID {
			known = true
			break
		}
	}
	if !known {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "worker not registered"})
	}

	s.workerRegistry.SetDraining(workerID, drain)

	return c.JSON(http.StatusOK, map[string]any{
		"workerID": workerID,
		"draining": drain,
	})
}
