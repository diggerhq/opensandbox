package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/pkg/types"
)

const (
	labelPrefix   = "opensandbox"
	labelID       = labelPrefix + ".id"
	labelTemplate = labelPrefix + ".template"
	labelCreated  = labelPrefix + ".created"
	labelTimeout  = labelPrefix + ".timeout"
	labelHostPort = labelPrefix + ".host_port"
	containerName = "osb"

	defaultTimeout  = 300 // 5 minutes
	defaultImage    = "docker.io/library/ubuntu:22.04"
	maxMemoryMB     = 2048
	maxCPU          = 4
)

// Compile-time check: PodmanManager implements Manager interface.
var _ Manager = (*PodmanManager)(nil)

// PodmanManager handles sandbox lifecycle via Podman containers.
// Timer management and state machine logic live in SandboxRouter.
type PodmanManager struct {
	podman          *podman.Client
	dataDir         string // base directory for per-sandbox persistent data (workspace, sqlite)
	defaultMemoryMB int
	defaultCPU      int
	defaultDiskMB   int // per-sandbox disk quota (0 = no quota)
}

// ManagerOption configures a PodmanManager.
type ManagerOption func(*PodmanManager)

// WithDataDir sets the base directory for per-sandbox persistent data.
func WithDataDir(dir string) ManagerOption {
	return func(m *PodmanManager) { m.dataDir = dir }
}

// WithDefaultMemoryMB sets the default memory per sandbox (in MB).
func WithDefaultMemoryMB(mb int) ManagerOption {
	return func(m *PodmanManager) { m.defaultMemoryMB = mb }
}

// WithDefaultCPUs sets the default vCPU count per sandbox.
func WithDefaultCPUs(cpus int) ManagerOption {
	return func(m *PodmanManager) { m.defaultCPU = cpus }
}

// WithDefaultDiskMB sets the default disk quota per sandbox (in MB). 0 = no quota.
func WithDefaultDiskMB(mb int) ManagerOption {
	return func(m *PodmanManager) { m.defaultDiskMB = mb }
}

// NewManager creates a new sandbox manager.
func NewManager(client *podman.Client, opts ...ManagerOption) *PodmanManager {
	m := &PodmanManager{
		podman:          client,
		defaultMemoryMB: 1024,
		defaultCPU:      1,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// DataDir returns the base data directory for sandbox storage.
func (m *PodmanManager) DataDir() string {
	return m.dataDir
}

// Create creates a new sandbox container and starts it.
func (m *PodmanManager) Create(ctx context.Context, cfg types.SandboxConfig) (*types.Sandbox, error) {
	id := uuid.New().String()[:8]
	name := fmt.Sprintf("%s-%s", containerName, id)

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	image := defaultImage
	if cfg.ImageRef != "" {
		// DB-resolved ECR URI takes precedence
		image = cfg.ImageRef
	} else if cfg.Template != "" {
		image = resolveTemplateImage(cfg.Template)
	}

	memoryMB := cfg.MemoryMB
	if memoryMB <= 0 {
		memoryMB = m.defaultMemoryMB
	}
	cpuCount := cfg.CpuCount
	if cpuCount <= 0 {
		cpuCount = m.defaultCPU
	}

	now := time.Now()

	ccfg := podman.DefaultContainerConfig(name, image)
	ccfg.Labels[labelID] = id
	ccfg.Labels[labelTemplate] = cfg.Template
	ccfg.Labels[labelCreated] = now.Format(time.RFC3339)
	ccfg.Labels[labelTimeout] = strconv.Itoa(timeout)
	ccfg.Memory = fmt.Sprintf("%dm", memoryMB)
	ccfg.CPUs = fmt.Sprintf("%d", cpuCount)

	for k, v := range cfg.Envs {
		ccfg.Env[k] = v
	}

	// Always use bridge networking for subdomain proxy access.
	// Container port defaults to 80 but can be overridden (e.g., 3000, 8080).
	ccfg.NetworkMode = "bridge"

	containerPort := cfg.Port
	if containerPort <= 0 {
		containerPort = 80
	}

	hostPort, err := podman.FindFreePort()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate host port for sandbox %s: %w", id, err)
	}
	ccfg.Publish = []string{fmt.Sprintf("%d:%d/tcp", hostPort, containerPort)}
	ccfg.Labels[labelHostPort] = strconv.Itoa(hostPort)

	// Make /tmp writable for sandbox use (scratch space, not persisted)
	ccfg.TmpFS["/tmp"] = "rw,size=100m"

	// Workspace: bind-mount from host disk if dataDir is configured,
	// otherwise fall back to tmpfs (e.g., dev mode without NVMe).
	if m.dataDir != "" {
		workspaceDir := filepath.Join(m.dataDir, id, "workspace")
		if err := os.MkdirAll(workspaceDir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create workspace dir for sandbox %s: %w", id, err)
		}
		ccfg.Volumes = append(ccfg.Volumes, workspaceDir+":/workspace")
		// Enforce disk quota on the sandbox's data directory (best-effort, requires XFS + prjquota)
		if m.defaultDiskMB > 0 {
			m.SetDiskQuota(id, m.defaultDiskMB)
		}
	} else {
		ccfg.TmpFS["/workspace"] = "rw,size=200m"
	}

	if _, err := m.podman.CreateContainer(ctx, ccfg); err != nil {
		return nil, fmt.Errorf("failed to create sandbox %s: %w", id, err)
	}

	if err := m.podman.StartContainer(ctx, name); err != nil {
		// Clean up the created container on start failure
		_ = m.podman.RemoveContainer(ctx, name, true)
		return nil, fmt.Errorf("failed to start sandbox %s: %w", id, err)
	}

	sandbox := &types.Sandbox{
		ID:        id,
		Template:  cfg.Template,
		Alias:     cfg.Alias,
		Status:    types.SandboxStatusRunning,
		StartedAt: now,
		EndAt:     now.Add(time.Duration(timeout) * time.Second),
		Metadata:  cfg.Metadata,
		CpuCount:  cpuCount,
		MemoryMB:  memoryMB,
		HostPort:  hostPort,
	}

	return sandbox, nil
}

// Get returns sandbox info by ID.
func (m *PodmanManager) Get(ctx context.Context, id string) (*types.Sandbox, error) {
	name := fmt.Sprintf("%s-%s", containerName, id)
	info, err := m.podman.InspectContainer(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s not found: %w", id, err)
	}
	return containerInfoToSandbox(info), nil
}

// Kill forcefully removes a sandbox and cleans up its on-disk workspace.
func (m *PodmanManager) Kill(ctx context.Context, id string) error {
	name := fmt.Sprintf("%s-%s", containerName, id)
	if err := m.podman.RemoveContainer(ctx, name, true); err != nil {
		return fmt.Errorf("failed to kill sandbox %s: %w", id, err)
	}
	// Clean up persistent workspace directory (best-effort)
	if m.dataDir != "" {
		_ = os.RemoveAll(filepath.Join(m.dataDir, id))
	}
	return nil
}

// List returns all sandboxes.
func (m *PodmanManager) List(ctx context.Context) ([]types.Sandbox, error) {
	entries, err := m.podman.ListContainers(ctx, labelID)
	if err != nil {
		return nil, fmt.Errorf("failed to list sandboxes: %w", err)
	}

	sandboxes := make([]types.Sandbox, 0, len(entries))
	for _, e := range entries {
		sandboxes = append(sandboxes, psEntryToSandbox(e))
	}
	return sandboxes, nil
}

// Count returns the number of active sandbox containers.
func (m *PodmanManager) Count(ctx context.Context) (int, error) {
	entries, err := m.podman.ListContainers(ctx, labelID)
	if err != nil {
		return 0, fmt.Errorf("failed to count sandboxes: %w", err)
	}
	return len(entries), nil
}

// ContainerName returns the podman container name for a sandbox ID.
func (m *PodmanManager) ContainerName(id string) string {
	return fmt.Sprintf("%s-%s", containerName, id)
}

// HostPort returns the mapped host port for a sandbox's container port 80.
func (m *PodmanManager) HostPort(ctx context.Context, sandboxID string) (int, error) {
	name := m.ContainerName(sandboxID)
	info, err := m.podman.InspectContainer(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("sandbox %s not found: %w", sandboxID, err)
	}
	portStr := info.Config.Labels[labelHostPort]
	if portStr == "" {
		return 0, fmt.Errorf("sandbox %s has no host port mapping", sandboxID)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0, fmt.Errorf("invalid host port label for sandbox %s: %w", sandboxID, err)
	}
	return port, nil
}

// ContainerAddr returns the container's bridge IP and the requested port as "ip:port".
// Used by the proxy to route preview URL traffic directly to a specific container port.
func (m *PodmanManager) ContainerAddr(ctx context.Context, sandboxID string, port int) (string, error) {
	name := m.ContainerName(sandboxID)
	info, err := m.podman.InspectContainer(ctx, name)
	if err != nil {
		return "", fmt.Errorf("sandbox %s not found: %w", sandboxID, err)
	}
	// Find bridge IP from network settings
	for _, net := range info.NetworkSettings.Networks {
		if net.IPAddress != "" {
			return fmt.Sprintf("%s:%d", net.IPAddress, port), nil
		}
	}
	return "", fmt.Errorf("sandbox %s has no network IP", sandboxID)
}

// Stats returns live CPU/memory stats for a running sandbox.
func (m *PodmanManager) Stats(ctx context.Context, sandboxID string) (*SandboxStats, error) {
	name := m.ContainerName(sandboxID)
	cs, err := m.podman.ContainerStats(ctx, name)
	if err != nil {
		return nil, err
	}
	return &SandboxStats{
		CPUPercent: cs.CPUPercent,
		MemUsage:   cs.MemUsage,
		MemLimit:   cs.MemLimit,
		NetInput:   cs.NetInput,
		NetOutput:  cs.NetOutput,
		PIDs:       cs.PIDs,
	}, nil
}

// Close is a no-op — timer management now lives in SandboxRouter.
func (m *PodmanManager) Close() {}

func resolveTemplateImage(template string) string {
	switch template {
	case "base", "":
		return "docker.io/library/ubuntu:22.04"
	case "python":
		return "docker.io/library/python:3.12-slim"
	case "node":
		return "docker.io/library/node:20-slim"
	default:
		// Custom template: assume it's an image name tagged by the template system
		return fmt.Sprintf("localhost/opensandbox-template/%s:latest", template)
	}
}

func containerInfoToSandbox(info *podman.ContainerInfo) *types.Sandbox {
	status := types.SandboxStatusStopped
	if info.State.Running {
		status = types.SandboxStatusRunning
	}

	id := info.Config.Labels[labelID]

	startedAt, _ := time.Parse(time.RFC3339, info.Config.Labels[labelCreated])
	timeoutSec, _ := strconv.Atoi(info.Config.Labels[labelTimeout])
	endAt := startedAt.Add(time.Duration(timeoutSec) * time.Second)
	hostPort, _ := strconv.Atoi(info.Config.Labels[labelHostPort])

	return &types.Sandbox{
		ID:        id,
		Template:  info.Config.Labels[labelTemplate],
		Status:    status,
		StartedAt: startedAt,
		EndAt:     endAt,
		HostPort:  hostPort,
	}
}

func psEntryToSandbox(entry podman.PSEntry) types.Sandbox {
	status := types.SandboxStatusStopped
	if entry.State == "running" {
		status = types.SandboxStatusRunning
	}

	id := entry.Labels[labelID]

	startedAt, _ := time.Parse(time.RFC3339, entry.Labels[labelCreated])
	timeoutSec, _ := strconv.Atoi(entry.Labels[labelTimeout])
	endAt := startedAt.Add(time.Duration(timeoutSec) * time.Second)
	hostPort, _ := strconv.Atoi(entry.Labels[labelHostPort])

	return types.Sandbox{
		ID:        id,
		Template:  entry.Labels[labelTemplate],
		Status:    status,
		StartedAt: startedAt,
		EndAt:     endAt,
		HostPort:  hostPort,
	}
}
