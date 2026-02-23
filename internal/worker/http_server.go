package worker

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// HTTPServer serves the REST/WebSocket API for direct SDK access on the worker.
// It exposes the same endpoints as the control plane but authenticates via sandbox-scoped JWTs.
type HTTPServer struct {
	echo          *echo.Echo
	manager       *sandbox.Manager
	ptyManager    *sandbox.PTYManager
	jwtIssuer     *auth.JWTIssuer
	sandboxDBs    *sandbox.SandboxDBManager
	router        *sandbox.SandboxRouter
	sandboxDomain string
}

// NewHTTPServer creates a new worker HTTP server for direct SDK access.
func NewHTTPServer(mgr *sandbox.Manager, ptyMgr *sandbox.PTYManager, jwtIssuer *auth.JWTIssuer, sandboxDBs *sandbox.SandboxDBManager, sbProxy *proxy.SandboxProxy, sbRouter *sandbox.SandboxRouter, sandboxDomain string) *HTTPServer {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &HTTPServer{
		echo:          e,
		manager:       mgr,
		ptyManager:    ptyMgr,
		jwtIssuer:     jwtIssuer,
		sandboxDBs:    sandboxDBs,
		router:        sbRouter,
		sandboxDomain: sandboxDomain,
	}

	// Global middleware
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORS())
	e.Use(middleware.RequestID())

	// Subdomain proxy middleware (before auth — subdomain traffic is public)
	if sbProxy != nil {
		e.Use(sbProxy.Middleware())
	}

	// Health check (no auth)
	e.GET("/health", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok", "role": "worker"})
	})

	// Caddy on-demand TLS check — validates that a domain is a valid sandbox subdomain
	e.GET("/caddy/check", func(c echo.Context) error {
		domain := c.QueryParam("domain")
		if domain == "" {
			return c.String(http.StatusForbidden, "missing domain")
		}
		suffix := "." + s.sandboxDomain
		if !strings.HasSuffix(domain, suffix) {
			return c.String(http.StatusForbidden, "invalid domain")
		}
		sub := strings.TrimSuffix(domain, suffix)
		if sub == "" || strings.Contains(sub, ".") {
			return c.String(http.StatusForbidden, "invalid subdomain")
		}
		return c.String(http.StatusOK, "ok")
	})

	// All sandbox routes require JWT auth
	api := e.Group("")
	api.Use(auth.SandboxJWTMiddleware(jwtIssuer))

	// Sandbox status
	api.GET("/sandboxes/:id", s.getSandbox)

	// Commands
	api.POST("/sandboxes/:id/commands", s.runCommand)

	// Timeout
	api.POST("/sandboxes/:id/timeout", s.setTimeout)

	// Filesystem
	api.GET("/sandboxes/:id/files", s.readFile)
	api.PUT("/sandboxes/:id/files", s.writeFile)
	api.GET("/sandboxes/:id/files/list", s.listDir)
	api.POST("/sandboxes/:id/files/mkdir", s.makeDir)
	api.DELETE("/sandboxes/:id/files", s.removeFile)

	// Token refresh
	api.POST("/sandboxes/:id/token/refresh", s.refreshToken)

	// PTY
	api.POST("/sandboxes/:id/pty", s.createPTY)
	api.GET("/sandboxes/:id/pty/:sessionID", s.ptyWebSocket)
	api.POST("/sandboxes/:id/pty/:sessionID/resize", s.resizePTY)
	api.DELETE("/sandboxes/:id/pty/:sessionID", s.killPTY)

	return s
}

// Start starts the HTTP server on the given address.
func (s *HTTPServer) Start(addr string) error {
	return s.echo.Start(addr)
}

// Close gracefully shuts down the server.
func (s *HTTPServer) Close() error {
	return s.echo.Close()
}
