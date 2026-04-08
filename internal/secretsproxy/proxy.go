package secretsproxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// privateIPNets contains the CIDR ranges considered private/internal.
// Connections to these ranges are blocked to prevent SSRF attacks.
var privateIPNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"::1/128",
		"fc00::/7",
		"fe80::/10",
	} {
		_, ipnet, _ := net.ParseCIDR(cidr)
		privateIPNets = append(privateIPNets, ipnet)
	}
}

// isPrivateIP returns true if the IP belongs to a private/loopback/link-local range.
func isPrivateIP(ip net.IP) bool {
	for _, ipnet := range privateIPNets {
		if ipnet.Contains(ip) {
			return true
		}
	}
	return false
}

const (
	defaultConnectTimeout   = 10 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
	defaultIdleTimeout      = 60 * time.Second
	defaultSessionTTL       = 24 * time.Hour
)

// Session holds the sealed→real token mapping for one sandbox.
type Session struct {
	SandboxID  string
	Secrets    map[string]string   // "osb_sealed_xxx" → real value
	TokenHosts map[string][]string // sealed token → allowed hosts; nil/empty entry = all allowed hosts
	Allowlist  []string            // nil = all hosts allowed; supports "*." prefix wildcards
	ExpiresAt  time.Time
}

// secretsForHost returns the subset of sealed tokens that are allowed to be replaced for this host.
// Tokens with no host restriction (empty/nil entry in TokenHosts) are always included.
func (s *Session) secretsForHost(host string) map[string]string {
	if len(s.TokenHosts) == 0 {
		return s.Secrets // no per-token restrictions
	}
	filtered := make(map[string]string, len(s.Secrets))
	for token, real := range s.Secrets {
		hosts := s.TokenHosts[token]
		if len(hosts) == 0 || hostAllowed(host, hosts) {
			filtered[token] = real
		}
	}
	return filtered
}

// SecretsProxy is an HTTP CONNECT proxy that intercepts HTTPS traffic from
// sandboxes and substitutes sealed opaque tokens with real secret values.
// Real values never enter the VM — the VM only sees tokens like osb_sealed_xxx.
type SecretsProxy struct {
	ca       *CA
	listener net.Listener

	mu       sync.RWMutex
	sessions map[string]*Session // guestIP → session

	wg     sync.WaitGroup // tracks active connections
	closed chan struct{}

	// dialFunc overrides the default dialUpstream for testing.
	// If nil, the default dialUpstream is used.
	dialFunc          func(target, serverName string) (*tls.Conn, error)
	skipPrivateIPCheck bool // for testing with loopback
}

// NewSecretsProxy creates a new secrets proxy. Call Start() to begin accepting.
func NewSecretsProxy(ca *CA, listenAddr string) (*SecretsProxy, error) {
	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	return &SecretsProxy{
		ca:       ca,
		listener: lis,
		sessions: make(map[string]*Session),
		closed:   make(chan struct{}),
	}, nil
}

// Start begins accepting connections in the background.
func (p *SecretsProxy) Start() {
	go p.serve()
	log.Printf("secrets-proxy: listening on %s", p.listener.Addr())
}

// Stop gracefully shuts down the proxy, waiting for active connections to finish.
func (p *SecretsProxy) Stop() error {
	close(p.closed)
	err := p.listener.Close()
	p.wg.Wait()
	return err
}

// RegisterSession creates or updates a proxy session for the given sandbox IP.
func (p *SecretsProxy) RegisterSession(guestIP string, session *Session) {
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = time.Now().Add(defaultSessionTTL)
	}
	p.mu.Lock()
	p.sessions[guestIP] = session
	p.mu.Unlock()
	log.Printf("secrets-proxy: registered session sandbox=%s ip=%s secrets=%d allowlist=%d ttl=%s",
		session.SandboxID, guestIP, len(session.Secrets), len(session.Allowlist), time.Until(session.ExpiresAt).Round(time.Second))
}

// UnregisterSession removes the proxy session for the given sandbox IP.
func (p *SecretsProxy) UnregisterSession(guestIP string) {
	p.mu.Lock()
	delete(p.sessions, guestIP)
	p.mu.Unlock()
}

// GetSessionTokens returns the sealed token → real value map for the given guest IP.
// Returns nil if no session exists. Used to persist token mappings during hibernate.
func (p *SecretsProxy) GetSessionTokens(guestIP string) map[string]string {
	p.mu.RLock()
	session := p.sessions[guestIP]
	p.mu.RUnlock()
	if session == nil {
		return nil
	}
	return session.Secrets
}

// GetSessionAllowlist returns the egress allowlist for the given guest IP's session.
func (p *SecretsProxy) GetSessionAllowlist(guestIP string) []string {
	p.mu.RLock()
	session := p.sessions[guestIP]
	p.mu.RUnlock()
	if session == nil {
		return nil
	}
	return session.Allowlist
}

// GetSessionTokenHosts returns the per-token host restrictions for the given guest IP's session.
func (p *SecretsProxy) GetSessionTokenHosts(guestIP string) map[string][]string {
	p.mu.RLock()
	session := p.sessions[guestIP]
	p.mu.RUnlock()
	if session == nil {
		return nil
	}
	return session.TokenHosts
}

// ReregisterSession creates a new proxy session from a previously persisted token map.
// Used on wake to restore the proxy session without re-generating tokens.
func (p *SecretsProxy) ReregisterSession(sandboxID, guestIP string, tokens map[string]string, allowlist []string, tokenHosts map[string][]string) {
	session := &Session{
		SandboxID:  sandboxID,
		Secrets:    tokens,
		TokenHosts: tokenHosts,
		Allowlist:  allowlist,
		ExpiresAt:  time.Now().Add(defaultSessionTTL),
	}
	p.mu.Lock()
	p.sessions[guestIP] = session
	p.mu.Unlock()
	log.Printf("secrets-proxy: re-registered session sandbox=%s ip=%s secrets=%d allowlist=%d tokenHosts=%d",
		sandboxID, guestIP, len(tokens), len(allowlist), len(tokenHosts))
}

// CreateSealedEnvs generates sealed tokens for the given env vars, registers a
// proxy session, and returns the complete env map to inject into the VM.
// Includes sealed tokens + proxy config vars (HTTP_PROXY, CA certs, etc.).
// Returns nil if envVars is empty.
//
// allowlist controls which hosts the sandbox can reach (nil = all).
// secretAllowedHosts maps env var name → allowed hosts for that secret (nil = all allowed hosts).
func (p *SecretsProxy) CreateSealedEnvs(sandboxID, guestIP, gatewayIP string, envVars map[string]string, sealedKeys map[string]struct{}, allowlist []string, secretAllowedHosts map[string][]string) map[string]string {
	if len(envVars) == 0 {
		return nil
	}

	// Tokenize only the env vars that came from a SecretStore. Everything else
	// is forwarded to the guest as plaintext — those values were supplied by
	// the user directly to the API, so sealing them would silently break
	// non-HTTP usage (echo $VAR, file writes, subprocess env) without adding
	// any protection.
	sealed := make(map[string]string, len(sealedKeys)) // envVar → token (sealed only)
	tokenMap := make(map[string]string, len(sealedKeys)) // token → real value
	for envVar := range sealedKeys {
		realValue, ok := envVars[envVar]
		if !ok {
			continue
		}
		token := "osb_sealed_" + randomHex(16)
		sealed[envVar] = token
		tokenMap[token] = realValue
	}

	// Build per-token host restrictions from per-env-var restrictions
	var tokenHosts map[string][]string
	if len(secretAllowedHosts) > 0 {
		tokenHosts = make(map[string][]string, len(secretAllowedHosts))
		for envVar, token := range sealed {
			if hosts, ok := secretAllowedHosts[envVar]; ok && len(hosts) > 0 {
				tokenHosts[token] = hosts
			}
		}
	}

	// Only register a proxy session if there's actually something to substitute.
	if len(tokenMap) > 0 {
		p.RegisterSession(guestIP, &Session{
			SandboxID:  sandboxID,
			Secrets:    tokenMap,
			TokenHosts: tokenHosts,
			Allowlist:  allowlist,
		})
	}

	// Build the complete env map for the VM: sealed values replace the original
	// entry under the same name; non-sealed entries pass through as plaintext.
	proxyURL := fmt.Sprintf("http://%s:3128", gatewayIP)
	caCertPath := "/usr/local/share/ca-certificates/opensandbox-proxy.crt"

	noProxy := "localhost,127.0.0.1,::1"

	result := make(map[string]string, len(envVars)+9)
	for k, v := range envVars {
		if token, isSealed := sealed[k]; isSealed {
			result[k] = token
		} else {
			result[k] = v
		}
	}
	result["HTTP_PROXY"] = proxyURL
	result["HTTPS_PROXY"] = proxyURL
	result["http_proxy"] = proxyURL
	result["https_proxy"] = proxyURL
	result["NO_PROXY"] = noProxy
	result["no_proxy"] = noProxy
	result["NODE_EXTRA_CA_CERTS"] = caCertPath
	result["REQUESTS_CA_BUNDLE"] = caCertPath
	result["SSL_CERT_FILE"] = caCertPath
	return result
}

// CACertPEM returns the CA certificate PEM for injection into sandboxes.
func (p *SecretsProxy) CACertPEM() []byte {
	return p.ca.CertPEM()
}

func (p *SecretsProxy) serve() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			select {
			case <-p.closed:
				return
			default:
				log.Printf("secrets-proxy: accept error: %v", err)
				continue
			}
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handleConn(conn)
		}()
	}
}

func (p *SecretsProxy) handleConn(conn net.Conn) {
	defer conn.Close()
	start := time.Now()

	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())

	// Look up session by source IP
	p.mu.RLock()
	session := p.sessions[clientIP]
	p.mu.RUnlock()

	if session == nil {
		writeHTTPError(conn, 407, "no session for this IP")
		log.Printf("secrets-proxy: audit src=%s action=rejected reason=no_session", clientIP)
		return
	}

	if time.Now().After(session.ExpiresAt) {
		writeHTTPError(conn, 407, "session expired")
		log.Printf("secrets-proxy: audit sandbox=%s src=%s action=rejected reason=expired", session.SandboxID, clientIP)
		p.UnregisterSession(clientIP)
		return
	}

	// Parse HTTP CONNECT request properly
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		log.Printf("secrets-proxy: audit sandbox=%s src=%s action=rejected reason=bad_request err=%v",
			session.SandboxID, clientIP, err)
		return
	}

	if req.Method != http.MethodConnect {
		writeHTTPError(conn, 405, "only CONNECT supported")
		return
	}

	targetHost, targetPort, err := net.SplitHostPort(req.Host)
	if err != nil {
		// No port specified — default to 443
		targetHost = req.Host
		targetPort = "443"
	}

	// Force TLS: rewrite port 80 to 443 (never forward plaintext with sealed tokens)
	if targetPort == "80" {
		targetPort = "443"
	}
	target := net.JoinHostPort(targetHost, targetPort)

	// Egress allowlist check
	if len(session.Allowlist) > 0 && !hostAllowed(targetHost, session.Allowlist) {
		writeHTTPError(conn, 403, "host not in allowlist")
		log.Printf("secrets-proxy: audit sandbox=%s src=%s host=%s action=blocked reason=allowlist duration=%s",
			session.SandboxID, clientIP, targetHost, time.Since(start))
		return
	}

	// Private IP blocking — prevent SSRF via proxy
	if !p.skipPrivateIPCheck {
		if err := checkPrivateIP(targetHost); err != nil {
			writeHTTPError(conn, 403, "private IP blocked")
			log.Printf("secrets-proxy: audit sandbox=%s src=%s host=%s action=blocked reason=private_ip duration=%s",
				session.SandboxID, clientIP, targetHost, time.Since(start))
			return
		}
	}

	// Filter secrets to only those allowed for this host
	hostSecrets := session.secretsForHost(targetHost)

	log.Printf("secrets-proxy: audit sandbox=%s src=%s host=%s action=connect secrets=%d filtered=%d",
		session.SandboxID, clientIP, targetHost, len(session.Secrets), len(hostSecrets))

	// Acknowledge the CONNECT tunnel
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	conn.SetReadDeadline(time.Time{}) // clear deadline

	// Sign an ephemeral cert for this host
	leafCert, err := p.ca.SignHost(targetHost)
	if err != nil {
		log.Printf("secrets-proxy: sign cert for %s failed: %v", targetHost, err)
		return
	}

	// TLS handshake with the VM (we act as the server)
	clientTLS := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{*leafCert},
		NextProtos:   []string{"http/1.1"}, // force HTTP/1.1 for reliable token replacement
	})
	clientTLS.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	if err := clientTLS.Handshake(); err != nil {
		log.Printf("secrets-proxy: client TLS handshake failed sandbox=%s host=%s: %v",
			session.SandboxID, targetHost, err)
		return
	}
	clientTLS.SetDeadline(time.Time{})
	defer clientTLS.Close()

	// Connect to the real upstream (always TLS)
	dial := dialUpstream
	if p.dialFunc != nil {
		dial = p.dialFunc
	}
	upstreamConn, err := dial(target, targetHost)
	if err != nil {
		log.Printf("secrets-proxy: dial %s failed: %v", target, err)
		return
	}
	defer upstreamConn.Close()

	if len(hostSecrets) > 0 {
		// HTTP-aware proxy: parse requests, replace tokens in headers + body
		p.proxyHTTPRequests(clientTLS, upstreamConn, hostSecrets, targetHost)
	} else {
		// No replacement needed — raw bidirectional pipe (faster)
		p.proxyRaw(clientTLS, upstreamConn)
	}

	log.Printf("secrets-proxy: audit sandbox=%s src=%s host=%s action=complete duration=%s",
		session.SandboxID, clientIP, targetHost, time.Since(start))
}

// dialUpstream connects to the upstream server with TLS, validating that the
// resolved IP is not private. Returns the TLS connection.
func dialUpstream(target, serverName string) (*tls.Conn, error) {
	// Resolve DNS first to check for private IPs
	host, port, _ := net.SplitHostPort(target)
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return nil, fmt.Errorf("host %s resolves to private IP %s", host, ip)
		}
	}

	tcpConn, err := net.DialTimeout("tcp", net.JoinHostPort(ips[0].String(), port), defaultConnectTimeout)
	if err != nil {
		return nil, err
	}

	upstreamTLS := tls.Client(tcpConn, &tls.Config{
		ServerName: serverName,
		NextProtos: []string{"http/1.1"},
	})
	upstreamTLS.SetDeadline(time.Now().Add(defaultHandshakeTimeout))
	if err := upstreamTLS.Handshake(); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("TLS handshake: %w", err)
	}
	upstreamTLS.SetDeadline(time.Time{})
	return upstreamTLS, nil
}

// checkPrivateIP resolves the host and returns an error if any resolved IP is private.
func checkPrivateIP(host string) error {
	// Check if it's a literal IP
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("private IP %s", ip)
		}
		return nil
	}
	// DNS resolution is checked in dialUpstream — this is a quick pre-check for literal IPs
	return nil
}

// proxyHTTPRequests performs HTTP-aware proxying with token replacement in
// both headers and body.
func (p *SecretsProxy) proxyHTTPRequests(clientTLS *tls.Conn, upstreamTLS *tls.Conn, secrets map[string]string, targetHost string) {
	clientReader := bufio.NewReader(clientTLS)
	upstreamReader := bufio.NewReader(upstreamTLS)

	for {
		// Read request from VM
		clientTLS.SetReadDeadline(time.Now().Add(defaultIdleTimeout))
		req, err := http.ReadRequest(clientReader)
		if err != nil {
			return // connection closed or timeout
		}
		clientTLS.SetReadDeadline(time.Time{})

		// Replace tokens in request headers
		replaceHeaderTokens(req.Header, secrets)

		// Replace tokens in request body
		if req.Body != nil && req.ContentLength != 0 {
			// Buffer the replaced body to compute correct Content-Length.
			// Sealed tokens (43 bytes) may differ in size from real values,
			// so the original Content-Length would be wrong after replacement.
			replacer := newStreamReplacer(req.Body, secrets)
			replaced, err := io.ReadAll(replacer)
			req.Body.Close()
			if err != nil {
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(replaced))
			req.ContentLength = int64(len(replaced))
		}

		// Fix the Host header and URL for upstream
		req.URL.Scheme = "https"
		req.URL.Host = targetHost
		req.RequestURI = "" // must be cleared for client requests

		// Forward to upstream
		if err := req.Write(upstreamTLS); err != nil {
			return
		}

		// Read response from upstream
		resp, err := http.ReadResponse(upstreamReader, req)
		if err != nil {
			return
		}

		// Forward response back to VM (no replacement — responses don't contain our tokens)
		if err := resp.Write(clientTLS); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()

		// Check for connection close
		if resp.Close || req.Close {
			return
		}
	}
}

// proxyRaw performs a raw bidirectional pipe with no token replacement.
func (p *SecretsProxy) proxyRaw(clientTLS *tls.Conn, upstreamTLS *tls.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(upstreamTLS, clientTLS)
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientTLS, upstreamTLS)
	}()

	wg.Wait()
}

// replaceHeaderTokens substitutes sealed tokens in all HTTP header values.
func replaceHeaderTokens(headers http.Header, tokens map[string]string) {
	for key, vals := range headers {
		for i, v := range vals {
			replaced := v
			for token, real := range tokens {
				replaced = strings.ReplaceAll(replaced, token, real)
			}
			if replaced != v {
				headers[key][i] = replaced
			}
		}
	}
}

// hostAllowed returns true if host matches any pattern in allowed.
// Supports exact matches and "*." prefix wildcards (e.g. "*.anthropic.com").
func hostAllowed(host string, allowed []string) bool {
	for _, pattern := range allowed {
		if pattern == "*" || pattern == host {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			if strings.HasSuffix(host, pattern[1:]) {
				return true
			}
		}
	}
	return false
}

func writeHTTPError(conn net.Conn, code int, msg string) {
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Length: 0\r\n\r\n", code, msg)
	conn.Write([]byte(resp))
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
