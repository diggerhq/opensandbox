package api

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/cloudflare"
	"github.com/opensandbox/opensandbox/internal/controlplane"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/ecr"
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
	mode       string                  // "server", "worker", "combined"
	workerID   string                  // this worker's ID
	region     string                  // this worker's region
	httpAddr   string                  // public HTTP address for direct access
	sandboxDBs      *sandbox.SandboxDBManager         // per-sandbox SQLite manager
	workos          *auth.WorkOSMiddleware            // nil if WorkOS not configured
	workerRegistry  *controlplane.RedisWorkerRegistry // nil in combined/worker mode
	checkpointStore *storage.CheckpointStore          // nil if hibernation not configured
	sandboxDomain   string                            // base domain for sandbox subdomains
	ecrConfig       *ecr.Config                       // nil if ECR not configured
	cfClient        *cloudflare.Client                // nil if Cloudflare not configured
}

// ServerOpts holds optional dependencies for the API server.
type ServerOpts struct {
	Store       *db.Store
	JWTIssuer   *auth.JWTIssuer
	Mode        string // "server", "worker", "combined"
	WorkerID    string
	Region      string
	HTTPAddr    string
	SandboxDBs     *sandbox.SandboxDBManager
	Router         *sandbox.SandboxRouter             // nil in server-only mode
	SandboxProxy   *proxy.SandboxProxy               // nil if subdomain routing not configured
	ControlPlaneProxy *proxy.ControlPlaneProxy        // nil except in server mode (routes subdomains to workers)
	SandboxDomain  string                             // base domain for sandbox subdomains
	WorkOSConfig    *auth.WorkOSConfig                // nil if WorkOS not configured
	WorkerRegistry  *controlplane.RedisWorkerRegistry  // nil in combined/worker mode
	CheckpointStore *storage.CheckpointStore           // nil if hibernation not configured
	ECRConfig       *ecr.Config                        // nil if ECR not configured
	CFClient        *cloudflare.Client                 // nil if Cloudflare not configured
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
		s.mode = opts.Mode
		s.workerID = opts.WorkerID
		s.region = opts.Region
		s.httpAddr = opts.HTTPAddr
		s.sandboxDBs = opts.SandboxDBs
		s.router = opts.Router
		s.workerRegistry = opts.WorkerRegistry
		s.checkpointStore = opts.CheckpointStore
		s.sandboxDomain = opts.SandboxDomain
		s.ecrConfig = opts.ECRConfig
		s.cfClient = opts.CFClient
	}

	// Global middleware
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORS())
	e.Use(middleware.RequestID())

	// Subdomain proxy middleware (before auth — subdomain traffic is public)
	if opts != nil && opts.SandboxProxy != nil {
		e.Use(opts.SandboxProxy.Middleware())
	}
	if opts != nil && opts.ControlPlaneProxy != nil {
		e.Use(opts.ControlPlaneProxy.Middleware())
	}

	// Health check (no auth)
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// API routes (with API key auth)
	api := e.Group("/api")
	api.Use(auth.PGAPIKeyMiddleware(s.store, apiKey))

	// Sandbox lifecycle
	api.POST("/sandboxes", s.createSandbox)
	api.GET("/sandboxes", s.listSandboxes)
	api.GET("/sandboxes/:id", s.getSandbox)
	api.DELETE("/sandboxes/:id", s.killSandbox)
	api.POST("/sandboxes/:id/timeout", s.setTimeout)

	// Hibernation
	api.POST("/sandboxes/:id/hibernate", s.hibernateSandbox)
	api.POST("/sandboxes/:id/wake", s.wakeSandbox)

	// Preview URLs (on-demand port-based)
	api.POST("/sandboxes/:id/preview", s.createPreviewURL)
	api.GET("/sandboxes/:id/preview", s.listPreviewURLs)
	api.DELETE("/sandboxes/:id/preview/:port", s.deletePreviewURL)

	// Commands
	api.POST("/sandboxes/:id/commands", s.runCommand)

	// Filesystem
	api.GET("/sandboxes/:id/files", s.readFile)
	api.PUT("/sandboxes/:id/files", s.writeFile)
	api.GET("/sandboxes/:id/files/list", s.listDir)
	api.POST("/sandboxes/:id/files/mkdir", s.makeDir)
	api.DELETE("/sandboxes/:id/files", s.removeFile)

	// PTY
	api.POST("/sandboxes/:id/pty", s.createPTY)
	api.GET("/sandboxes/:id/pty/:sessionID", s.ptyWebSocket)
	api.DELETE("/sandboxes/:id/pty/:sessionID", s.killPTY)

	// Templates
	api.POST("/templates", s.buildTemplate)
	api.GET("/templates", s.listTemplates)
	api.GET("/templates/:name", s.getTemplate)
	api.DELETE("/templates/:name", s.deleteTemplate)

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
		dash.GET("/templates", s.dashboardListTemplates)
		dash.POST("/templates", s.dashboardBuildTemplate)
		dash.DELETE("/templates/:id", s.dashboardDeleteTemplate)

		// Session detail + stats
		dash.GET("/sessions/:sandboxId", s.dashboardGetSession)
		dash.GET("/sessions/:sandboxId/stats", s.dashboardGetSessionStats)

		// PTY (terminal)
		dash.POST("/sessions/:sandboxId/pty", s.dashboardCreatePTY)
		dash.GET("/sessions/:sandboxId/pty/:sessionId", s.dashboardPTYWebSocket)
		dash.POST("/sessions/:sandboxId/pty/:sessionId/resize", s.dashboardResizePTY)
		dash.DELETE("/sessions/:sandboxId/pty/:sessionId", s.dashboardKillPTY)
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
func (s *Server) Start(addr string) error {
	return s.echo.Start(addr)
}

// Close gracefully shuts down the server.
func (s *Server) Close() error {
	return s.echo.Close()
}

// Echo returns the underlying echo instance for reuse (e.g., worker HTTP server).
func (s *Server) Echo() *echo.Echo {
	return s.echo
}
