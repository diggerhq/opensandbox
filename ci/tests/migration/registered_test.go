// Package migration validates that every *.up.sql file in
// internal/db/migrations/ is registered in store.go's hand-maintained
// migration slice. The slice is the source of truth at runtime: a SQL file
// that's only on disk but not in the slice will silently never run.
//
// This test is the cheapest defense against the listed migrations.md pitfall.
// It needs no infrastructure — runs as a plain unit test.
package migration_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// repoRoot walks up from the test source file to find go.mod.
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
			t.Fatal("walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

func TestMigrations_AllSQLFilesRegistered(t *testing.T) {
	root := repoRoot(t)
	migrationsDir := filepath.Join(root, "internal", "db", "migrations")

	// Collect on-disk *.up.sql files.
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			onDisk[e.Name()] = true
		}
	}
	if len(onDisk) == 0 {
		t.Fatal("found zero *.up.sql files — migrations dir layout changed?")
	}

	// Extract registered entries from store.go's migration slice.
	storeGo, err := os.ReadFile(filepath.Join(root, "internal", "db", "store.go"))
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	re := regexp.MustCompile(`\{\s*(\d+)\s*,\s*"migrations/([^"]+)"\s*\}`)
	matches := re.FindAllStringSubmatch(string(storeGo), -1)
	if len(matches) == 0 {
		t.Fatal("no migration entries found in store.go — slice format changed?")
	}
	registered := map[string]int{} // filename → version
	versions := map[int]string{}   // version → filename
	for _, m := range matches {
		ver := m[1]
		fn := m[2]
		registered[fn] = atoi(t, ver)
		if existing, dup := versions[atoi(t, ver)]; dup {
			t.Errorf("duplicate migration version %s: %q and %q", ver, existing, fn)
		}
		versions[atoi(t, ver)] = fn
	}

	// Every on-disk file must be registered.
	var unregistered []string
	for fn := range onDisk {
		if _, ok := registered[fn]; !ok {
			unregistered = append(unregistered, fn)
		}
	}
	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		t.Errorf("unregistered migrations (in dir but not in store.go's slice — they will never run!):\n  %s",
			strings.Join(unregistered, "\n  "))
	}

	// Every registered entry must have a real file.
	var dangling []string
	for fn := range registered {
		if !onDisk[fn] {
			dangling = append(dangling, fn)
		}
	}
	if len(dangling) > 0 {
		sort.Strings(dangling)
		t.Errorf("dangling references in store.go (in slice but no .up.sql file):\n  %s",
			strings.Join(dangling, "\n  "))
	}

	t.Logf("validated %d migrations: dir=%d registered=%d", len(onDisk), len(onDisk), len(registered))
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit in version %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}
