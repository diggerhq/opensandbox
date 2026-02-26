package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/google/uuid"
	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// ptySession holds a live PTY session inside the VM.
type ptySession struct {
	id       string
	cmd      *exec.Cmd
	ptyFile  *os.File
	dataPort uint32
	cancel   context.CancelFunc
}

// PTYCreate starts a new PTY session and returns the vsock data port.
// The caller connects to that vsock port for raw I/O.
func (s *Server) PTYCreate(ctx context.Context, req *pb.PTYCreateRequest) (*pb.PTYCreateResponse, error) {
	shell := req.Shell
	if shell == "" {
		// Try common shells
		for _, sh := range []string{"/bin/bash", "/bin/sh"} {
			if _, err := os.Stat(sh); err == nil {
				shell = sh
				break
			}
		}
		if shell == "" {
			return nil, fmt.Errorf("no shell found")
		}
	}

	sessionID := uuid.New().String()[:8]

	s.ptyMu.Lock()
	port := s.nextPTYPort
	s.nextPTYPort++
	s.ptyMu.Unlock()

	sessCtx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(sessCtx, shell)
	cmd.Dir = "/workspace"
	cmd.Env = append(baseEnv(),
		"TERM=xterm-256color",
		fmt.Sprintf("COLUMNS=%d", req.Cols),
		fmt.Sprintf("LINES=%d", req.Rows),
	)

	cols := uint16(req.Cols)
	rows := uint16(req.Rows)
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start pty: %w", err)
	}

	sess := &ptySession{
		id:       sessionID,
		cmd:      cmd,
		ptyFile:  ptmx,
		dataPort: port,
		cancel:   cancel,
	}

	s.ptyMu.Lock()
	s.ptySessions[sessionID] = sess
	s.ptyMu.Unlock()

	// Start vsock listener for PTY data on the allocated port.
	// We must ensure the listener is ready before returning the port to the
	// caller, otherwise the host may CONNECT before we're listening.
	lisReady := make(chan error, 1)
	go s.servePTYData(sess, sessCtx, lisReady)

	if err := <-lisReady; err != nil {
		cancel()
		s.ptyMu.Lock()
		delete(s.ptySessions, sessionID)
		s.ptyMu.Unlock()
		return nil, fmt.Errorf("pty data listen: %w", err)
	}

	return &pb.PTYCreateResponse{
		SessionId: sessionID,
		DataPort:  port,
	}, nil
}

// servePTYData listens on a vsock port and bridges I/O to/from the PTY.
// Uses native AF_VSOCK so the host can connect via Firecracker's vsock UDS.
// The ready channel signals when the listener is up (nil) or failed (error).
func (s *Server) servePTYData(sess *ptySession, ctx context.Context, ready chan<- error) {
	if s.ListenPort == nil {
		ready <- fmt.Errorf("no ListenPort function set")
		return
	}

	lis, err := s.ListenPort(sess.dataPort)
	if err != nil {
		ready <- fmt.Errorf("listen port %d: %w", sess.dataPort, err)
		return
	}
	defer lis.Close()

	// Signal that we're ready to accept connections
	ready <- nil

	// Accept at most one connection per PTY session
	go func() {
		<-ctx.Done()
		lis.Close()
	}()

	conn, err := lis.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Bidirectional copy: conn ↔ ptmx
	var wg sync.WaitGroup
	wg.Add(2)

	// PTY → client
	go func() {
		defer wg.Done()
		io.Copy(conn, sess.ptyFile)
	}()

	// Client → PTY
	go func() {
		defer wg.Done()
		io.Copy(sess.ptyFile, conn)
	}()

	// Wait for command to exit
	sess.cmd.Wait()
	sess.ptyFile.Close()
	wg.Wait()

	// Clean up session
	s.ptyMu.Lock()
	delete(s.ptySessions, sess.id)
	s.ptyMu.Unlock()
}

// PTYResize changes the terminal size for an active PTY session.
func (s *Server) PTYResize(ctx context.Context, req *pb.PTYResizeRequest) (*pb.PTYResizeResponse, error) {
	s.ptyMu.Lock()
	sess, ok := s.ptySessions[req.SessionId]
	s.ptyMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("pty session %s not found", req.SessionId)
	}

	if err := pty.Setsize(sess.ptyFile, &pty.Winsize{
		Cols: uint16(req.Cols),
		Rows: uint16(req.Rows),
	}); err != nil {
		return nil, fmt.Errorf("resize: %w", err)
	}
	return &pb.PTYResizeResponse{}, nil
}

// PTYKill terminates a PTY session.
func (s *Server) PTYKill(ctx context.Context, req *pb.PTYKillRequest) (*pb.PTYKillResponse, error) {
	s.ptyMu.Lock()
	sess, ok := s.ptySessions[req.SessionId]
	if ok {
		delete(s.ptySessions, sess.id)
	}
	s.ptyMu.Unlock()

	if !ok {
		return nil, fmt.Errorf("pty session %s not found", req.SessionId)
	}

	sess.cancel()
	sess.ptyFile.Close()
	if sess.cmd.Process != nil {
		sess.cmd.Process.Kill()
	}

	return &pb.PTYKillResponse{}, nil
}
