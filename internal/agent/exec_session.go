package agent

import (
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// execSession holds a live exec session inside the VM.
type execSession struct {
	id         string
	cmd        *exec.Cmd
	cancel     context.CancelFunc
	command    string
	args       []string
	started    time.Time
	scrollback *sandbox.ScrollbackBuffer
	stdinPipe  io.WriteCloser
	exitCode   *int32
	exited     chan struct{}

	mu              sync.Mutex
	attachedClients int
	lastDisconnect  time.Time
	maxRunAfterDisc time.Duration
	disconnectTimer *time.Timer
}

// ExecSessionCreate starts a command and returns a session ID.
func (s *Server) ExecSessionCreate(ctx context.Context, req *pb.ExecSessionCreateRequest) (*pb.ExecSessionCreateResponse, error) {
	sessionID := uuid.New().String()[:8]

	sessCtx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(sessCtx, req.Command, req.Args...)
	cmd.Dir = req.Cwd
	if cmd.Dir == "" {
		cmd.Dir = sandboxHome
	}

	// Build environment: base < sandbox-level < per-command
	env := baseEnv()
	s.envMu.RLock()
	env = append(env, s.sandboxEnvs...)
	s.envMu.RUnlock()
	env = append(env, mapToEnv(req.Envs)...)
	cmd.Env = env

	// Run as sandbox user in its own process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Credential: sandboxCredential(),
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	maxScrollback := int(req.ScrollbackMaxBytes)
	scrollback := sandbox.NewScrollbackBuffer(maxScrollback)

	sess := &execSession{
		id:         sessionID,
		cmd:        cmd,
		cancel:     cancel,
		command:    req.Command,
		args:       req.Args,
		started:    time.Now(),
		scrollback: scrollback,
		stdinPipe:  stdinPipe,
		exited:     make(chan struct{}),
	}

	if req.MaxRunAfterDisconnect > 0 {
		sess.maxRunAfterDisc = time.Duration(req.MaxRunAfterDisconnect) * time.Second
	}

	log.Printf("exec-session: starting %s %v (SysProcAttr=%+v, Dir=%s)", req.Command, req.Args, cmd.SysProcAttr, cmd.Dir)
	if err := cmd.Start(); err != nil {
		cancel()
		log.Printf("exec-session: start failed: %v (cmd=%s)", err, req.Command)
		return nil, fmt.Errorf("start command: %w", err)
	}
	moveToCgroup(cmd.Process.Pid)

	s.execMu.Lock()
	s.execSessions[sessionID] = sess
	s.execMu.Unlock()

	// Pipe stdout/stderr into scrollback buffer
	var pipeWg sync.WaitGroup
	pipeWg.Add(2)

	go func() {
		defer pipeWg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				scrollback.Write(1, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer pipeWg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				scrollback.Write(2, buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for command exit — ensure all pipe output is collected first
	go func() {
		_ = cmd.Wait()
		// Wait for stdout/stderr goroutines to finish reading remaining pipe data
		pipeWg.Wait()
		code := int32(cmd.ProcessState.ExitCode())
		sess.mu.Lock()
		sess.exitCode = &code
		sess.mu.Unlock()
		close(sess.exited)

		// Schedule cleanup after 5 minutes
		time.AfterFunc(5*time.Minute, func() {
			s.execMu.Lock()
			delete(s.execSessions, sessionID)
			s.execMu.Unlock()
		})
	}()

	// Timeout: kill after N seconds if set
	if req.TimeoutSeconds > 0 {
		go func() {
			timer := time.NewTimer(time.Duration(req.TimeoutSeconds) * time.Second)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancel()
			case <-sess.exited:
			}
		}()
	}

	return &pb.ExecSessionCreateResponse{SessionId: sessionID}, nil
}

// ExecSessionAttach implements bidi streaming: sends scrollback + live output, accepts stdin.
func (s *Server) ExecSessionAttach(stream pb.SandboxAgent_ExecSessionAttachServer) error {
	// First message must contain session_id
	firstMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv first message: %w", err)
	}

	sessionID := firstMsg.SessionId
	if sessionID == "" {
		return fmt.Errorf("first message must contain session_id")
	}

	s.execMu.RLock()
	sess, ok := s.execSessions[sessionID]
	s.execMu.RUnlock()
	if !ok {
		return fmt.Errorf("exec session %s not found", sessionID)
	}

	// Increment attached clients — check if session is already dying
	sess.mu.Lock()
	if sess.disconnectTimer != nil {
		if !sess.disconnectTimer.Stop() {
			// Timer already fired — session is being torn down
			sess.mu.Unlock()
			return fmt.Errorf("exec session %s is shutting down", sessionID)
		}
		sess.disconnectTimer = nil
	}
	sess.attachedClients++
	sess.mu.Unlock()

	defer func() {
		sess.mu.Lock()
		sess.attachedClients--
		if sess.attachedClients == 0 {
			sess.lastDisconnect = time.Now()
			if sess.maxRunAfterDisc > 0 {
				sess.disconnectTimer = time.AfterFunc(sess.maxRunAfterDisc, func() {
					sess.cancel()
				})
			}
		}
		sess.mu.Unlock()
	}()

	// Send scrollback snapshot
	snapshot := sess.scrollback.Snapshot()
	for _, chunk := range snapshot {
		outType := pb.ExecSessionOutput_STDOUT
		if chunk.Stream == 2 {
			outType = pb.ExecSessionOutput_STDERR
		}
		if err := stream.Send(&pb.ExecSessionOutput{
			Type: outType,
			Data: chunk.Data,
		}); err != nil {
			return err
		}
	}

	// Send scrollback_end marker
	if err := stream.Send(&pb.ExecSessionOutput{
		Type: pb.ExecSessionOutput_SCROLLBACK_END,
	}); err != nil {
		return err
	}

	// Subscribe for live output
	sub := sess.scrollback.Subscribe()
	defer sess.scrollback.Unsubscribe(sub)

	// Read stdin from client in a goroutine
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		// Process first message's stdin if any
		if len(firstMsg.Stdin) > 0 {
			sess.stdinPipe.Write(firstMsg.Stdin)
		}
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if len(msg.Stdin) > 0 {
				sess.stdinPipe.Write(msg.Stdin)
			}
		}
	}()

	// Send live output and wait for exit
	for {
		select {
		case chunk, ok := <-sub:
			if !ok {
				return nil
			}
			outType := pb.ExecSessionOutput_STDOUT
			if chunk.Stream == 2 {
				outType = pb.ExecSessionOutput_STDERR
			}
			if err := stream.Send(&pb.ExecSessionOutput{
				Type: outType,
				Data: chunk.Data,
			}); err != nil {
				return err
			}

		case <-sess.exited:
			// Drain remaining chunks from subscriber
			for {
				select {
				case chunk := <-sub:
					outType := pb.ExecSessionOutput_STDOUT
					if chunk.Stream == 2 {
						outType = pb.ExecSessionOutput_STDERR
					}
					_ = stream.Send(&pb.ExecSessionOutput{
						Type: outType,
						Data: chunk.Data,
					})
				default:
					goto sendExit
				}
			}
		sendExit:
			sess.mu.Lock()
			exitCode := *sess.exitCode
			sess.mu.Unlock()
			_ = stream.Send(&pb.ExecSessionOutput{
				Type:     pb.ExecSessionOutput_EXIT,
				ExitCode: exitCode,
			})
			return nil

		case <-stdinDone:
			// Client disconnected, stop sending
			return nil

		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// ExecSessionList returns metadata for all sessions.
func (s *Server) ExecSessionList(ctx context.Context, req *pb.ExecSessionListRequest) (*pb.ExecSessionListResponse, error) {
	s.execMu.RLock()
	defer s.execMu.RUnlock()

	var sessions []*pb.ExecSessionInfo
	for _, sess := range s.execSessions {
		sess.mu.Lock()
		info := &pb.ExecSessionInfo{
			SessionId:       sess.id,
			Command:         sess.command,
			Args:            sess.args,
			Running:         sess.exitCode == nil,
			StartedAt:       sess.started.Unix(),
			AttachedClients: int32(sess.attachedClients),
		}
		if sess.exitCode != nil {
			info.ExitCode = *sess.exitCode
		}
		sess.mu.Unlock()
		sessions = append(sessions, info)
	}

	return &pb.ExecSessionListResponse{Sessions: sessions}, nil
}

// ExecSessionKill sends a signal to a session's process group.
func (s *Server) ExecSessionKill(ctx context.Context, req *pb.ExecSessionKillRequest) (*pb.ExecSessionKillResponse, error) {
	s.execMu.RLock()
	sess, ok := s.execSessions[req.SessionId]
	s.execMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("exec session %s not found", req.SessionId)
	}

	sig := syscall.Signal(req.Signal)
	if sig == 0 {
		sig = syscall.SIGKILL
	}

	if sess.cmd.Process != nil {
		// Kill the process group (-pgid)
		_ = syscall.Kill(-sess.cmd.Process.Pid, sig)
	}

	return &pb.ExecSessionKillResponse{}, nil
}
