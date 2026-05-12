package api_test

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHibernate_Wake exercises the hibernate→S3→wake round-trip and verifies
// workspace state survives. Writes a marker file in the workspace, hibernates,
// waits for status=hibernated, wakes, waits for running, reads the marker back.
//
// This catches a class of regressions: hibernate upload silently failing,
// wake fetching an empty workspace, status transitions stuck mid-flight, or
// the marker file being lost across the round-trip.
func TestHibernate_Wake(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping hibernate test", envWorkers)
	}
	c := newClient(t)
	sandboxID, _ := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 600,
	})
	t.Logf("source sandbox: %s", sandboxID)

	// Step 1: write a marker file in the workspace.
	const marker = "ci-hibernate-marker-v1"
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
	code, err := c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/exec/run", body, &execResult)
	if err != nil || code != http.StatusOK {
		t.Fatalf("write marker: code=%d err=%v stderr=%q", code, err, execResult.Stderr)
	}
	if !strings.Contains(execResult.Stdout, marker) {
		t.Fatalf("marker write didn't echo back: stdout=%q", execResult.Stdout)
	}

	// Step 2: hibernate
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/hibernate", nil, nil)
	if err != nil || code/100 != 2 {
		t.Fatalf("hibernate: code=%d err=%v", code, err)
	}
	t.Logf("hibernate request sent")

	// Step 3: poll until status=hibernated. Upload of mem+workspace can take
	// 10-30s on a small sandbox; allow generous budget.
	if !waitForStatus(t, c, sandboxID, "hibernated", 3*time.Minute) {
		t.Fatalf("sandbox never reached hibernated status")
	}
	t.Logf("hibernate complete")

	// Step 4: wake
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/wake", nil, nil)
	if err != nil || code/100 != 2 {
		t.Fatalf("wake: code=%d err=%v", code, err)
	}
	t.Logf("wake request sent")

	if !waitForStatus(t, c, sandboxID, "running", 3*time.Minute) {
		t.Fatalf("sandbox never returned to running status")
	}
	t.Logf("wake complete")

	// Step 5: wait for exec readiness post-wake, then verify marker survived.
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
		t.Fatalf("read marker post-wake: code=%d err=%v stderr=%q", code, err, execResult.Stderr)
	}
	if execResult.ExitCode != 0 {
		t.Fatalf("read marker post-wake: exit=%d stderr=%q (file lost?)", execResult.ExitCode, execResult.Stderr)
	}
	if !strings.Contains(execResult.Stdout, marker) {
		t.Errorf("marker survived hibernate→wake but content corrupted: want %q, got %q", marker, execResult.Stdout)
	}
}

// waitForStatus polls /api/sandboxes/:id and returns true when status matches
// `want` within `timeout`, false otherwise.
func waitForStatus(t *testing.T, c *client, sandboxID, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		var sb struct {
			Status string `json:"status"`
		}
		code, err := c.do(t, http.MethodGet, "/api/sandboxes/"+sandboxID, nil, &sb)
		if err == nil && code == http.StatusOK {
			if sb.Status == want {
				return true
			}
			if sb.Status != last {
				t.Logf("  status: %s", sb.Status)
				last = sb.Status
			}
		}
		time.Sleep(3 * time.Second)
	}
	return false
}
