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

	var created struct {
		SandboxID string `json:"sandboxID"`
		Status    string `json:"status"`
		WorkerID  string `json:"workerID"`
	}
	code, err := c.do(t, http.MethodPost, "/api/sandboxes", map[string]any{
		"cpuCount": 1,
		"memoryMB": 1024,
		"diskMB":   20480,
		"timeout":  120,
	}, &created)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if code/100 != 2 {
		t.Fatalf("create: want 2xx, got %d", code)
	}
	if created.SandboxID == "" {
		t.Fatalf("create: empty sandboxID (resp=%+v)", created)
	}
	if created.Status != "running" {
		t.Fatalf("create: want status=running, got %q", created.Status)
	}
	if created.WorkerID == "" {
		t.Fatalf("create: empty workerID (resp=%+v)", created)
	}
	t.Logf("created %s on %s", created.SandboxID, created.WorkerID)
	t.Cleanup(func() {
		code, _ := c.do(t, http.MethodDelete, "/api/sandboxes/"+created.SandboxID, nil, nil)
		if code/100 != 2 && code != http.StatusNotFound {
			t.Logf("cleanup: failed to delete %s (code=%d)", created.SandboxID, code)
		}
	})

	t.Run("get returns same sandbox", func(t *testing.T) {
		var sb struct {
			SandboxID string `json:"sandboxID"`
			Status    string `json:"status"`
			WorkerID  string `json:"workerID"`
		}
		code, err := c.do(t, http.MethodGet, "/api/sandboxes/"+created.SandboxID, nil, &sb)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if code != http.StatusOK {
			t.Fatalf("get: want 200, got %d", code)
		}
		if sb.SandboxID != created.SandboxID {
			t.Fatalf("get: sandboxID mismatch (want %q, got %q)", created.SandboxID, sb.SandboxID)
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
			if s.SandboxID == created.SandboxID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("created %s not in list (got %d sandboxes)", created.SandboxID, len(sandboxes))
		}
	})

	t.Run("delete returns 2xx", func(t *testing.T) {
		code, err := c.do(t, http.MethodDelete, "/api/sandboxes/"+created.SandboxID, nil, nil)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if code/100 != 2 {
			t.Fatalf("delete: want 2xx, got %d", code)
		}
	})
}
