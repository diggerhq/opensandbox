package secretsproxy

import (
	"bufio"
	"container/list"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Helpers ──────────────────────────────────────────────────────────────────

func makeToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "osb_sealed_" + hex.EncodeToString(b)
}

func makeTokenMap(n int) map[string]string {
	m := make(map[string]string, n)
	for range n {
		token := makeToken()
		m[token] = "sk-real-" + token[11:]
	}
	return m
}

// generateTestCA creates an in-memory CA for benchmarks (no disk).
func generateTestCA() *CA {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Bench CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(certDER)

	return &CA{
		cert: cert,
		key:  key,
		certPEM: []byte("-----BEGIN CERTIFICATE-----\n" +
			"(bench CA)\n-----END CERTIFICATE-----\n"),
		cache: make(map[string]*cachedCert),
		lru:   list.New(),
	}
}

// startFakeUpstream starts a TLS server that accepts HTTP/1.1 requests
// and replies with a fixed 200 OK + JSON body. Returns the address and
// a TLS config the proxy can use to connect to it.
func startFakeUpstream(t testing.TB) (addr string, cleanup func()) {
	t.Helper()
	// Self-signed cert for the upstream
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "upstream.test"},
		DNSNames:     []string{"upstream.test", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}

	lis, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"http/1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	responseBody := `{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"claude-3-opus","stop_reason":"end_turn"}`

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				reader := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(reader)
					if err != nil {
						return
					}
					if req.Body != nil {
						io.Copy(io.Discard, req.Body)
						req.Body.Close()
					}
					resp := &http.Response{
						StatusCode:    200,
						ProtoMajor:    1,
						ProtoMinor:    1,
						Header:        http.Header{"Content-Type": {"application/json"}},
						Body:          io.NopCloser(strings.NewReader(responseBody)),
						ContentLength: int64(len(responseBody)),
					}
					resp.Write(c)
				}
			}(conn)
		}
	}()

	return lis.Addr().String(), func() { lis.Close() }
}

// ── Pretty-printed report ───────────────────────────────────────────────────

func TestProxyBenchmarkReport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark report in short mode")
	}

	// Suppress proxy audit logs during benchmark
	log.SetOutput(io.Discard)
	defer log.SetOutput(nil) // nil restores default (os.Stderr)

	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                Secrets Proxy — Performance Report                            ║")
	fmt.Println("║  Architecture: 1 proxy per worker, shared across all sandboxes               ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	// ──────────────────────────────────────────────────────────────────
	// Section 1: Replacement CPU cost (isolated)
	// ──────────────────────────────────────────────────────────────────
	fmt.Println("║                                                                              ║")
	fmt.Println("║  1. REPLACEMENT CPU COST (no I/O — pure computation)                         ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	singleToken := makeToken()

	type cpuTest struct {
		name   string
		iters  int
		run    func()
		bodyN  int // for throughput calc, 0 = skip
	}

	cpuTests := []cpuTest{
		{
			"Header replacement (5 hdrs, 1 secret)",
			200_000,
			func() {
				tokens := map[string]string{singleToken: "sk-real-value"}
				headers := http.Header{
					"Authorization": {"Bearer " + singleToken},
					"Content-Type":  {"application/json"},
					"Accept":        {"application/json"},
					"User-Agent":    {"sdk/1.0"},
					"X-Req-Id":      {"abc123"},
				}
				replaceHeaderTokens(headers, tokens)
			},
			0,
		},
		{
			"Body scan: 90B, 1 token (typical)",
			200_000,
			func() {
				body := fmt.Sprintf(`{"api_key":"%s"}`, singleToken)
				r := newStreamReplacer(strings.NewReader(body), map[string]string{singleToken: "sk-real"})
				io.Copy(io.Discard, r)
			},
			90,
		},
		{
			"Body scan: 7KB, no tokens (scan-only)",
			100_000,
			func() {
				body := strings.Repeat(`{"model":"claude","msg":"hello world, this is padding"}`, 130)
				r := newStreamReplacer(strings.NewReader(body), map[string]string{singleToken: "sk-real"})
				io.Copy(io.Discard, r)
			},
			7000,
		},
		{
			"Body scan: 64KB, 1 token",
			20_000,
			func() {
				body := singleToken + strings.Repeat("x", 64*1024-tokenLen)
				r := newStreamReplacer(strings.NewReader(body), map[string]string{singleToken: "sk-real"})
				io.Copy(io.Discard, r)
			},
			64 * 1024,
		},
		{
			"Body buffer + Content-Length recalc (io.ReadAll)",
			100_000,
			func() {
				body := fmt.Sprintf(`{"api_key":"%s","data":"%s"}`, singleToken, strings.Repeat("x", 2000))
				tokens := map[string]string{singleToken: "sk-ant-real-key-that-is-longer"}
				replacer := newStreamReplacer(strings.NewReader(body), tokens)
				replaced, _ := io.ReadAll(replacer) // this is what the real code does
				_ = int64(len(replaced))            // recalc Content-Length
			},
			2100,
		},
		{
			"Host filtering: 20 secrets, half restricted",
			200_000,
			func() {
				secrets := makeTokenMap(20)
				tokenHosts := make(map[string][]string)
				j := 0
				for tok := range secrets {
					if j%2 == 0 {
						tokenHosts[tok] = []string{"api.anthropic.com"}
					}
					j++
				}
				s := &Session{Secrets: secrets, TokenHosts: tokenHosts}
				s.secretsForHost("api.anthropic.com")
			},
			0,
		},
	}

	fmt.Printf("║  %-50s %8s  %10s  ║\n", "Operation", "Latency", "Throughput")
	fmt.Println("║  ────────────────────────────────────────────────── ────────  ──────────  ║")
	for _, tt := range cpuTests {
		start := time.Now()
		for range tt.iters {
			tt.run()
		}
		elapsed := time.Since(start)
		perOp := elapsed / time.Duration(tt.iters)
		tp := ""
		if tt.bodyN > 0 {
			bps := float64(tt.bodyN) * float64(tt.iters) / elapsed.Seconds()
			tp = fmtBytes(bps) + "/s"
		}
		fmt.Printf("║  %-50s %8s  %10s  ║\n", tt.name, fmtDuration(perOp), tp)
	}

	// ──────────────────────────────────────────────────────────────────
	// Section 2: Cert signing (the hidden cost)
	// ──────────────────────────────────────────────────────────────────
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  2. CERT SIGNING (CA generates ephemeral TLS cert per host)                 ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	ca := generateTestCA()

	fmt.Printf("║  %-50s %8s  %10s  ║\n", "Operation", "Latency", "")
	fmt.Println("║  ────────────────────────────────────────────────── ────────  ──────────  ║")

	// Cold: first sign for a host (generates ECDSA key + signs cert)
	{
		iters := 1000
		start := time.Now()
		for i := range iters {
			host := fmt.Sprintf("host-%d.example.com", i)
			ca.SignHost(host)
		}
		elapsed := time.Since(start)
		fmt.Printf("║  %-50s %8s  %10s  ║\n", "Cold sign (new host, ECDSA keygen + X.509 sign)", fmtDuration(elapsed/time.Duration(iters)), "")
	}
	// Warm: cached host
	{
		ca.SignHost("api.anthropic.com") // prime cache
		iters := 200_000
		start := time.Now()
		for range iters {
			ca.SignHost("api.anthropic.com")
		}
		elapsed := time.Since(start)
		fmt.Printf("║  %-50s %8s  %10s  ║\n", "Warm sign (cached host, LRU hit)", fmtDuration(elapsed/time.Duration(iters)), "")
	}

	// ──────────────────────────────────────────────────────────────────
	// Section 3: TLS handshake cost (loopback)
	// ──────────────────────────────────────────────────────────────────
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  3. TLS HANDSHAKE COST (loopback — the real bottleneck)                     ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	fmt.Printf("║  %-50s %8s  %10s  ║\n", "Operation", "Latency", "")
	fmt.Println("║  ────────────────────────────────────────────────── ────────  ──────────  ║")

	// Measure raw TLS handshake cost using a real TCP listener (avoids net.Pipe deadlock)
	{
		leafCert, _ := ca.SignHost("bench.test")
		serverConf := &tls.Config{
			Certificates: []tls.Certificate{*leafCert},
			NextProtos:   []string{"http/1.1"},
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		clientConf := &tls.Config{
			RootCAs:    pool,
			ServerName: "bench.test",
			NextProtos: []string{"http/1.1"},
		}

		// Start a TCP server that accepts, completes TLS handshake, then closes
		rawLis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				rawConn, err := rawLis.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					srv := tls.Server(c, serverConf)
					srv.Handshake()
					c.Close()
				}(rawConn)
			}
		}()

		iters := 2000
		start := time.Now()
		for range iters {
			conn, err := tls.Dial("tcp", rawLis.Addr().String(), clientConf)
			if err != nil {
				t.Fatalf("TLS dial: %v", err)
			}
			conn.Close()
		}
		elapsed := time.Since(start)
		rawLis.Close()
		fmt.Printf("║  %-50s %8s  %10s  ║\n", "Single TLS handshake (ECDSA P-256, loopback)", fmtDuration(elapsed/time.Duration(iters)), "")
	}

	// Proxy path = 2 handshakes (client↔proxy + proxy↔upstream)
	fmt.Printf("║  %-50s %8s  %10s  ║\n", "Proxy path = 2 handshakes (client + upstream)", "~2x above", "")

	// ──────────────────────────────────────────────────────────────────
	// Section 4: End-to-end through the real proxy
	// ──────────────────────────────────────────────────────────────────
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  4. END-TO-END: real proxy over loopback (CONNECT + TLS + replace + HTTP)   ║")
	fmt.Println("║     Client → CONNECT → proxy MITM → upstream TLS server → response          ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	upstreamAddr, upstreamCleanup := startFakeUpstream(t)
	defer upstreamCleanup()

	// Start the real secrets proxy with a custom dial function that
	// skips private-IP checks and trusts the fake upstream's self-signed cert.
	proxyLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &SecretsProxy{
		ca:                 ca,
		listener:           proxyLis,
		sessions:           make(map[string]*Session),
		closed:             make(chan struct{}),
		skipPrivateIPCheck: true,
		dialFunc: func(target, serverName string) (*tls.Conn, error) {
			tcpConn, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				return nil, err
			}
			tlsConn := tls.Client(tcpConn, &tls.Config{
				InsecureSkipVerify: true, // fake upstream uses self-signed cert
				NextProtos:         []string{"http/1.1"},
			})
			if err := tlsConn.Handshake(); err != nil {
				tcpConn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
	}
	proxy.Start()
	defer proxy.Stop()

	proxyAddr := proxyLis.Addr().String()

	// Register a session for 127.0.0.1
	token := makeToken()
	proxy.RegisterSession("127.0.0.1", &Session{
		SandboxID:  "bench-sandbox",
		Secrets:    map[string]string{token: "sk-ant-real-api-key-1234567890"},
		TokenHosts: map[string][]string{token: {"localhost", "127.0.0.1"}},
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	// Resolve upstream host:port for CONNECT (use IP to avoid DNS)
	upstreamHost, upstreamPort, _ := net.SplitHostPort(upstreamAddr)
	connectTarget := net.JoinHostPort(upstreamHost, upstreamPort)

	// Trust the proxy's CA for TLS from client side (MITM cert)
	caPool := x509.NewCertPool()
	caPool.AddCert(ca.cert)

	// Helper: do one full proxied request
	doProxiedRequest := func(reuseConn net.Conn) (net.Conn, error) {
		conn := reuseConn
		var err error
		if conn == nil {
			conn, err = net.Dial("tcp", proxyAddr)
			if err != nil {
				return nil, err
			}
			// Send CONNECT
			fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", connectTarget, connectTarget)
			// Read 200 response
			reader := bufio.NewReader(conn)
			resp, err := http.ReadResponse(reader, nil)
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("CONNECT response: %w", err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				conn.Close()
				return nil, fmt.Errorf("CONNECT status: %d", resp.StatusCode)
			}

			// TLS handshake with proxy (MITM)
			tlsConn := tls.Client(conn, &tls.Config{
				RootCAs:            caPool,
				ServerName:         upstreamHost,
				InsecureSkipVerify: true, // proxy signs for a different host
				NextProtos:         []string{"http/1.1"},
			})
			if err := tlsConn.Handshake(); err != nil {
				conn.Close()
				return nil, fmt.Errorf("TLS handshake: %w", err)
			}
			conn = tlsConn
		}

		// Send HTTP request with sealed token
		body := fmt.Sprintf(`{"api_key":"%s","model":"claude-3"}`, token)
		reqStr := fmt.Sprintf("POST /v1/messages HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s",
			upstreamHost, token, len(body), body)
		conn.Write([]byte(reqStr))

		// Read response
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			return conn, fmt.Errorf("response: %w", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		return conn, nil
	}

	fmt.Printf("║  %-50s %8s  %10s  ║\n", "Scenario", "Latency", "Reqs/sec")
	fmt.Println("║  ────────────────────────────────────────────────── ────────  ──────────  ║")

	// 4a: Single request (cold — includes CONNECT + TLS + replace + response)
	{
		iters := 200
		start := time.Now()
		for range iters {
			conn, err := doProxiedRequest(nil)
			if err != nil {
				t.Fatalf("proxied request failed: %v", err)
			}
			conn.Close()
		}
		elapsed := time.Since(start)
		perReq := elapsed / time.Duration(iters)
		rps := float64(iters) / elapsed.Seconds()
		fmt.Printf("║  %-50s %8s  %8.0f    ║\n", "New connection (CONNECT + TLS + req + resp)", fmtDuration(perReq), rps)
	}

	// 4b: Pipelined requests (reuse CONNECT tunnel — no new TLS)
	{
		conn, err := doProxiedRequest(nil)
		if err != nil {
			t.Fatalf("initial request failed: %v", err)
		}
		iters := 2000
		start := time.Now()
		for range iters {
			conn, err = doProxiedRequest(conn)
			if err != nil {
				t.Fatalf("pipelined request failed: %v", err)
			}
		}
		elapsed := time.Since(start)
		conn.Close()
		perReq := elapsed / time.Duration(iters)
		rps := float64(iters) / elapsed.Seconds()
		fmt.Printf("║  %-50s %8s  %8.0f    ║\n", "Pipelined (reuse tunnel, no new TLS)", fmtDuration(perReq), rps)
	}

	// ──────────────────────────────────────────────────────────────────
	// Section 5: 400 sandboxes worst case (real proxy, real TLS)
	// ──────────────────────────────────────────────────────────────────
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  5. WORST CASE: 400 sandboxes concurrent, real proxy, real TLS              ║")
	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")

	// Register 400 sessions (different IPs won't work with loopback,
	// so we use a single session but 400 concurrent goroutines — this
	// tests the real contention: RLock, cert cache, goroutine scheduling)
	{
		nSandboxes := 400
		reqsPerSandbox := 10 // each sandbox does 10 pipelined requests
		var totalReqs atomic.Int64
		var totalErrors atomic.Int64

		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(nSandboxes)
		for range nSandboxes {
			go func() {
				defer wg.Done()
				conn, err := doProxiedRequest(nil)
				if err != nil {
					totalErrors.Add(1)
					return
				}
				totalReqs.Add(1)
				for range reqsPerSandbox - 1 {
					conn, err = doProxiedRequest(conn)
					if err != nil {
						totalErrors.Add(1)
						conn.Close()
						return
					}
					totalReqs.Add(1)
				}
				conn.Close()
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)

		total := totalReqs.Load()
		errors := totalErrors.Load()
		perReq := time.Duration(0)
		rps := float64(0)
		if total > 0 {
			perReq = elapsed / time.Duration(total)
			rps = float64(total) / elapsed.Seconds()
		}

		fmt.Printf("║  %-50s %8s            ║\n", fmt.Sprintf("%d connections × %d reqs each", nSandboxes, reqsPerSandbox), fmtDuration(elapsed))
		fmt.Printf("║  %-50s %8d            ║\n", "Total requests completed", total)
		if errors > 0 {
			fmt.Printf("║  %-50s %8d            ║\n", "Errors", errors)
		}
		fmt.Printf("║  %-50s %8s            ║\n", "Avg latency per request (incl. contention)", fmtDuration(perReq))
		fmt.Printf("║  %-50s %8.0f            ║\n", "Aggregate throughput (reqs/sec)", rps)
	}

	fmt.Println("╠══════════════════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  NOTES:                                                                      ║")
	fmt.Println("║  • New-connection latency is dominated by 2× TLS handshake (~1-2ms each)     ║")
	fmt.Println("║  • Pipelined requests skip TLS — only HTTP parse + replacement (~µs)         ║")
	fmt.Println("║  • Most real API calls use HTTP keep-alive → pipeline numbers are typical     ║")
	fmt.Println("║  • Real network RTT to upstream APIs (10-100ms) dwarfs all proxy overhead     ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
}

// ── Formatting helpers ──────────────────────────────────────────────────────

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Microsecond:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d.Nanoseconds())/1000)
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Nanoseconds())/1_000_000)
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

func fmtBytes(bytesPerSec float64) string {
	switch {
	case bytesPerSec >= 1_000_000_000:
		return fmt.Sprintf("%.1fGB", bytesPerSec/1_000_000_000)
	case bytesPerSec >= 1_000_000:
		return fmt.Sprintf("%.0fMB", bytesPerSec/1_000_000)
	case bytesPerSec >= 1_000:
		return fmt.Sprintf("%.0fKB", bytesPerSec/1_000)
	default:
		return fmt.Sprintf("%.0fB", bytesPerSec)
	}
}
