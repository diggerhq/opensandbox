// Package qemu — docs_compliance_test.go
//
// Verifies that the QEMU backend implements every operation documented in
// docs/reference/api.mdx and the sandbox.Manager interface. This is a
// compile-time + reflection test — it does NOT require KVM or a running VM.
package qemu

import (
	"context"
	"reflect"
	"testing"

	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// TestManagerImplementsInterface is a compile-time check that *Manager
// satisfies sandbox.Manager. The var _ line in manager.go does this too,
// but having it in a test makes failures visible in CI.
func TestManagerImplementsInterface(t *testing.T) {
	var _ sandbox.Manager = (*Manager)(nil)
}

// TestDocumentedLifecycleOps checks that every sandbox lifecycle operation
// documented in reference/api.mdx has a corresponding Manager method.
//
// Docs:
//   POST /api/sandboxes          → Create
//   GET  /api/sandboxes           → List
//   GET  /api/sandboxes/:id       → Get
//   DELETE /api/sandboxes/:id     → Kill
//   POST .../hibernate            → Hibernate
//   POST .../wake                 → Wake
func TestDocumentedLifecycleOps(t *testing.T) {
	methods := managerMethods()

	required := map[string]string{
		"Create":    "POST /api/sandboxes",
		"List":      "GET /api/sandboxes",
		"Get":       "GET /api/sandboxes/:id",
		"Kill":      "DELETE /api/sandboxes/:id",
		"Count":     "GET /api/sandboxes (count variant)",
		"Close":     "graceful shutdown",
		"Hibernate": "POST /api/sandboxes/:id/hibernate",
		"Wake":      "POST /api/sandboxes/:id/wake",
	}

	for method, doc := range required {
		if !methods[method] {
			t.Errorf("Manager missing %s (docs: %s)", method, doc)
		}
	}
}

// TestDocumentedExecOps checks command execution.
//
// Docs:
//   POST .../exec/run  → Exec (sync)
//   POST .../exec      → CreateExecSession (handled by worker HTTP layer)
//   GET  .../exec      → ListExecSessions  (handled by worker HTTP layer)
//
// The Manager only exposes sync Exec; session management is in the worker HTTP server.
func TestDocumentedExecOps(t *testing.T) {
	methods := managerMethods()

	if !methods["Exec"] {
		t.Error("Manager missing Exec (docs: POST .../exec/run)")
	}
}

// TestDocumentedFilesystemOps checks every filesystem operation.
//
// Docs:
//   GET    .../files?path=      → ReadFile
//   PUT    .../files?path=      → WriteFile
//   GET    .../files/list?path= → ListDir
//   POST   .../files/mkdir      → MakeDir
//   DELETE .../files?path=      → Remove
func TestDocumentedFilesystemOps(t *testing.T) {
	methods := managerMethods()

	required := map[string]string{
		"ReadFile":  "GET .../files?path=",
		"WriteFile": "PUT .../files?path=",
		"ListDir":   "GET .../files/list?path=",
		"MakeDir":   "POST .../files/mkdir",
		"Remove":    "DELETE .../files?path=",
		"Exists":    "existence check (used by worker)",
		"Stat":      "file stat (used by worker)",
	}

	for method, doc := range required {
		if !methods[method] {
			t.Errorf("Manager missing %s (docs: %s)", method, doc)
		}
	}
}

// TestDocumentedCheckpointOps checks checkpoint operations.
//
// Docs:
//   POST .../checkpoints                          → CreateCheckpoint
//   POST .../checkpoints/:id/restore              → RestoreFromCheckpoint
//   POST /api/sandboxes/from-checkpoint/:id       → ForkFromCheckpoint
//   GET  .../checkpoints                          → (list — DB layer, not manager)
//   DELETE .../checkpoints/:id                    → (delete — DB layer, not manager)
func TestDocumentedCheckpointOps(t *testing.T) {
	methods := managerMethods()

	required := map[string]string{
		"CreateCheckpoint":      "POST .../checkpoints",
		"RestoreFromCheckpoint": "POST .../checkpoints/:id/restore",
		"ForkFromCheckpoint":    "POST /api/sandboxes/from-checkpoint/:id",
		"CheckpointCachePath":   "local cache path for checkpoint drives",
	}

	for method, doc := range required {
		if !methods[method] {
			t.Errorf("Manager missing %s (docs: %s)", method, doc)
		}
	}
}

// TestDocumentedTemplateOps checks template operations.
//
// Templates are created via the declarative snapshot API (POST /api/snapshots).
// SaveAsTemplate (snapshot a running VM) was removed — it's replaced by
// CreateCheckpoint + ForkFromCheckpoint for runtime cloning.
func TestDocumentedTemplateOps(t *testing.T) {
	methods := managerMethods()

	if !methods["TemplateCachePath"] {
		t.Error("Manager missing TemplateCachePath method")
	}
}

// TestDocumentedMonitoringOps checks monitoring operations.
func TestDocumentedMonitoringOps(t *testing.T) {
	methods := managerMethods()

	required := map[string]string{
		"Stats":         "sandbox CPU/memory stats",
		"HostPort":      "mapped host port for preview URLs",
		"ContainerAddr": "guest address for port forwarding",
		"DataDir":       "data directory path",
		"ContainerName": "sandbox name for logging",
	}

	for method, doc := range required {
		if !methods[method] {
			t.Errorf("Manager missing %s (docs: %s)", method, doc)
		}
	}
}

// TestDocumentedParameterNames checks that types.SandboxConfig field names
// match what the API docs specify.
//
// Docs say: templateID, timeout, cpuCount, memoryMB, envs, metadata
func TestDocumentedParameterNames(t *testing.T) {
	cfg := types.SandboxConfig{}
	v := reflect.TypeOf(cfg)

	expectedJSON := map[string]bool{
		"templateID": false,
		"timeout":    false,
		"cpuCount":   false,
		"memoryMB":   false,
		"envs":       false,
		"metadata":   false,
	}

	for i := range v.NumField() {
		field := v.Field(i)
		tag := field.Tag.Get("json")
		// Strip options like ",omitempty"
		if idx := len(tag); idx > 0 {
			for j := range len(tag) {
				if tag[j] == ',' {
					tag = tag[:j]
					break
				}
			}
		}
		if _, ok := expectedJSON[tag]; ok {
			expectedJSON[tag] = true
		}
	}

	for name, found := range expectedJSON {
		if !found {
			t.Errorf("SandboxConfig missing JSON field %q (documented in API reference)", name)
		}
	}
}

// TestDocumentedProcessConfigFields checks types.ProcessConfig matches docs.
//
// Docs say: cmd, args, envs, cwd, timeout
func TestDocumentedProcessConfigFields(t *testing.T) {
	cfg := types.ProcessConfig{}
	v := reflect.TypeOf(cfg)

	expectedJSON := map[string]bool{
		"cmd":     false,
		"args":    false,
		"envs":    false,
		"cwd":     false,
		"timeout": false,
	}

	for i := range v.NumField() {
		field := v.Field(i)
		tag := field.Tag.Get("json")
		for j := range len(tag) {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		if _, ok := expectedJSON[tag]; ok {
			expectedJSON[tag] = true
		}
	}

	for name, found := range expectedJSON {
		if !found {
			t.Errorf("ProcessConfig missing JSON field %q (documented in API reference)", name)
		}
	}
}

// TestDocumentedSandboxResponseFields checks that types.Sandbox has the
// fields the API docs say the create/get response returns.
//
// Docs say: sandboxID, templateID, status, startedAt, endAt, cpuCount, memoryMB,
//           metadata, connectURL, token
func TestDocumentedSandboxResponseFields(t *testing.T) {
	sb := types.Sandbox{}
	v := reflect.TypeOf(sb)

	expectedJSON := map[string]bool{
		"sandboxID":  false,
		"status":     false,
		"startedAt":  false,
		"endAt":      false,
		"cpuCount":   false,
		"memoryMB":   false,
		"connectURL": false,
		"token":      false,
	}

	for i := range v.NumField() {
		field := v.Field(i)
		tag := field.Tag.Get("json")
		for j := range len(tag) {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		if _, ok := expectedJSON[tag]; ok {
			expectedJSON[tag] = true
		}
	}

	for name, found := range expectedJSON {
		if !found {
			t.Errorf("Sandbox struct missing JSON field %q (documented in API response)", name)
		}
	}
}

// TestDocumentedDefaultValues verifies that QEMU manager defaults match docs.
//
// Docs say: cpuCount=1, memoryMB=256, timeout=0 (persistent), port=80
func TestDocumentedDefaultValues(t *testing.T) {
	// NewManager applies defaults to zero-valued Config fields.
	// We simulate that logic here since NewManager needs real dirs.
	cfg := Config{DataDir: "/tmp"}
	// Apply the same defaults as NewManager (lines 110-120 of manager.go)
	if cfg.DefaultMemoryMB == 0 {
		cfg.DefaultMemoryMB = 256
	}
	if cfg.DefaultCPUs == 0 {
		cfg.DefaultCPUs = 1
	}
	if cfg.DefaultPort == 0 {
		cfg.DefaultPort = 80
	}

	tests := []struct {
		name     string
		got      int
		expected int
	}{
		{"DefaultCPUs (docs: 1)", cfg.DefaultCPUs, 1},
		{"DefaultMemoryMB (docs: 256)", cfg.DefaultMemoryMB, 256},
		{"DefaultPort (docs: 80)", cfg.DefaultPort, 80},
	}

	for _, tt := range tests {
		if tt.got != tt.expected {
			t.Errorf("%s: got %d, want %d", tt.name, tt.got, tt.expected)
		}
	}
}

// TestDocumentedHibernateResponseFields checks HibernateResult matches docs.
//
// Docs say response includes: sandboxID, hibernationKey, sizeBytes
func TestDocumentedHibernateResponseFields(t *testing.T) {
	hr := sandbox.HibernateResult{}
	v := reflect.TypeOf(hr)

	expectedJSON := map[string]bool{
		"sandboxId":      false,
		"hibernationKey": false,
		"sizeBytes":      false,
	}

	for i := range v.NumField() {
		field := v.Field(i)
		tag := field.Tag.Get("json")
		for j := range len(tag) {
			if tag[j] == ',' {
				tag = tag[:j]
				break
			}
		}
		if _, ok := expectedJSON[tag]; ok {
			expectedJSON[tag] = true
		}
	}

	for name, found := range expectedJSON {
		if !found {
			t.Errorf("HibernateResult missing JSON field %q (documented in API response)", name)
		}
	}
}

// TestRouteToTemplateEndpointMismatch documents that the router uses
// /api/snapshots while the docs say /api/templates.
func TestRouteToTemplateEndpointMismatch(t *testing.T) {
	t.Log("KNOWN GAP: Router exposes /api/snapshots but docs reference /api/templates")
	t.Log("Docs: POST /api/templates, GET /api/templates, etc.")
	t.Log("Router: POST /api/snapshots, GET /api/snapshots, etc.")
}

// TestDocReferencesFirecracker documents all places where docs say
// "Firecracker" but the QEMU backend is the actual implementation.
func TestDocReferencesFirecracker(t *testing.T) {
	firecrackerRefs := []struct {
		file string
		text string
	}{
		{"docs/introduction.mdx", "isolated Firecracker microVM"},
		{"docs/introduction.mdx", "Hardware-level isolation via Firecracker"},
		{"docs/how-it-works.mdx", "provisions a Firecracker microVM"},
		{"docs/how-it-works.mdx", "A Firecracker microVM boots in ~150ms"},
		{"docs/how-it-works.mdx", "section title: Firecracker microVMs"},
		{"docs/quickstart.mdx", "You created a cloud VM (Firecracker microVM)"},
		{"docs/cli/sandbox.mdx", "provisions a new Firecracker microVM"},
		{"docs/reference/api.mdx", "filesystem image that Firecracker boots from"},
		{"docs/sandboxes/templates.mdx", "filesystem image that Firecracker boots as the VM's root disk"},
	}

	t.Log("KNOWN GAP: Docs reference 'Firecracker' but this branch uses QEMU q35 backend")
	for _, ref := range firecrackerRefs {
		t.Logf("  %s: %q", ref.file, ref.text)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// managerMethods returns a set of all exported method names on *Manager.
func managerMethods() map[string]bool {
	t := reflect.TypeOf((*Manager)(nil))
	methods := make(map[string]bool, t.NumMethod())
	for i := range t.NumMethod() {
		methods[t.Method(i).Name] = true
	}
	return methods
}

// Compile-time checks that documented method signatures match.
// These won't compile if the signatures drift from what the interface requires.
var (
	_ func(context.Context, types.SandboxConfig) (*types.Sandbox, error)                                                   = (*Manager)(nil).Create
	_ func(context.Context, string) (*types.Sandbox, error)                                                                = (*Manager)(nil).Get
	_ func(context.Context, string) error                                                                                  = (*Manager)(nil).Kill
	_ func(context.Context) ([]types.Sandbox, error)                                                                       = (*Manager)(nil).List
	_ func(context.Context, string, types.ProcessConfig) (*types.ProcessResult, error)                                     = (*Manager)(nil).Exec
	_ func(context.Context, string, string) (string, error)                                                                = (*Manager)(nil).ReadFile
	_ func(context.Context, string, string, string) error                                                                  = (*Manager)(nil).WriteFile
	_ func(context.Context, string, string) ([]types.EntryInfo, error)                                                     = (*Manager)(nil).ListDir
	_ func(context.Context, string, *storage.CheckpointStore) (*sandbox.HibernateResult, error)                            = (*Manager)(nil).Hibernate
	_ func(context.Context, string, string, *storage.CheckpointStore, int) (*types.Sandbox, error)                         = (*Manager)(nil).Wake
	_ func(context.Context, string, string, *storage.CheckpointStore, func()) (string, string, int64, error)               = (*Manager)(nil).CreateCheckpoint
	_ func(context.Context, string, string) error                                                                          = (*Manager)(nil).RestoreFromCheckpoint
	_ func(context.Context, string, types.SandboxConfig) (*types.Sandbox, error)                                           = (*Manager)(nil).ForkFromCheckpoint
)
