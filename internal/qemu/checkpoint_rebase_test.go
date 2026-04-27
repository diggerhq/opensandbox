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
	objects map[string][]byte
	heads   int64
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

func testManager(t *testing.T, goldenVersion string) (mgr *Manager, store *storage.CheckpointStore, mock *mockBlobClient, cleanup func()) {
	t.Helper()
	dataDir := t.TempDir()
	imagesDir := t.TempDir()

	basePath := filepath.Join(imagesDir, "default.ext4")
	if err := os.WriteFile(basePath, []byte("fake-base-content"), 0644); err != nil {
		t.Fatal(err)
	}

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
		// Reset in-flight map between tests so residual entries don't confuse
		// the next test's coordination logic.
		flightMu.Lock()
		for k := range downloadFlight {
			delete(downloadFlight, k)
		}
		flightMu.Unlock()
	}
	return
}

func writeCheckpointMeta(t *testing.T, dataDir, checkpointID string, meta SnapshotMeta) string {
	t.Helper()
	cpDir := filepath.Join(dataDir, "checkpoint-snapshots", checkpointID)
	snapDir := filepath.Join(cpDir, "snapshot")
	if err := os.MkdirAll(snapDir, 0755); err != nil {
		t.Fatal(err)
	}
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

// ---------------------------------------------------------------------------
// resolveBaseForVersion
// ---------------------------------------------------------------------------

func TestResolveBase_CurrentVersion(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	path, err := mgr.resolveBaseForVersion(context.Background(), "v-current")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(mgr.cfg.ImagesDir, "default.ext4")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestResolveBase_Retained(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	retained := filepath.Join(mgr.cfg.ImagesDir, "bases", "v-prev", "default.ext4")
	if err := os.MkdirAll(filepath.Dir(retained), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retained, []byte("prev-base"), 0644); err != nil {
		t.Fatal(err)
	}

	path, err := mgr.resolveBaseForVersion(context.Background(), "v-prev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != retained {
		t.Fatalf("path = %q, want %q", path, retained)
	}
}

func TestResolveBase_DownloadOnDemand(t *testing.T) {
	mgr, _, mock, cleanup := testManager(t, "v-current")
	defer cleanup()

	payload := bytes.Repeat([]byte("B"), 4096)
	mock.put("checkpoints", "bases/v-gone/default.ext4", payload)

	path, err := mgr.resolveBaseForVersion(context.Background(), "v-gone")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(mgr.cfg.ImagesDir, "bases", "v-gone", "default.ext4")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded content differs (got %d bytes, want %d)", len(got), len(payload))
	}
}

func TestResolveBase_MissingInBlob(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	_, err := mgr.resolveBaseForVersion(context.Background(), "v-never-uploaded")
	if err == nil {
		t.Fatal("expected error when base not in blob storage")
	}
	if !contains(err.Error(), "v-never-uploaded") {
		t.Fatalf("error should reference missing version, got: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// ensureCheckpointRebased (pin-to-base semantics)
// ---------------------------------------------------------------------------

// Same version: base resolves to current default.ext4. Rebase would run but
// qemu-img isn't available in unit tests — we fail at that step with a
// clear exec error, NOT at the resolve step.
func TestEnsureCheckpointRebased_SameVersion_ResolvesCurrent(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-same", SnapshotMeta{
		SandboxID:     "sb-test",
		GoldenVersion: "v-current",
		SnapshotedAt:  time.Now(),
	})

	err := mgr.ensureCheckpointRebased(context.Background(), "cp-same")
	// Either nil (qemu-img present on dev machine) or a qemu-img error.
	// What we're checking: no "resolve" error and no missing-base error.
	if err != nil && (contains(err.Error(), "resolve") || contains(err.Error(), "not found")) {
		t.Fatalf("unexpected resolve failure: %v", err)
	}
}

func TestEnsureCheckpointRebased_NoMetaPassthrough(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	err := mgr.ensureCheckpointRebased(context.Background(), "cp-nonexistent")
	if err != nil {
		t.Fatalf("expected nil for uncached checkpoint, got: %v", err)
	}
}

// Cross-golden with blob missing: fail at resolve step (no partial qcow2
// corruption, clean error).
func TestEnsureCheckpointRebased_CrossGolden_MissingBlob(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	writeCheckpointMeta(t, mgr.cfg.DataDir, "cp-missing-base", SnapshotMeta{
		SandboxID:     "sb-test",
		GoldenVersion: "v-vanished",
		SnapshotedAt:  time.Now(),
	})

	err := mgr.ensureCheckpointRebased(context.Background(), "cp-missing-base")
	if err == nil {
		t.Fatal("expected error when old base is not in blob store")
	}
	if !contains(err.Error(), "v-vanished") {
		t.Fatalf("error should reference missing version, got: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Legacy checkpoints (no goldenVersion recorded)
// ---------------------------------------------------------------------------

func TestLegacyCheckpoint_AfterBaseInstall(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	basePath := filepath.Join(mgr.cfg.ImagesDir, "default.ext4")
	past := time.Now().Add(-1 * time.Hour)
	os.Chtimes(basePath, past, past)

	meta := SnapshotMeta{
		SandboxID:    "sb-test",
		SnapshotedAt: time.Now(),
	}

	if err := mgr.checkLegacyCheckpoint("cp-legacy-ok", meta); err != nil {
		t.Fatalf("expected nil for post-install checkpoint, got: %v", err)
	}
}

func TestLegacyCheckpoint_BeforeBaseInstall(t *testing.T) {
	mgr, _, _, cleanup := testManager(t, "v-current")
	defer cleanup()

	basePath := filepath.Join(mgr.cfg.ImagesDir, "default.ext4")
	os.Chtimes(basePath, time.Now(), time.Now())

	meta := SnapshotMeta{
		SandboxID:    "sb-test",
		SnapshotedAt: time.Now().Add(-2 * time.Hour),
	}

	err := mgr.checkLegacyCheckpoint("cp-legacy-old", meta)
	if err == nil {
		t.Fatal("expected error for pre-install checkpoint")
	}
	if !contains(err.Error(), "predates current base image") {
		t.Fatalf("error should mention 'predates current base image', got: %s", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Concurrent downloads: one fetch, many waiters
// ---------------------------------------------------------------------------

func TestDownloadBase_ConcurrentSingleFetch(t *testing.T) {
	mgr, _, mock, cleanup := testManager(t, "v-current")
	defer cleanup()

	payload := bytes.Repeat([]byte("X"), 1024)
	mock.put("checkpoints", "bases/v-shared/default.ext4", payload)

	var wg sync.WaitGroup
	paths := make([]string, 10)
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p, err := mgr.resolveBaseForVersion(context.Background(), "v-shared")
			paths[idx] = p
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	want := filepath.Join(mgr.cfg.ImagesDir, "bases", "v-shared", "default.ext4")
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if paths[i] != want {
			t.Fatalf("goroutine %d: path = %q, want %q", i, paths[i], want)
		}
	}

	if !fileExists(want) {
		t.Fatal("expected cached base to persist after resolve")
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
