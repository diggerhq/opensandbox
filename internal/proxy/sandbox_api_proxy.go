package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// SandboxAPIProxy proxies data-plane HTTP/WebSocket requests from the control
// plane to the worker that owns the sandbox. This enables ALB-based TLS
// termination: clients talk to the control plane through the ALB, and the
// control plane forwards exec/files/pty/agent requests to workers over the
// internal VPC network.
type SandboxAPIProxy struct {
	store        *db.Store
	registry     *controlplane.RedisWorkerRegistry
	jwtIssuer    *auth.JWTIssuer
	waitForReady func(ctx context.Context, sandboxID string) error // blocks until async sandbox creation completes; nil = no-op
}

// NewSandboxAPIProxy creates a new sandbox API proxy.
func NewSandboxAPIProxy(store *db.Store, registry *controlplane.RedisWorkerRegistry, jwtIssuer *auth.JWTIssuer) *SandboxAPIProxy {
	return &SandboxAPIProxy{
		store:    store,
		registry: registry,
		jwtIssuer: jwtIssuer,
	}
}

// SetWaitForReady sets a callback that blocks until an async sandbox creation
// completes. The proxy calls this before forwarding requests to avoid proxying
// to a worker that hasn't finished booting the sandbox yet.
func (p *SandboxAPIProxy) SetWaitForReady(fn func(ctx context.Context, sandboxID string) error) {
	p.waitForReady = fn
}

// ProxyHandler forwards requests for a sandbox to the worker that owns it.
// It extracts the sandbox ID from the path param ":id", looks up the worker,
// mints a short-lived JWT, and proxies the request.
func (p *SandboxAPIProxy) ProxyHandler(c echo.Context) error {
	sandboxID := c.Param("id")
	if sandboxID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "sandbox ID required",
		})
	}

	ctx := c.Request().Context()

	// If this sandbox is still being created asynchronously, wait for it.
	if p.waitForReady != nil {
		if err := p.waitForReady(ctx, sandboxID); err != nil {
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s: creation failed: %v", sandboxID, err),
			})
		}
	}

	// Look up which worker owns this sandbox
	session, err := p.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("sandbox %s not found", sandboxID),
		})
	}

	// If hibernated, wake on demand
	if session.Status == "hibernated" {
		worker, workerURL, err := p.wakeHibernatedSandbox(ctx, sandboxID)
		if err != nil {
			log.Printf("sandbox-api-proxy: wake-on-request failed for sandbox %s: %v", sandboxID, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s: failed to wake: %v", sandboxID, err),
			})
		}
		log.Printf("sandbox-api-proxy: wake-on-request succeeded for sandbox %s → worker %s (%s)", sandboxID, worker.ID, workerURL)
		return p.forward(c, sandboxID, workerURL, session.WorkerID)
	}

	if session.Status == "stopped" || session.Status == "error" {
		return c.JSON(http.StatusGone, map[string]string{
			"error": fmt.Sprintf("sandbox %s has been stopped", sandboxID),
		})
	}

	// Look up worker address
	worker := p.registry.GetWorker(session.WorkerID)
	if worker == nil {
		// Worker gone — try to recover from hibernation checkpoint
		return p.tryRecoverOrFail(c, ctx, sandboxID, session)
	}

	if worker.HTTPAddr == "" {
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("worker %s has no HTTP address", session.WorkerID),
		})
	}

	return p.forward(c, sandboxID, worker.HTTPAddr, session.WorkerID)
}

// forward proxies the request to the worker, adding a sandbox JWT for auth.
func (p *SandboxAPIProxy) forward(c echo.Context, sandboxID, workerURL, workerID string) error {
	// Mint a short-lived JWT for the worker
	token := ""
	if p.jwtIssuer != nil {
		orgID, _ := auth.GetOrgID(c)
		t, err := p.jwtIssuer.IssueSandboxToken(orgID, sandboxID, workerID, 5*time.Minute)
		if err == nil {
			token = t
		}
	}

	if isWebSocketUpgrade(c.Request()) {
		return p.doWebSocket(c, sandboxID, workerURL, token)
	}
	return p.doHTTP(c, sandboxID, workerURL, token)
}

// doHTTP reverse-proxies a normal HTTP request to the worker.
// Streams the response directly to the client without buffering, enabling
// large file downloads via signed URLs.
func (p *SandboxAPIProxy) doHTTP(c echo.Context, sandboxID, workerURL, token string) error {
	target, err := url.Parse(workerURL)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": "invalid worker URL",
		})
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1 // flush chunks immediately
	proxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 600 * time.Second, // exec/run can take minutes (npm build, etc.)
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       120 * time.Second,
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("sandbox-api-proxy: error proxying sandbox %s to %s: %v", sandboxID, workerURL, err)
		// Write error JSON directly — headers may not have been sent yet
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"sandbox %s: upstream unavailable"}`, sandboxID)
	}

	// Rewrite request to target worker, preserving the original path
	proxy.Director = func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		// Path is already correct (/sandboxes/:id/...)
		// Remove the /api prefix — worker routes don't have it
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		r.URL.RawQuery = c.Request().URL.RawQuery
		r.Host = target.Host

		// Set sandbox JWT auth for the worker
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}

	// Serve directly to the client ResponseWriter — no buffering
	proxy.ServeHTTP(c.Response().Writer, c.Request())
	return nil
}

// doWebSocket hijacks the connection and pipes it to the worker.
func (p *SandboxAPIProxy) doWebSocket(c echo.Context, sandboxID, workerURL, token string) error {
	target, err := url.Parse(workerURL)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{"error": "invalid worker URL"})
	}

	// Connect to the worker
	workerAddr := target.Host
	if !strings.Contains(workerAddr, ":") {
		if target.Scheme == "https" {
			workerAddr += ":443"
		} else {
			workerAddr += ":80"
		}
	}

	upstream, err := net.DialTimeout("tcp", workerAddr, 5*time.Second)
	if err != nil {
		log.Printf("sandbox-api-proxy: websocket dial failed for sandbox %s (%s): %v", sandboxID, workerAddr, err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("sandbox %s: upstream unavailable", sandboxID),
		})
	}
	defer upstream.Close()

	// Hijack client connection
	hijacker, ok := c.Response().Writer.(http.Hijacker)
	if !ok {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "websocket hijack not supported",
		})
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		log.Printf("sandbox-api-proxy: websocket hijack failed for sandbox %s: %v", sandboxID, err)
		return err
	}
	defer clientConn.Close()

	// Modify the request: strip /api prefix and inject JWT auth
	req := c.Request()
	req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api")
	req.Host = target.Host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// Forward the modified request to the upstream worker
	if err := req.Write(upstream); err != nil {
		log.Printf("sandbox-api-proxy: websocket write request failed for sandbox %s: %v", sandboxID, err)
		return nil
	}

	// Flush any buffered client data
	if clientBuf.Reader.Buffered() > 0 {
		buffered := make([]byte, clientBuf.Reader.Buffered())
		n, _ := clientBuf.Read(buffered)
		if n > 0 {
			upstream.Write(buffered[:n])
		}
	}

	// Bidirectional pipe
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(clientConn, upstream)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		io.Copy(upstream, clientConn)
		if tc, ok := upstream.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
	return nil
}

// tryRecoverOrFail handles the case where a sandbox's worker is gone.
func (p *SandboxAPIProxy) tryRecoverOrFail(c echo.Context, ctx context.Context, sandboxID string, session *db.SandboxSession) error {
	checkpoint, err := p.store.GetActiveHibernation(ctx, sandboxID)
	if err == nil && checkpoint != nil {
		log.Printf("sandbox-api-proxy: sandbox %s has active hibernation, attempting recovery wake", sandboxID)
		worker, workerURL, err := p.wakeHibernatedSandbox(ctx, sandboxID)
		if err != nil {
			log.Printf("sandbox-api-proxy: recovery wake failed for sandbox %s: %v", sandboxID, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s: worker unavailable", sandboxID),
			})
		}

		log.Printf("sandbox-api-proxy: recovery wake succeeded for sandbox %s → worker %s (%s)", sandboxID, worker.ID, workerURL)
		return p.forward(c, sandboxID, workerURL, worker.ID)
	}

	// No hibernation — sandbox is truly gone
	log.Printf("sandbox-api-proxy: sandbox %s has no hibernation and worker is gone, marking stopped", sandboxID)
	errMsg := "worker lost, sandbox not recoverable"
	_ = p.store.UpdateSandboxSessionStatus(ctx, sandboxID, "stopped", &errMsg)

	return c.JSON(http.StatusGone, map[string]string{
		"error": fmt.Sprintf("sandbox %s is no longer available (worker was lost)", sandboxID),
	})
}

// wakeHibernatedSandbox wakes a hibernated sandbox on the least loaded worker.
func (p *SandboxAPIProxy) wakeHibernatedSandbox(ctx context.Context, sandboxID string) (*controlplane.WorkerEntry, string, error) {
	checkpoint, err := p.store.GetActiveHibernation(ctx, sandboxID)
	if err != nil {
		return nil, "", fmt.Errorf("no active hibernation: %w", err)
	}

	region := checkpoint.Region
	worker, grpcClient, err := p.registry.GetLeastLoadedWorker(region)
	if err != nil {
		return nil, "", fmt.Errorf("no workers available in region %s: %w", region, err)
	}

	log.Printf("sandbox-api-proxy: waking sandbox %s on worker %s (region=%s)", sandboxID, worker.ID, region)

	grpcCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	_, err = grpcClient.WakeSandbox(grpcCtx, &pb.WakeSandboxRequest{
		SandboxId:     sandboxID,
		CheckpointKey: checkpoint.HibernationKey,
		Timeout:       300,
	})
	if err != nil {
		return nil, "", fmt.Errorf("gRPC WakeSandbox failed: %w", err)
	}

	_ = p.store.MarkHibernationRestored(ctx, sandboxID)
	_ = p.store.UpdateSandboxSessionForWake(ctx, sandboxID, worker.ID)

	if worker.HTTPAddr == "" {
		return nil, "", fmt.Errorf("worker %s has no HTTP address", worker.ID)
	}

	return worker, worker.HTTPAddr, nil
}
