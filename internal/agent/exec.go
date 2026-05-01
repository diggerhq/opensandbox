package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

const (
	sandboxHome = "/home/sandbox"
	sandboxUser = "sandbox"
)

// sandboxCredential returns the SysProcAttr.Credential for the sandbox user.
func sandboxCredential() *syscall.Credential {
	return &syscall.Credential{Uid: sandboxUID, Gid: sandboxGID}
}

// baseEnv returns the environment for sandbox user commands.
// HOME is set to /home/sandbox, USER to sandbox, and
// PATH is guaranteed to include /usr/local/bin and /usr/local/sbin.
func baseEnv() []string {
	var env []string
	hasPath := false
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "USER=") || strings.HasPrefix(e, "LOGNAME=") {
			continue
		}
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
			path := e[5:]
			// Ensure /usr/local/bin and /usr/local/sbin are in PATH
			if !strings.Contains(path, "/usr/local/bin") {
				path = "/usr/local/bin:/usr/local/sbin:" + path
			}
			env = append(env, "PATH="+path)
			continue
		}
		env = append(env, e)
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/bin:/sbin")
	}
	env = append(env, "HOME="+sandboxHome)
	env = append(env, "USER="+sandboxUser)
	env = append(env, "LOGNAME="+sandboxUser)
	return env
}

// SetEnvs stores sandbox-level environment variables that are injected into
// every subsequent Exec/ExecStream call. Safe to call multiple times; last write wins.
func (s *Server) SetEnvs(ctx context.Context, req *pb.SetEnvsRequest) (*pb.SetEnvsResponse, error) {
	envs := mapToEnv(req.Envs)
	s.envMu.Lock()
	s.sandboxEnvs = envs
	s.envMu.Unlock()
	return &pb.SetEnvsResponse{}, nil
}

// Exec runs a command synchronously and returns stdout/stderr/exit code.
func (s *Server) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)

	// Set working directory
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	} else {
		cmd.Dir = sandboxHome
	}

	// Build env: base < sandbox-level < per-command
	cmd.Env = baseEnv()
	s.envMu.RLock()
	if len(s.sandboxEnvs) > 0 {
		cmd.Env = append(cmd.Env, s.sandboxEnvs...)
	}
	s.envMu.RUnlock()
	if len(req.Envs) > 0 {
		cmd.Env = append(cmd.Env, mapToEnv(req.Envs)...)
	}

	// Run in its own process group; use sandbox user unless run_as_root is set
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !req.RunAsRoot {
		cmd.SysProcAttr.Credential = sandboxCredential()
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Start + move to sandbox cgroup + wait (instead of Run)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("exec start: %w", err)
	}
	pid := cmd.Process.Pid
	// Register with reaper BEFORE the child can exit. If the SIGCHLD reaper
	// races us and reaps the child first, cmd.Wait() returns ECHILD; we
	// recover the status from this channel. See reap_registry.go.
	exitCh := RegisterReap(pid)
	moveToCgroup(pid)

	// Wait with cgroup kill on timeout — if the process doesn't exit when the
	// context deadline fires, kill the entire sandbox cgroup to clean up fork
	// bombs and runaway children.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var err error
	select {
	case err = <-waitDone:
		// Normal completion
	case <-ctx.Done():
		// Timeout — kill process group first, then nuke the cgroup
		if cmd.Process != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		killCgroup()
		<-waitDone // wait for cmd.Wait to return after kills
		UnregisterReap(pid)
		return &pb.ExecResponse{
			ExitCode: -1,
			Stdout:   stdout.String(),
			Stderr:   stderr.String() + "\nProcess timed out",
		}, nil
	}

	exitCode := 0
	if err != nil {
		switch {
		case isExitError(err):
			exitCode = err.(*exec.ExitError).ExitCode()
			UnregisterReap(pid)
		case errors.Is(err, syscall.ECHILD):
			// Reaper got there first; pull WaitStatus from registry channel.
			// The reaper has already populated it (otherwise wait4 would
			// have returned the status to cmd.Wait, not ECHILD).
			ws := <-exitCh
			exitCode = ws.ExitStatus()
		default:
			UnregisterReap(pid)
			return nil, fmt.Errorf("exec failed: %w", err)
		}
	} else {
		UnregisterReap(pid)
	}

	return &pb.ExecResponse{
		ExitCode: int32(exitCode),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func isExitError(err error) bool {
	_, ok := err.(*exec.ExitError)
	return ok
}

// ExecStream runs a command and streams stdout/stderr chunks.
func (s *Server) ExecStream(req *pb.ExecRequest, stream pb.SandboxAgent_ExecStreamServer) error {
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	ctx, cancel := context.WithTimeout(stream.Context(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)

	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	} else {
		cmd.Dir = sandboxHome
	}

	// Build env: base < sandbox-level < per-command
	cmd.Env = baseEnv()
	s.envMu.RLock()
	if len(s.sandboxEnvs) > 0 {
		cmd.Env = append(cmd.Env, s.sandboxEnvs...)
	}
	s.envMu.RUnlock()
	if len(req.Envs) > 0 {
		cmd.Env = append(cmd.Env, mapToEnv(req.Envs)...)
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if !req.RunAsRoot {
		cmd.SysProcAttr.Credential = sandboxCredential()
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	moveToCgroup(cmd.Process.Pid)

	// Stream stdout and stderr in parallel
	errCh := make(chan error, 2)

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&pb.ExecOutputChunk{
					Stream: pb.ExecOutputChunk_STDOUT,
					Data:   buf[:n],
				}); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if err != nil {
				errCh <- nil
				return
			}
		}
	}()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&pb.ExecOutputChunk{
					Stream: pb.ExecOutputChunk_STDERR,
					Data:   buf[:n],
				}); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if err != nil {
				errCh <- nil
				return
			}
		}
	}()

	// Wait for both pipes to close
	<-errCh
	<-errCh

	// Wait for command to finish
	_ = cmd.Wait()

	return nil
}

// mapToEnv converts a map to KEY=VALUE slice.
func mapToEnv(m map[string]string) []string {
	env := make([]string, 0, len(m))
	for k, v := range m {
		env = append(env, k+"="+v)
	}
	return env
}

const sandboxCgroupProcs = "/sys/fs/cgroup/sandbox/cgroup.procs"

// sandboxUID/GID for running user commands as non-root.
// Prevents users from modifying cgroup limits or kernel interfaces.
const (
	sandboxUID = 1000
	sandboxGID = 1000
)

// moveToCgroup moves a process into the sandbox cgroup.
// This ensures user commands are subject to resource limits (pids, memory, cpu)
// while the agent (PID 1) stays in the root cgroup, protected from exhaustion.
func moveToCgroup(pid int) {
	_ = os.WriteFile(sandboxCgroupProcs, []byte(fmt.Sprintf("%d", pid)), 0644)
}

// killCgroup kills all processes in the sandbox cgroup.
// Used on exec timeout to clean up fork bombs and runaway children.
func killCgroup() {
	// cgroup.kill is the fast path (kernel 5.14+) — kills all processes atomically
	if err := os.WriteFile("/sys/fs/cgroup/sandbox/cgroup.kill", []byte("1"), 0644); err == nil {
		return
	}
	// Fallback: read all PIDs and kill them individually
	data, err := os.ReadFile(sandboxCgroupProcs)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(line, "%d", &pid); err == nil && pid > 1 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}

	// Brief wait, then check for survivors (e.g., D-state processes immune to SIGKILL)
	time.Sleep(100 * time.Millisecond)
	if remaining, err := os.ReadFile(sandboxCgroupProcs); err == nil {
		lines := strings.TrimSpace(string(remaining))
		if lines != "" {
			fmt.Fprintf(os.Stderr, "agent: WARNING: %d PIDs remain in sandbox cgroup after SIGKILL: %s\n",
				len(strings.Split(lines, "\n")), lines)
		}
	}
}

// SetResourceLimits adjusts the sandbox cgroup limits at runtime.
// Called by the worker when a sandbox scales up/down.
func (s *Server) SetResourceLimits(ctx context.Context, req *pb.SetResourceLimitsRequest) (*pb.SetResourceLimitsResponse, error) {
	const cgroupDir = "/sys/fs/cgroup/sandbox"

	if req.MaxPids > 0 {
		if err := os.WriteFile(cgroupDir+"/pids.max", []byte(fmt.Sprintf("%d", req.MaxPids)), 0644); err != nil {
			return nil, fmt.Errorf("set pids.max: %w", err)
		}
	}
	if req.MaxMemoryBytes > 0 {
		if err := os.WriteFile(cgroupDir+"/memory.max", []byte(fmt.Sprintf("%d", req.MaxMemoryBytes)), 0644); err != nil {
			return nil, fmt.Errorf("set memory.max: %w", err)
		}
	}
	if req.CpuMaxUsec > 0 {
		// cpu.max format: "$MAX $PERIOD" — e.g., "100000 100000" = 1 vCPU
		period := int64(100000)
		if req.CpuPeriodUsec > 0 {
			period = req.CpuPeriodUsec
		}
		val := fmt.Sprintf("%d %d", req.CpuMaxUsec, period)
		if err := os.WriteFile(cgroupDir+"/cpu.max", []byte(val), 0644); err != nil {
			return nil, fmt.Errorf("set cpu.max: %w", err)
		}
	}

	return &pb.SetResourceLimitsResponse{}, nil
}
