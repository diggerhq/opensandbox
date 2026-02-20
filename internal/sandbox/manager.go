package sandbox

import (
	"context"
	"fmt"
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
	defaultMemoryMB = 512
	defaultCPU      = 1
)

// Manager handles sandbox lifecycle operations (pure container executor).
// Timer management and state machine logic live in SandboxRouter.
type Manager struct {
	podman *podman.Client
}

// NewManager creates a new sandbox manager.
func NewManager(client *podman.Client) *Manager {
	return &Manager{
		podman: client,
	}
}

// Create creates a new sandbox container and starts it.
func (m *Manager) Create(ctx context.Context, cfg types.SandboxConfig) (*types.Sandbox, error) {
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
		memoryMB = defaultMemoryMB
	}
	cpuCount := cfg.CpuCount
	if cpuCount <= 0 {
		cpuCount = defaultCPU
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
	// Containers get port 80 published to a random host port.
	ccfg.NetworkMode = "bridge"
	ccfg.CapAdd = []string{"NET_BIND_SERVICE"} // Allow binding to port 80 inside the container

	hostPort, err := podman.FindFreePort()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate host port for sandbox %s: %w", id, err)
	}
	ccfg.Publish = []string{fmt.Sprintf("%d:80/tcp", hostPort)}
	ccfg.Labels[labelHostPort] = strconv.Itoa(hostPort)

	// Make /tmp writable for sandbox use
	ccfg.TmpFS["/tmp"] = "rw,size=100m"
	// Add a writable home directory
	ccfg.TmpFS["/home/user"] = "rw,size=200m"

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
func (m *Manager) Get(ctx context.Context, id string) (*types.Sandbox, error) {
	name := fmt.Sprintf("%s-%s", containerName, id)
	info, err := m.podman.InspectContainer(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s not found: %w", id, err)
	}
	return containerInfoToSandbox(info), nil
}

// Kill forcefully removes a sandbox.
func (m *Manager) Kill(ctx context.Context, id string) error {
	name := fmt.Sprintf("%s-%s", containerName, id)
	if err := m.podman.RemoveContainer(ctx, name, true); err != nil {
		return fmt.Errorf("failed to kill sandbox %s: %w", id, err)
	}
	return nil
}

// List returns all sandboxes.
func (m *Manager) List(ctx context.Context) ([]types.Sandbox, error) {
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
func (m *Manager) Count(ctx context.Context) (int, error) {
	entries, err := m.podman.ListContainers(ctx, labelID)
	if err != nil {
		return 0, fmt.Errorf("failed to count sandboxes: %w", err)
	}
	return len(entries), nil
}

// ContainerName returns the podman container name for a sandbox ID.
func (m *Manager) ContainerName(id string) string {
	return fmt.Sprintf("%s-%s", containerName, id)
}

// HostPort returns the mapped host port for a sandbox's container port 80.
func (m *Manager) HostPort(ctx context.Context, sandboxID string) (int, error) {
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

// Close is a no-op — timer management now lives in SandboxRouter.
func (m *Manager) Close() {}

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
