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
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

// ControlPlaneProxy routes subdomain requests to the correct worker.
// It looks up which worker owns the sandbox via the DB, then reverse-proxies
// the request to that worker's HTTP server (which handles the local proxy to the VM).
type ControlPlaneProxy struct {
	baseDomain string
	store      *db.Store
	registry   *controlplane.RedisWorkerRegistry
}

// NewControlPlaneProxy creates a proxy for control plane subdomain routing.
func NewControlPlaneProxy(baseDomain string, store *db.Store, registry *controlplane.RedisWorkerRegistry) *ControlPlaneProxy {
	return &ControlPlaneProxy{
		baseDomain: baseDomain,
		store:      store,
		registry:   registry,
	}
}

// Middleware returns an Echo middleware that intercepts subdomain requests
// and proxies them to the correct worker.
func (p *ControlPlaneProxy) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			host := c.Request().Host

			// Strip port from host
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
func (p *ControlPlaneProxy) extractSandboxID(host string) (string, bool) {
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

// doProxy looks up the worker that owns this sandbox and reverse-proxies to it.
// If the sandbox is hibernated, it triggers a wake-on-request: picks the least
// loaded worker, wakes the sandbox via gRPC, updates the DB, then proxies.
func (p *ControlPlaneProxy) doProxy(c echo.Context, sandboxID string) error {
	ctx := c.Request().Context()

	// Look up which worker owns this sandbox
	session, err := p.store.GetSandboxSession(ctx, sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("sandbox %s not found", sandboxID),
		})
	}

	// If the sandbox is hibernated, wake it on-demand before proxying
	if session.Status == "hibernated" {
		worker, workerURL, err := p.wakeHibernatedSandbox(ctx, sandboxID)
		if err != nil {
			log.Printf("cp-proxy: wake-on-request failed for sandbox %s: %v", sandboxID, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s: wake failed: %v", sandboxID, err),
			})
		}

		log.Printf("cp-proxy: wake-on-request succeeded for sandbox %s → worker %s (%s)", sandboxID, worker.ID, workerURL)

		if isWebSocketUpgrade(c.Request()) {
			return p.doWebSocket(c, sandboxID, workerURL)
		}
		return p.doHTTP(c, sandboxID, workerURL)
	}

	// If the sandbox is stopped/error, return a clear message
	if session.Status == "stopped" || session.Status == "error" {
		return c.JSON(http.StatusGone, map[string]string{
			"error": fmt.Sprintf("sandbox %s has been stopped", sandboxID),
		})
	}

	// Session says "running" — check if the worker is still available.
	// If the worker is gone (e.g., scaled down, restarted), try to recover:
	// check for a checkpoint and wake, or mark as stopped.
	worker := p.registry.GetWorker(session.WorkerID)
	if worker == nil {
		log.Printf("cp-proxy: sandbox %s session says running on worker %s, but worker not in registry", sandboxID, session.WorkerID)
		return p.tryRecoverOrFail(c, ctx, sandboxID, session)
	}

	workerURL := worker.HTTPAddr
	if workerURL == "" {
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": "worker has no HTTP address",
		})
	}

	log.Printf("cp-proxy: routing sandbox %s (worker %s) → %s", sandboxID, session.WorkerID, workerURL)

	// WebSocket requests need raw TCP hijacking
	if isWebSocketUpgrade(c.Request()) {
		return p.doWebSocket(c, sandboxID, workerURL)
	}

	return p.doHTTP(c, sandboxID, workerURL)
}

// tryRecoverOrFail handles the case where a sandbox session says "running" but
// the worker is no longer available. It checks if there's a checkpoint to wake
// from (sandbox may have been hibernated but session not updated). If a checkpoint
// exists, it wakes the sandbox on a new worker. Otherwise, it marks the session
// as stopped and returns a clear error.
func (p *ControlPlaneProxy) tryRecoverOrFail(c echo.Context, ctx context.Context, sandboxID string, session *db.SandboxSession) error {
	// Check if there's a checkpoint we can wake from
	checkpoint, err := p.store.GetActiveCheckpoint(ctx, sandboxID)
	if err == nil && checkpoint != nil {
		log.Printf("cp-proxy: sandbox %s has active checkpoint, attempting recovery wake", sandboxID)
		worker, workerURL, err := p.wakeHibernatedSandbox(ctx, sandboxID)
		if err != nil {
			log.Printf("cp-proxy: recovery wake failed for sandbox %s: %v", sandboxID, err)
			return c.JSON(http.StatusBadGateway, map[string]string{
				"error": fmt.Sprintf("sandbox %s: recovery wake failed: %v", sandboxID, err),
			})
		}

		log.Printf("cp-proxy: recovery wake succeeded for sandbox %s → worker %s (%s)", sandboxID, worker.ID, workerURL)

		if isWebSocketUpgrade(c.Request()) {
			return p.doWebSocket(c, sandboxID, workerURL)
		}
		return p.doHTTP(c, sandboxID, workerURL)
	}

	// No checkpoint — sandbox is truly gone. Mark session as stopped.
	log.Printf("cp-proxy: sandbox %s has no checkpoint and worker is gone, marking stopped", sandboxID)
	errMsg := "worker lost, sandbox not recoverable"
	_ = p.store.UpdateSandboxSessionStatus(ctx, sandboxID, "stopped", &errMsg)

	return c.JSON(http.StatusGone, map[string]string{
		"error": fmt.Sprintf("sandbox %s is no longer available (worker was lost)", sandboxID),
	})
}

// wakeHibernatedSandbox wakes a hibernated sandbox on-demand when its subdomain
// is accessed. It picks the least loaded worker, sends a WakeSandbox gRPC call,
// and updates the DB. Returns the worker entry and its HTTP address for proxying.
func (p *ControlPlaneProxy) wakeHibernatedSandbox(ctx context.Context, sandboxID string) (*controlplane.WorkerEntry, string, error) {
	// Look up the active checkpoint
	checkpoint, err := p.store.GetActiveCheckpoint(ctx, sandboxID)
	if err != nil {
		return nil, "", fmt.Errorf("no active checkpoint: %w", err)
	}

	// Pick the least loaded worker in the same region
	region := checkpoint.Region
	worker, grpcClient, err := p.registry.GetLeastLoadedWorker(region)
	if err != nil {
		return nil, "", fmt.Errorf("no workers available in region %s: %w", region, err)
	}

	log.Printf("cp-proxy: waking sandbox %s on worker %s (region=%s)", sandboxID, worker.ID, region)

	// Wake via gRPC with a generous timeout (cold boot + S3 download)
	grpcCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	_, err = grpcClient.WakeSandbox(grpcCtx, &pb.WakeSandboxRequest{
		SandboxId:     sandboxID,
		CheckpointKey: checkpoint.CheckpointKey,
		Timeout:       300, // default 5 min timeout after wake
	})
	if err != nil {
		return nil, "", fmt.Errorf("gRPC WakeSandbox failed: %w", err)
	}

	// Update DB: mark checkpoint restored, update session to running on new worker
	_ = p.store.MarkCheckpointRestored(ctx, sandboxID)
	_ = p.store.UpdateSandboxSessionForWake(ctx, sandboxID, worker.ID)

	workerURL := worker.HTTPAddr
	if workerURL == "" {
		return nil, "", fmt.Errorf("worker %s has no HTTP address", worker.ID)
	}

	return worker, workerURL, nil
}

// doHTTP reverse-proxies a normal HTTP request to the worker.
// If the worker returns a "not found" error for the sandbox (e.g., after worker restart),
// it marks the session as stopped so future requests get a clean 410.
func (p *ControlPlaneProxy) doHTTP(c echo.Context, sandboxID, workerURL string) error {
	target, err := url.Parse(workerURL)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": "invalid worker URL",
		})
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   10,
	}

	var proxyErr error
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		proxyErr = err
	}

	// The worker's proxy middleware matches on the Host header,
	// so we keep the original Host (sb-xxx.workers.opensandbox.ai).
	proxy.Director = func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		// Preserve original Host header so the worker's proxy can match the subdomain
		r.Host = c.Request().Host
	}

	rec := &responseRecorder{
		header: make(http.Header),
	}
	proxy.ServeHTTP(rec, c.Request())

	if proxyErr != nil {
		log.Printf("cp-proxy: error proxying sandbox %s to %s: %v", sandboxID, workerURL, proxyErr)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("sandbox %s: worker unavailable", sandboxID),
		})
	}

	// If the worker returned a 502 with "not found", the sandbox was lost
	// (e.g., worker restarted). Mark the session as stopped so future
	// requests get a clean 410 Gone instead of a confusing 502.
	if rec.statusCode == http.StatusBadGateway {
		body := rec.body.String()
		if strings.Contains(body, "not found") || strings.Contains(body, "not available") {
			log.Printf("cp-proxy: sandbox %s not found on worker, marking session stopped", sandboxID)
			errMsg := "sandbox lost on worker"
			_ = p.store.UpdateSandboxSessionStatus(c.Request().Context(), sandboxID, "stopped", &errMsg)
			return c.JSON(http.StatusGone, map[string]string{
				"error": fmt.Sprintf("sandbox %s is no longer available", sandboxID),
			})
		}
	}

	rec.writeTo(c.Response())
	return nil
}

// doWebSocket hijacks the connection and pipes it to the worker.
func (p *ControlPlaneProxy) doWebSocket(c echo.Context, sandboxID, workerURL string) error {
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
		log.Printf("cp-proxy: websocket dial failed for sandbox %s (%s): %v", sandboxID, workerAddr, err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": fmt.Sprintf("sandbox %s: worker unavailable", sandboxID),
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
		log.Printf("cp-proxy: websocket hijack failed for sandbox %s: %v", sandboxID, err)
		return err
	}
	defer clientConn.Close()

	// Forward the original request to the worker (preserving Host header)
	if err := c.Request().Write(upstream); err != nil {
		log.Printf("cp-proxy: websocket write request failed for sandbox %s: %v", sandboxID, err)
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
