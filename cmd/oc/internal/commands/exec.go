package commands

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
)

// Exec WebSocket binary protocol stream IDs.
const (
	execStreamStdin         = 0x00
	execStreamStdout        = 0x01
	execStreamStderr        = 0x02
	execStreamExit          = 0x03
	execStreamScrollbackEnd = 0x04
)

var execCmd = &cobra.Command{
	Use:   "exec <sandbox-id> -- <command> [args...]",
	Short: "Execute a command in a sandbox",
	Long: `Execute a command in a running sandbox using session-based exec.

By default, creates a session, attaches to it over WebSocket, and streams
stdout/stderr live to your terminal. The CLI exits with the command's
exit code. Stdin is forwarded to the process (Ctrl-C sends SIGINT; press
Ctrl-C twice to force-detach).

Modes:
  (default)   create, stream live, exit with the command's exit code
  --detach    create and print the session id, don't wait
  --wait      run synchronously via /exec/run (buffered, no streaming)

Examples:
  oc exec abc123 -- echo hello
  oc exec abc123 -- npm install           # streams output live
  oc exec abc123 --detach -- long-job     # fire and forget
  oc exec abc123 --cwd /app -- ls -la

Subcommands:
  oc exec list <sandbox-id>                   List active exec sessions
  oc exec attach <sandbox-id> <session-id>    Reconnect + stream
  oc exec kill <sandbox-id> <session-id>      Kill a session`,
	Args:               cobra.MinimumNArgs(1),
	DisableFlagParsing: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		sandboxID := args[0]

		if len(args) < 2 {
			return fmt.Errorf("no command specified. Usage: oc exec <sandbox-id> -- <command>")
		}
		command := args[1]
		cmdArgs := args[2:]

		cwd, _ := cmd.Flags().GetString("cwd")
		timeout, _ := cmd.Flags().GetInt("timeout")
		envSlice, _ := cmd.Flags().GetStringSlice("env")
		wait, _ := cmd.Flags().GetBool("wait")
		detach, _ := cmd.Flags().GetBool("detach")

		if wait && detach {
			return fmt.Errorf("--wait and --detach are mutually exclusive")
		}

		if wait {
			// Buffered mode: POST /exec/run, print everything at the end.
			req := types.ProcessConfig{
				Command: command,
				Args:    cmdArgs,
				Cwd:     cwd,
				Timeout: timeout,
				Env:     parseKVSlice(envSlice),
			}

			var result types.ProcessResult
			if err := c.Post(cmd.Context(), "/sandboxes/"+sandboxID+"/exec/run", req, &result); err != nil {
				return err
			}

			if jsonOutput {
				printer.PrintJSON(result)
			} else {
				if result.Stdout != "" {
					fmt.Print(result.Stdout)
				}
				if result.Stderr != "" {
					fmt.Fprint(os.Stderr, result.Stderr)
				}
				if result.ExitCode != 0 {
					os.Exit(result.ExitCode)
				}
			}
			return nil
		}

		// Create an exec session.
		req := types.ExecSessionCreateRequest{
			Command: command,
			Args:    cmdArgs,
			Cwd:     cwd,
			Timeout: timeout,
			Env:     parseKVSlice(envSlice),
		}

		var sessionInfo types.ExecSessionInfo
		if err := c.Post(cmd.Context(), "/sandboxes/"+sandboxID+"/exec", req, &sessionInfo); err != nil {
			return err
		}

		if detach {
			if jsonOutput {
				printer.PrintJSON(sessionInfo)
			} else {
				fmt.Printf("Session %s created (command: %s)\n", sessionInfo.SessionID, sessionInfo.Command)
				fmt.Printf("Attach with: oc exec attach %s %s\n", sandboxID, sessionInfo.SessionID)
			}
			return nil
		}

		// Default: attach + stream.
		code, err := streamExecSession(cmd.Context(), c, sandboxID, sessionInfo.SessionID, true)
		if err != nil {
			return err
		}
		if code != 0 {
			os.Exit(code)
		}
		return nil
	},
}

var execListCmd = &cobra.Command{
	Use:   "list <sandbox-id>",
	Short: "List active exec sessions",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		sandboxID := args[0]

		var sessions []types.ExecSessionInfo
		if err := c.Get(cmd.Context(), "/sandboxes/"+sandboxID+"/exec", &sessions); err != nil {
			return err
		}

		if jsonOutput {
			printer.PrintJSON(sessions)
		} else {
			if len(sessions) == 0 {
				fmt.Println("No active exec sessions.")
				return nil
			}
			for _, s := range sessions {
				status := "running"
				if !s.Running {
					exitCode := 0
					if s.ExitCode != nil {
						exitCode = *s.ExitCode
					}
					status = fmt.Sprintf("exited (%d)", exitCode)
				}
				cmdStr := s.Command
				if len(s.Args) > 0 {
					cmdStr += " " + strings.Join(s.Args, " ")
				}
				fmt.Printf("  %s  %s  %s  clients=%d\n", s.SessionID, status, cmdStr, s.AttachedClients)
			}
		}
		return nil
	},
}

var execAttachCmd = &cobra.Command{
	Use:   "attach <sandbox-id> <session-id>",
	Short: "Reconnect to an exec session and stream its output",
	Long: `Open a WebSocket to an existing exec session. The server replays the
scrollback buffer (historical output), then switches to live streaming.
Stdin is forwarded to the process; Ctrl-C sends SIGINT.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		sandboxID := args[0]
		sessionID := args[1]

		code, err := streamExecSession(cmd.Context(), c, sandboxID, sessionID, true)
		if err != nil {
			return err
		}
		if code != 0 {
			os.Exit(code)
		}
		return nil
	},
}

var execKillCmd = &cobra.Command{
	Use:   "kill <sandbox-id> <session-id>",
	Short: "Kill an exec session",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		sandboxID := args[0]
		sessionID := args[1]

		signal, _ := cmd.Flags().GetInt("signal")
		body := map[string]int{"signal": signal}

		var resp map[string]interface{}
		if err := c.Post(cmd.Context(), "/sandboxes/"+sandboxID+"/exec/"+sessionID+"/kill", body, &resp); err != nil {
			return err
		}

		fmt.Printf("Session %s killed\n", sessionID)
		return nil
	},
}

// streamExecSession attaches to an exec session's WebSocket, pipes
// stdout/stderr to the terminal, forwards stdin when requested, and
// returns when the session ends. Returns the process exit code, or -1
// if the WS closed without an exit frame.
//
// First SIGINT sends SIGINT to the process via the HTTP kill endpoint;
// a second SIGINT aborts the CLI without waiting.
func streamExecSession(ctx context.Context, c *client.Client, sandboxID, sessionID string, forwardStdin bool) (int, error) {
	wsPath := fmt.Sprintf("/sandboxes/%s/exec/%s", sandboxID, sessionID)
	conn, err := c.DialWebSocket(ctx, wsPath)
	if err != nil {
		return -1, fmt.Errorf("attach failed: %w", err)
	}
	defer conn.Close()

	// Graceful Ctrl-C: forward SIGINT to the process. Second Ctrl-C bails.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	interrupts := 0
	go func() {
		for range sigCh {
			interrupts++
			if interrupts >= 2 {
				conn.Close()
				return
			}
			body := map[string]int{"signal": int(syscall.SIGINT)}
			_ = c.Post(ctx, fmt.Sprintf("/sandboxes/%s/exec/%s/kill", sandboxID, sessionID), body, nil)
			fmt.Fprintln(os.Stderr, "\n^C sent SIGINT (press again to force-detach)")
		}
	}()

	// Stdin → WS (stream 0x00). Runs until EOF/error.
	if forwardStdin {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					msg := make([]byte, 1+n)
					msg[0] = execStreamStdin
					copy(msg[1:], buf[:n])
					if werr := conn.WriteMessage(websocket.BinaryMessage, msg); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}

	// WS → stdout/stderr. Exit frame terminates.
	exitCode := -1
	gotExit := false
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if len(msg) < 1 {
			continue
		}
		switch msg[0] {
		case execStreamStdout:
			_, _ = os.Stdout.Write(msg[1:])
		case execStreamStderr:
			_, _ = os.Stderr.Write(msg[1:])
		case execStreamExit:
			gotExit = true
			if len(msg) >= 5 {
				exitCode = int(int32(binary.BigEndian.Uint32(msg[1:5])))
			} else {
				exitCode = 0
			}
		case execStreamScrollbackEnd:
			// Marks the boundary between replayed history and live output.
			// Nothing to do for the CLI — we stream both the same way.
		}
	}

	if !gotExit {
		// WS closed without an exit frame — either the user force-detached
		// (second Ctrl-C) or the network dropped. Return -1 so the caller
		// can decide what to do.
		return -1, nil
	}
	return exitCode, nil
}

func init() {
	execCmd.Flags().String("cwd", "", "Working directory")
	execCmd.Flags().Int("timeout", 0, "Timeout in seconds (0 = no timeout)")
	execCmd.Flags().StringSlice("env", nil, "Environment variables (KEY=VALUE)")
	execCmd.Flags().Bool("wait", false, "Run synchronously via /exec/run (buffered, no streaming)")
	execCmd.Flags().Bool("detach", false, "Create the session and print its id; don't stream")

	execKillCmd.Flags().Int("signal", 9, "Signal to send (default SIGKILL=9)")

	execCmd.AddCommand(execListCmd)
	execCmd.AddCommand(execAttachCmd)
	execCmd.AddCommand(execKillCmd)
}
