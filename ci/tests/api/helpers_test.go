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
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// do issues a request and decodes JSON into out (if non-nil). Returns the
// response status code and any error. The body is fully consumed.
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
