package api

import (
	"context"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/redis/go-redis/v9"

	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/billing"
	"github.com/opensandbox/opensandbox/internal/cloudflare"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/edgeclient"
	"github.com/opensandbox/opensandbox/internal/metrics"
	"github.com/opensandbox/opensandbox/internal/observability"
	"github.com/opensandbox/opensandbox/internal/obslog"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
)

var errSandboxNotAvailable = map[string]string{
	"error": "sandbox execution not available in server-only mode",
}

// Server holds the API server dependencies.
type Server struct {
	echo       *echo.Echo
	manager    sandbox.Manager
	router     *sandbox.SandboxRouter  // routes all sandbox interactions (state machine, auto-wake, rolling timeout)
	ptyManager *sandbox.PTYManager
	store      *db.Store               // nil in combined/dev mode without PG
	jwtIssuer  *auth.JWTIssuer         // nil if JWT not configured
	capTokenIssuer *auth.JWTIssuer     // verifies edge→CP capability tokens; nil if SESSION_JWT_SECRET unset
	cfAdminSecret  string              // HMAC shared with CreditAccount DO for /admin/halt-org and /admin/resume-org; empty disables auth (dev only)
	cfEventSecret  string              // HMAC shared with the api-edge Worker for /internal/secret-refresh and other edge-→cell push paths
	cellID     string                  // this control plane's cell_id (for the cap-token cell check)
	mode       string                  // "server", "worker", "combined"
	workerID   string                  // this worker's ID
	region     string                  // this worker's region
	httpAddr   string                  // public HTTP address for direct access
	execSessionManager *sandbox.ExecSessionManager     // nil if not configured
	sandboxDBs      *sandbox.SandboxDBManager         // per-sandbox SQLite manager
	workos          *auth.WorkOSMiddleware            // nil if WorkOS not configured
	workerRegistry  *controlplane.RedisWorkerRegistry // nil in combined/worker mode
	checkpointStore *storage.CheckpointStore          // nil if hibernation not configured
	sandboxDomain   string                            // base domain for sandbox subdomains
	cfClient        *cloudflare.Client                // nil if Cloudflare not configured
	pendingCreates  sync.Map                          // map[sandboxID]*pendingCreate — async sandbox creation tracking
	sandboxAPIProxy *proxy.SandboxAPIProxy            // nil except in server mode (proxies data-plane to workers)
	stripeClient    *billing.StripeClient              // nil if Stripe not configured
	redisClient     *redis.Client                     // nil if Redis not configured (for health checks)
	adminEvents     *AdminEventBus                    // real-time event bus for admin dashboard
	ready           int32                             // atomic: 1 = ready, 0 = not ready

	// Axiom log query (sandbox session logs read API).
	// Empty token = endpoint returns 503.
	axiomQueryToken string
	axiomDataset    string

	// CF api-edge HTTP client — HMAC-signed calls to /internal/templates and
	// /internal/secret-stores. nil when the edge isn't configured (legacy
	// single-PG mode or tests); resolveSecretStoreInto + template lookup fall
	// back to s.store in that case.
	edge *edgeclient.Client
}

// SetEdgeClient wires the api-edge HTTP client. Caller is responsible for
// constructing it with the right base URL + HMAC secret (CFEventSecret).
func (s *Server) SetEdgeClient(c *edgeclient.Client) {
	s.edge = c
}

// SetAxiomQueryConfig wires the read-only Axiom token and dataset for
// the sandbox session logs read API. Token never leaves the control
// plane; the UI proxies through us.
func (s *Server) SetAxiomQueryConfig(queryToken, dataset string) {
	s.axiomQueryToken = queryToken
	s.axiomDataset = dataset
}

// pendingCreate tracks an async sandbox creation.
type pendingCreate struct {
	ready chan struct{} // closed when creation completes
	err   error        // set before closing ready
}

// ServerOpts holds optional dependencies for the API server.
type ServerOpts struct {
	Store       *db.Store
	JWTIssuer   *auth.JWTIssuer
	SessionJWTSecret string // shared edge↔CP HMAC secret; enables /internal/sandboxes/create
	CFAdminSecret    string // HMAC shared with CF CreditAccount DO; enables /admin/halt-org and /admin/resume-org
	CFEventSecret    string // HMAC shared with the api-edge Worker; enables /internal/secret-refresh and other edge-→cell push paths
	CellID      string // this control plane's cell_id
	Mode        string // "server", "worker", "combined"
	WorkerID    string
	Region      string
	HTTPAddr    string
	ExecSessionManager *sandbox.ExecSessionManager
	SandboxDBs     *sandbox.SandboxDBManager
	Router         *sandbox.SandboxRouter             // nil in server-only mode
	SandboxProxy   *proxy.SandboxProxy               // nil if subdomain routing not configured
	ControlPlaneProxy *proxy.ControlPlaneProxy        // nil except in server mode (routes subdomains to workers)
	SandboxDomain  string                             // base domain for sandbox subdomains
	WorkOSConfig    *auth.WorkOSConfig                // nil if WorkOS not configured
	WorkerRegistry  *controlplane.RedisWorkerRegistry  // nil in combined/worker mode
	CheckpointStore *storage.CheckpointStore           // nil if hibernation not configured
	CFClient        *cloudflare.Client                 // nil if Cloudflare not configured
	SandboxAPIProxy *proxy.SandboxAPIProxy             // nil except in server mode (proxies data-plane to workers)
	StripeClient    *billing.StripeClient              // nil if Stripe not configured
	RedisClient     *redis.Client                     // nil if Redis not configured (for health checks)
}

// NewServer creates a new API server with all routes configured.
func NewServer(mgr sandbox.Manager, ptyMgr *sandbox.PTYManager, apiKey string, opts *ServerOpts) *Server {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &Server{
		echo:       e,
		manager:    mgr,
		ptyManager: ptyMgr,
	}

	if opts != nil {
		s.store = opts.Store
		s.jwtIssuer = opts.JWTIssuer
		if opts.SessionJWTSecret != "" {
			s.capTokenIssuer = auth.NewJWTIssuer(opts.SessionJWTSecret)
		}
		s.cfAdminSecret = opts.CFAdminSecret
		s.cfEventSecret = opts.CFEventSecret
		s.cellID = opts.CellID
		s.mode = opts.Mode
		s.workerID = opts.WorkerID
		s.region = opts.Region
		s.httpAddr = opts.HTTPAddr
		s.execSessionManager = opts.ExecSessionManager
		s.sandboxDBs = opts.SandboxDBs
		s.router = opts.Router
		s.workerRegistry = opts.WorkerRegistry
		s.checkpointStore = opts.CheckpointStore
		s.sandboxDomain = opts.SandboxDomain
		s.cfClient = opts.CFClient
		s.sandboxAPIProxy = opts.SandboxAPIProxy
		s.stripeClient = opts.StripeClient
		s.redisClient = opts.RedisClient
		s.adminEvents = NewAdminEventBus()

		// Wire up readiness waiting so the proxy blocks until async creates finish
		if s.sandboxAPIProxy != nil {
			s.sandboxAPIProxy.SetWaitForReady(func(ctx context.Context, sandboxID string) error {
				val, ok := s.pendingCreates.Load(sandboxID)
				if !ok {
					return nil // not a pending create — proceed normally
				}
				pending := val.(*pendingCreate)
				select {
				case <-pending.ready:
					s.pendingCreates.Delete(sandboxID)
					return pending.err
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}
	}

	// Global middleware. Sentry goes first so it can attach request context and
	// observe panics before echo's Recover middleware converts them to 500s.
	// RequestID() runs before obslog.EchoMiddleware so the X-Request-Id header
	// is on the response by the time obslog reads it. obslog replaces Echo's
	// built-in Logger() — same access log line, but JSON with the host
	// envelope and request_id/sandbox_id pulled from context.
	e.Use(observability.EchoMiddleware())
	e.Use(middleware.Recover())
	e.Use(middleware.RequestID())
	e.Use(obslog.EchoMiddleware())
	// Prometheus instrumentation: counts requests by status, observes handler
	// latency, tracks in-flight. Uses c.Path() (route template) for the path
	// label so high-cardinality IDs don't blow up the metric.
	e.Use(metrics.EchoMiddleware())
	e.Use(middleware.CORS())

	// Subdomain proxy middleware (before auth — subdomain traffic is public)
	if opts != nil && opts.SandboxProxy != nil {
		e.Use(opts.SandboxProxy.Middleware())
	}
	if opts != nil && opts.ControlPlaneProxy != nil {
		e.Use(opts.ControlPlaneProxy.Middleware())

		// Edge-forwarded preview URL traffic. The api-edge Worker resolves
		// the public preview hostname (sb-{id}-p{port}.opensandbox.ai) to a
		// cell via D1, then forwards via this cell's Tunnel here. We:
		//
		//   1. strip the /internal/preview/{id}/{port} prefix so the inner
		//      URL path matches what the user originally requested
		//   2. synthesize the Host header the downstream worker proxy
		//      expects (sb-{id}-p{port}.{sandbox_domain}) so the worker's
		//      SandboxProxy.Middleware parses it correctly
		//   3. delegate to ControlPlaneProxy.HandleSandboxRequest, which
		//      reuses the same doProxy logic used by the host-header path —
		//      hibernation wake, worker-loss recovery, all of it.
		//
		// No cap-token auth on this route: it's public-internet sandbox
		// traffic that the edge has already validated (via D1 lookup +
		// sandbox-running check). The auth model is "edge gate, cell
		// trust" — same as POST /internal/sandboxes/create.
		cp := opts.ControlPlaneProxy
		sandboxDomain := opts.SandboxDomain
		e.Any("/internal/preview/:id/:port/*", func(c echo.Context) error {
			id := c.Param("id")
			portStr := c.Param("port")
			port, err := strconv.Atoi(portStr)
			if err != nil || port < 1 || port > 65535 {
				return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid port"})
			}
			prefix := "/internal/preview/" + id + "/" + portStr
			rest := strings.TrimPrefix(c.Request().URL.Path, prefix)
			if rest == "" {
				rest = "/"
			}
			c.Request().URL.Path = rest
			c.Request().Host = id + "-p" + portStr + "." + sandboxDomain
			return cp.HandleSandboxRequest(c, id, port)
		})
	}

	// Health checks (no auth)
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "alive"})
	})
	e.GET("/readyz", s.readinessCheck)
	// Admin routes — accept API key via header or ?key= query param
	admin := e.Group("/admin", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			key := c.Request().Header.Get("X-API-Key")
			if key == "" {
				key = c.QueryParam("key")
			}
			if key == "" || key != apiKey {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
			}
			return next(c)
		}
	})
	admin.GET("/status", s.adminStatusPage)
	admin.GET("/events", s.adminEventsSSE)
	admin.GET("/events/history", s.adminEventsHistory)
	admin.GET("/report", s.adminReport)
	admin.POST("/events/clear", s.adminClearEvents)
	admin.POST("/workers/:id/drain", s.adminSetWorkerDraining)
	admin.GET("/demo/migration", s.demoPingPongPage)
	admin.GET("/demo/chaos", s.demoChaosPage)

	// Signed URL endpoints (self-authenticated via HMAC, no API key required)
	e.GET("/api/sandboxes/:id/files/download", s.signedDownload)
	e.PUT("/api/sandboxes/:id/files/upload", s.signedUpload)

	// Internal routes — called by the api-edge Worker, authenticated by a
	// capability token (HMAC with the shared session-JWT secret). Only mounted
	// when that secret is configured and this is a server-mode CP that can
	// dispatch to workers.
	if s.capTokenIssuer != nil && s.workerRegistry != nil {
		internal := e.Group("/internal", s.capTokenMiddleware)
		internal.POST("/sandboxes/create", s.internalCreateSandbox)

		// Edge-proxied dashboard routes. Same handler functions as
		// /api/dashboard/* (gated below by WorkOS session cookie), exposed
		// under /internal/dashboard/* gated by the edge's cap-token instead.
		// This lets api-edge forward sandbox-runtime ops (PTY, stats, reboot,
		// logs) without requiring users to have a WorkOS cookie on every cell.
		//
		// The dashboard handlers read auth via auth.GetOrgID(c), which both
		// middlewares set — so the same functions work under either auth
		// chain. We only mount the sandbox-runtime subset here; the data-
		// authority routes (api-keys, org settings, checkpoints list) belong
		// on the edge proper, where D1 is the source of truth.
		idash := internal.Group("/dashboard")
		idash.GET("/sessions/:sandboxId", s.dashboardGetSession)
		idash.GET("/sessions/:sandboxId/stats", s.dashboardGetSessionStats)
		idash.POST("/sessions/:sandboxId/reboot", s.dashboardRebootSession)
		idash.POST("/sessions/:sandboxId/power-cycle", s.dashboardPowerCycleSession)
		idash.POST("/sessions/:sandboxId/pty", s.dashboardCreatePTY)
		idash.GET("/sessions/:sandboxId/pty/:sessionId", s.dashboardPTYWebSocket)
		idash.POST("/sessions/:sandboxId/pty/:sessionId/resize", s.dashboardResizePTY)
		idash.DELETE("/sessions/:sandboxId/pty/:sessionId", s.dashboardKillPTY)
		// /images is cell-local (image_cache table lives in each cell's PG).
		// Proxy these here so dashboard can render per-cell image lists.
		idash.GET("/images", s.dashboardListImages)
		idash.DELETE("/images/:id", s.dashboardDeleteImage)
		// Agents — currently proxied to an external service from each cell.
		// Easier to keep that wiring intact than reimplement on the edge.
		idash.Any("/agents", s.dashboardAgentsProxy)
		idash.Any("/agents/*", s.dashboardAgentsProxy)
		idash.GET("/agents/:agentId/entitlements", s.dashboardListAgentEntitlements)
		idash.POST("/agents/:agentId/subscriptions/:feature", s.dashboardSubscribeAgentFeature)
		idash.DELETE("/agents/:agentId/subscriptions/:feature", s.dashboardCancelAgentFeature)
		idash.GET("/sessions/:sandboxId/logs", s.getSandboxLogs)
	}

	// CF admin webhooks — dispatched by the CreditAccount DO on free-tier
	// halt/resume events. HMAC-authenticated via the shared CF_ADMIN_SECRET.
	// Routes live under /admin/ to match the DO's hard-coded path, but use a
	// distinct middleware chain from the human-API-key /admin group above.
	// Echo treats route registrations independently — there's no conflict.
	//
	// Only mounted when this CP can actually halt sandboxes (workers registry
	// present); a server-only CP without workers has no work to do here.
	if s.workerRegistry != nil {
		if s.cfAdminSecret == "" {
			log.Printf("api: WARNING: CF admin webhooks mounted without CF_ADMIN_SECRET — anyone can halt/resume orgs. Set OPENSANDBOX_CF_ADMIN_SECRET in production.")
		}
		cfAdmin := e.Group("/admin", controlplane.AdminAuth(s.cfAdminSecret))
		ah := controlplane.NewAdminHandlers(s)
		cfAdmin.POST("/halt-org", ah.HaltOrg)
		cfAdmin.POST("/resume-org", ah.ResumeOrg)
	}

	// Edge-→cell push: secret-refresh fan-in. The api-edge Worker posts here
	// after writing a new secret entry to D1 so this cell can update any
	// running sandboxes that bind the store. HMAC-authenticated with the
	// shared event secret (same one the forwarder uses, same one the edge
	// signs internal lookups with). Mounted only when both a workerRegistry
	// exists (otherwise there's nothing to fan out to) AND the event secret
	// is configured.
	if s.workerRegistry != nil && s.cfEventSecret != "" {
		cfEdge := e.Group("/internal", controlplane.AdminAuth(s.cfEventSecret))
		cfEdge.POST("/secret-refresh", s.internalSecretRefresh)
	}

	// API routes (with API key auth)
	api := e.Group("/api")
	api.Use(auth.PGAPIKeyMiddleware(s.store, apiKey, s.jwtIssuer, s.capTokenIssuer, s.cellID))

	// Identity
	api.POST("/auth/token", s.createAuthToken)

	// Per-agent paywalled-feature entitlement check, callable from
	// sessions-api with a JWT (aud=opencomputer-api) right before
	// allowing a connect-channel operation.
	api.GET("/agents/:agentId/entitlements/:feature", s.apiAgentEntitlement)

	// Sandbox lifecycle
	api.POST("/sandboxes", s.createSandbox)
	api.GET("/sandboxes", s.listSandboxes)
	api.GET("/sandboxes/:id", s.getSandbox)
	api.DELETE("/sandboxes/:id", s.killSandbox)

	// Sandbox session logs — SDK / curl variant. Same handler as the
	// dashboard's /api/dashboard/sessions/:sandboxId/logs route below;
	// auth here is X-API-Key (or identity-JWT) instead of cookie.
	// Useful for headless testing and SDK consumers.
	api.GET("/sandboxes/:id/logs", s.getSandboxLogs)

	// Reserved capacity (spec: ws-pricing/design/001-reserved-capacity-squares.md)
	api.GET("/capacity/calendar", s.getCapacityCalendar)
	api.POST("/capacity/reservations", s.createCapacityReservation)
	api.GET("/capacity/reservations", s.listCapacityReservations)
	// Internal/undocumented — phase-2 outbox inspection. See note in
	// getCapacityBillableEvents handler.
	api.GET("/capacity/billable-events", s.getCapacityBillableEvents)

	// Usage + tags (design: .agents/design/sandbox-tags-and-usage.md)
	api.GET("/usage", s.getUsage)
	api.GET("/tags", s.listTags)
	api.GET("/sandboxes/:id/usage", s.getSandboxUsage)
	api.GET("/sandboxes/:id/tags", s.getSandboxTags)
	api.PUT("/sandboxes/:id/tags", s.putSandboxTags)

	// Hibernation
	api.POST("/sandboxes/:id/hibernate", s.hibernateSandbox)
	api.POST("/sandboxes/:id/wake", s.wakeSandbox)

	// Reset operations: reboot is a soft, in-place guest restart; power-cycle
	// is a hard restart that re-creates the QEMU process. Both preserve the
	// sandbox's identity and persistent data.
	api.POST("/sandboxes/:id/reboot", s.rebootSandbox)
	api.POST("/sandboxes/:id/power-cycle", s.powerCycleSandbox)

	// Live migration
	api.POST("/sandboxes/:id/migrate", s.migrateSandbox)

	// Resource limits
	api.PUT("/sandboxes/:id/limits", s.setLimits)
	api.POST("/sandboxes/:id/scale", s.scaleSandbox)
	api.PUT("/sandboxes/:id/autoscale", s.setAutoscale)
	api.GET("/sandboxes/:id/autoscale", s.getAutoscale)
	api.PUT("/sandboxes/:id/scaling-lock", s.setScalingLock)
	api.GET("/sandboxes/:id/scaling-lock", s.getScalingLock)
	api.GET("/sandboxes/:id/allowed-hosts", s.getSandboxAllowedHosts)

	// Checkpoints
	api.POST("/sandboxes/:id/checkpoints", s.createCheckpoint)
	api.GET("/sandboxes/:id/checkpoints", s.listCheckpoints)
	api.POST("/sandboxes/:id/checkpoints/:checkpointId/restore", s.restoreCheckpoint)
	api.POST("/sandboxes/from-checkpoint/:checkpointId", s.createFromCheckpoint)
	api.DELETE("/sandboxes/:id/checkpoints/:checkpointId", s.deleteCheckpoint)

	// Checkpoint patches
	api.POST("/sandboxes/checkpoints/:checkpointId/patches", s.createCheckpointPatch)
	api.GET("/sandboxes/checkpoints/:checkpointId/patches", s.listCheckpointPatches)
	api.DELETE("/sandboxes/checkpoints/:checkpointId/patches/:patchId", s.deleteCheckpointPatch)

	// Checkpoint publish / unpublish (design 009)
	api.POST("/sandboxes/checkpoints/:checkpointId/publish", s.publishCheckpoint)
	api.POST("/sandboxes/checkpoints/:checkpointId/unpublish", s.unpublishCheckpoint)

	// Signed file URLs
	api.POST("/sandboxes/:id/files/download-url", s.createDownloadURL)
	api.POST("/sandboxes/:id/files/upload-url", s.createUploadURL)

	// Preview URLs (on-demand port-based)
	api.POST("/sandboxes/:id/preview", s.createPreviewURL)
	api.GET("/sandboxes/:id/preview", s.listPreviewURLs)
	api.DELETE("/sandboxes/:id/preview/:port", s.deletePreviewURL)

	// Data-plane routes: in server mode, proxy to workers; otherwise handle locally
	if s.sandboxAPIProxy != nil {
		// Server mode: proxy all data-plane requests to the worker that owns the sandbox
		pxy := s.sandboxAPIProxy.ProxyHandler

		// Exec
		api.POST("/sandboxes/:id/exec", pxy)
		api.GET("/sandboxes/:id/exec", pxy)
		api.GET("/sandboxes/:id/exec/:sessionID", pxy)
		api.POST("/sandboxes/:id/exec/:sessionID/kill", pxy)
		api.POST("/sandboxes/:id/exec/run", pxy)

		// Agent
		api.POST("/sandboxes/:id/agent", pxy)
		api.GET("/sandboxes/:id/agent", pxy)
		api.POST("/sandboxes/:id/agent/:sid/prompt", pxy)
		api.POST("/sandboxes/:id/agent/:sid/interrupt", pxy)
		api.POST("/sandboxes/:id/agent/:sid/kill", pxy)

		// Filesystem
		api.GET("/sandboxes/:id/files", pxy)
		api.PUT("/sandboxes/:id/files", pxy)
		api.GET("/sandboxes/:id/files/list", pxy)
		api.POST("/sandboxes/:id/files/mkdir", pxy)
		api.DELETE("/sandboxes/:id/files", pxy)

		// PTY
		api.POST("/sandboxes/:id/pty", pxy)
		api.GET("/sandboxes/:id/pty/:sessionID", pxy)
		api.POST("/sandboxes/:id/pty/:sessionID/resize", pxy)
		api.DELETE("/sandboxes/:id/pty/:sessionID", pxy)

		// Timeout
		api.POST("/sandboxes/:id/timeout", pxy)

		// Token refresh
		api.POST("/sandboxes/:id/token/refresh", pxy)
	} else {
		// Combined/worker mode: handle locally
		api.POST("/sandboxes/:id/exec", s.createExecSession)
		api.GET("/sandboxes/:id/exec", s.listExecSessions)
		api.GET("/sandboxes/:id/exec/:sessionID", s.execSessionWebSocket)
		api.POST("/sandboxes/:id/exec/:sessionID/kill", s.killExecSession)
		api.POST("/sandboxes/:id/exec/run", s.execRun)

		api.POST("/sandboxes/:id/agent", s.createAgentSession)
		api.GET("/sandboxes/:id/agent", s.listAgentSessions)
		api.POST("/sandboxes/:id/agent/:sid/prompt", s.sendAgentPrompt)
		api.POST("/sandboxes/:id/agent/:sid/interrupt", s.interruptAgent)
		api.POST("/sandboxes/:id/agent/:sid/kill", s.killAgentSession)

		api.GET("/sandboxes/:id/files", s.readFile)
		api.PUT("/sandboxes/:id/files", s.writeFile)
		api.GET("/sandboxes/:id/files/list", s.listDir)
		api.POST("/sandboxes/:id/files/mkdir", s.makeDir)
		api.DELETE("/sandboxes/:id/files", s.removeFile)

		api.POST("/sandboxes/:id/pty", s.createPTY)
		api.GET("/sandboxes/:id/pty/:sessionID", s.ptyWebSocket)
		api.POST("/sandboxes/:id/pty/:sessionID/resize", s.resizePTY)
		api.DELETE("/sandboxes/:id/pty/:sessionID", s.killPTY)

		api.POST("/sandboxes/:id/timeout", s.setTimeout)
	}

	// Snapshots (pre-built declarative images)
	api.POST("/snapshots", s.createSnapshot)
	api.GET("/snapshots", s.listSnapshots)
	api.GET("/snapshots/:name", s.getSnapshot)
	api.DELETE("/snapshots/:name", s.deleteSnapshot)

	// Snapshot patches (resolve snapshot name → checkpoint, then delegate to checkpoint patch logic)
	api.POST("/snapshots/:name/patches", s.createSnapshotPatch)
	api.GET("/snapshots/:name/patches", s.listSnapshotPatches)
	api.DELETE("/snapshots/:name/patches/:patchId", s.deleteSnapshotPatch)

	// Images (all cached images, named or unnamed)
	api.GET("/images", s.listImages)

	// Image patches — by name or by ID
	api.POST("/images/:name/patches", s.createImagePatch)
	api.GET("/images/:name/patches", s.listImagePatches)
	api.DELETE("/images/:name/patches/:patchId", s.deleteImagePatch)

	// Secret stores
	api.POST("/secret-stores", s.createSecretStore)
	api.GET("/secret-stores", s.listSecretStores)
	api.GET("/secret-stores/:id", s.getSecretStore)
	api.PUT("/secret-stores/:id", s.updateSecretStore)
	api.DELETE("/secret-stores/:id", s.deleteSecretStore)

	// Secret store entries
	api.PUT("/secret-stores/:id/secrets/:name", s.setSecretEntry)
	api.DELETE("/secret-stores/:id/secrets/:name", s.deleteSecretEntry)
	api.GET("/secret-stores/:id/secrets", s.listSecretEntries)

	// Workers (server mode only — queries worker registry)
	api.GET("/workers", s.listWorkers)

	// Session history (requires PG)
	api.GET("/sessions", s.listSessions)

	// WorkOS OAuth + Dashboard API routes (only if WorkOS is configured)
	var frontendURL string
	if opts != nil && opts.WorkOSConfig != nil && opts.WorkOSConfig.APIKey != "" {
		frontendURL = opts.WorkOSConfig.FrontendURL

		s.workos = auth.NewWorkOSMiddleware(*opts.WorkOSConfig, s.store)
		oauthHandlers := auth.NewOAuthHandlers(s.workos)

		// Public OAuth routes
		e.GET("/auth/login", oauthHandlers.HandleLogin)
		e.GET("/auth/callback", oauthHandlers.HandleCallback)
		e.POST("/auth/logout", oauthHandlers.HandleLogout)

		// Dashboard API routes (protected by WorkOS session middleware)
		dash := e.Group("/api/dashboard")
		dash.Use(s.workos.Middleware())

		dash.GET("/me", s.dashboardMe)
		dash.GET("/sessions", s.dashboardSessions)
		dash.GET("/api-keys", s.dashboardListAPIKeys)
		dash.POST("/api-keys", s.dashboardCreateAPIKey)
		dash.DELETE("/api-keys/:keyId", s.dashboardDeleteAPIKey)
		dash.GET("/org", s.dashboardGetOrg)
		dash.PUT("/org", s.dashboardUpdateOrg)
		dash.PUT("/org/custom-domain", s.dashboardSetCustomDomain)
		dash.DELETE("/org/custom-domain", s.dashboardDeleteCustomDomain)
		dash.POST("/org/custom-domain/refresh", s.dashboardRefreshCustomDomain)
		dash.GET("/checkpoints", s.dashboardListCheckpoints)
		dash.DELETE("/checkpoints/:id", s.dashboardDeleteCheckpoint)
		dash.GET("/images", s.dashboardListImages)
		dash.DELETE("/images/:id", s.dashboardDeleteImage)

		// Organization members and invitations
		dash.GET("/org/members", s.dashboardListOrgMembers)
		dash.DELETE("/org/members/:membershipId", s.dashboardRemoveMember)
		dash.POST("/org/invitations", s.dashboardSendInvitation)
		dash.GET("/org/invitations", s.dashboardListInvitations)
		dash.DELETE("/org/invitations/:id", s.dashboardRevokeInvitation)
		dash.GET("/orgs", s.dashboardListOrgs)
		dash.POST("/org/switch", s.dashboardSwitchOrg)
		dash.GET("/org/credits", s.dashboardGetCredits)

		// Billing
		dash.POST("/billing/setup", s.billingSetup)
		dash.GET("/billing", s.billingGet)
		dash.GET("/billing/invoices", s.billingInvoices)
		dash.POST("/billing/redeem", s.billingRedeem)
		dash.POST("/billing/portal", s.billingPortal)
		dash.GET("/billing/agent-subscriptions", s.dashboardListOrgAgentSubscriptions)

		// Admin endpoints

		// Agents — reverse-proxy to sessions-api. Mints short-lived identity
		// JWTs for the inbound (sessions-api) and downstream (OC API) hops so
		// no API key is needed end-to-end. CLI users bypass this and hit
		// sessions-api directly with X-API-Key.
		dash.Any("/agents", s.dashboardAgentsProxy)
		// Per-agent paywalled-feature subscriptions (telegram et al).
		// Mounted BEFORE the catch-all /agents/* proxy so they don't
		// get forwarded to sessions-api.
		dash.GET("/agents/:agentId/entitlements", s.dashboardListAgentEntitlements)
		dash.POST("/agents/:agentId/subscriptions/:feature", s.dashboardSubscribeAgentFeature)
		dash.DELETE("/agents/:agentId/subscriptions/:feature", s.dashboardCancelAgentFeature)

		dash.Any("/agents/*", s.dashboardAgentsProxy)

		// Session detail + stats
		dash.GET("/sessions/:sandboxId", s.dashboardGetSession)
		dash.GET("/sessions/:sandboxId/stats", s.dashboardGetSessionStats)
		// Reset operations
		dash.POST("/sessions/:sandboxId/reboot", s.dashboardRebootSession)
		dash.POST("/sessions/:sandboxId/power-cycle", s.dashboardPowerCycleSession)
		// Sandbox session logs (SSE; historical + 1s-poll live tail).
		// Server queries Axiom server-side with a read-only token that
		// never reaches the browser. Org-ownership enforced via
		// GetSandboxSessionInOrg (404 on mismatch — no cross-org leak).
		dash.GET("/sessions/:sandboxId/logs", s.getSandboxLogs)
		// PTY (terminal)
		dash.POST("/sessions/:sandboxId/pty", s.dashboardCreatePTY)
		dash.GET("/sessions/:sandboxId/pty/:sessionId", s.dashboardPTYWebSocket)
		dash.POST("/sessions/:sandboxId/pty/:sessionId/resize", s.dashboardResizePTY)
		dash.DELETE("/sessions/:sandboxId/pty/:sessionId", s.dashboardKillPTY)
	}

	// Stripe webhook (public — verified by Stripe signature)
	if s.stripeClient != nil {
		e.POST("/webhooks/stripe", s.stripeWebhook)
	}

	// Auto-detect FrontendURL for dev: if web/dist doesn't exist, assume Vite dev on :3000
	if frontendURL == "" && !dashboardDistExists() {
		frontendURL = "http://localhost:3000"
		log.Println("opensandbox: web/dist/ not found, auto-setting FrontendURL=http://localhost:3000 (Vite dev)")
	}

	// Serve web dashboard SPA at root (catch-all after API/auth routes)
	s.serveDashboardUI(e, frontendURL)

	return s
}

// dashboardDistExists checks if the built web dashboard exists.
func dashboardDistExists() bool {
	if _, err := os.Stat("web/dist/index.html"); err == nil {
		return true
	}
	execPath, _ := os.Executable()
	distIndex := filepath.Join(filepath.Dir(execPath), "web", "dist", "index.html")
	if _, err := os.Stat(distIndex); err == nil {
		return true
	}
	return false
}

// serveDashboardUI serves the web dashboard SPA from web/dist/ at the root path.
// All unmatched routes fall through to the SPA (client-side routing).
func (s *Server) serveDashboardUI(e *echo.Echo, frontendURL string) {
	// Look for web/dist relative to the working directory
	distDir := "web/dist"
	if _, err := os.Stat(distDir); err != nil {
		execPath, _ := os.Executable()
		distDir = filepath.Join(filepath.Dir(execPath), "web", "dist")
	}

	if _, err := os.Stat(distDir); err == nil {
		// Production: serve built static files at root
		fsys := os.DirFS(distDir)
		fileServer := http.FileServer(http.FS(fsys))

		spaHandler := echo.WrapHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path := r.URL.Path
			if path == "" || path == "/" {
				http.ServeFileFS(w, r, fsys, "index.html")
				return
			}

			// Serve static asset if it exists
			if f, err := fs.Stat(fsys, strings.TrimPrefix(path, "/")); err == nil && !f.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}

			// SPA fallback — serve index.html for client-side routes
			http.ServeFileFS(w, r, fsys, "index.html")
		}))

		e.GET("/*", spaHandler)
		return
	}

	// Dev mode: proxy to the Vite dev server
	e.GET("/*", func(c echo.Context) error {
		if frontendURL != "" {
			target := frontendURL + c.Request().URL.Path
			return c.Redirect(http.StatusFound, target)
		}
		return c.HTML(http.StatusOK, `<!DOCTYPE html>
<html><head><title>OpenSandbox</title></head><body style="font-family:sans-serif;padding:40px;text-align:center">
<h1>Dashboard not built</h1>
<p>Run <code>cd web && npm run build</code> or start Vite dev: <code>cd web && npm run dev</code></p>
</body></html>`)
	})
}

// Start starts the HTTP server on the given address.
// SetReady marks the server as ready to accept traffic.
func (s *Server) SetReady() {
	atomic.StoreInt32(&s.ready, 1)
}

// SetNotReady marks the server as not ready (draining).
func (s *Server) SetNotReady() {
	atomic.StoreInt32(&s.ready, 0)
}

// readinessCheck verifies the server can serve requests (DB + Redis reachable).
func (s *Server) readinessCheck(c echo.Context) error {
	if atomic.LoadInt32(&s.ready) == 0 {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"reason": "server is draining or starting up",
		})
	}

	result := map[string]string{"status": "ready"}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
	defer cancel()

	if s.store != nil {
		if err := s.store.Ping(ctx); err != nil {
			result["status"] = "not ready"
			result["postgres"] = err.Error()
			return c.JSON(http.StatusServiceUnavailable, result)
		}
		result["postgres"] = "ok"
	}

	if s.redisClient != nil {
		if err := s.redisClient.Ping(ctx).Err(); err != nil {
			result["status"] = "not ready"
			result["redis"] = err.Error()
			return c.JSON(http.StatusServiceUnavailable, result)
		}
		result["redis"] = "ok"
	}

	return c.JSON(http.StatusOK, result)
}

func (s *Server) Start(addr string) error {
	return s.echo.Start(addr)
}

// Shutdown gracefully drains in-flight requests and stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.echo.Shutdown(ctx)
}

// Close immediately shuts down the server (no drain).
func (s *Server) Close() error {
	return s.echo.Close()
}

// Echo returns the underlying echo instance for reuse (e.g., worker HTTP server).
func (s *Server) Echo() *echo.Echo {
	return s.echo
}
