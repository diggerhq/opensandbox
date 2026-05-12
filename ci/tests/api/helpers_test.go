// Package api contains integration tests against a running opensandbox stack.
// Tests connect to OSB_TEST_SERVER_URL with OSB_TEST_API_KEY (set by ci/pr/up.sh).
// If those env vars are missing, every test in the package skips so `go test ./...`
// stays green for unit-test runs that don't have a stack provisioned.
package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	envServerURL = "OSB_TEST_SERVER_URL"
	envAPIKey    = "OSB_TEST_API_KEY"
	envWorkers   = "OSB_TEST_WORKERS"
)

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(t *testing.T) *client {
	t.Helper()
	url := os.Getenv(envServerURL)
	key := os.Getenv(envAPIKey)
	if url == "" || key == "" {
		t.Skipf("%s and %s must be set to run integration tests", envServerURL, envAPIKey)
	}
	return &client{
		baseURL: strings.TrimRight(url, "/"),
		apiKey:  key,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

// do issues a request and decodes JSON into out (if non-nil). Returns the
// response status code and any error. The body is fully consumed.
//
// No retries on 5xx — we want failures visible. The product surface is what
// customers see; if the worker→agent transport is flaky, tests should mirror
// that, not paper over it.
func (c *client) do(t *testing.T, method, path string, body, out any) (int, error) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s %s (status=%d, body=%q): %w",
				method, path, resp.StatusCode, truncate(raw, 200), err)
		}
	}
	return resp.StatusCode, nil
}

// raw issues a request without auth headers (for testing missing/wrong keys).
func (c *client) raw(t *testing.T, method, path string, headers map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, c.baseURL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func truncate(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return append(b[:n:n], []byte("…")...)
}

// createReadySandbox creates a sandbox and waits 5s before returning.
//
// The API returns status:"running" once the VM has booted and the worker
// Pinged the agent, but the agent has a known post-boot window where Exec
// calls can hang. Tests that hit Exec immediately see ~40% flake. A short
// pause sidesteps the worst of it. Tracked separately as a product bug;
// the real fix is the worker doing an Exec round-trip readiness probe
// before declaring the sandbox running.
//
// Use for any test that will Exec right after creating a sandbox. Tests
// that only verify list/get/delete don't need it.
func createReadySandbox(t *testing.T, c *client, cfg map[string]any) (string, string) {
	t.Helper()
	var sb struct {
		SandboxID string `json:"sandboxID"`
		Status    string `json:"status"`
		WorkerID  string `json:"workerID"`
	}
	code, err := c.do(t, http.MethodPost, "/api/sandboxes", cfg, &sb)
	if err != nil || code/100 != 2 {
		t.Fatalf("create sandbox: code=%d err=%v resp=%+v", code, err, sb)
	}
	if sb.SandboxID == "" || sb.Status != "running" {
		t.Fatalf("create sandbox: unexpected response %+v", sb)
	}
	t.Cleanup(func() { c.do(t, http.MethodDelete, "/api/sandboxes/"+sb.SandboxID, nil, nil) })
	time.Sleep(5 * time.Second)
	return sb.SandboxID, sb.WorkerID
}
