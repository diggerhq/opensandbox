package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// SandboxProxy reverse-proxies HTTP traffic from subdomain requests
// to the corresponding sandbox container's published port.
type SandboxProxy struct {
	baseDomain string
	manager    *sandbox.Manager
	router     *sandbox.SandboxRouter
}

// New creates a new SandboxProxy.
// baseDomain is the base domain for sandbox subdomains (e.g., "workers.opensandbox.dev" or "localhost").
func New(baseDomain string, mgr *sandbox.Manager, router *sandbox.SandboxRouter) *SandboxProxy {
	return &SandboxProxy{
		baseDomain: baseDomain,
		manager:    mgr,
		router:     router,
	}
}

// Middleware returns an Echo middleware that intercepts subdomain requests
// and proxies them to the sandbox container. Non-subdomain requests pass through.
func (p *SandboxProxy) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			host := c.Request().Host

			// Strip port from host for matching
			hostOnly := host
			if idx := strings.LastIndex(host, ":"); idx != -1 {
				hostOnly = host[:idx]
			}

			sandboxID, ok := p.extractSandboxID(hostOnly)
			if !ok {
				return next(c)
			}

			return p.doProxy(c, sandboxID)
		}
	}
}

// extractSandboxID parses "{sandboxID}.{baseDomain}" from the host.
// For baseDomain "localhost", matches "{sandboxID}.localhost".
// For baseDomain "workers.opensandbox.dev", matches "{sandboxID}.workers.opensandbox.dev".
func (p *SandboxProxy) extractSandboxID(host string) (string, bool) {
	suffix := "." + p.baseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	sandboxID := strings.TrimSuffix(host, suffix)
	if sandboxID == "" || strings.Contains(sandboxID, ".") {
		return "", false
	}
	return sandboxID, true
}

// doProxy looks up the sandbox's host port and reverse-proxies the request.
// If the sandbox is hibernated, it auto-wakes via the router first.
// After restore, the upstream process may need a moment to stabilize its
// listening socket. We retry on transient connection errors (reset, refused)
// rather than using a static sleep, so the first request after wake succeeds
// without adding unnecessary latency.
func (p *SandboxProxy) doProxy(c echo.Context, sandboxID string) error {
	ctx := c.Request().Context()

	// Route through the sandbox router for auto-wake and rolling timeout reset
	var hostPort int
	var portErr error

	routeOp := func(ctx context.Context) error {
		hostPort, portErr = p.manager.HostPort(ctx, sandboxID)
		return portErr
	}

	if p.router != nil {
		if err := p.router.Route(ctx, sandboxID, "proxy", routeOp); err != nil {
			log.Printf("proxy: route failed for sandbox %s: %v", sandboxID, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s not available: %v", sandboxID, err),
			})
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s not available: %v", sandboxID, err),
			})
		}
	}

	// Buffer the request body so we can replay it across retries.
	var bodyBytes []byte
	if c.Request().Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(c.Request().Body)
		if err != nil {
			return c.JSON(http.StatusBadRequest, map[string]string{
				"error": "failed to read request body",
			})
		}
		c.Request().Body.Close()
	}

	addr := fmt.Sprintf("127.0.0.1:%d", hostPort)
	target := &url.URL{
		Scheme: "http",
		Host:   addr,
	}

	// Retry loop: after CRIU restore the process's listening socket may need
	// a few hundred ms to stabilize. We retry the full proxy attempt on
	// transient errors (connection refused, reset, EOF) with backoff.
	const maxRetries = 6
	delay := 50 * time.Millisecond

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			delay *= 2
			if delay > 500*time.Millisecond {
				delay = 500 * time.Millisecond
			}
		}

		// Reset body for this attempt
		if bodyBytes != nil {
			c.Request().Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		// Use a recorder to capture the proxy response so we can detect
		// errors and retry without having already written to the client.
		rec := &responseRecorder{
			header: make(http.Header),
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 2 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second,
		}

		var proxyErr error
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err
		}

		proxy.ServeHTTP(rec, c.Request())

		if proxyErr != nil {
			if isRetryable(proxyErr) && attempt < maxRetries {
				continue
			}
			log.Printf("proxy: error proxying to sandbox %s (port %d) after %d attempts: %v",
				sandboxID, hostPort, attempt+1, proxyErr)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s: upstream unavailable", sandboxID),
			})
		}

		// Success — flush the recorded response to the real client.
		rec.writeTo(c.Response())
		return nil
	}

	// Should not reach here, but just in case.
	return c.JSON(http.StatusBadGateway, map[string]string{
		"error": fmt.Sprintf("sandbox %s: upstream unavailable after retries", sandboxID),
	})
}

// responseRecorder captures an HTTP response in memory so we can decide
// whether to retry (on connection error) or flush it to the real client.
type responseRecorder struct {
	header     http.Header
	body       bytes.Buffer
	statusCode int
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	return r.body.Write(b)
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for k, vals := range r.header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	code := r.statusCode
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	w.Write(r.body.Bytes())
}

// isRetryable returns true for transient connection errors that may resolve
// after a CRIU-restored process stabilizes its listening socket.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "closed network connection") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "broken pipe")
}
