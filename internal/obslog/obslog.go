// Package obslog provides structured JSON logging with a stable per-host
// envelope (service, service_id, cell_id, region, hostname, host_ip, version)
// shared across the control plane and worker daemon. Per-request fields
// (request_id, sandbox_id, worker_id) are added via context.
//
// All logs are emitted as JSON on stdout/stderr; Vector reads journald/Docker
// stdout on each host and ships to Axiom. Field names are snake_case and
// fixed — adding a new well-known field requires a code change here so that
// the dashboard schema stays stable across services.
package obslog

import (
	"context"
	"log"
	"log/slog"
	"net"
	"os"
	"strings"
)

// Service identifiers used in the `service` field. Keep this list short and
// stable; adding a value is fine but renaming one breaks dashboards.
const (
	ServiceControlPlane = "control-plane"
	ServiceWorker       = "worker"
	ServiceDB           = "db"
)

// HostFields is the per-process envelope, captured at startup and attached
// to every log line. Empty values are omitted.
type HostFields struct {
	Service   string // "control-plane" | "worker" | "db"
	ServiceID string // worker_id for workers; hostname for control plane
	CellID    string // <region>-<pool>, e.g. "eastus2-default"
	Region    string
	Hostname  string
	HostIP    string
	Version   string
}

// contextKey is unexported to prevent collisions with other packages.
type contextKey struct{}

// RequestFields are per-request attributes attached to log lines via context.
// Empty fields are omitted from output.
type RequestFields struct {
	RequestID string // X-Request-Id, propagated across services
	SandboxID string // when in scope
	WorkerID  string // on control-plane logs, the worker chosen for this request
}

// Init builds a JSON slog.Logger with HostFields baked in, installs it as the
// default slog logger, AND redirects the stdlib `log` package through it so
// existing `log.Printf` call sites are picked up automatically. Returns the
// logger for explicit use.
//
// Call this once from main() before any logging happens.
func Init(h HostFields, level slog.Level) *slog.Logger {
	if h.Hostname == "" {
		h.Hostname, _ = os.Hostname()
	}
	if h.HostIP == "" {
		h.HostIP = detectHostIP()
	}

	handler := &ctxHandler{
		inner: slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: level,
		}),
	}

	attrs := []slog.Attr{}
	if h.Service != "" {
		attrs = append(attrs, slog.String("service", h.Service))
	}
	if h.ServiceID != "" {
		attrs = append(attrs, slog.String("service_id", h.ServiceID))
	}
	if h.CellID != "" {
		attrs = append(attrs, slog.String("cell_id", h.CellID))
	}
	if h.Region != "" {
		attrs = append(attrs, slog.String("region", h.Region))
	}
	if h.Hostname != "" {
		attrs = append(attrs, slog.String("hostname", h.Hostname))
	}
	if h.HostIP != "" {
		attrs = append(attrs, slog.String("host_ip", h.HostIP))
	}
	if h.Version != "" {
		attrs = append(attrs, slog.String("version", h.Version))
	}

	logger := slog.New(handler.WithAttrs(attrs))
	slog.SetDefault(logger)

	// Redirect stdlib log.Printf / log.Println / log.Fatalf through slog so
	// existing call sites emit JSON with the host envelope. Stdlib log calls
	// land at INFO; this loses no semantics because the existing call sites
	// don't differentiate levels.
	log.SetFlags(0)
	log.SetOutput(&slogWriter{logger: logger, level: slog.LevelInfo})

	return logger
}

// WithRequest returns a new context carrying request fields. Subsequent log
// lines emitted with this context (via slog.InfoContext etc.) include them.
func WithRequest(ctx context.Context, f RequestFields) context.Context {
	if existing, ok := ctx.Value(contextKey{}).(RequestFields); ok {
		// Merge — non-empty new fields win, but don't clobber an existing
		// request_id mid-flight.
		if f.RequestID == "" {
			f.RequestID = existing.RequestID
		}
		if f.SandboxID == "" {
			f.SandboxID = existing.SandboxID
		}
		if f.WorkerID == "" {
			f.WorkerID = existing.WorkerID
		}
	}
	return context.WithValue(ctx, contextKey{}, f)
}

// Request returns the RequestFields stored on ctx, or the zero value.
func Request(ctx context.Context) RequestFields {
	if ctx == nil {
		return RequestFields{}
	}
	f, _ := ctx.Value(contextKey{}).(RequestFields)
	return f
}

// ctxHandler wraps a slog.Handler, injecting per-request fields from ctx into
// every record. This means call sites don't have to pass attrs manually —
// `slog.InfoContext(ctx, "msg")` automatically picks up request_id etc.
type ctxHandler struct {
	inner slog.Handler
}

func (h *ctxHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	rf := Request(ctx)
	if rf.RequestID != "" {
		r.AddAttrs(slog.String("request_id", rf.RequestID))
	}
	if rf.SandboxID != "" {
		r.AddAttrs(slog.String("sandbox_id", rf.SandboxID))
	}
	if rf.WorkerID != "" {
		r.AddAttrs(slog.String("worker_id", rf.WorkerID))
	}
	return h.inner.Handle(ctx, r)
}

func (h *ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ctxHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *ctxHandler) WithGroup(name string) slog.Handler {
	return &ctxHandler{inner: h.inner.WithGroup(name)}
}

// slogWriter adapts an io.Writer call (used by stdlib log.Logger) to a slog
// call so legacy `log.Printf("...")` lines flow through the JSON pipeline.
type slogWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	w.logger.Log(context.Background(), w.level, msg)
	return len(p), nil
}

// detectHostIP returns the first non-loopback IPv4 address on the host, or ""
// if none can be discovered. Used as a fallback when OPENSANDBOX_HOST_IP is
// not provisioned at boot.
func detectHostIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		// Skip loopback (127.x) and link-local (169.254.x). On Azure, the
		// IMDS metadata interface advertises 169.254.169.253 and shows up
		// first in InterfaceAddrs() — without this filter, every Azure host
		// reports the same IP, which collapses per-host filtering in Axiom.
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		ip4 := ipnet.IP.To4()
		if ip4 == nil {
			continue
		}
		return ip4.String()
	}
	return ""
}
