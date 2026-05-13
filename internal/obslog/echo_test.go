package obslog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// TestEchoMiddlewareForwardsRequestIDIntoHandlerContext exercises the real
// middleware chain a server uses: middleware.RequestID generates the header
// (or reuses an inbound one), obslog.EchoMiddleware pulls it into context,
// and a handler emits a log line via slog.InfoContext that carries the same
// id. This is the contract that makes the cross-service join in Axiom work.
func TestEchoMiddlewareForwardsRequestIDIntoHandlerContext(t *testing.T) {
	var buf bytes.Buffer
	// Capture slog output to buf. slog.Default is process-wide so other tests
	// running in parallel would see this; run sequentially.
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	handler := &ctxHandler{
		inner: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	slog.SetDefault(slog.New(handler))

	e := echo.New()
	e.Use(middleware.RequestID())
	e.Use(EchoMiddleware())
	e.GET("/sandboxes/:id/files", func(c echo.Context) error {
		slog.InfoContext(c.Request().Context(), "handler_invoked")
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/sandboxes/sb-test-1/files", nil)
	req.Header.Set(echo.HeaderXRequestID, "req-from-control-plane")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Parse every JSON line; we expect at least one with msg=handler_invoked
	// and one with msg=http_request, both carrying the same request_id +
	// sandbox_id.
	var handlerLine, accessLine map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("non-JSON log line: %q (err=%v)", line, err)
		}
		switch m["msg"] {
		case "handler_invoked":
			handlerLine = m
		case "http_request":
			accessLine = m
		}
	}

	if handlerLine == nil {
		t.Fatalf("no handler log line found in:\n%s", buf.String())
	}
	if accessLine == nil {
		t.Fatalf("no http_request access log line found in:\n%s", buf.String())
	}

	for _, ll := range []map[string]any{handlerLine, accessLine} {
		if ll["request_id"] != "req-from-control-plane" {
			t.Errorf("request_id = %v, want req-from-control-plane (msg=%v)", ll["request_id"], ll["msg"])
		}
		if ll["sandbox_id"] != "sb-test-1" {
			t.Errorf("sandbox_id = %v, want sb-test-1 (msg=%v)", ll["sandbox_id"], ll["msg"])
		}
	}

	if accessLine["status"] != float64(200) {
		t.Errorf("access log status = %v, want 200", accessLine["status"])
	}
	if accessLine["method"] != "GET" {
		t.Errorf("access log method = %v, want GET", accessLine["method"])
	}
}

// TestEchoMiddlewareGeneratesRequestIDIfMissing verifies that Echo's
// middleware.RequestID generates an id when no inbound header is present,
// and that obslog still picks it up. This is the case when a client hits
// the control plane directly.
func TestEchoMiddlewareGeneratesRequestIDIfMissing(t *testing.T) {
	var buf bytes.Buffer
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	handler := &ctxHandler{
		inner: slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	slog.SetDefault(slog.New(handler))

	e := echo.New()
	e.Use(middleware.RequestID())
	e.Use(EchoMiddleware())
	e.GET("/health", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	rid, _ := got["request_id"].(string)
	if rid == "" {
		t.Fatalf("expected generated request_id, got %v", got["request_id"])
	}
}

// Compile-time check: ctxHandler implements slog.Handler so it can be passed
// to slog.New.
var _ slog.Handler = (*ctxHandler)(nil)
var _ context.Context = context.Background()
