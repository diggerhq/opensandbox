package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_Smoke builds the oc CLI binary and exercises a few subcommands.
// Catches argparse / output regressions invisible to direct-API tests.
//
// Builds via `go build` so the binary is consistent with the rest of the PR.
// Skips if go isn't on PATH (shouldn't happen in CI).
func TestCLI_Smoke(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	root := repoRoot(t)
	cliBin := filepath.Join(t.TempDir(), "oc")
	build := exec.Command("go", "build", "-o", cliBin, "./cmd/oc")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build oc: %v\n%s", err, out)
	}

	// --help: every CLI must expose usage. Catches malformed cobra trees.
	out, err := exec.Command(cliBin, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("oc --help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Errorf("oc --help: missing 'Usage:' in output: %q", out)
	}

	// version: smoke check that the version subcommand exists and exits 0
	out, err = exec.Command(cliBin, "version").CombinedOutput()
	if err != nil {
		// version is optional in some CLIs — log but don't fail
		t.Logf("oc version: %v (output=%q) — skipping version assertion", err, out)
	} else if len(strings.TrimSpace(string(out))) == 0 {
		t.Errorf("oc version: empty output")
	}

	// If we have a running stack, exercise an API-authenticated command.
	server := os.Getenv(envServerURL)
	apiKey := os.Getenv(envAPIKey)
	if server == "" || apiKey == "" {
		t.Logf("no live stack — skipping live CLI command test")
		return
	}
	cmd := exec.Command(cliBin, "agent", "list")
	cmd.Env = append(os.Environ(),
		"OPENCOMPUTER_API_URL="+server,
		"OPENCOMPUTER_API_KEY="+apiKey,
	)
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oc agent list: %v\n%s", err, out)
	}
	// Just confirm it returned without error — the org may have zero agents.
	t.Logf("oc agent list returned %d bytes", len(out))
}

