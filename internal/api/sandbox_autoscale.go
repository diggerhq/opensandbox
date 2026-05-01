package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/pkg/types"
	pbw "github.com/opensandbox/opensandbox/proto/worker"
)

// PUT /api/sandboxes/:id/autoscale
//
// Enables/disables per-sandbox autoscaler. Body:
//
//	{
//	  "enabled": true,
//	  "minMemoryMB": 1024,
//	  "maxMemoryMB": 16384
//	}
//
// minMemoryMB and maxMemoryMB must be allowed memory tiers (validated via
// types.ValidateMemoryMB). vCPU follows the tier table — there's no
// independent CPU bound. If `enabled=false`, min/max are cleared.
//
// Plan-level cap: an org's plan max_memory_gb (in orgs table) is the hard
// upper bound regardless of what the user requests. Free tier is capped at
// 4 GB just like the manual scale endpoint.
func (s *Server) setAutoscale(c echo.Context) error {
	sandboxID := c.Param("id")
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	var req struct {
		Enabled     bool `json:"enabled"`
		MinMemoryMB int  `json:"minMemoryMB"`
		MaxMemoryMB int  `json:"maxMemoryMB"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid body"})
	}

	if req.Enabled {
		if _, err := types.ValidateMemoryMB(req.MinMemoryMB); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "minMemoryMB: " + err.Error()})
		}
		if _, err := types.ValidateMemoryMB(req.MaxMemoryMB); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "maxMemoryMB: " + err.Error()})
		}
		if req.MinMemoryMB > req.MaxMemoryMB {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "minMemoryMB must be ≤ maxMemoryMB"})
		}
		// Plan cap: free tier is bounded at 4 GB just like manual /scale.
		// Without this a free-tier user could PUT max=16GB and the
		// autoscaler would obediently scale them past their plan. Mirror the
		// same check we apply in scaleSandbox so the two paths can't diverge.
		if orgID, hasOrg := auth.GetOrgID(c); hasOrg {
			if org, err := s.store.GetOrg(c.Request().Context(), orgID); err == nil && org.Plan == "free" {
				if req.MaxMemoryMB > 4096 {
					return c.JSON(http.StatusPaymentRequired, map[string]string{
						"error": "free plan caps autoscale at 4096 MB — upgrade to pro for larger instances",
					})
				}
			}
		}
	}

	if err := s.store.SetSandboxAutoscale(c.Request().Context(), sandboxID, req.Enabled, req.MinMemoryMB, req.MaxMemoryMB); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"sandboxID":   sandboxID,
		"enabled":     req.Enabled,
		"minMemoryMB": req.MinMemoryMB,
		"maxMemoryMB": req.MaxMemoryMB,
	})
}

// GET /api/sandboxes/:id/autoscale
func (s *Server) getAutoscale(c echo.Context) error {
	sandboxID := c.Param("id")
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}
	enabled, minMB, maxMB, err := s.store.GetSandboxAutoscale(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"sandboxID":   sandboxID,
		"enabled":     enabled,
		"minMemoryMB": minMB,
		"maxMemoryMB": maxMB,
	})
}

// AutoscalerSetter implements controlplane.AutoscalerScaleSetter by routing
// the scale request through the same SetSandboxLimits gRPC call that the
// manual /scale endpoint uses. We don't go through the HTTP layer — the
// autoscaler runs in-process and we already have direct access to the
// worker registry.
//
// CPU follows memory per types.AllowedResourceTiers (e.g., 8 GB ⇒ 4 vCPU).
type AutoscalerSetter struct {
	server *Server
}

// NewAutoscalerSetter wires the existing Server's worker registry into the
// autoscaler's scale-applier interface.
func NewAutoscalerSetter(server *Server) *AutoscalerSetter {
	return &AutoscalerSetter{server: server}
}

// SetSandboxMemoryMB applies a tier-aligned memory size to a sandbox. The
// autoscaler should only ever pass values that are already allowed tiers,
// but we re-validate here as defense in depth — if a future caller passes
// 6 GB we'd silently corrupt the worker's expected scaling table.
//
// On success we also emit an admin event so the operator dashboard sees
// every autoscaler-driven scale event ("why did sandbox X grow last
// night?" — answer is now visible without grepping logs).
func (a *AutoscalerSetter) SetSandboxMemoryMB(ctx context.Context, sandboxID string, fromMB, toMB int) error {
	memoryMB := toMB
	vcpus, err := types.ValidateMemoryMB(memoryMB)
	if err != nil {
		return fmt.Errorf("invalid memoryMB %d: %w", memoryMB, err)
	}
	if a.server.workerRegistry == nil || a.server.store == nil {
		return fmt.Errorf("autoscaler: server not configured for remote worker calls")
	}

	session, err := a.server.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}
	if session.Status != "running" {
		// Don't try to scale stopped/hibernated sandboxes — the autoscaler
		// shouldn't have picked them, but better to no-op cleanly than fail.
		return nil
	}

	// Defense in depth: re-check the org plan cap here in case the autoscale
	// row was set before a downgrade or the cap was raised by another path.
	// The PUT /autoscale handler already enforces this; this is the second
	// gate so an obsolete row can't keep scaling past plan limits.
	if org, err := a.server.store.GetOrg(ctx, session.OrgID); err == nil && org.Plan == "free" && memoryMB > 4096 {
		return fmt.Errorf("plan cap: free plan limited to 4096 MB, refusing autoscale to %d", memoryMB)
	}

	client, err := a.server.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		return fmt.Errorf("get worker client: %w", err)
	}

	rpcCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cpuPercent := vcpus * 100
	_, err = client.SetSandboxLimits(rpcCtx, &pbw.SetSandboxLimitsRequest{
		SandboxId:      sandboxID,
		MaxPids:        0,
		MaxMemoryBytes: int64(memoryMB) * 1024 * 1024,
		CpuMaxUsec:     int64(cpuPercent) * 1000,
		CpuPeriodUsec:  100000,
	})
	if err != nil {
		return err
	}
	// Audit event — visible in /admin/events SSE + /admin/events/history.
	if a.server.adminEvents != nil {
		a.server.adminEvents.Publish("autoscale", sandboxID, session.WorkerID,
			fmt.Sprintf("autoscaler: %dMB → %dMB", fromMB, toMB))
	}
	return nil
}
