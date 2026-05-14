package api_test

import (
	"net/http"
	"os"
	"strconv"
	"testing"
)

// TestWorkers_List queries /api/workers and asserts the count matches what
// up.sh provisioned (OSB_TEST_WORKERS env var). If WORKERS is 0 the test
// just confirms the endpoint returns successfully.
func TestWorkers_List(t *testing.T) {
	c := newClient(t)
	want := 0
	if v := os.Getenv(envWorkers); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("parse %s: %v", envWorkers, err)
		}
		want = n
	}

	// /api/workers returns either an array or {"workers":[...]} — accept both.
	var arr []map[string]any
	var wrapped struct {
		Workers []map[string]any `json:"workers"`
	}

	// Try array first.
	code, err := c.do(t, http.MethodGet, "/api/workers", nil, &arr)
	got := len(arr)
	if err != nil {
		// Try the wrapped form.
		code2, err2 := c.do(t, http.MethodGet, "/api/workers", nil, &wrapped)
		if err2 != nil {
			t.Fatalf("/api/workers: %v / %v", err, err2)
		}
		code = code2
		got = len(wrapped.Workers)
	}
	if code != http.StatusOK {
		t.Fatalf("/api/workers: want 200, got %d", code)
	}
	if got < want {
		t.Fatalf("/api/workers: want at least %d worker(s), got %d", want, got)
	}
	t.Logf("workers registered: %d (expected at least %d)", got, want)
}
