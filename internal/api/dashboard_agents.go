package api

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
)

const (
	// inboundJWTTTL is the lifetime of the bearer token sessions-api verifies
	// on the proxied request. Sessions-api validates once at the route entry,
	// so this only needs to cover the initial verification — keep it tight.
	inboundJWTTTL = 90 * time.Second

	// downstreamJWTTTL is the lifetime of the JWT sessions-api forwards on
	// its OC API calls (sandbox creation, secret stores, exec.run via SDK).
	// Has to outlast the longest single agent operation: managed-core boot
	// involves checkpoint restore + entrypoint start + adapter waitReady,
	// which can take several minutes. 10m is a comfortable upper bound.
	downstreamJWTTTL = 10 * time.Minute

	// downstreamHeaderName is the header sessions-api relays as a Bearer
	// token on its calls back into OC's /api/sandboxes and /api/secret-stores.
	// Sessions-api never mints; OC pre-mints both tokens here.
	downstreamHeaderName = "X-OC-Downstream-Token"
)

// sessionsAPIBaseURL returns the upstream URL for the agents service. The env
// var matches the CLI's SESSIONS_API_URL (cmd/oc/internal/config) so a single
// value drives both surfaces.
func sessionsAPIBaseURL() string {
	if v := os.Getenv("SESSIONS_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.opencomputer.dev"
}

// dashboardAgentsProxy is a thin reverse proxy from /api/dashboard/agents/*
// → ${SESSIONS_API_URL}/v1/agents/*. It runs after the WorkOS cookie
// middleware, so the request is already authenticated as a real user.
//
// Auth handling:
//   - Mint two identity JWTs (sessions-api inbound, opencomputer-api downstream)
//     using the user's identity from the echo context.
//   - Strip Cookie / X-API-Key on the way out (sessions-api should rely on the
//     Bearer token, never the dashboard cookie).
//   - Set Authorization: Bearer <inbound-jwt>.
//   - Set X-OC-Downstream-Token: <downstream-jwt> for sessions-api to relay.
//
// The CLI continues to use sessions-api directly with X-API-Key — that path
// is untouched.
func (s *Server) dashboardAgentsProxy(c echo.Context) error {
	if s.jwtIssuer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "agents proxy unavailable: jwt issuer not configured",
		})
	}

	orgID, ok := auth.GetOrgID(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, map[string]string{
			"error": "org context required",
		})
	}

	in := auth.IdentityTokenInput{OrgID: orgID.String(), Audience: auth.AudSessionsAPI}
	if userID := auth.GetUserID(c); userID != nil {
		userStr := userID.String()
		in.UserID = &userStr
	}
	if email, ok := c.Get("user_email").(string); ok && email != "" {
		in.Email = &email
	}
	// Lookup workos_user_id (best-effort — used for analytics attribution).
	if s.store != nil {
		if userID := auth.GetUserID(c); userID != nil {
			if user, err := s.store.GetUserByID(c.Request().Context(), *userID); err == nil {
				if user.WorkOSUserID != nil && *user.WorkOSUserID != "" {
					in.WorkOSUserID = user.WorkOSUserID
				}
			}
		}
	}

	inboundToken, err := s.jwtIssuer.IssueIdentityToken(in, inboundJWTTTL)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to mint inbound token",
		})
	}

	downstreamIn := in
	downstreamIn.Audience = auth.AudOpenComputerAPI
	downstreamToken, err := s.jwtIssuer.IssueIdentityToken(downstreamIn, downstreamJWTTTL)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to mint downstream token",
		})
	}

	// Map /api/dashboard/agents[/...] → /v1/agents[/...]
	upstreamPath := "/v1/agents"
	if rest := strings.TrimPrefix(c.Request().URL.Path, "/api/dashboard/agents"); rest != "" {
		upstreamPath += rest
	}
	upstream, err := url.Parse(sessionsAPIBaseURL() + upstreamPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "invalid upstream URL",
		})
	}
	upstream.RawQuery = c.Request().URL.RawQuery

	req, err := http.NewRequestWithContext(c.Request().Context(), c.Request().Method, upstream.String(), c.Request().Body)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to build upstream request",
		})
	}

	// Forward request headers, but drop the ones that should not cross the
	// service boundary. The cookie carries the dashboard session; sessions-api
	// must rely on the Bearer token instead.
	for k, v := range c.Request().Header {
		switch strings.ToLower(k) {
		case "cookie", "authorization", "x-api-key", "host", "content-length":
			continue
		}
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+inboundToken)
	req.Header.Set(downstreamHeaderName, downstreamToken)
	if ct := c.Request().Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("dashboard_agents: upstream call failed: %v", err)
		return c.JSON(http.StatusBadGateway, map[string]string{
			"error": "upstream request failed",
		})
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		// Hop-by-hop headers shouldn't be forwarded.
		switch strings.ToLower(k) {
		case "transfer-encoding", "connection", "keep-alive":
			continue
		}
		c.Response().Header()[k] = v
	}
	c.Response().WriteHeader(resp.StatusCode)
	if _, err := io.Copy(c.Response().Writer, resp.Body); err != nil {
		log.Printf("dashboard_agents: copy upstream body failed: %v", err)
	}
	return nil
}

