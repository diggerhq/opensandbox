package api_test

import (
	"net/http"
	"os"
	"strconv"
	"testing"
)

// TestSandboxes_Lifecycle creates a sandbox, confirms it shows up in list/get,
// then deletes it. Skipped when WORKERS=0 since sandbox creation requires an
// actual worker to spawn the VM. Sandbox returns already-running on create.
func TestSandboxes_Lifecycle(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping sandbox lifecycle test", envWorkers)
	}
	c := newClient(t)
	sandboxID, workerID := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 120,
	})
	t.Logf("created %s on %s", sandboxID, workerID)

	t.Run("get returns same sandbox", func(t *testing.T) {
		var sb struct {
			SandboxID string `json:"sandboxID"`
			Status    string `json:"status"`
			WorkerID  string `json:"workerID"`
		}
		code, err := c.do(t, http.MethodGet, "/api/sandboxes/"+sandboxID, nil, &sb)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if code != http.StatusOK {
			t.Fatalf("get: want 200, got %d", code)
		}
		if sb.SandboxID != sandboxID {
			t.Fatalf("get: sandboxID mismatch (want %q, got %q)", sandboxID, sb.SandboxID)
		}
		if sb.Status != "running" {
			t.Fatalf("get: status=%q", sb.Status)
		}
	})

	t.Run("list includes new sandbox", func(t *testing.T) {
		var sandboxes []struct {
			SandboxID string `json:"sandboxID"`
		}
		code, err := c.do(t, http.MethodGet, "/api/sandboxes", nil, &sandboxes)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if code != http.StatusOK {
			t.Fatalf("list: want 200, got %d", code)
		}
		var found bool
		for _, s := range sandboxes {
			if s.SandboxID == sandboxID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("created %s not in list (got %d sandboxes)", sandboxID, len(sandboxes))
		}
	})

	t.Run("delete returns 2xx", func(t *testing.T) {
		code, err := c.do(t, http.MethodDelete, "/api/sandboxes/"+sandboxID, nil, nil)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if code/100 != 2 {
			t.Fatalf("delete: want 2xx, got %d", code)
		}
	})
}
