package sandbox

import (
	"context"
	"io"

	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// HibernateResult holds the result of a hibernate operation.
type HibernateResult struct {
	SandboxID      string `json:"sandboxId"`
	HibernationKey string `json:"hibernationKey"`
	SizeBytes      int64  `json:"sizeBytes"`
}

// MigrationSecrets is the per-sandbox secrets-proxy state that must move
// with the VM during a live migration. The proxy keeps these in-process
// per-worker (see internal/secretsproxy), so without an explicit handoff
// the destination has no substitution map and the guest's env vars
// (`osb_sealed_xxx`) leak verbatim to upstream services. Hibernate
// persists the same data into snapshot-meta.json; live migration carries
// it through the PreCopyDrives → PrepareMigrationIncoming RPC chain.
//
// Lives in package sandbox (not qemu) so the worker grpc layer can name
// it without importing qemu (which would create a cycle).
type MigrationSecrets struct {
	SealedTokens    map[string]string
	EgressAllowlist []string
	TokenHosts      map[string][]string
	// SealedNames is the env-var-name → sealed-token index. Carried across
	// migration so refresh-by-name (UpdateSecretValue) keeps working on the
	// destination. Without it, post-migration secret-store updates would
	// silently miss because the destination's session has no name index.
	SealedNames map[string]string
}

// SandboxStats holds live resource usage for a sandbox.
// Runtime-agnostic interface for sandbox resource stats.
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
// not on a concrete implementation. Currently implemented by the Firecracker backend.
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
	ReadFileStream(ctx context.Context, sandboxID, path string) (io.ReadCloser, int64, error)
	WriteFileStream(ctx context.Context, sandboxID, path string, mode uint32, r io.Reader) (int64, error)
	ListDir(ctx context.Context, sandboxID, path string) ([]types.EntryInfo, error)
	MakeDir(ctx context.Context, sandboxID, path string) error
	Remove(ctx context.Context, sandboxID, path string) error
	Exists(ctx context.Context, sandboxID, path string) (bool, error)
	Stat(ctx context.Context, sandboxID, path string) (*types.FileInfo, error)

	// Resource limits
	SetResourceLimits(ctx context.Context, sandboxID string, maxPids int32, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec int64) error

	// UpdateSandboxSecret refreshes the proxy session value for one secret name
	// (env var name) without changing the sealed token id seen by the sandbox.
	// Used by the secret-store-update flow to push new values to running
	// sandboxes. Returns (true, nil) on success; (false, nil) if no session
	// or no name match (transient miss e.g. mid-migration; caller logs).
	UpdateSandboxSecret(ctx context.Context, sandboxID, secretName, value string) (bool, error)

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

	// Reset operations. RebootSandbox is a soft, in-place guest restart;
	// PowerCycleSandbox is a hard restart that re-creates the QEMU process
	// with the same on-disk drives. Both preserve the sandbox's identity
	// and persistent data; power-cycle returns a new external host port.
	RebootSandbox(ctx context.Context, sandboxID string) error
	PowerCycleSandbox(ctx context.Context, sandboxID string) (newHostPort int, err error)

	// TemplateCachePath returns the local path to a cached template drive file (e.g., "rootfs.ext4"),
	// or "" if the template is not cached locally. Used to skip S3 download when creating from template.
	TemplateCachePath(templateID, filename string) string

	// Checkpointing
	CreateCheckpoint(ctx context.Context, sandboxID, checkpointID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error)
	RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error
	ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error)
	CheckpointCachePath(checkpointID, filename string) string
}
