package api

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/internal/auth"
)

const (
	defaultSignedURLExpiry = 3600     // 1 hour in seconds
	maxSignedURLExpiry     = 86400    // 24 hours in seconds
)

// signedURLSecret returns the HMAC secret or an error if not configured.
func (s *Server) signedURLSecret() ([]byte, error) {
	if s.jwtIssuer == nil {
		return nil, fmt.Errorf("signed URLs require OPENSANDBOX_JWT_SECRET to be configured")
	}
	return s.jwtIssuer.SigningSecret(), nil
}

// createDownloadURL generates a signed download URL for a sandbox file.
// POST /api/sandboxes/:id/files/download-url  (API key authenticated)
func (s *Server) createDownloadURL(c echo.Context) error {
	return s.createSignedURL(c, "download")
}

// createUploadURL generates a signed upload URL for a sandbox file.
// POST /api/sandboxes/:id/files/upload-url  (API key authenticated)
func (s *Server) createUploadURL(c echo.Context) error {
	return s.createSignedURL(c, "upload")
}

type signedURLRequest struct {
	Path      string `json:"path"`
	ExpiresIn int    `json:"expiresIn"` // seconds, optional
}

type signedURLResponse struct {
	URL       string `json:"url"`
	ExpiresAt string `json:"expiresAt"`
}

func (s *Server) createSignedURL(c echo.Context, operation string) error {
	secret, err := s.signedURLSecret()
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}

	sandboxID := c.Param("id")

	var req signedURLRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path is required"})
	}

	expiresIn := req.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultSignedURLExpiry
	}
	if expiresIn > maxSignedURLExpiry {
		expiresIn = maxSignedURLExpiry
	}

	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second)

	params := auth.SignedURLParams{
		SandboxID: sandboxID,
		Path:      req.Path,
		Operation: operation,
		ExpiresAt: expiresAt,
	}

	signature := auth.SignURL(secret, params)

	// Build the URL pointing back at this control plane
	scheme := "https"
	if c.Request().TLS == nil {
		// Check X-Forwarded-Proto for ALB
		if proto := c.Request().Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else {
			scheme = "http"
		}
	}
	host := c.Request().Host

	var endpoint string
	if operation == "download" {
		endpoint = "download"
	} else {
		endpoint = "upload"
	}

	signedURL := fmt.Sprintf("%s://%s/api/sandboxes/%s/files/%s?path=%s&expires=%s&signature=%s",
		scheme, host, sandboxID, endpoint,
		urlEncode(req.Path), params.ExpiresAtUnix(), signature,
	)

	return c.JSON(http.StatusOK, signedURLResponse{
		URL:       signedURL,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
}

// signedDownload serves a file download using a signed URL (no API key required).
// GET /api/sandboxes/:id/files/download?path=...&expires=...&signature=...
func (s *Server) signedDownload(c echo.Context) error {
	secret, err := s.signedURLSecret()
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}

	sandboxID := c.Param("id")
	path := c.QueryParam("path")
	expiresStr := c.QueryParam("expires")
	signature := c.QueryParam("signature")

	if path == "" || expiresStr == "" || signature == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path, expires, and signature are required"})
	}

	expiresAt, err := auth.ParseExpiresUnix(expiresStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	params := auth.SignedURLParams{
		SandboxID: sandboxID,
		Path:      path,
		Operation: "download",
		ExpiresAt: expiresAt,
	}

	if err := auth.VerifyURL(secret, params, signature); err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{"error": err.Error()})
	}

	// Delegate to the existing file read logic.
	// Rewrite query params so readFile/proxy picks up the right path.
	c.QueryParams().Set("path", path)

	if s.sandboxAPIProxy != nil {
		// Server mode: proxy to worker. Rewrite the request path so the proxy
		// strips /api and forwards to the worker's /sandboxes/:id/files endpoint.
		c.Request().URL.Path = fmt.Sprintf("/api/sandboxes/%s/files", sandboxID)
		return s.sandboxAPIProxy.ProxyHandler(c)
	}

	// Combined mode: handle locally
	return s.readFile(c)
}

// signedUpload handles a file upload using a signed URL (no API key required).
// PUT /api/sandboxes/:id/files/upload?path=...&expires=...&signature=...
func (s *Server) signedUpload(c echo.Context) error {
	secret, err := s.signedURLSecret()
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}

	sandboxID := c.Param("id")
	path := c.QueryParam("path")
	expiresStr := c.QueryParam("expires")
	signature := c.QueryParam("signature")

	if path == "" || expiresStr == "" || signature == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "path, expires, and signature are required"})
	}

	expiresAt, err := auth.ParseExpiresUnix(expiresStr)
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	params := auth.SignedURLParams{
		SandboxID: sandboxID,
		Path:      path,
		Operation: "upload",
		ExpiresAt: expiresAt,
	}

	if err := auth.VerifyURL(secret, params, signature); err != nil {
		return c.JSON(http.StatusForbidden, map[string]string{"error": err.Error()})
	}

	// Delegate to the existing file write logic.
	c.QueryParams().Set("path", path)

	if s.sandboxAPIProxy != nil {
		c.Request().URL.Path = fmt.Sprintf("/api/sandboxes/%s/files", sandboxID)
		return s.sandboxAPIProxy.ProxyHandler(c)
	}

	return s.writeFile(c)
}

func urlEncode(s string) string {
	return url.QueryEscape(s)
}
