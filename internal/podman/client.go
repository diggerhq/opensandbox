package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Client wraps the podman CLI for image build operations (template building).
// Podman is used purely as a build tool (like Docker) to create ext4 rootfs images
// from Dockerfiles. It is NOT used as a container runtime — Firecracker handles that.
type Client struct {
	binaryPath string
	authFile   string // dedicated auth file to avoid Docker credential helper conflicts
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

	return &Client{binaryPath: path, authFile: authFile}, nil
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
