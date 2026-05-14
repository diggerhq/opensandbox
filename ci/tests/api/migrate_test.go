package api_test

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMigrate_CrossWorker exercises live migration: create sandbox on worker A,
// write a marker, migrate to worker B, verify the marker survives AND that the
// sandbox is actually now on worker B.
//
// Catches: migration upload/download bugs, vmstate replay races, sandbox session
// row not updated post-migration, workspace state lost across handoff.
//
// Requires WORKERS>=2. Skipped on single-worker stacks.
func TestMigrate_CrossWorker(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 2 {
		t.Skipf("%s<2 (need at least 2 workers), skipping cross-worker test", envWorkers)
	}
	c := newClient(t)

	// Discover available workers. Cross-golden migration is supported as long
	// as each worker has uploaded its golden base to blob (uploadBaseImageIfNew
	// runs in background after golden snapshot creation).
	var workers []struct {
		WorkerID string `json:"worker_id"`
		Draining bool   `json:"draining"`
	}
	code, err := c.do(t, http.MethodGet, "/api/workers", nil, &workers)
	if err != nil || code != http.StatusOK {
		t.Fatalf("list workers: code=%d err=%v", code, err)
	}
	var liveIDs []string
	for _, w := range workers {
		if !w.Draining {
			liveIDs = append(liveIDs, w.WorkerID)
		}
	}
	if len(liveIDs) < 2 {
		t.Fatalf("need 2 live workers, got %d: %+v", len(liveIDs), liveIDs)
	}

	// Create sandbox.
	sandboxID, sourceWorker := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 600,
	})
	t.Logf("source sandbox: %s on %s", sandboxID, sourceWorker)

	// Pick a destination worker that's NOT the current one.
	var target string
	for _, w := range liveIDs {
		if w != sourceWorker {
			target = w
			break
		}
	}
	if target == "" {
		t.Fatalf("no candidate target worker (all %d workers match source %q)", len(liveIDs), sourceWorker)
	}
	t.Logf("migrating: %s → %s", sourceWorker, target)

	// Write marker.
	const marker = "ci-migrate-marker-v1"
	body := map[string]any{
		"cmd":     "sh",
		"args":    []string{"-c", "echo " + marker + " > /home/sandbox/marker && cat /home/sandbox/marker"},
		"timeout": 10,
	}
	var execResult struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/exec/run", body, &execResult)
	if err != nil || code != http.StatusOK {
		t.Fatalf("write marker: code=%d err=%v stderr=%q", code, err, execResult.Stderr)
	}
	if !strings.Contains(execResult.Stdout, marker) {
		t.Fatalf("marker write didn't echo back: stdout=%q", execResult.Stdout)
	}

	// Migrate.
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/migrate",
		map[string]any{"targetWorker": target}, nil)
	if err != nil || code/100 != 2 {
		t.Fatalf("migrate: code=%d err=%v", code, err)
	}
	t.Logf("migrate request accepted")

	// Wait for status to return to "running" (it goes via "migrating").
	if !waitForStatus(t, c, sandboxID, "running", 3*time.Minute) {
		t.Fatalf("sandbox didn't return to running after migrate")
	}

	// Verify the sandbox's workerID changed.
	var sb struct {
		SandboxID string `json:"sandboxID"`
		Status    string `json:"status"`
		WorkerID  string `json:"workerID"`
	}
	code, err = c.do(t, http.MethodGet, "/api/sandboxes/"+sandboxID, nil, &sb)
	if err != nil || code != http.StatusOK {
		t.Fatalf("get post-migrate: code=%d err=%v", code, err)
	}
	if sb.WorkerID != target {
		t.Errorf("workerID after migrate: want %q, got %q", target, sb.WorkerID)
	}
	t.Logf("sandbox now on: %s", sb.WorkerID)

	// Verify the marker survived.
	time.Sleep(2 * time.Second)
	body = map[string]any{
		"cmd":     "cat",
		"args":    []string{"/home/sandbox/marker"},
		"timeout": 10,
	}
	execResult = struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}{}
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/exec/run", body, &execResult)
	if err != nil || code != http.StatusOK {
		t.Fatalf("read marker post-migrate: code=%d err=%v stderr=%q", code, err, execResult.Stderr)
	}
	if execResult.ExitCode != 0 {
		t.Fatalf("read marker post-migrate: exit=%d stderr=%q (file lost?)", execResult.ExitCode, execResult.Stderr)
	}
	if !strings.Contains(execResult.Stdout, marker) {
		t.Errorf("marker corrupted across migrate: want %q, got %q", marker, execResult.Stdout)
	}
}
