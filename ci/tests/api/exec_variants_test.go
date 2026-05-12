package api_test

import (
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// These variants help characterize the worker→agent exec flake we observed:
// the current TestExec_Run (5 commands ~50ms apart on a fresh sandbox) shows
// ~30-40% flake. Customers don't usually fire commands that fast, but agents
// running setup-style sequences (`cat /etc/...`, `which X`, `pwd`) can.
//
// Each variant isolates one dimension. Compare flake rates across variants:
//   - Burst (baseline)       : matches TestExec_Run; expected ~30% flake
//   - Paced                  : same 5 cmds with 1s sleeps — customer-like cadence
//   - Stress                 : 50 fast cmds on one sandbox — bursty leak detector
//   - Warm                   : burst cmds on a sandbox after 10s warmup — separates "cold sandbox" from "fast burst"

func runExecCmd(t *testing.T, c *client, sandboxID, cmd string, args []string) (exitCode int, stdout string, latency time.Duration) {
	t.Helper()
	body := map[string]any{"cmd": cmd, "timeout": 10}
	if len(args) > 0 {
		body["args"] = args
	}
	var result struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	start := time.Now()
	code, err := c.do(t, http.MethodPost,
		"/api/sandboxes/"+sandboxID+"/exec/run", body, &result)
	latency = time.Since(start)
	if err != nil {
		t.Errorf("exec %q: %v (latency=%s)", cmd, err, latency)
		return -1, "", latency
	}
	if code != http.StatusOK {
		t.Errorf("exec %q: HTTP %d (latency=%s, stderr=%q)", cmd, code, latency, result.Stderr)
		return -1, "", latency
	}
	return result.ExitCode, result.Stdout, latency
}

func createSandboxForExec(t *testing.T, c *client, timeout int) string {
	t.Helper()
	var sb struct {
		SandboxID string `json:"sandboxID"`
		Status    string `json:"status"`
	}
	code, err := c.do(t, http.MethodPost, "/api/sandboxes", map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": timeout,
	}, &sb)
	if err != nil || code/100 != 2 || sb.Status != "running" {
		t.Fatalf("create sandbox: code=%d err=%v resp=%+v", code, err, sb)
	}
	t.Cleanup(func() { c.do(t, http.MethodDelete, "/api/sandboxes/"+sb.SandboxID, nil, nil) })
	return sb.SandboxID
}

// Variant A: Burst — current pattern. 5 trivial commands back-to-back on a
// fresh sandbox.
func TestExecVariant_Burst(t *testing.T) {
	if os.Getenv("OSB_TEST_VARIANTS") != "1" {
		t.Skip("variant diagnostics gated by OSB_TEST_VARIANTS=1")
	}
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping", envWorkers)
	}
	c := newClient(t)
	sbox := createSandboxForExec(t, c, 120)

	cmds := []string{"echo", "true", "false", "uname", "hostname"}
	for i, cmd := range cmds {
		exit, _, lat := runExecCmd(t, c, sbox, cmd, nil)
		t.Logf("burst[%d] %s: exit=%d latency=%s", i, cmd, exit, lat)
	}
}

// Variant B: Paced — same 5 commands but with 1s sleeps between.
// If this is much less flaky than Burst, the issue is timing-specific
// (fast back-to-back exec) and customers at normal pace are fine.
func TestExecVariant_Paced(t *testing.T) {
	if os.Getenv("OSB_TEST_VARIANTS") != "1" {
		t.Skip("variant diagnostics gated by OSB_TEST_VARIANTS=1")
	}
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping", envWorkers)
	}
	c := newClient(t)
	sbox := createSandboxForExec(t, c, 120)

	cmds := []string{"echo", "true", "false", "uname", "hostname"}
	for i, cmd := range cmds {
		exit, _, lat := runExecCmd(t, c, sbox, cmd, nil)
		t.Logf("paced[%d] %s: exit=%d latency=%s", i, cmd, exit, lat)
		if i < len(cmds)-1 {
			time.Sleep(1 * time.Second)
		}
	}
}

// Variant C: Stress — 50 fast commands on one sandbox. Hammers the worker→agent
// path. If the flake is a leak (e.g. accumulating stuck channels), the failure
// rate should rise across the 50 calls. If it's a fixed-probability transient,
// it should hit early and then either recover or stay flaky.
func TestExecVariant_Stress(t *testing.T) {
	if os.Getenv("OSB_TEST_VARIANTS") != "1" {
		t.Skip("variant diagnostics gated by OSB_TEST_VARIANTS=1")
	}
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping", envWorkers)
	}
	c := newClient(t)
	sbox := createSandboxForExec(t, c, 300)

	const n = 50
	var slow, failed int
	for i := range n {
		exit, _, lat := runExecCmd(t, c, sbox, "true", nil)
		if lat > 2*time.Second {
			slow++
			t.Logf("stress[%d] SLOW: latency=%s exit=%d", i, lat, exit)
		}
		if exit < 0 || exit > 1 {
			failed++
		}
	}
	t.Logf("stress summary: n=%d slow(>2s)=%d failed=%d", n, slow, failed)
	if failed > 0 {
		t.Errorf("stress: %d/%d calls failed", failed, n)
	}
}

// Variant D: Warm — burst the same 5 commands but only after a 10-second pause
// post-create. If this is much less flaky than Burst, the issue is specific
// to "cold sandbox + immediate exec" (something inside the agent or worker's
// per-sandbox bookkeeping isn't ready yet right after boot).
func TestExecVariant_Warm(t *testing.T) {
	if os.Getenv("OSB_TEST_VARIANTS") != "1" {
		t.Skip("variant diagnostics gated by OSB_TEST_VARIANTS=1")
	}
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping", envWorkers)
	}
	c := newClient(t)
	sbox := createSandboxForExec(t, c, 120)

	t.Logf("warm: sleeping 10s before first exec")
	time.Sleep(10 * time.Second)

	cmds := []string{"echo", "true", "false", "uname", "hostname"}
	for i, cmd := range cmds {
		exit, _, lat := runExecCmd(t, c, sbox, cmd, nil)
		t.Logf("warm[%d] %s: exit=%d latency=%s", i, cmd, exit, lat)
	}
}

