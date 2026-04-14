package types

import (
	"encoding/json"
	"fmt"
	"time"
)

// SandboxStatus represents the current state of a sandbox.
type SandboxStatus string

const (
	SandboxStatusRunning    SandboxStatus = "running"
	SandboxStatusStopped    SandboxStatus = "stopped"
	SandboxStatusError      SandboxStatus = "error"
	SandboxStatusHibernated SandboxStatus = "hibernated"
)

// Sandbox represents a running sandbox instance.
type Sandbox struct {
	ID         string            `json:"sandboxID"`
	Template   string            `json:"templateID,omitempty"`
	Alias      string            `json:"alias,omitempty"`
	ClientID   string            `json:"clientID,omitempty"`
	Status     SandboxStatus     `json:"status"`
	StartedAt  time.Time         `json:"startedAt"`
	EndAt      time.Time         `json:"endAt"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CpuCount   int               `json:"cpuCount"`
	MemoryMB   int               `json:"memoryMB"`
	MachineID  string            `json:"machineID,omitempty"`
	// ConnectURL and Token are currently unused by SDKs. All data-plane traffic
	// flows through the control plane's SandboxAPIProxy, which proxies to workers
	// over the internal VPC network. Direct worker access support coming in a future release.
	ConnectURL string            `json:"connectURL,omitempty"`
	Token      string            `json:"token,omitempty"`
	HostPort   int               `json:"hostPort,omitempty"`   // Mapped host port for the sandbox's container port
}

// SandboxConfig is the request body for creating a sandbox.
type SandboxConfig struct {
	Template   string            `json:"templateID,omitempty"`
	Alias      string            `json:"alias,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`    // seconds, default 300
	CpuCount   int               `json:"cpuCount,omitempty"`   // default 1
	MemoryMB   int               `json:"memoryMB,omitempty"`   // default 256
	DiskMB     int               `json:"diskMB,omitempty"`    // workspace disk in MB (default 20480)
	Envs       map[string]string `json:"envs,omitempty"`
	Port       int               `json:"port,omitempty"`       // container port to expose via subdomain (default 80)
	NetworkEnabled bool          `json:"networkEnabled,omitempty"`
	ImageRef       string        `json:"imageRef,omitempty"`       // resolved ECR URI for custom templates
	// Sandbox snapshot template: S3 keys for rootfs and workspace drives.
	// When set, the sandbox boots from these drives instead of the standard base image.
	TemplateRootfsKey    string `json:"templateRootfsKey,omitempty"`
	TemplateWorkspaceKey string `json:"templateWorkspaceKey,omitempty"`
	// SecretStore name — resolves secrets from the named secret store.
	// When layered (base snapshot had a store + fork supplies another),
	// this holds the child (fork-supplied) store. BaseSecretStore holds the parent.
	SecretStore string `json:"secretStore,omitempty"`
	// BaseSecretStore records the parent snapshot's store when a fork layers
	// a different child store on top. On fork-of-fork, the checkpoint's
	// SecretStore already represents its full merged ancestry.
	BaseSecretStore string `json:"baseSecretStore,omitempty"`
	// EgressAllowlist restricts outbound HTTPS from the sandbox to these hosts.
	// Supports exact matches ("api.anthropic.com") and wildcards ("*.openai.com").
	// Empty = all hosts allowed (no restriction).
	EgressAllowlist []string `json:"egressAllowlist,omitempty"`
	// SecretAllowedHosts maps env var name → allowed hosts for that secret.
	// Secrets are only substituted in requests to matching hosts.
	// Missing key or empty slice = substitute on all allowed hosts.
	SecretAllowedHosts map[string][]string `json:"secretAllowedHosts,omitempty"`
	// Declarative image manifest: when set, the server builds and caches
	// the image as a checkpoint before creating the sandbox.
	ImageManifest json.RawMessage `json:"image,omitempty"`
	// Snapshot name: when set, the server resolves a named snapshot (image cache)
	// and creates the sandbox from its cached checkpoint.
	Snapshot string `json:"snapshot,omitempty"`
	// SandboxID allows pre-determining the sandbox ID for async creation.
	// If empty, a new ID is generated automatically.
	SandboxID string `json:"-"`
	// CheckpointID is the source checkpoint for template/snapshot creates.
	// Used by the worker to key per-template golden snapshots.
	CheckpointID string `json:"-"`
	// SecretEnvs holds env vars resolved from a SecretStore. Carrying them in
	// a separate field (rather than mixing them into Envs) preserves their
	// provenance end-to-end: the worker's secrets proxy seals exactly these
	// entries and passes everything in Envs through as plaintext. Never
	// persisted (json:"-") and never set directly by the SDK — populated by
	// the API layer in resolveSecretStoreInto and re-derived on every fork.
	SecretEnvs map[string]string `json:"-"`
}

// ResourceTier defines an allowed memory/CPU combination.
type ResourceTier struct {
	MemoryMB int
	VCPUs    int
}

// AllowedResourceTiers lists the valid memory→vCPU combinations.
var AllowedResourceTiers = []ResourceTier{
	{MemoryMB: 1024, VCPUs: 1},
	{MemoryMB: 4096, VCPUs: 1},
	{MemoryMB: 8192, VCPUs: 2},
	{MemoryMB: 16384, VCPUs: 4},
	{MemoryMB: 32768, VCPUs: 8},
	{MemoryMB: 65536, VCPUs: 16},
}

// ValidateMemoryMB checks that memoryMB matches an allowed tier and returns the corresponding vCPU count.
// Returns 0, nil if memoryMB is 0 (use defaults).
func ValidateMemoryMB(memoryMB int) (vcpus int, err error) {
	if memoryMB == 0 {
		return 0, nil
	}
	for _, t := range AllowedResourceTiers {
		if memoryMB == t.MemoryMB {
			return t.VCPUs, nil
		}
	}
	return 0, fmt.Errorf("memoryMB must be one of: 1024, 4096, 8192, 16384, 32768, 65536 (got %d)", memoryMB)
}

// ValidateCPUCount checks that cpuCount matches an allowed tier and returns the corresponding memoryMB.
// Returns 0, nil if cpuCount is 0 (use defaults).
func ValidateCPUCount(cpuCount int) (memoryMB int, err error) {
	if cpuCount == 0 {
		return 0, nil
	}
	for _, t := range AllowedResourceTiers {
		if cpuCount == t.VCPUs {
			return t.MemoryMB, nil
		}
	}
	return 0, fmt.Errorf("cpuCount must be one of: 1, 2, 4, 8, 16 (got %d)", cpuCount)
}

// ValidateResourceTier validates and normalizes CPU/memory on a SandboxConfig.
// If only one is set, the other is inferred from the tier. If both are set, they must match.
func ValidateResourceTier(cfg *SandboxConfig) error {
	if cfg.MemoryMB == 0 && cfg.CpuCount == 0 {
		return nil // use defaults
	}
	if cfg.MemoryMB > 0 && cfg.CpuCount == 0 {
		vcpus, err := ValidateMemoryMB(cfg.MemoryMB)
		if err != nil {
			return err
		}
		cfg.CpuCount = vcpus
		return nil
	}
	if cfg.CpuCount > 0 && cfg.MemoryMB == 0 {
		mem, err := ValidateCPUCount(cfg.CpuCount)
		if err != nil {
			return err
		}
		cfg.MemoryMB = mem
		return nil
	}
	// Both set — verify they match a tier
	for _, t := range AllowedResourceTiers {
		if cfg.MemoryMB == t.MemoryMB && cfg.CpuCount == t.VCPUs {
			return nil
		}
	}
	return fmt.Errorf("cpuCount %d and memoryMB %d do not match an allowed tier; valid combinations: 1/1024, 1/4096, 2/8192, 4/16384, 8/32768, 16/65536", cfg.CpuCount, cfg.MemoryMB)
}

// SandboxListResponse is the response for listing sandboxes.
type SandboxListResponse struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

// TimeoutRequest is the request body for updating sandbox timeout.
type TimeoutRequest struct {
	Timeout int `json:"timeout"` // seconds
}

// HibernationInfo holds metadata about a hibernated sandbox.
type HibernationInfo struct {
	SandboxID      string    `json:"sandboxId"`
	HibernationKey string    `json:"hibernationKey"`
	SizeBytes      int64     `json:"sizeBytes"`
	Region         string    `json:"region"`
	Template       string    `json:"template"`
	HibernatedAt   time.Time `json:"hibernatedAt"`
}

// WakeRequest is the request body for waking a hibernated sandbox.
type WakeRequest struct {
	Timeout int `json:"timeout,omitempty"` // new timeout in seconds after wake
}
