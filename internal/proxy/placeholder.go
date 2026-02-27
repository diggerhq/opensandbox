package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// placeholderHTML is the friendly waiting page shown to browsers when the
// upstream port inside the sandbox isn't reachable yet. It auto-refreshes
// every 3 seconds so the user sees the real page as soon as it comes up.
const placeholderHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="3">
  <title>Waiting for port %d…</title>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
      background: #0a0a0a;
      color: #e5e5e5;
    }
    .container {
      text-align: center;
      max-width: 480px;
      padding: 2rem;
    }
    .spinner {
      width: 40px;
      height: 40px;
      margin: 0 auto 1.5rem;
      border: 3px solid #333;
      border-top-color: #3b82f6;
      border-radius: 50%%;
      animation: spin 0.8s linear infinite;
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    h1 {
      font-size: 1.25rem;
      font-weight: 600;
      margin-bottom: 0.75rem;
      color: #f5f5f5;
    }
    p {
      font-size: 0.875rem;
      color: #a3a3a3;
      line-height: 1.6;
    }
    .port {
      display: inline-block;
      background: #1e1e1e;
      border: 1px solid #333;
      border-radius: 4px;
      padding: 0.1rem 0.4rem;
      font-family: "SF Mono", SFMono-Regular, Consolas, "Liberation Mono", Menlo, monospace;
      font-size: 0.8rem;
      color: #3b82f6;
    }
    .sandbox-id {
      margin-top: 1.5rem;
      font-size: 0.75rem;
      color: #525252;
      font-family: "SF Mono", SFMono-Regular, Consolas, "Liberation Mono", Menlo, monospace;
    }
  </style>
</head>
<body>
  <div class="container">
    <div class="spinner"></div>
    <h1>Waiting for your app…</h1>
    <p>
      Nothing is listening on port <span class="port">%d</span> yet.<br>
      This page will auto-refresh until the port is ready.
    </p>
    <div class="sandbox-id">%s</div>
  </div>
</body>
</html>`

// wantsBrowserResponse checks if the request Accept header indicates a browser
// (i.e. prefers text/html over JSON).
func wantsBrowserResponse(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// serveUpstreamUnavailable returns either a friendly HTML placeholder page
// (for browsers) or a JSON error (for API clients / curl).
func serveUpstreamUnavailable(c echo.Context, sandboxID string, port int) error {
	if wantsBrowserResponse(c.Request()) {
		html := fmt.Sprintf(placeholderHTML, port, port, sandboxID)
		return c.HTML(http.StatusBadGateway, html)
	}
	return c.JSON(http.StatusBadGateway, map[string]string{
		"error": fmt.Sprintf("sandbox %s: upstream unavailable", sandboxID),
	})
}
