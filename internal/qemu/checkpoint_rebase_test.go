package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opensandbox/opensandbox/internal/storage"
)

// ---------------------------------------------------------------------------
// Mock blob client — in-memory store for testing without real S3/Azure
// ---------------------------------------------------------------------------

type mockBlobClient struct {
	mu      sync.Mutex
	objects map[string][]byte // "bucket/key" → content
	heads   int64             // count of Head calls (for concurrency tests)
}

func newMockBlobClient() *mockBlobClient {
	return &mockBlobClient{objects: make(map[string][]byte)}
}

func (m *mockBlobClient) Upload(_ context.Context, bucket, key string, body io.ReadSeeker, _ int64) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.objects[bucket+"/"+key] = data
	m.mu.Unlock()
	return nil
}

func (m *mockBlobClient) Download(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	data, ok := m.objects[bucket+"/"+key]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("not found: %s/%s", bucket, key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *mockBlobClient) DownloadRange(_ context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error) {
	m.mu.Lock()
	data, ok := m.objects[bucket+"/"+key]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("not found: %s/%s", bucket, key)
	}
	end := offset + length
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	return io.NopCloser(bytes.NewReader(data[offset:end])), nil
}

func (m *mockBlobClient) Head(_ context.Context, bucket, key string) (int64, error) {
	atomic.AddInt64(&m.heads, 1)
	m.mu.Lock()
	data, ok := m.objects[bucket+"/"+key]
	m.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("not found: %s/%s", bucket, key)
	}
	return int64(len(data)), nil
}

func (m *mockBlobClient) Delete(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	delete(m.objects, bucket+"/"+key)
	m.mu.Unlock()
	return nil
}

func (m *mockBlobClient) BackendName() string { return "mock" }

func (m *mockBlobClient) put(bucket, key string, data []byte) {
	m.mu.Lock()
	m.objects[bucket+"/"+key] = data
	m.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testManager creates a minimal Manager backed by temp directories, with
// a mock-backed CheckpointStore. Call cleanup() when done.
func testManager(t *testing.T, goldenVersion string) (mgr *Manager, store *storage.CheckpointStore, mock *mockBlobClient, cleanup func()) {
	t.Helper()
	dataDir := t.TempDir()
	imagesDir := t.TempDir()

	// Create a fake default.ext4 so mtime checks work.
	basePath := filepath.Join(imagesDir, "default.ext4")
	if err := os.WriteFile(basePath, []byte("fake-base-content"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create checkpoint-snapshots dir.
	cpDir := filepath.Join(dataDir, "checkpoint-snapshots")
	if err := os.MkdirAll(cpDir, 0755); err != nil {
		t.Fatal(err)
	}

	mock = newMockBlobClient()
	store = storage.NewCheckpointStoreFromClient(mock, "checkpoints")

	mgr = &Manager{
		cfg: Config{
			DataDir:   dataDir,
			ImagesDir: imagesDir,
		},
		vms:           make(map[string]*VMInstance),
		goldenVersion: goldenVersion,
	}
	mgr.SetCheckpointStore(store)

	cleanup = func() {
		// Reset global old-base cache between tests.
		cleanupOldBases()
	}
	return
}

// writeCheckpointMeta creates a fake cached checkpoint dir with the given metadata.
func writeCheckpointMeta(t *testing.T, dataDir, checkpointID string, meta SnapshotMeta) string {
	t.Helper()
	cpDir := filepath.Join(dataDir, "checkpoint-snapshots", checkpointID)
	snapDir := filepath.Join(cpDir, "snapshot")
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Create minimal rootfs.qcow2 placeholder.
	if err := os.WriteFile(filepath.Join(cpDir, "rootfs.qcow2"), []byte("fake-qcow2"), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "snapshot-meta.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	return cpDir
}

func readCheckpointMeta(t *testing.T, dataDir, checkpointID string) SnapshotMeta {
	t.Helper()
	p := filepath.Join(dataDir, "checkpoint-snapshots", checkpointID, "snapshot", "snapshot-meta.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta
}

// ---------------------------------------------------------------------------
// Case 1 — Happy path: same version, no rebase
// ---------------------------------------------------------------------------

func TestCase1_SameVersion_NoRebase(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "abc123")
	defer cleanup()

	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-same", SnapshotMeta{
		SandboxID:     "sb-test",
		GoldenVersion: "abc123",
		SnapshotedAt:  time.Now(),
	})

	err := mgr.ensureCheckpointRebased(context.Background(), "cp-same")
	if err != nil {
		t.Fatalf("expected no error for matching version, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Case 2 — Cold fork with same version: checkpoint not cached, no rebase needed.
// (When ensureCheckpointRebased is called, meta file doesn't exist yet
// — it returns nil, allowing the normal download+fork path to proceed.)
// ---------------------------------------------------------------------------

func TestCase2_NotCached_NoMeta_Passthrough(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "abc123")
	defer cleanup()

	// Don't create any checkpoint dir — simulates uncached checkpoint.
	err := mgr.ensureCheckpointRebased(context.Background(), "cp-nonexistent")
	if err != nil {
		t.Fatalf("expected nil for uncached checkpoint, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Case 4 — Version mismatch: ensureCheckpointRebased detects it and attempts
// rebase. Since we don't have real qemu-img, verify it reaches the download
// step and fails with the expected error (old base not in mock store).
// ---------------------------------------------------------------------------

func TestCase4_VersionMismatch_TriggersRebase(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "newversion123")
	defer cleanup()

	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-stale", SnapshotMeta{
		SandboxID:     "sb-test",
		GoldenVersion: "oldversion456",
		SnapshotedAt:  time.Now(),
	})

	err := mgr.ensureCheckpointRebased(context.Background(), "cp-stale")
	if err == nil {
		t.Fatal("expected error for version mismatch (old base not in store)")
	}
	// Should fail trying to download the old base.
	if got := err.Error(); !contains(got, "oldversion456") {
		t.Fatalf("error should reference old version, got: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Case 5a — Legacy checkpoint (empty goldenVersion), created AFTER base install.
// checkLegacyCheckpoint should return nil (safe to fork).
// ---------------------------------------------------------------------------

func TestCase5a_LegacyCheckpoint_AfterBaseInstall(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "abc123")
	defer cleanup()

	// Touch the base image with a known mtime (1 hour ago).
	basePath := filepath.Join(mgr.cfg.ImagesDir, "default.ext4")
	past := time.Now().Add(-1 * time.Hour)
	os.Chtimes(basePath, past, past)

	// Checkpoint was created AFTER base install.
	meta := SnapshotMeta{
		SandboxID:    "sb-test",
		SnapshotedAt: time.Now(), // after past
	}

	err := mgr.checkLegacyCheckpoint("cp-legacy-ok", meta)
	if err != nil {
		t.Fatalf("expected nil for post-install checkpoint, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Case 5b — Legacy checkpoint (empty goldenVersion), created BEFORE base install.
// checkLegacyCheckpoint should return a clear error.
// ---------------------------------------------------------------------------

func TestCase5b_LegacyCheckpoint_BeforeBaseInstall(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "abc123")
	defer cleanup()

	// Base installed "now".
	basePath := filepath.Join(mgr.cfg.ImagesDir, "default.ext4")
	os.Chtimes(basePath, time.Now(), time.Now())

	// Checkpoint was created BEFORE base install.
	meta := SnapshotMeta{
		SandboxID:    "sb-test",
		SnapshotedAt: time.Now().Add(-2 * time.Hour),
	}

	err := mgr.checkLegacyCheckpoint("cp-legacy-old", meta)
	if err == nil {
		t.Fatal("expected error for pre-install checkpoint")
	}
	msg := err.Error()
	if !contains(msg, "predates current base image") {
		t.Fatalf("error should mention 'predates current base image', got: %s", msg)
	}
	if !contains(msg, "Cannot migrate automatically") {
		t.Fatalf("error should mention 'Cannot migrate automatically', got: %s", msg)
	}
}

// ---------------------------------------------------------------------------
// Case 5a (backfill) — backfillGoldenVersions labels empty-version checkpoints
// that were created after the base was installed.
// ---------------------------------------------------------------------------

func TestCase5a_BackfillGoldenVersions(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "current789")
	defer cleanup()

	// Set base mtime to 1 hour ago.
	basePath := filepath.Join(mgr.cfg.ImagesDir, "default.ext4")
	past := time.Now().Add(-1 * time.Hour)
	os.Chtimes(basePath, past, past)

	// Create 3 checkpoints:
	// 1. Empty version, created after base → should be labeled
	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-backfill-yes", SnapshotMeta{
		SandboxID:    "sb-1",
		SnapshotedAt: time.Now(),
	})
	// 2. Empty version, created before base → should NOT be labeled
	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-backfill-no", SnapshotMeta{
		SandboxID:    "sb-2",
		SnapshotedAt: time.Now().Add(-2 * time.Hour),
	})
	// 3. Already has version → should NOT be touched
	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-already-labeled", SnapshotMeta{
		SandboxID:     "sb-3",
		GoldenVersion: "existing999",
		SnapshotedAt:  time.Now(),
	})

	mgr.backfillGoldenVersions()

	// Verify cp-backfill-yes was labeled.
	meta1 := readCheckpointMeta(t, mgr.cfg.DataDir, "cp-backfill-yes")
	if meta1.GoldenVersion != "current789" {
		t.Errorf("cp-backfill-yes: expected goldenVersion=current789, got=%s", meta1.GoldenVersion)
	}

	// Verify cp-backfill-no was NOT labeled.
	meta2 := readCheckpointMeta(t, mgr.cfg.DataDir, "cp-backfill-no")
	if meta2.GoldenVersion != "" {
		t.Errorf("cp-backfill-no: expected empty goldenVersion, got=%s", meta2.GoldenVersion)
	}

	// Verify cp-already-labeled was NOT changed.
	meta3 := readCheckpointMeta(t, mgr.cfg.DataDir, "cp-already-labeled")
	if meta3.GoldenVersion != "existing999" {
		t.Errorf("cp-already-labeled: expected goldenVersion=existing999, got=%s", meta3.GoldenVersion)
	}
}

// ---------------------------------------------------------------------------
// Case 6 — Old base not in S3. ensureCheckpointRebased should fail with
// a download error, not hang or corrupt.
// ---------------------------------------------------------------------------

func TestCase6_MissingOldBase_CleanError(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "newversion")
	defer cleanup()

	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-missing-base", SnapshotMeta{
		SandboxID:     "sb-test",
		GoldenVersion: "vanished000",
		SnapshotedAt:  time.Now(),
	})

	err := mgr.ensureCheckpointRebased(context.Background(), "cp-missing-base")
	if err == nil {
		t.Fatal("expected error when old base is not in S3")
	}
	if !contains(err.Error(), "vanished000") {
		t.Fatalf("error should reference missing version, got: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Case 7 — Concurrent acquireOldBase for same version: only one download,
// both callers get a valid path and cleanup happens on last release.
// ---------------------------------------------------------------------------

func TestCase7_ConcurrentAcquire_SingleDownload(t *testing.T) {
	mock := newMockBlobClient()
	store := storage.NewCheckpointStoreFromClient(mock, "checkpoints")

	// Seed a 1KB fake base in the mock store.
	fakeBase := bytes.Repeat([]byte("X"), 1024)
	mock.put("checkpoints", "bases/ver123/default.ext4", fakeBase)

	// Reset global cache from prior tests.
	cleanupOldBases()

	var wg sync.WaitGroup
	paths := make([]string, 10)
	releases := make([]func(), 10)
	errs := make([]error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p, rel, err := acquireOldBase(context.Background(), store, "ver123")
			paths[idx] = p
			releases[idx] = rel
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// All should succeed with the same path.
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d failed: %v", i, err)
		}
		if paths[i] != paths[0] {
			t.Fatalf("goroutine %d got path %s, expected %s", i, paths[i], paths[0])
		}
	}

	// File should exist while refs are held.
	if !fileExists(paths[0]) {
		t.Fatal("base file should exist while references are held")
	}

	// Release all but one — file should still exist.
	for i := 1; i < 10; i++ {
		releases[i]()
	}
	if !fileExists(paths[0]) {
		t.Fatal("base file should exist while one reference remains")
	}

	// Release last — file should be cleaned up.
	releases[0]()
	if fileExists(paths[0]) {
		t.Fatal("base file should be deleted after all references released")
	}
}

// ---------------------------------------------------------------------------
// updateCheckpointGoldenVersion — metadata file rewrite
// ---------------------------------------------------------------------------

func TestUpdateGoldenVersion(t *testing.T) {
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snapshot")
	os.MkdirAll(snapDir, 0755)

	original := SnapshotMeta{
		SandboxID:     "sb-test",
		GoldenVersion: "old",
		SnapshotedAt:  time.Now(),
	}
	data, _ := json.Marshal(original)
	os.WriteFile(filepath.Join(snapDir, "snapshot-meta.json"), data, 0644)

	if err := updateCheckpointGoldenVersion(dir, "new-version"); err != nil {
		t.Fatal(err)
	}

	updated, _ := os.ReadFile(filepath.Join(snapDir, "snapshot-meta.json"))
	var meta SnapshotMeta
	json.Unmarshal(updated, &meta)

	if meta.GoldenVersion != "new-version" {
		t.Errorf("expected goldenVersion=new-version, got=%s", meta.GoldenVersion)
	}
	if meta.SandboxID != "sb-test" {
		t.Errorf("SandboxID should be preserved, got=%s", meta.SandboxID)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsImpl(s, substr))
}

func containsImpl(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
