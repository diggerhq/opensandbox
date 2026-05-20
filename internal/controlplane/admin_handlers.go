package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

// OrgHalter is the cell-local entry point for halting and resuming all of
// an org's sandboxes. Implemented by *api.Server (so the existing hibernate /
// wake code paths are reused); kept as an interface here to avoid a circular
// import between the api and controlplane packages.
type OrgHalter interface {
	HaltOrg(ctx context.Context, orgID, reason string) (halted int, err error)
	ResumeOrg(ctx context.Context, orgID string, skipResume bool) (resumed int, err error)
}

// AdminHandlers implements /admin/halt-org and /admin/resume-org. Both are
// HMAC-authenticated webhooks dispatched from the CreditAccount DO on
// Cloudflare. Delegates the actual sandbox iteration to the OrgHalter
// implementation (Server in api package).
//
// Wire on the existing Echo server:
//
//	g := e.Group("/admin", AdminAuth(cfg.CFAdminSecret))
//	g.POST("/halt-org",   h.HaltOrg)
//	g.POST("/resume-org", h.ResumeOrg)
type AdminHandlers struct {
	halter OrgHalter
}

// NewAdminHandlers constructs the handlers around an OrgHalter implementation.
func NewAdminHandlers(h OrgHalter) *AdminHandlers {
	return &AdminHandlers{halter: h}
}

// HaltOrg iterates running sandboxes for the org in this cell and triggers
// hibernation via the existing checkpoint machinery (same path as manual
// hibernate). Idempotent: re-halting an already-halted org is a no-op.
//
// Body: {"org_id": "<uuid>", "reason": "credits_exhausted"}
func (h *AdminHandlers) HaltOrg(c echo.Context) error {
	var body struct {
		OrgID  string `json:"org_id"`
		Reason string `json:"reason"`
	}
	if err := c.Bind(&body); err != nil || body.OrgID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing or invalid org_id"})
	}
	if h.halter == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "halter not wired"})
	}
	n, err := h.halter.HaltOrg(c.Request().Context(), body.OrgID, body.Reason)
	if err != nil {
		log.Printf("admin: halt-org %s failed: %v", body.OrgID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"org_id": body.OrgID, "halted": n})
}

// ResumeOrg iterates hibernated sandboxes for the org (where halt_reason
// indicates a credits halt, not a user-initiated hibernation) and wakes them.
//
// Body: {"org_id": "<uuid>", "skip_resume": false}
func (h *AdminHandlers) ResumeOrg(c echo.Context) error {
	var body struct {
		OrgID      string `json:"org_id"`
		SkipResume bool   `json:"skip_resume"`
	}
	if err := c.Bind(&body); err != nil || body.OrgID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing or invalid org_id"})
	}
	if h.halter == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "halter not wired"})
	}
	n, err := h.halter.ResumeOrg(c.Request().Context(), body.OrgID, body.SkipResume)
	if err != nil {
		log.Printf("admin: resume-org %s failed: %v", body.OrgID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]any{"org_id": body.OrgID, "resumed": n})
}

// AdminAuth verifies HMAC-SHA256 signature on inbound admin webhooks.
// Headers: X-Timestamp (unix sec), X-Signature (hex). Body is signed as
// "{timestamp}.{body}". Rejects timestamps older than 5 minutes (replay
// protection — the DO refreshes its sig on each retry). Empty secret
// disables auth (dev/test only); operators see a startup warning when
// the admin routes are wired with an empty secret.
func AdminAuth(secret string) echo.MiddlewareFunc {
	keyBytes := []byte(secret)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if secret == "" {
				// Dev-mode pass-through. Logged once at startup, not per request.
				return next(c)
			}
			tsHeader := c.Request().Header.Get("X-Timestamp")
			sigHeader := c.Request().Header.Get("X-Signature")
			if tsHeader == "" || sigHeader == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing signature headers"})
			}
			ts, err := strconv.ParseInt(tsHeader, 10, 64)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid timestamp"})
			}
			if abs(time.Now().Unix()-ts) > 5*60 {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "timestamp out of window"})
			}
			// Read the body, then put it back on the request so the handler can re-bind.
			bodyBytes, err := io.ReadAll(c.Request().Body)
			if err != nil {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": "read body"})
			}
			c.Request().Body = io.NopCloser(bytesReader(bodyBytes))

			mac := hmac.New(sha256.New, keyBytes)
			mac.Write([]byte(tsHeader))
			mac.Write([]byte{'.'})
			mac.Write(bodyBytes)
			expected := hex.EncodeToString(mac.Sum(nil))
			if !hmac.Equal([]byte(expected), []byte(sigHeader)) {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "signature mismatch"})
			}
			return next(c)
		}
	}
}

// bytesReader avoids pulling in bytes.NewReader just for the io.Reader
// interface — we only need Read. Keeps the import surface tight.
type byteSliceReader struct{ b []byte }

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func bytesReader(b []byte) io.Reader { return &byteSliceReader{b: b} }

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// signRequest is a small helper exposed for the halt_reconciler — both
// sides of the HMAC handshake live in this file so the format can't drift.
func signRequest(secret string, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// signGet signs an HMAC over "{ts}.{path_with_query}" — used for the
// halt-list GET request which has no body.
func signGet(secret, ts, pathWithQuery string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte{'.'})
	mac.Write([]byte(pathWithQuery))
	return hex.EncodeToString(mac.Sum(nil))
}

// jsonBody is a tiny helper used by halt_reconciler.go in the same package.
func jsonBody(v any) ([]byte, error) { return json.Marshal(v) }
