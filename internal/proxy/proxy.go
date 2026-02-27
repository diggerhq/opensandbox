package proxy

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// SandboxProxy reverse-proxies HTTP traffic from subdomain requests
// to the corresponding sandbox container's published port.
type SandboxProxy struct {
	baseDomain string
	manager    sandbox.Manager
	router     *sandbox.SandboxRouter
}

// New creates a new SandboxProxy.
// baseDomain is the base domain for sandbox subdomains (e.g., "workers.opensandbox.dev" or "localhost").
func New(baseDomain string, mgr sandbox.Manager, router *sandbox.SandboxRouter) *SandboxProxy {
	return &SandboxProxy{
		baseDomain: baseDomain,
		manager:    mgr,
		router:     router,
	}
}

// parsePreviewHostname extracts sandbox ID and port from a preview hostname.
// Format: "{sandboxID}-p{port}.{anyDomain}" — the first subdomain label encodes both.
// Returns ("", 0, false) if the hostname doesn't match the preview pattern.
func parsePreviewHostname(host string) (sandboxID string, port int, ok bool) {
	// Get the first subdomain label (before the first dot)
	dot := strings.Index(host, ".")
	if dot < 0 {
		return "", 0, false
	}
	label := host[:dot]

	// Find last "-p" to split sandboxID and port
	idx := strings.LastIndex(label, "-p")
	if idx < 0 {
		return "", 0, false
	}

	sandboxID = label[:idx]
	portStr := label[idx+2:]
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 || sandboxID == "" {
		return "", 0, false
	}
	return sandboxID, p, true
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

			// Parse preview hostname: {sandboxID}-p{port}.{domain}
			sandboxID, port, ok := parsePreviewHostname(hostOnly)
			if !ok {
				return next(c)
			}

			return p.doProxy(c, sandboxID, port)
		}
	}
}

// isWebSocketUpgrade returns true if the request is a WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// doProxy looks up the sandbox's container address for the given port and reverse-proxies
// the request. If the sandbox is hibernated, it auto-wakes via the router first.
// WebSocket upgrade requests are handled via raw TCP hijacking.
// Normal HTTP requests use a buffered reverse proxy with retry logic
// for transient connection errors after CRIU restore.
func (p *SandboxProxy) doProxy(c echo.Context, sandboxID string, port int) error {
	ctx := c.Request().Context()

	// Route through the sandbox router for auto-wake and rolling timeout reset
	var addr string
	var addrErr error

	routeOp := func(ctx context.Context) error {
		addr, addrErr = p.manager.ContainerAddr(ctx, sandboxID, port)
		return addrErr
	}

	if p.router != nil {
		if err := p.router.Route(ctx, sandboxID, "proxy", routeOp); err != nil {
			log.Printf("proxy: route failed for sandbox %s: %v", sandboxID, err)
			return serveUpstreamUnavailable(c, sandboxID, port)
		}
	} else {
		if err := routeOp(ctx); err != nil {
			return serveUpstreamUnavailable(c, sandboxID, port)
		}
	}

	// WebSocket requests need raw TCP hijacking — can't buffer these.
	if isWebSocketUpgrade(c.Request()) {
		return p.doWebSocket(c, sandboxID, addr, port)
	}

	return p.doHTTP(c, sandboxID, addr, port)
}

// doWebSocket hijacks the client connection and pipes it to the upstream
// container over raw TCP, enabling WebSocket (and any other Upgrade) traffic.
func (p *SandboxProxy) doWebSocket(c echo.Context, sandboxID, addr string, port int) error {
	// Dial the upstream container
	upstream, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Printf("proxy: websocket dial failed for sandbox %s (%s): %v", sandboxID, addr, err)
		return serveUpstreamUnavailable(c, sandboxID, port)
	}
	defer upstream.Close()

	// Hijack the client connection from Echo/net/http
	hijacker, ok := c.Response().Writer.(http.Hijacker)
	if !ok {
		upstream.Close()
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "websocket hijack not supported",
		})
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		log.Printf("proxy: websocket hijack failed for sandbox %s: %v", sandboxID, err)
		return err // connection is in unknown state
	}
	defer clientConn.Close()

	// Forward the original HTTP request (including Upgrade headers) to upstream.
	// This is the raw HTTP request that the upstream will interpret as a WebSocket handshake.
	if err := c.Request().Write(upstream); err != nil {
		log.Printf("proxy: websocket write request failed for sandbox %s: %v", sandboxID, err)
		return nil // connections will close via defers
	}

	// Flush any buffered data the client already sent (e.g., after the headers)
	if clientBuf.Reader.Buffered() > 0 {
		buffered := make([]byte, clientBuf.Reader.Buffered())
		n, _ := clientBuf.Read(buffered)
		if n > 0 {
			upstream.Write(buffered[:n])
		}
	}

	// Bidirectional pipe: client ↔ upstream
	var wg sync.WaitGroup
	wg.Add(2)

	// upstream → client
	go func() {
		defer wg.Done()
		io.Copy(clientConn, upstream)
		// When upstream closes, close the write side of client
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	// client → upstream
	go func() {
		defer wg.Done()
		io.Copy(upstream, clientConn)
		// When client closes, close the write side of upstream
		if tc, ok := upstream.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
	return nil
}

// doHTTP handles normal (non-WebSocket) HTTP requests with buffered reverse proxy
// and retry logic for transient errors after CRIU restore.
func (p *SandboxProxy) doHTTP(c echo.Context, sandboxID, addr string, hostPort int) error {
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
			return serveUpstreamUnavailable(c, sandboxID, hostPort)
		}

		// Success — flush the recorded response to the real client.
		rec.writeTo(c.Response())
		return nil
	}

	// Should not reach here, but just in case.
	return serveUpstreamUnavailable(c, sandboxID, hostPort)
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
