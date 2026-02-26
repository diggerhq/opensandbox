package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	ptylib "github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// PTYManager manages PTY sessions.
// Supports both Podman (host-side PTY via podman exec) and Firecracker
// (agent gRPC + vsock data port) by allowing the session creation function
// to be overridden.
type PTYManager struct {
	podmanPath string
	authFile   string
	mu         sync.RWMutex
	sessions   map[string]*PTYSessionHandle

	// createFunc allows overriding session creation for Firecracker mode.
	// If nil, defaults to Podman-based PTY via podman exec.
	createFunc func(sandboxID string, req types.PTYCreateRequest) (*PTYSessionHandle, error)
}

// PTYSessionHandle holds the state for an active PTY session.
type PTYSessionHandle struct {
	ID        string
	SandboxID string
	Cmd       *exec.Cmd          // Podman mode only (nil for Firecracker)
	PTY       io.ReadWriteCloser // PTY I/O stream (*os.File for Podman, net.Conn for Firecracker)
	Done      chan struct{}

	// onKill is called when the session is killed (Firecracker: sends gRPC PTYKill).
	onKill func()
	// onResize is called when the session is resized (Firecracker: sends gRPC PTYResize).
	onResize func(cols, rows int) error
}

// NewPTYManager creates a new Podman-based PTY manager.
func NewPTYManager(podmanPath, authFile string) *PTYManager {
	return &PTYManager{
		podmanPath: podmanPath,
		authFile:   authFile,
		sessions:   make(map[string]*PTYSessionHandle),
	}
}

// NewAgentPTYManager creates a PTY manager that delegates to a custom
// create function (used by Firecracker mode).
func NewAgentPTYManager(createFunc func(sandboxID string, req types.PTYCreateRequest) (*PTYSessionHandle, error)) *PTYManager {
	return &PTYManager{
		sessions:   make(map[string]*PTYSessionHandle),
		createFunc: createFunc,
	}
}

// CreateSession starts a new PTY session inside a sandbox.
func (pm *PTYManager) CreateSession(sandboxID string, req types.PTYCreateRequest) (*PTYSessionHandle, error) {
	// Use override if set (Firecracker mode)
	if pm.createFunc != nil {
		handle, err := pm.createFunc(sandboxID, req)
		if err != nil {
			return nil, err
		}
		pm.mu.Lock()
		pm.sessions[handle.ID] = handle
		pm.mu.Unlock()
		return handle, nil
	}

	// Default: Podman-based PTY
	return pm.createPodmanSession(sandboxID, req)
}

// createPodmanSession creates a PTY session using podman exec.
func (pm *PTYManager) createPodmanSession(sandboxID string, req types.PTYCreateRequest) (*PTYSessionHandle, error) {
	sessionID := uuid.New().String()[:8]
	containerName := fmt.Sprintf("osb-%s", sandboxID)

	shell := req.Shell
	if shell == "" {
		shell = "/bin/bash"
	}

	cols := req.Cols
	if cols <= 0 {
		cols = 80
	}
	rows := req.Rows
	if rows <= 0 {
		rows = 24
	}

	args := []string{
		"exec", "-it",
		"--env", "TERM=xterm-256color",
		"--workdir", "/workspace",
		containerName,
		shell,
	}

	cmd := exec.Command(pm.podmanPath, args...)
	cmd.Env = append(os.Environ(), "REGISTRY_AUTH_FILE="+pm.authFile)

	// Start with a real pseudo-terminal
	ptmx, err := ptylib.StartWithSize(cmd, &ptylib.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start PTY session: %w", err)
	}

	handle := &PTYSessionHandle{
		ID:        sessionID,
		SandboxID: sandboxID,
		Cmd:       cmd,
		PTY:       ptmx,
		Done:      make(chan struct{}),
	}

	go func() {
		_ = cmd.Wait()
		close(handle.Done)
	}()

	pm.mu.Lock()
	pm.sessions[sessionID] = handle
	pm.mu.Unlock()

	return handle, nil
}

// Resize changes the terminal size for a PTY session.
func (pm *PTYManager) Resize(sessionID string, cols, rows int) error {
	pm.mu.RLock()
	session, ok := pm.sessions[sessionID]
	pm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("PTY session %s not found", sessionID)
	}

	// Use Firecracker resize if available
	if session.onResize != nil {
		return session.onResize(cols, rows)
	}

	// Podman mode: resize via pty ioctl
	if f, ok := session.PTY.(*os.File); ok {
		return ptylib.Setsize(f, &ptylib.Winsize{
			Rows: uint16(rows),
			Cols: uint16(cols),
		})
	}
	return fmt.Errorf("resize not supported for this session type")
}

// GetSession returns a PTY session by ID.
func (pm *PTYManager) GetSession(sessionID string) (*PTYSessionHandle, error) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	session, ok := pm.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("PTY session %s not found", sessionID)
	}
	return session, nil
}

// KillSession terminates a PTY session.
func (pm *PTYManager) KillSession(sessionID string) error {
	pm.mu.Lock()
	session, ok := pm.sessions[sessionID]
	if ok {
		delete(pm.sessions, sessionID)
	}
	pm.mu.Unlock()

	if !ok {
		return fmt.Errorf("PTY session %s not found", sessionID)
	}

	// Call Firecracker kill callback if set
	if session.onKill != nil {
		session.onKill()
	}

	session.PTY.Close()
	if session.Cmd != nil && session.Cmd.Process != nil {
		_ = session.Cmd.Process.Kill()
	}
	return nil
}

// CloseAll terminates all PTY sessions.
func (pm *PTYManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, session := range pm.sessions {
		if session.onKill != nil {
			session.onKill()
		}
		session.PTY.Close()
		if session.Cmd != nil && session.Cmd.Process != nil {
			_ = session.Cmd.Process.Kill()
		}
	}
	pm.sessions = make(map[string]*PTYSessionHandle)
}
