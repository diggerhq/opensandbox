package commands

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

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

		// WebSocket keepalive: send ping every 30s to prevent idle timeout
		// (Cloudflare drops idle WebSocket connections after 100s)
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				case <-done:
					return
				}
			}
		}()

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

func init() {
	shellCmd.Flags().String("shell", "", "Shell to use (default: /bin/bash)")
}
