package controlplane

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// AdminHandlers implements /admin/halt-org and /admin/resume-org.
// Both are HMAC-authenticated webhooks dispatched from the CreditAccount
// Durable Object on Cloudflare.
//
// Wire on the existing Echo server:
//
//	g := e.Group("/admin", AdminAuth(cfg.CFAdminSecret))
//	g.POST("/halt-org", h.HaltOrg)
//	g.POST("/resume-org", h.ResumeOrg)
type AdminHandlers struct {
	// TODO: dependencies — billing.Enforcer, qemu.Manager, db.Store, etc.
}

// NewAdminHandlers constructs the handlers.
func NewAdminHandlers( /* TODO */ ) *AdminHandlers {
	return &AdminHandlers{}
}

// HaltOrg iterates running sandboxes for the org in this cell and triggers
// hibernation via existing checkpoint machinery (same path as manual hibernate).
// Body: {"org_id": "<uuid>", "reason": "credits_exhausted"}
func (h *AdminHandlers) HaltOrg(c echo.Context) error {
	// TODO:
	// 1. Parse body {org_id, reason}
	// 2. Query sandbox_sessions WHERE org_id = ? AND status IN ('running')
	// 3. For each: call qemu.Manager.Hibernate via existing path
	// 4. Update PG status to 'hibernated' with halt_reason
	// 5. Emit sandbox.hibernated events into stream (forwarder picks up)
	_ = c
	return c.NoContent(http.StatusOK)
}

// ResumeOrg iterates hibernated sandboxes for the org and restores them.
// Body: {"org_id": "<uuid>", "skip_resume": false}
func (h *AdminHandlers) ResumeOrg(c echo.Context) error {
	// TODO:
	// 1. Parse body {org_id, skip_resume}
	// 2. If skip_resume: return early
	// 3. Query sandbox_sessions WHERE org_id = ? AND status = 'hibernated'
	//    AND (halt_reason = 'credits_exhausted' OR halt_reason IS NULL)
	// 4. For each: restore from S3 via existing wake machinery
	// 5. Update PG status to 'running'
	_ = c
	return c.NoContent(http.StatusOK)
}

// AdminAuth verifies HMAC-SHA256 signature on inbound admin webhooks.
// Headers: X-Timestamp (unix sec), X-Signature (hex). Body is signed as
// "{timestamp}.{body}". Rejects timestamps older than 5 minutes (replay).
func AdminAuth(secret string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			// TODO:
			// 1. Read X-Timestamp, X-Signature
			// 2. Verify abs(now - timestamp) <= 5min
			// 3. Read body, recompute HMAC, compare in constant time
			// 4. On mismatch return 401
			_ = secret
			return next(c)
		}
	}
}
