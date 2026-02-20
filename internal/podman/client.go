package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Client wraps the podman CLI for container operations.
type Client struct {
	binaryPath string
	authFile   string // dedicated auth file to avoid Docker credential helper conflicts
	useSSH     bool   // true when checkpoint/restore must run inside a Podman machine VM (macOS)
}

// NewClient creates a new Podman client. It verifies podman is available.
func NewClient() (*Client, error) {
	path, err := exec.LookPath("podman")
	if err != nil {
		return nil, fmt.Errorf("podman not found in PATH: %w", err)
	}

	// Create a dedicated auth file so Podman doesn't inherit Docker Desktop's
	// credential helpers (docker-credential-gcloud, docker-credential-desktop, etc.)
	authFile, err := ensureAuthFile()
	if err != nil {
		return nil, fmt.Errorf("failed to set up podman auth: %w", err)
	}

	client := &Client{binaryPath: path, authFile: authFile}

	// Detect if we're on macOS (remote podman). Checkpoint/restore requires
	// running inside the VM via "podman machine ssh".
	if runtime.GOOS == "darwin" {
		client.useSSH = true
	}

	return client, nil
}

// AuthFile returns the path to the dedicated auth file.
func (c *Client) AuthFile() string {
	return c.authFile
}

func ensureAuthFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "opensandbox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	authFile := filepath.Join(dir, "auth.json")
	if _, err := os.Stat(authFile); os.IsNotExist(err) {
		if err := os.WriteFile(authFile, []byte(`{"auths":{}}`), 0600); err != nil {
			return "", err
		}
	}
	return authFile, nil
}

// ExecResult holds the output from a podman command.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Run executes a podman command and returns the result.
func (c *Client) Run(ctx context.Context, args ...string) (*ExecResult, error) {
	cmd := exec.CommandContext(ctx, c.binaryPath, args...)
	cmd.Env = append(os.Environ(), "REGISTRY_AUTH_FILE="+c.authFile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("podman exec failed: %w", err)
	}

	return result, nil
}

// RunJSON executes a podman command and parses JSON output into dest.
func (c *Client) RunJSON(ctx context.Context, dest interface{}, args ...string) error {
	result, err := c.Run(ctx, args...)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman %s failed (exit %d): %s",
			strings.Join(args, " "), result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	if err := json.Unmarshal([]byte(result.Stdout), dest); err != nil {
		return fmt.Errorf("failed to parse podman output: %w", err)
	}
	return nil
}

// runSSH executes a command inside the Podman machine VM via "podman machine ssh".
func (c *Client) runSSH(ctx context.Context, sshCmd string) (*ExecResult, error) {
	cmd := exec.CommandContext(ctx, c.binaryPath, "machine", "ssh", sshCmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := &ExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("podman machine ssh failed: %w", err)
	}

	return result, nil
}

// CheckpointContainer checkpoints a running container and exports to a tar archive.
// Uses zstd compression for optimized checkpoint size.
// On macOS, checkpoint runs inside the Podman machine VM via SSH because
// the remote podman client doesn't support CRIU checkpoint operations.
func (c *Client) CheckpointContainer(ctx context.Context, nameOrID, exportPath string) error {
	if c.useSSH {
		// exportPath is a VM-local path (e.g. /tmp/...) since we'll copy it out separately
		sshCmd := fmt.Sprintf("podman container checkpoint --tcp-established --export %s --compress zstd %s", exportPath, nameOrID)
		result, err := c.runSSH(ctx, sshCmd)
		if err != nil {
			return fmt.Errorf("failed to checkpoint container %s: %w", nameOrID, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("podman checkpoint failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
		}
		return nil
	}

	result, err := c.Run(ctx, "container", "checkpoint", "--tcp-established", "--export", exportPath, "--compress", "zstd", nameOrID)
	if err != nil {
		return fmt.Errorf("failed to checkpoint container %s: %w", nameOrID, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman checkpoint failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// RestoreContainer restores a container from a checkpoint archive on disk.
// On macOS, restore runs inside the Podman machine VM via SSH.
// It force-removes any existing container with the same name first to avoid
// "that ID is already in use" errors from previous restore cycles.
func (c *Client) RestoreContainer(ctx context.Context, importPath, name string) error {
	// Pre-cleanup: remove any existing container with this name to avoid ID conflicts.
	// This is best-effort — the container may not exist, which is fine.
	_ = c.RemoveContainer(ctx, name, true)

	if c.useSSH {
		sshCmd := fmt.Sprintf("podman container restore --tcp-established --import %s", importPath)
		result, err := c.runSSH(ctx, sshCmd)
		if err != nil {
			return fmt.Errorf("failed to restore container %s: %w", name, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("podman restore failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
		}
		return nil
	}

	result, err := c.Run(ctx, "container", "restore", "--tcp-established", "--import", importPath)
	if err != nil {
		return fmt.Errorf("failed to restore container %s: %w", name, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman restore failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// CopyFromVM copies a file from the Podman machine VM to the local host (macOS only).
func (c *Client) CopyFromVM(ctx context.Context, vmPath, localPath string) error {
	// podman machine ssh "cat <vmPath>" > localPath
	cmd := exec.CommandContext(ctx, c.binaryPath, "machine", "ssh", "cat "+vmPath)
	outFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file %s: %w", localPath, err)
	}
	defer outFile.Close()
	cmd.Stdout = outFile
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy from VM: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CopyToVM copies a file from the local host to the Podman machine VM (macOS only).
func (c *Client) CopyToVM(ctx context.Context, localPath, vmPath string) error {
	// cat localPath | podman machine ssh "cat > <vmPath>"
	inFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file %s: %w", localPath, err)
	}
	defer inFile.Close()
	cmd := exec.CommandContext(ctx, c.binaryPath, "machine", "ssh", "cat > "+vmPath)
	cmd.Stdin = inFile
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to copy to VM: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RemoveVMFile removes a file inside the Podman machine VM (macOS only).
func (c *Client) RemoveVMFile(ctx context.Context, vmPath string) error {
	_, err := c.runSSH(ctx, "rm -f "+vmPath)
	return err
}

// UseSSH returns whether the client routes checkpoint/restore through VM SSH.
func (c *Client) UseSSH() bool {
	return c.useSSH
}

// RestoreContainerFromStream restores a container by piping a checkpoint archive
// from an io.Reader (e.g., S3 download body) into podman via a named FIFO pipe.
// This avoids writing the full checkpoint to local disk.
// On macOS, streams through the VM: write to local temp file, copy to VM, restore from VM path.
func (c *Client) RestoreContainerFromStream(ctx context.Context, reader io.Reader, name string) error {
	if c.useSSH {
		// On macOS, we can't use FIFOs across the VM boundary.
		// Write to local temp, copy into VM, restore from VM path.
		tmpFile, err := os.CreateTemp("", "osb-restore-*.tar.zst")
		if err != nil {
			return fmt.Errorf("failed to create temp file: %w", err)
		}
		localPath := tmpFile.Name()
		defer os.Remove(localPath)

		if _, err := io.Copy(tmpFile, reader); err != nil {
			tmpFile.Close()
			return fmt.Errorf("failed to write checkpoint to temp file: %w", err)
		}
		tmpFile.Close()

		vmPath := "/tmp/osb-restore-" + filepath.Base(localPath)
		if err := c.CopyToVM(ctx, localPath, vmPath); err != nil {
			return fmt.Errorf("failed to copy checkpoint to VM: %w", err)
		}
		defer c.RemoveVMFile(ctx, vmPath)

		return c.RestoreContainer(ctx, vmPath, name)
	}

	// Download to temp file, then restore from file
	tmpFile, err := os.CreateTemp("", "osb-restore-*.tar.zst")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	localPath := tmpFile.Name()
	defer os.Remove(localPath)

	if _, err := io.Copy(tmpFile, reader); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write checkpoint to temp file: %w", err)
	}
	tmpFile.Close()

	return c.RestoreContainer(ctx, localPath, name)
}

// FindFreePort asks the OS for a free TCP port by listening on :0.
func FindFreePort() (int, error) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		return 0, fmt.Errorf("failed to find free port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// TagImage tags a local image with a new name/reference.
func (c *Client) TagImage(ctx context.Context, source, target string) error {
	result, err := c.Run(ctx, "tag", source, target)
	if err != nil {
		return fmt.Errorf("failed to tag image: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman tag failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// PushImage pushes an image to a remote registry.
func (c *Client) PushImage(ctx context.Context, imageRef string) error {
	result, err := c.Run(ctx, "push", imageRef)
	if err != nil {
		return fmt.Errorf("failed to push image %s: %w", imageRef, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman push failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// LoginRegistry authenticates podman to a container registry.
func (c *Client) LoginRegistry(ctx context.Context, registry, username, password string) error {
	result, err := c.Run(ctx, "login", "--username", username, "--password", password, registry)
	if err != nil {
		return fmt.Errorf("failed to login to registry %s: %w", registry, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman login failed (exit %d): %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// Version returns the podman version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	result, err := c.Run(ctx, "version", "--format", "{{.Client.Version}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}
