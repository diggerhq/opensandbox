package sandbox

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// ReadFile reads a file from inside the sandbox.
func (m *PodmanManager) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	container := m.ContainerName(sandboxID)
	content, err := m.podman.ExecSimple(ctx, container, "cat", path)
	if err != nil {
		return "", fmt.Errorf("failed to read %s in sandbox %s: %w", path, sandboxID, err)
	}
	return content, nil
}

// WriteFile writes content to a file inside the sandbox.
func (m *PodmanManager) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	container := m.ContainerName(sandboxID)

	// Ensure parent directory exists
	dir := path[:strings.LastIndex(path, "/")]
	if dir != "" {
		_, _ = m.podman.ExecSimple(ctx, container, "mkdir", "-p", dir)
	}

	result, err := m.podman.ExecInContainer(ctx, podman.ExecConfig{
		Container: container,
		Command:   []string{"/bin/sh", "-c", fmt.Sprintf("cat > %s", shellQuote(path))},
		Stdin:     strings.NewReader(content),
	})
	if err != nil {
		return fmt.Errorf("failed to write %s in sandbox %s: %w", path, sandboxID, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write to %s failed: %s", path, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// ListDir lists directory contents inside the sandbox.
func (m *PodmanManager) ListDir(ctx context.Context, sandboxID, path string) ([]types.EntryInfo, error) {
	container := m.ContainerName(sandboxID)

	// Use a script that outputs machine-parseable format
	script := fmt.Sprintf(
		`find %s -maxdepth 1 -mindepth 1 -printf '%%y\t%%s\t%%f\n' 2>/dev/null || ls -1a %s`,
		shellQuote(path), shellQuote(path),
	)
	output, err := m.podman.ExecSimple(ctx, container, "/bin/sh", "-c", script)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s in sandbox %s: %w", path, sandboxID, err)
	}

	var entries []types.EntryInfo
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 3)
		if len(parts) == 3 {
			// find -printf output: type\tsize\tname
			isDir := parts[0] == "d"
			size, _ := strconv.ParseInt(parts[1], 10, 64)
			name := parts[2]
			entryPath := path + "/" + name
			if strings.HasSuffix(path, "/") {
				entryPath = path + name
			}
			entries = append(entries, types.EntryInfo{
				Name:  name,
				IsDir: isDir,
				Size:  size,
				Path:  entryPath,
			})
		} else {
			// Fallback: ls output (just names)
			name := strings.TrimSpace(line)
			if name == "." || name == ".." {
				continue
			}
			entries = append(entries, types.EntryInfo{
				Name: name,
				Path: path + "/" + name,
			})
		}
	}
	return entries, nil
}

// MakeDir creates a directory inside the sandbox.
func (m *PodmanManager) MakeDir(ctx context.Context, sandboxID, path string) error {
	container := m.ContainerName(sandboxID)
	_, err := m.podman.ExecSimple(ctx, container, "mkdir", "-p", path)
	if err != nil {
		return fmt.Errorf("failed to mkdir %s in sandbox %s: %w", path, sandboxID, err)
	}
	return nil
}

// Remove removes a file or directory inside the sandbox.
func (m *PodmanManager) Remove(ctx context.Context, sandboxID, path string) error {
	container := m.ContainerName(sandboxID)
	_, err := m.podman.ExecSimple(ctx, container, "rm", "-rf", path)
	if err != nil {
		return fmt.Errorf("failed to remove %s in sandbox %s: %w", path, sandboxID, err)
	}
	return nil
}

// Exists checks if a path exists inside the sandbox.
func (m *PodmanManager) Exists(ctx context.Context, sandboxID, path string) (bool, error) {
	container := m.ContainerName(sandboxID)
	result, err := m.podman.ExecInContainer(ctx, podman.ExecConfig{
		Container: container,
		Command:   []string{"test", "-e", path},
	})
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

// Stat returns file info for a path inside the sandbox.
func (m *PodmanManager) Stat(ctx context.Context, sandboxID, path string) (*types.FileInfo, error) {
	container := m.ContainerName(sandboxID)
	output, err := m.podman.ExecSimple(ctx, container,
		"stat", "--format", "%n\t%F\t%s\t%a\t%Y", path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s in sandbox %s: %w", path, sandboxID, err)
	}

	parts := strings.SplitN(strings.TrimSpace(output), "\t", 5)
	if len(parts) < 5 {
		return nil, fmt.Errorf("unexpected stat output for %s", path)
	}

	size, _ := strconv.ParseInt(parts[2], 10, 64)
	isDir := strings.Contains(parts[1], "directory")

	return &types.FileInfo{
		Name:    parts[0],
		IsDir:   isDir,
		Size:    size,
		Mode:    parts[3],
		ModTime: parts[4],
		Path:    path,
	}, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
