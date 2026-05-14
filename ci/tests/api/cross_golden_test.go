package api_test

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestCheckpoint_ForkCrossGolden creates a checkpoint on a worker running the
// OLD golden (gallery-baked binaries), drains that worker, then forks. The
// fork lands on a worker running the NEW golden (PR binaries) and must
// successfully resume the checkpoint's workspace.
//
// This is the canonical regression class: a customer's checkpoint was taken
// against last week's golden, and now they fork after a rolling-replace
// upgraded all workers. If the new code can't load the old checkpoint's
// rootfs/workspace formats, every customer using that checkpoint breaks.
//
// Requires BASELINE_FROM_GALLERY=1 + WORKERS>=2 (i.e. distinct goldens on the
// two workers). Skips if goldens are the same.
func TestCheckpoint_ForkCrossGolden(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 2 {
		t.Skipf("%s<2, skipping cross-golden test", envWorkers)
	}
	c := newClient(t)

	// Verify workers have distinct goldens; otherwise this test isn't testing
	// the cross-golden path.
	var workers []struct {
		WorkerID      string `json:"worker_id"`
		GoldenVersion string `json:"golden_version"`
		Draining      bool   `json:"draining"`
	}
	code, err := c.do(t, http.MethodGet, "/api/workers", nil, &workers)
	if err != nil || code != http.StatusOK {
		t.Fatalf("list workers: code=%d err=%v", code, err)
	}
	if len(workers) < 2 {
		t.Skipf("need 2 workers, got %d", len(workers))
	}
	goldens := map[string]bool{}
	for _, w := range workers {
		if !w.Draining {
			goldens[w.GoldenVersion] = true
		}
	}
	if len(goldens) < 2 {
		t.Skipf("workers share the same golden_version (%v); set BASELINE_FROM_GALLERY=1 in up.sh to get distinct goldens", goldens)
	}

	// Create sandbox, write marker
	sourceID, sourceWorker := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 600,
	})
	sourceGolden := goldenVersionOf(t, c, sourceWorker)
	t.Logf("source sandbox: %s on %s (golden=%s)", sourceID, sourceWorker, sourceGolden)

	const marker = "ci-cross-golden-marker"
	writeMarker(t, c, sourceID, marker)

	// Create checkpoint against the source golden
	cpName := fmt.Sprintf("ci-xgolden-%d", time.Now().UnixNano())
	var cp struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sourceID+"/checkpoints",
		map[string]any{"name": cpName}, &cp)
	if err != nil || code/100 != 2 {
		t.Fatalf("create checkpoint: code=%d err=%v", code, err)
	}
	t.Cleanup(func() {
		c.do(t, http.MethodDelete, "/api/sandboxes/"+sourceID+"/checkpoints/"+cp.ID, nil, nil)
	})

	// Wait for status=ready
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var cps []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		c.do(t, http.MethodGet, "/api/sandboxes/"+sourceID+"/checkpoints", nil, &cps)
		var s string
		for _, x := range cps {
			if x.ID == cp.ID {
				s = x.Status
				break
			}
		}
		if s == "ready" {
			break
		}
		if s == "failed" {
			t.Fatalf("checkpoint failed during setup")
		}
		time.Sleep(3 * time.Second)
	}
	t.Logf("checkpoint ready (captured against golden %s)", sourceGolden)

	// Drain the source worker so the fork lands on a different-golden worker.
	drainWorker(t, c, sourceWorker)

	// Fork
	var fork struct {
		SandboxID string `json:"sandboxID"`
		Status    string `json:"status"`
		WorkerID  string `json:"workerID"`
	}
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/from-checkpoint/"+cp.ID,
		map[string]any{
			"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 120,
		}, &fork)
	if err != nil || code/100 != 2 {
		t.Fatalf("fork: code=%d err=%v resp=%+v", code, err, fork)
	}
	if fork.SandboxID == "" || fork.Status != "running" {
		t.Fatalf("fork response: %+v", fork)
	}
	t.Cleanup(func() { c.do(t, http.MethodDelete, "/api/sandboxes/"+fork.SandboxID, nil, nil) })

	// Verify fork landed on a DIFFERENT golden than the source
	forkGolden := goldenVersionOf(t, c, fork.WorkerID)
	t.Logf("fork: %s on %s (golden=%s)", fork.SandboxID, fork.WorkerID, forkGolden)
	if forkGolden == sourceGolden {
		t.Fatalf("fork landed on same-golden worker (%s); drain didn't redirect — expected cross-golden", forkGolden)
	}

	// Verify marker survived the cross-golden fork.
	time.Sleep(5 * time.Second)
	if got := readMarker(t, c, fork.SandboxID); got != marker {
		t.Errorf("marker corrupted across golden boundary (%s → %s): want %q, got %q",
			sourceGolden, forkGolden, marker, got)
	}
}
