package api_test

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestCheckpoints_CreateAndFork covers the warm-cache checkpoint path:
// create sandbox → checkpoint → wait status=ready → fork → verify keys.
//
// Key assertion: when status=ready, both rootfsS3Key and workspaceS3Key
// must be non-empty. This catches the regression class where the worker's
// async upload silently failed but the DB row was still marked ready
// (leading to forks of empty workspaces). The /api/sandboxes/from-checkpoint
// path then has real keys to download, exercising the upload-and-fetch flow.
func TestCheckpoints_CreateAndFork(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping checkpoint test", envWorkers)
	}
	c := newClient(t)
	sourceID, _ := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 300,
	})
	t.Logf("source sandbox: %s", sourceID)

	// Step 2: create checkpoint
	cpName := fmt.Sprintf("ci-cp-%d", time.Now().UnixNano())
	var cp struct {
		ID             string  `json:"id"`
		Status         string  `json:"status"`
		Name           string  `json:"name"`
		RootfsS3Key    *string `json:"rootfsS3Key"`
		WorkspaceS3Key *string `json:"workspaceS3Key"`
	}
	code, err := c.do(t, http.MethodPost,
		"/api/sandboxes/"+sourceID+"/checkpoints",
		map[string]any{"name": cpName}, &cp)
	if err != nil || code/100 != 2 {
		t.Fatalf("create checkpoint: code=%d err=%v", code, err)
	}
	if cp.ID == "" || cp.Status != "processing" {
		t.Fatalf("create checkpoint response: %+v", cp)
	}
	t.Logf("checkpoint created: id=%s status=processing", cp.ID)

	// Step 3: poll until status=ready (or timeout). Empty sandboxes take ~20-40s.
	deadline := time.Now().Add(5 * time.Minute)
	var ready struct {
		Status         string
		RootfsS3Key    *string
		WorkspaceS3Key *string
		SizeBytes      int64
	}
	for time.Now().Before(deadline) {
		var cps []struct {
			ID             string  `json:"id"`
			Status         string  `json:"status"`
			RootfsS3Key    *string `json:"rootfsS3Key"`
			WorkspaceS3Key *string `json:"workspaceS3Key"`
			SizeBytes      int64   `json:"sizeBytes"`
		}
		code, err := c.do(t, http.MethodGet,
			"/api/sandboxes/"+sourceID+"/checkpoints", nil, &cps)
		if err == nil && code == http.StatusOK {
			for _, x := range cps {
				if x.ID == cp.ID {
					ready.Status = x.Status
					ready.RootfsS3Key = x.RootfsS3Key
					ready.WorkspaceS3Key = x.WorkspaceS3Key
					ready.SizeBytes = x.SizeBytes
					break
				}
			}
		}
		if ready.Status == "ready" || ready.Status == "failed" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if ready.Status != "ready" {
		t.Fatalf("checkpoint never reached ready (final status=%q)", ready.Status)
	}
	t.Logf("checkpoint ready: size=%d bytes", ready.SizeBytes)

	// Step 4: critical regression check — keys must be populated.
	if ready.RootfsS3Key == nil || *ready.RootfsS3Key == "" {
		t.Errorf("checkpoint status=ready but rootfsS3Key is empty (regression: empty-key checkpoint)")
	}
	if ready.WorkspaceS3Key == nil || *ready.WorkspaceS3Key == "" {
		t.Errorf("checkpoint status=ready but workspaceS3Key is empty (regression: empty-key checkpoint)")
	}
	if ready.SizeBytes <= 0 {
		t.Errorf("checkpoint sizeBytes=%d (regression: size not persisted)", ready.SizeBytes)
	}
	if t.Failed() {
		t.FailNow() // don't try forking against a broken checkpoint
	}

	// Step 5: fork — POST /api/sandboxes/from-checkpoint/:checkpointId
	var fork struct {
		SandboxID string `json:"sandboxID"`
		Status    string `json:"status"`
		WorkerID  string `json:"workerID"`
	}
	code, err = c.do(t, http.MethodPost,
		"/api/sandboxes/from-checkpoint/"+cp.ID,
		map[string]any{
			"cpuCount": 1,
			"memoryMB": 1024,
			"diskMB":   20480,
			"timeout":  120,
		}, &fork)
	if err != nil || code/100 != 2 {
		t.Fatalf("fork: code=%d err=%v", code, err)
	}
	if fork.SandboxID == "" || fork.Status != "running" {
		t.Fatalf("fork response: %+v", fork)
	}
	t.Logf("forked sandbox: %s on %s", fork.SandboxID, fork.WorkerID)
	t.Cleanup(func() {
		c.do(t, http.MethodDelete, "/api/sandboxes/"+fork.SandboxID, nil, nil)
		c.do(t, http.MethodDelete,
			"/api/sandboxes/"+sourceID+"/checkpoints/"+cp.ID, nil, nil)
	})

	// Step 6: confirm fork shows up in list
	var sandboxes []struct {
		SandboxID string `json:"sandboxID"`
	}
	code, err = c.do(t, http.MethodGet, "/api/sandboxes", nil, &sandboxes)
	if err != nil || code != http.StatusOK {
		t.Fatalf("list: %v code=%d", err, code)
	}
	var found bool
	for _, s := range sandboxes {
		if s.SandboxID == fork.SandboxID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("fork %s not in /api/sandboxes list", fork.SandboxID)
	}
}
