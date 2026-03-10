package worker

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/opensandbox/opensandbox/internal/auth"
	"github.com/opensandbox/opensandbox/internal/certmanager"
	"github.com/opensandbox/opensandbox/internal/proxy"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// HTTPServer serves the REST/WebSocket API for direct SDK access on the worker.
// It exposes the same endpoints as the control plane but authenticates via sandbox-scoped JWTs.
type HTTPServer struct {
	echo          *echo.Echo
	manager       sandbox.Manager
	ptyManager    *sandbox.PTYManager
	jwtIssuer     *auth.JWTIssuer
	sandboxDBs    *sandbox.SandboxDBManager
	router        *sandbox.SandboxRouter
	sandboxDomain string
	certFetcher   *certmanager.CertFetcher
}

// NewHTTPServer creates a new worker HTTP server for direct SDK access.
func NewHTTPServer(mgr sandbox.Manager, ptyMgr *sandbox.PTYManager, jwtIssuer *auth.JWTIssuer, sandboxDBs *sandbox.SandboxDBManager, sbProxy *proxy.SandboxProxy, sbRouter *sandbox.SandboxRouter, sandboxDomain string) *HTTPServer {
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

	// Health check (no auth) — includes TLS cert status
	e.GET("/health", func(c echo.Context) error {
		resp := map[string]interface{}{
			"status": "ok",
			"role":   "worker",
		}
		if s.certFetcher != nil {
			exp := s.certFetcher.CertExpiry()
			if exp.IsZero() {
				resp["tls"] = "no_cert"
				resp["status"] = "degraded"
			} else if time.Now().After(exp) {
				resp["tls"] = "expired"
				resp["tls_expiry"] = exp.Format(time.RFC3339)
				resp["status"] = "degraded"
			} else if time.Until(exp) < 24*time.Hour {
				resp["tls"] = "expiring_soon"
				resp["tls_expiry"] = exp.Format(time.RFC3339)
				resp["status"] = "degraded"
			} else {
				resp["tls"] = "ok"
				resp["tls_expiry"] = exp.Format(time.RFC3339)
			}
		}
		status := http.StatusOK
		if resp["status"] == "degraded" {
			status = http.StatusServiceUnavailable
		}
		return c.JSON(status, resp)
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

// SetCertFetcher sets the cert fetcher for TLS health reporting.
func (s *HTTPServer) SetCertFetcher(cf *certmanager.CertFetcher) {
	s.certFetcher = cf
}

// Start starts the HTTP server on the given address.
func (s *HTTPServer) Start(addr string) error {
	return s.echo.Start(addr)
}

// StartTLSWithCert starts the HTTPS server using a dynamic certificate provider.
// The getCert callback is called for each TLS handshake, enabling hot-swap of certs.
func (s *HTTPServer) StartTLSWithCert(addr string, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)) error {
	s.echo.TLSServer.TLSConfig = &tls.Config{
		GetCertificate: getCert,
		MinVersion:     tls.VersionTLS12,
	}
	s.echo.TLSServer.Addr = addr
	return s.echo.StartServer(s.echo.TLSServer)
}

// Close gracefully shuts down the server.
func (s *HTTPServer) Close() error {
	return s.echo.Close()
}
