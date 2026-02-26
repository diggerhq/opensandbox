package sandbox

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/internal/podman"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// Exec runs a command inside a sandbox and returns the result.
func (m *PodmanManager) Exec(ctx context.Context, sandboxID string, cfg types.ProcessConfig) (*types.ProcessResult, error) {
	container := m.ContainerName(sandboxID)

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60
	}

	execCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	command := buildCommand(cfg.Command, cfg.Args)

	cwd := cfg.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}

	result, err := m.podman.ExecInContainer(execCtx, podman.ExecConfig{
		Container: container,
		Command:   command,
		Env:       cfg.Env,
		Cwd:       cwd,
	})
	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			return &types.ProcessResult{
				ExitCode: 124,
				Stderr:   fmt.Sprintf("command timed out after %ds", timeout),
			}, nil
		}
		return nil, fmt.Errorf("exec in sandbox %s failed: %w", sandboxID, err)
	}

	return &types.ProcessResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

func buildCommand(cmd string, args []string) []string {
	if len(args) > 0 {
		return append([]string{cmd}, args...)
	}
	// If no args, use shell to interpret the command string
	if strings.Contains(cmd, " ") || strings.Contains(cmd, "|") || strings.Contains(cmd, ";") {
		return []string{"/bin/sh", "-c", cmd}
	}
	return []string{cmd}
}
