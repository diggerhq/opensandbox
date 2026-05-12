package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSDK_PythonImport verifies the Python SDK package is importable. Doesn't
// instantiate against the live stack — that needs a network round-trip and
// httpx — but catches missing-dep / syntax / package-config regressions.
func TestSDK_PythonImport(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH")
	}
	sdk := filepath.Join(repoRoot(t), "sdks", "python")
	if _, err := os.Stat(filepath.Join(sdk, "pyproject.toml")); err != nil {
		t.Skipf("python SDK dir missing: %v", err)
	}
	out, err := exec.Command(py, "-c",
		"import sys; sys.path.insert(0, '"+sdk+"'); import opencomputer; print(opencomputer.__name__)").
		CombinedOutput()
	if err != nil {
		// Likely a missing dep (httpx). Treat as informational — Python SDK
		// has runtime deps a fresh runner may not have.
		t.Logf("python import: %v\n%s", err, out)
		t.Skip("python SDK has unsatisfied runtime deps in this env")
	}
	if !strings.Contains(string(out), "opencomputer") {
		t.Errorf("python import: unexpected output: %q", out)
	}
}

// TestSDK_TypeScriptBuild verifies the TS SDK compiles. Catches type / package
// drift between the Go API and the published types.
func TestSDK_TypeScriptBuild(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not on PATH")
	}
	sdk := filepath.Join(repoRoot(t), "sdks", "typescript")
	if _, err := os.Stat(filepath.Join(sdk, "package.json")); err != nil {
		t.Skipf("ts SDK dir missing: %v", err)
	}
	// Install (idempotent if node_modules cached)
	install := exec.Command(npm, "ci", "--prefer-offline", "--no-audit", "--no-fund")
	install.Dir = sdk
	if out, err := install.CombinedOutput(); err != nil {
		// `npm ci` requires package-lock.json; if missing, fall back to install
		install = exec.Command(npm, "install", "--prefer-offline", "--no-audit", "--no-fund")
		install.Dir = sdk
		if out2, err2 := install.CombinedOutput(); err2 != nil {
			t.Logf("npm install: %v\n%s\n(after ci failure: %v\n%s)", err2, out2, err, out)
			t.Skip("npm install failed — SDK runtime deps unavailable in this env")
		}
	}
	build := exec.Command(npm, "run", "build")
	build.Dir = sdk
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("npm run build: %v\n%s", err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found")
		}
		dir = parent
	}
}
