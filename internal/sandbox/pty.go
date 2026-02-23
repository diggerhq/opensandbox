package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"sync"

	ptylib "github.com/creack/pty"
	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// PTYManager manages PTY sessions.
type PTYManager struct {
	podmanPath string
	authFile   string
	mu         sync.RWMutex
	sessions   map[string]*PTYSessionHandle
}

// PTYSessionHandle holds the state for an active PTY session.
type PTYSessionHandle struct {
	ID        string
	SandboxID string
	Cmd       *exec.Cmd
	PTY       *os.File // master side of the pseudo-terminal (read + write)
	Done      chan struct{}
}

// NewPTYManager creates a new PTY manager.
func NewPTYManager(podmanPath, authFile string) *PTYManager {
	return &PTYManager{
		podmanPath: podmanPath,
		authFile:   authFile,
		sessions:   make(map[string]*PTYSessionHandle),
	}
}

// CreateSession starts a new PTY session inside a sandbox.
func (pm *PTYManager) CreateSession(sandboxID string, req types.PTYCreateRequest) (*PTYSessionHandle, error) {
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

	return ptylib.Setsize(session.PTY, &ptylib.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
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

	session.PTY.Close()
	if session.Cmd.Process != nil {
		_ = session.Cmd.Process.Kill()
	}
	return nil
}

// CloseAll terminates all PTY sessions.
func (pm *PTYManager) CloseAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, session := range pm.sessions {
		session.PTY.Close()
		if session.Cmd.Process != nil {
			_ = session.Cmd.Process.Kill()
		}
	}
	pm.sessions = make(map[string]*PTYSessionHandle)
}
