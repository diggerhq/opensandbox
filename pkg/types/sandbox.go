package types

import "time"

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
	ConnectURL string            `json:"connectURL,omitempty"` // Direct worker URL for SDK access
	Token      string            `json:"token,omitempty"`      // Sandbox-scoped JWT for worker auth
	Domain     string            `json:"domain,omitempty"`     // Subdomain for web access (e.g., "abc123.workers.opensandbox.dev")
	HostPort   int               `json:"hostPort,omitempty"`   // Mapped host port for the sandbox's container port
}

// SandboxConfig is the request body for creating a sandbox.
type SandboxConfig struct {
	Template   string            `json:"templateID,omitempty"`
	Alias      string            `json:"alias,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timeout    int               `json:"timeout,omitempty"`    // seconds, default 300
	CpuCount   int               `json:"cpuCount,omitempty"`   // default 1
	MemoryMB   int               `json:"memoryMB,omitempty"`   // default 512
	Envs       map[string]string `json:"envs,omitempty"`
	Port       int               `json:"port,omitempty"`       // container port to expose via subdomain (default 80)
	NetworkEnabled bool          `json:"networkEnabled,omitempty"`
	ImageRef       string            `json:"imageRef,omitempty"`       // resolved ECR URI for custom templates
}

// SandboxListResponse is the response for listing sandboxes.
type SandboxListResponse struct {
	Sandboxes []Sandbox `json:"sandboxes"`
}

// TimeoutRequest is the request body for updating sandbox timeout.
type TimeoutRequest struct {
	Timeout int `json:"timeout"` // seconds
}

// CheckpointInfo holds metadata about a hibernated sandbox's checkpoint.
type CheckpointInfo struct {
	SandboxID     string    `json:"sandboxId"`
	CheckpointKey string    `json:"checkpointKey"`
	SizeBytes     int64     `json:"sizeBytes"`
	Region        string    `json:"region"`
	Template      string    `json:"template"`
	HibernatedAt  time.Time `json:"hibernatedAt"`
}

// WakeRequest is the request body for waking a hibernated sandbox.
type WakeRequest struct {
	Timeout int `json:"timeout,omitempty"` // new timeout in seconds after wake
}
