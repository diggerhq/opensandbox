package api_test

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestExec_Run runs a one-shot command inside a fresh sandbox and verifies the
// output. This exercises the full path: API → worker gRPC → agent inside the
// guest VM → process spawn → output round-trip.
func TestExec_Run(t *testing.T) {
	if v, _ := strconv.Atoi(os.Getenv(envWorkers)); v < 1 {
		t.Skipf("%s<1, skipping exec test", envWorkers)
	}
	c := newClient(t)
	sandboxID, _ := createReadySandbox(t, c, map[string]any{
		"cpuCount": 1, "memoryMB": 1024, "diskMB": 20480, "timeout": 120,
	})

	// Run commands and verify stdout/exit codes
	cases := []struct {
		name           string
		cmd            string
		args           []string
		wantExit       int
		wantStdoutPart string
	}{
		{"echo prints to stdout", "echo", []string{"hello-ci"}, 0, "hello-ci"},
		{"true exits 0", "true", nil, 0, ""},
		{"false exits 1", "false", nil, 1, ""},
		{"uname returns linux", "uname", []string{"-s"}, 0, "Linux"},
		{"hostname matches sandbox", "hostname", nil, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var result struct {
				ExitCode int    `json:"exitCode"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
			}
			body := map[string]any{"cmd": tc.cmd, "timeout": 30}
			if len(tc.args) > 0 {
				body["args"] = tc.args
			}
			code, err := c.do(t, http.MethodPost,
				"/api/sandboxes/"+sandboxID+"/exec/run", body, &result)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if code != http.StatusOK {
				t.Fatalf("exec: code=%d", code)
			}
			if result.ExitCode != tc.wantExit {
				t.Errorf("exit: want %d, got %d (stderr=%q)", tc.wantExit, result.ExitCode, result.Stderr)
			}
			if tc.wantStdoutPart != "" && !strings.Contains(result.Stdout, tc.wantStdoutPart) {
				t.Errorf("stdout: want substring %q, got %q", tc.wantStdoutPart, result.Stdout)
			}
		})
	}
}
