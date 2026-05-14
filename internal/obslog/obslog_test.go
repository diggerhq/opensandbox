package obslog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"testing"
)

// newTestLogger builds an isolated logger writing to buf, with the same
// ctxHandler wrapping the real package uses. Init() can't be used in tests
// because it mutates process globals.
func newTestLogger(buf *bytes.Buffer, host HostFields) *slog.Logger {
	handler := &ctxHandler{
		inner: slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}
	attrs := []slog.Attr{
		slog.String("service", host.Service),
		slog.String("service_id", host.ServiceID),
		slog.String("cell_id", host.CellID),
	}
	return slog.New(handler.WithAttrs(attrs))
}

func TestHostEnvelopeAppearsOnEveryLine(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, HostFields{
		Service:   ServiceControlPlane,
		ServiceID: "cp-1",
		CellID:    "eastus2-default",
	})
	logger.Info("hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if got["service"] != "control-plane" {
		t.Errorf("service = %v, want control-plane", got["service"])
	}
	if got["service_id"] != "cp-1" {
		t.Errorf("service_id = %v, want cp-1", got["service_id"])
	}
	if got["cell_id"] != "eastus2-default" {
		t.Errorf("cell_id = %v, want eastus2-default", got["cell_id"])
	}
	if got["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", got["msg"])
	}
}

func TestRequestFieldsFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, HostFields{Service: ServiceWorker})

	ctx := WithRequest(context.Background(), RequestFields{
		RequestID: "req-abc",
		SandboxID: "sb-xyz",
	})
	logger.InfoContext(ctx, "handling")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if got["request_id"] != "req-abc" {
		t.Errorf("request_id = %v, want req-abc", got["request_id"])
	}
	if got["sandbox_id"] != "sb-xyz" {
		t.Errorf("sandbox_id = %v, want sb-xyz", got["sandbox_id"])
	}
}

func TestRequestFieldsMergeWithoutClobbering(t *testing.T) {
	ctx := WithRequest(context.Background(), RequestFields{RequestID: "req-1"})
	ctx = WithRequest(ctx, RequestFields{SandboxID: "sb-1"})

	got := Request(ctx)
	if got.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want req-1 (must survive second WithRequest)", got.RequestID)
	}
	if got.SandboxID != "sb-1" {
		t.Errorf("SandboxID = %q, want sb-1", got.SandboxID)
	}
}

func TestDetectHostIPSkipsLinkLocal(t *testing.T) {
	// We can't easily inject fake interfaces, but we can assert the returned
	// IP — if any — is not in the link-local or loopback ranges. On a host
	// with at least one normal interface this guards against the Azure IMDS
	// regression where 169.254.169.253 was being reported.
	ip := detectHostIP()
	if ip == "" {
		t.Skip("no non-loopback interface available in test environment")
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		t.Fatalf("detectHostIP returned %q which is not a valid IP", ip)
	}
	if parsed.IsLoopback() {
		t.Errorf("detectHostIP returned loopback %q", ip)
	}
	if parsed.IsLinkLocalUnicast() {
		t.Errorf("detectHostIP returned link-local %q", ip)
	}
}

func TestRequestFieldsOmittedWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf, HostFields{Service: ServiceWorker})
	logger.Info("no request context")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := got["request_id"]; exists {
		t.Errorf("request_id should be absent when not set, got %v", got["request_id"])
	}
	if _, exists := got["sandbox_id"]; exists {
		t.Errorf("sandbox_id should be absent when not set, got %v", got["sandbox_id"])
	}
}
