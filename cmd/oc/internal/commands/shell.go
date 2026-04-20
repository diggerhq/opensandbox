package commands

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var shellCmd = &cobra.Command{
	Use:   "shell <sandbox-id>",
	Short: "Open an interactive shell in a sandbox",
	Long: `Open an interactive terminal session in a running sandbox via WebSocket PTY.

Examples:
  oc shell abc123
  oc shell abc123 --shell /bin/zsh`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		sandboxID := args[0]
		shellPath, _ := cmd.Flags().GetString("shell")

		// If the argument doesn't look like a sandbox ID (full UUID or sb-* short form),
		// try to resolve it as an agent name through sessions-api.
		isSandboxID := uuidPattern.MatchString(sandboxID) || strings.HasPrefix(sandboxID, "sb-")
		if !isSandboxID {
			sc := client.SessionsFromContext(cmd.Context())
			if sc != nil {
				resolved, err := resolveAgentSandbox(cmd, sc, sandboxID)
				if err != nil {
					return fmt.Errorf("failed to resolve agent %q: %w", sandboxID, err)
				}
				sandboxID = resolved
			} else {
				return fmt.Errorf("%q does not look like a sandbox ID. Set SESSIONS_API_URL to resolve agent names.", sandboxID)
			}
		}

		// Get sandbox info to resolve connectURL for direct worker access
		var sandbox struct {
			ConnectURL string `json:"connectURL"`
			Token      string `json:"token"`
		}
		if err := c.Get(cmd.Context(), fmt.Sprintf("/sandboxes/%s", sandboxID), &sandbox); err != nil {
			return fmt.Errorf("failed to get sandbox: %w", err)
		}

		// Use worker client if connectURL is available (server mode)
		ptyClient := c
		if sandbox.ConnectURL != "" {
			ptyClient = client.NewWorker(sandbox.ConnectURL, sandbox.Token)
		}

		// Get current terminal size
		cols, rows := 80, 24
		if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
			cols, rows = w, h
		}

		// Create PTY session
		req := types.PTYCreateRequest{
			Cols:  cols,
			Rows:  rows,
			Shell: shellPath,
		}
		var session types.PTYSession
		if err := ptyClient.Post(cmd.Context(), fmt.Sprintf("/sandboxes/%s/pty", sandboxID), req, &session); err != nil {
			return fmt.Errorf("failed to create PTY: %w", err)
		}

		// Connect WebSocket
		wsPath := fmt.Sprintf("/sandboxes/%s/pty/%s", sandboxID, session.SessionID)
		conn, err := ptyClient.DialWebSocket(cmd.Context(), wsPath)
		if err != nil {
			return fmt.Errorf("failed to connect: %w", err)
		}
		defer conn.Close()

		// Enter raw terminal mode
		fd := int(os.Stdin.Fd())
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("failed to enter raw mode: %w", err)
		}
		defer term.Restore(fd, oldState)

		// Handle SIGWINCH for terminal resize
		sigWinch := make(chan os.Signal, 1)
		signal.Notify(sigWinch, syscall.SIGWINCH)
		go func() {
			for range sigWinch {
				if w, h, err := term.GetSize(fd); err == nil {
					resizeReq := map[string]any{"cols": w, "rows": h}
					// Best-effort resize via HTTP POST
					ptyClient.Post(cmd.Context(), fmt.Sprintf("/sandboxes/%s/pty/%s/resize", sandboxID, session.SessionID), resizeReq, nil)
				}
			}
		}()

		done := make(chan struct{})

		// Read from WebSocket → stdout
		go func() {
			defer close(done)
			for {
				_, msg, err := conn.ReadMessage()
				if err != nil {
					return
				}
				os.Stdout.Write(msg)
			}
		}()

		// Read from stdin → WebSocket
		go func() {
			buf := make([]byte, 1024)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
						return
					}
				}
				if err != nil {
					if err != io.EOF {
						return
					}
					return
				}
			}
		}()

		<-done

		// Cleanup: kill PTY session
		signal.Stop(sigWinch)
		ptyClient.Delete(cmd.Context(), fmt.Sprintf("/sandboxes/%s/pty/%s", sandboxID, session.SessionID))

		return nil
	},
}

// resolveAgentSandbox looks up an agent by name through sessions-api and returns
// the sandbox ID of its first running instance.
func resolveAgentSandbox(cmd *cobra.Command, sc *client.Client, agentName string) (string, error) {
	var instResp struct {
		Instances []struct {
			ID        string  `json:"id"`
			Status    string  `json:"status"`
			SandboxID *string `json:"sandbox_id"`
		} `json:"instances"`
	}
	if err := sc.Get(cmd.Context(), "/v1/agents/"+agentName+"/instances", &instResp); err != nil {
		return "", err
	}
	for _, inst := range instResp.Instances {
		if inst.Status == "running" && inst.SandboxID != nil {
			return *inst.SandboxID, nil
		}
	}
	return "", fmt.Errorf("no running instance found for agent %q", agentName)
}

func init() {
	shellCmd.Flags().String("shell", "", "Shell to use (default: /bin/bash)")
}
