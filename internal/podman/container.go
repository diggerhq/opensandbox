package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ContainerConfig defines how to create a container.
type ContainerConfig struct {
	Name           string
	Image          string
	Labels         map[string]string
	Env            map[string]string
	Memory         string // e.g. "512m"
	CPUs           string // e.g. "1"
	PidsLimit      int
	NetworkMode    string // "none", "slirp4netns", "bridge"
	ReadOnly       bool
	TmpFS          map[string]string // mount -> options
	CapDrop        []string
	SecurityOpts   []string
	UserNS         string
	Publish        []string // port mappings, e.g. ["12345:80/tcp"]
	CapAdd         []string
	Entrypoint     []string
	Command        []string
}

// DefaultContainerConfig returns a security-hardened container config.
func DefaultContainerConfig(name, image string) ContainerConfig {
	return ContainerConfig{
		Name:        name,
		Image:       image,
		Labels:      make(map[string]string),
		Env:         make(map[string]string),
		Memory:      "512m",
		CPUs:        "1",
		PidsLimit:   256,
		NetworkMode: "none",
		ReadOnly: false,
		TmpFS:   map[string]string{},
		CapDrop:      []string{"ALL"},
		SecurityOpts: []string{},
		UserNS:       "",
		Entrypoint:   []string{"/bin/sleep"},
		Command:      []string{"infinity"},
	}
}

// CreateContainer creates a container with the given config. Returns the container ID.
func (c *Client) CreateContainer(ctx context.Context, cfg ContainerConfig) (string, error) {
	args := []string{"create", "--name", cfg.Name}

	for k, v := range cfg.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}

	for k, v := range cfg.Env {
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}

	if cfg.Memory != "" {
		args = append(args, "--memory", cfg.Memory)
	}
	if cfg.CPUs != "" {
		args = append(args, "--cpus", cfg.CPUs)
	}
	if cfg.PidsLimit > 0 {
		args = append(args, "--pids-limit", fmt.Sprintf("%d", cfg.PidsLimit))
	}
	if cfg.NetworkMode != "" {
		args = append(args, "--network", cfg.NetworkMode)
	}
	if cfg.ReadOnly {
		args = append(args, "--read-only")
	}
	for mount, opts := range cfg.TmpFS {
		args = append(args, "--tmpfs", fmt.Sprintf("%s:%s", mount, opts))
	}
	for _, cap := range cfg.CapDrop {
		args = append(args, "--cap-drop", cap)
	}
	for _, opt := range cfg.SecurityOpts {
		args = append(args, "--security-opt", opt)
	}
	if cfg.UserNS != "" {
		args = append(args, "--userns", cfg.UserNS)
	}
	for _, pub := range cfg.Publish {
		args = append(args, "--publish", pub)
	}
	for _, cap := range cfg.CapAdd {
		args = append(args, "--cap-add", cap)
	}

	if len(cfg.Entrypoint) > 0 {
		args = append(args, "--entrypoint", cfg.Entrypoint[0])
	}

	args = append(args, cfg.Image)
	args = append(args, cfg.Command...)

	result, err := c.Run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("failed to create container %s: %w", cfg.Name, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("podman create failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	return strings.TrimSpace(result.Stdout), nil
}

// StartContainer starts a container by name or ID.
func (c *Client) StartContainer(ctx context.Context, nameOrID string) error {
	result, err := c.Run(ctx, "start", nameOrID)
	if err != nil {
		return fmt.Errorf("failed to start container %s: %w", nameOrID, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman start failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// StopContainer stops a container by name or ID.
func (c *Client) StopContainer(ctx context.Context, nameOrID string, timeoutSec int) error {
	args := []string{"stop"}
	if timeoutSec > 0 {
		args = append(args, "--time", fmt.Sprintf("%d", timeoutSec))
	}
	args = append(args, nameOrID)

	result, err := c.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("failed to stop container %s: %w", nameOrID, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman stop failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// RemoveContainer removes a container by name or ID. Force=true kills running containers.
func (c *Client) RemoveContainer(ctx context.Context, nameOrID string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "--force", "--time", "0")
	}
	args = append(args, nameOrID)

	result, err := c.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("failed to remove container %s: %w", nameOrID, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman rm failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// ContainerInfo holds inspect output for a container.
type ContainerInfo struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
}

// InspectContainer returns detailed info about a container.
func (c *Client) InspectContainer(ctx context.Context, nameOrID string) (*ContainerInfo, error) {
	var infos []ContainerInfo
	if err := c.RunJSON(ctx, &infos, "inspect", nameOrID); err != nil {
		return nil, fmt.Errorf("failed to inspect container %s: %w", nameOrID, err)
	}
	if len(infos) == 0 {
		return nil, fmt.Errorf("container %s not found", nameOrID)
	}
	return &infos[0], nil
}

// PSEntry represents a container from podman ps.
type PSEntry struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Image  string            `json:"Image"`
}

// ListContainers lists containers matching the given label filter.
func (c *Client) ListContainers(ctx context.Context, labelFilter string) ([]PSEntry, error) {
	args := []string{"ps", "-a", "--format", "json"}
	if labelFilter != "" {
		args = append(args, "--filter", fmt.Sprintf("label=%s", labelFilter))
	}

	result, err := c.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("podman ps failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return nil, nil
	}

	var entries []PSEntry
	if err := parseJSONOutput(output, &entries); err != nil {
		return nil, fmt.Errorf("failed to parse podman ps output: %w", err)
	}
	return entries, nil
}

// parseJSONOutput handles both JSON array and newline-delimited JSON.
func parseJSONOutput(output string, dest *[]PSEntry) error {
	output = strings.TrimSpace(output)
	if output == "" || output == "[]" {
		return nil
	}

	// Try array first (newer podman versions)
	if strings.HasPrefix(output, "[") {
		return json.Unmarshal([]byte(output), dest)
	}

	// Newline-delimited JSON (older podman versions)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry PSEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return err
		}
		*dest = append(*dest, entry)
	}
	return nil
}

// ContainerStats holds resource usage stats for a running container.
type ContainerStats struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemUsage   uint64  `json:"memUsage"` // bytes
	MemLimit   uint64  `json:"memLimit"` // bytes
	NetInput   uint64  `json:"netInput"` // bytes
	NetOutput  uint64  `json:"netOutput"`// bytes
	PIDs       int     `json:"pids"`
}

// podmanStatsEntry matches the JSON output of podman stats --format json.
// Note: podman outputs pids as a string (e.g. "1") and mem_usage includes both
// usage and limit combined (e.g. "303.1kB / 536.9MB").
type podmanStatsEntry struct {
	CPU      string `json:"cpu_percent"`
	MemUsage string `json:"mem_usage"` // "usage / limit" combined
	NetIO    string `json:"net_io"`
	PIDs     string `json:"pids"` // podman outputs as string
}

// ContainerStats returns live resource usage stats for a running container.
func (c *Client) ContainerStats(ctx context.Context, nameOrID string) (*ContainerStats, error) {
	result, err := c.Run(ctx, "stats", "--no-stream", "--no-reset", "--format", "json", nameOrID)
	if err != nil {
		return nil, fmt.Errorf("failed to get stats for %s: %w", nameOrID, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("podman stats failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}

	output := strings.TrimSpace(result.Stdout)
	if output == "" {
		return nil, fmt.Errorf("no stats output for %s", nameOrID)
	}

	var entries []podmanStatsEntry
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return nil, fmt.Errorf("failed to parse stats JSON: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no stats entries for %s", nameOrID)
	}

	e := entries[0]
	stats := &ContainerStats{}

	// Parse PIDs: podman outputs as string (e.g. "1")
	if v, err := strconv.Atoi(strings.TrimSpace(e.PIDs)); err == nil {
		stats.PIDs = v
	}

	// Parse CPU percent: "12.5%" -> 12.5
	cpu := strings.TrimSpace(strings.TrimSuffix(e.CPU, "%"))
	if v, err := strconv.ParseFloat(cpu, 64); err == nil {
		stats.CPUPercent = v
	}

	// Parse memory: podman combines usage and limit as "303.1kB / 536.9MB"
	if parts := strings.SplitN(e.MemUsage, "/", 2); len(parts) == 2 {
		stats.MemUsage = parseBytes(strings.TrimSpace(parts[0]))
		stats.MemLimit = parseBytes(strings.TrimSpace(parts[1]))
	} else {
		stats.MemUsage = parseBytes(e.MemUsage)
	}

	// Parse net I/O: "1.2kB / 3.4kB"
	if parts := strings.SplitN(e.NetIO, "/", 2); len(parts) == 2 {
		stats.NetInput = parseBytes(strings.TrimSpace(parts[0]))
		stats.NetOutput = parseBytes(strings.TrimSpace(parts[1]))
	}

	return stats, nil
}

// parseBytes converts human-readable byte strings like "45.2MB", "1.5GiB", "512kB" to bytes.
func parseBytes(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "--" {
		return 0
	}

	multipliers := map[string]float64{
		"B":   1,
		"kB":  1000,
		"KB":  1000,
		"MB":  1000 * 1000,
		"GB":  1000 * 1000 * 1000,
		"TB":  1000 * 1000 * 1000 * 1000,
		"KiB": 1024,
		"MiB": 1024 * 1024,
		"GiB": 1024 * 1024 * 1024,
		"TiB": 1024 * 1024 * 1024 * 1024,
	}

	for suffix, mult := range multipliers {
		if strings.HasSuffix(s, suffix) {
			numStr := strings.TrimSpace(strings.TrimSuffix(s, suffix))
			if v, err := strconv.ParseFloat(numStr, 64); err == nil {
				return uint64(v * mult)
			}
		}
	}

	// Try as plain number
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		return v
	}
	return 0
}

// PullImage pulls a container image.
func (c *Client) PullImage(ctx context.Context, image string) error {
	result, err := c.Run(ctx, "pull", image)
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", image, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman pull failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// ImageExists checks whether an image is available locally.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	result, err := c.Run(ctx, "image", "exists", image)
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}
