package worker

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/observability"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// HTTPServer serves the REST/WebSocket API for direct SDK access on the worker.
// It exposes the same endpoints as the control plane but authenticates via sandbox-scoped JWTs.
type HTTPServer struct {
	echo               *echo.Echo
	manager            sandbox.Manager
	ptyManager         *sandbox.PTYManager
	execSessionManager *sandbox.ExecSessionManager
	jwtIssuer          *auth.JWTIssuer
	sandboxDBs         *sandbox.SandboxDBManager
	router             *sandbox.SandboxRouter
	sandboxDomain      string
}

// NewHTTPServer creates a new worker HTTP server for direct SDK access.
func NewHTTPServer(mgr sandbox.Manager, ptyMgr *sandbox.PTYManager, execMgr *sandbox.ExecSessionManager, jwtIssuer *auth.JWTIssuer, sandboxDBs *sandbox.SandboxDBManager, sbProxy *proxy.SandboxProxy, sbRouter *sandbox.SandboxRouter, sandboxDomain string) *HTTPServer {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	s := &HTTPServer{
		echo:               e,
		manager:            mgr,
		ptyManager:         ptyMgr,
		execSessionManager: execMgr,
		jwtIssuer:          jwtIssuer,
		sandboxDBs:         sandboxDBs,
		router:             sbRouter,
		sandboxDomain:      sandboxDomain,
	}

	// Global middleware. Sentry goes first so it can observe panics and
	// attach request context before echo's Recover middleware handles them.
	e.Use(observability.EchoMiddleware())
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

	// All sandbox routes require JWT auth
	api := e.Group("")
	api.Use(auth.SandboxJWTMiddleware(jwtIssuer))

	// Sandbox status
	api.GET("/sandboxes/:id", s.getSandbox)

	// Exec sessions (replaces old /commands)
	api.POST("/sandboxes/:id/exec/run", s.execRun) // static path before parameterized
	api.POST("/sandboxes/:id/exec", s.createExecSession)
	api.GET("/sandboxes/:id/exec", s.listExecSessions)
	api.GET("/sandboxes/:id/exec/:sessionID", s.execSessionWebSocket)
	api.POST("/sandboxes/:id/exec/:sessionID/kill", s.killExecSession)

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

	// Agent sessions (Claude Agent SDK)
	api.POST("/sandboxes/:id/agent", s.createAgentSession)
	api.GET("/sandboxes/:id/agent", s.listAgentSessions)
	api.POST("/sandboxes/:id/agent/:sid/prompt", s.sendAgentPrompt)
	api.POST("/sandboxes/:id/agent/:sid/interrupt", s.interruptAgent)
	api.POST("/sandboxes/:id/agent/:sid/kill", s.killAgentSession)

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
