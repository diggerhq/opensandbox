package sandbox

import (
	"context"

	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// SandboxStats holds live resource usage for a sandbox.
// Runtime-agnostic: both Podman and Firecracker backends populate these fields.
type SandboxStats struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemUsage   uint64  `json:"memUsage"` // bytes
	MemLimit   uint64  `json:"memLimit"` // bytes
	NetInput   uint64  `json:"netInput"` // bytes
	NetOutput  uint64  `json:"netOutput"`// bytes
	PIDs       int     `json:"pids"`
}

// Manager defines the sandbox lifecycle interface.
// Upper layers (SandboxRouter, HTTP/gRPC servers, proxy) depend on this interface,
// not on a concrete implementation. This allows swapping the backend
// (e.g., Podman containers → Firecracker microVMs) without changing callers.
type Manager interface {
	// Lifecycle
	Create(ctx context.Context, cfg types.SandboxConfig) (*types.Sandbox, error)
	Get(ctx context.Context, id string) (*types.Sandbox, error)
	Kill(ctx context.Context, id string) error
	List(ctx context.Context) ([]types.Sandbox, error)
	Count(ctx context.Context) (int, error)
	Close()

	// Execution
	Exec(ctx context.Context, sandboxID string, cfg types.ProcessConfig) (*types.ProcessResult, error)

	// Filesystem
	ReadFile(ctx context.Context, sandboxID, path string) (string, error)
	WriteFile(ctx context.Context, sandboxID, path, content string) error
	ListDir(ctx context.Context, sandboxID, path string) ([]types.EntryInfo, error)
	MakeDir(ctx context.Context, sandboxID, path string) error
	Remove(ctx context.Context, sandboxID, path string) error
	Exists(ctx context.Context, sandboxID, path string) (bool, error)
	Stat(ctx context.Context, sandboxID, path string) (*types.FileInfo, error)

	// Monitoring
	Stats(ctx context.Context, sandboxID string) (*SandboxStats, error)
	HostPort(ctx context.Context, sandboxID string) (int, error)
	ContainerAddr(ctx context.Context, sandboxID string, port int) (string, error)
	DataDir() string

	// Sandbox name (for logging/cleanup)
	ContainerName(id string) string

	// Hibernation
	Hibernate(ctx context.Context, sandboxID string, checkpointStore *storage.CheckpointStore) (*HibernateResult, error)
	Wake(ctx context.Context, sandboxID string, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error)

	// SaveAsTemplate snapshots a running sandbox's drives (rootfs + workspace) for use as a
	// template. The VM is briefly paused during file copy then resumed. Archive upload is async.
	// Returns the pre-computed storage keys immediately. onReady is called when the async upload finishes (may be nil).
	SaveAsTemplate(ctx context.Context, sandboxID, templateID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error)

	// TemplateCachePath returns the local path to a cached template drive file (e.g., "rootfs.ext4"),
	// or "" if the template is not cached locally. Used to skip S3 download when creating from template.
	TemplateCachePath(templateID, filename string) string

	// Checkpointing
	CreateCheckpoint(ctx context.Context, sandboxID, checkpointID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error)
	RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error
	CheckpointCachePath(checkpointID, filename string) string
}
