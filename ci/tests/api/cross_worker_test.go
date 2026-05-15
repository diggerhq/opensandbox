package api_test

import (
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestHibernate_WakeCrossWorker hibernates a sandbox on worker A, drains A so
// the scheduler can't pick it again, then wakes — should land on worker B.
// Verifies the workspace state survives the S3 round-trip + handoff.
//
// This is distinct from TestMigrate_CrossWorker: that one uses live migration
// (direct TCP between workers). This one goes through blob storage, which is
// the path customers feel during worker retirements / rolling-replace.
func TestHibernate_WakeCrossWorker(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 2 {
		t.Skipf("%s<2, skipping cross-worker hibernate test", envWorkers)
	}
	c := newClient(t)

	sandboxID, sourceWorker := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 600,
	})
	t.Logf("source sandbox: %s on %s", sandboxID, sourceWorker)

	const marker = "ci-hib-cross-marker"
	writeMarker(t, c, sandboxID, marker)

	// Hibernate
	code, err := c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/hibernate", nil, nil)
	if err != nil || code/100 != 2 {
		t.Fatalf("hibernate: code=%d err=%v", code, err)
	}
	if !waitForStatus(t, c, sandboxID, "hibernated", 3*time.Minute) {
		t.Fatalf("sandbox never reached hibernated")
	}
	t.Logf("hibernated on %s", sourceWorker)

	// Hibernation upload to S3 is async — `status=hibernated` is set before
	// uploaded_at. If we drain the source before upload completes, wake on
	// the other worker correctly refuses with 503 ("source unavailable and
	// upload not complete"). Wait long enough for the upload to finish.
	// For a 1GB empty sandbox this is ~5-15s; 30s gives margin.
	t.Logf("waiting 30s for hibernation S3 upload to complete...")
	time.Sleep(30 * time.Second)

	// Drain the source worker so the scheduler will pick the other one for wake.
	drainWorker(t, c, sourceWorker)

	// Wake
	code, err = c.do(t, http.MethodPost, "/api/sandboxes/"+sandboxID+"/wake", nil, nil)
	if err != nil || code/100 != 2 {
		t.Fatalf("wake: code=%d err=%v", code, err)
	}
	if !waitForStatus(t, c, sandboxID, "running", 3*time.Minute) {
		t.Fatalf("sandbox never returned to running")
	}

	// Verify the wake landed on the OTHER worker.
	var sb struct {
		WorkerID string `json:"workerID"`
	}
	c.do(t, http.MethodGet, "/api/sandboxes/"+sandboxID, nil, &sb)
	if sb.WorkerID == sourceWorker {
		t.Fatalf("wake landed on same worker %s despite drain; expected cross-worker", sourceWorker)
	}
	t.Logf("woke on %s (different worker)", sb.WorkerID)

	// Verify marker survived.
	time.Sleep(5 * time.Second)
	if got := readMarker(t, c, sandboxID); got != marker {
		t.Errorf("marker corrupted across hibernate→wake→cross-worker: want %q, got %q", marker, got)
	}
}
