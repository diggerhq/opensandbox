package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
)

var execCmd = &cobra.Command{
	Use:   "exec <sandbox-id> -- <command> [args...]",
	Short: "Execute a command in a sandbox",
	Long: `Execute a command in a running sandbox using session-based exec.

Creates an exec session, attaches via WebSocket, and streams output.
Use --wait to wait for completion and exit with the process exit code.

Examples:
  oc exec abc123 -- echo hello
  oc exec abc123 --wait -- npm install
  oc exec abc123 --cwd /app -- ls -la

Subcommands:
  oc exec list <sandbox-id>                   List active exec sessions
  oc exec attach <sandbox-id> <session-id>    Reconnect to a session
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

		if wait {
			// Wait mode: use exec/run endpoint for synchronous execution
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

		// Non-wait mode: create exec session
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

		if jsonOutput {
			printer.PrintJSON(sessionInfo)
		} else {
			fmt.Printf("Session %s created (command: %s)\n", sessionInfo.SessionID, sessionInfo.Command)
			fmt.Printf("Attach with: oc exec attach %s %s\n", sandboxID, sessionInfo.SessionID)
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
	Short: "Reconnect to an exec session",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sandboxID := args[0]
		sessionID := args[1]
		fmt.Fprintf(os.Stderr, "Attach to session %s on sandbox %s via WebSocket (not yet implemented in CLI)\n", sessionID, sandboxID)
		fmt.Fprintf(os.Stderr, "Use the SDK or websocat to connect to: /sandboxes/%s/exec/%s\n", sandboxID, sessionID)
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

func init() {
	execCmd.Flags().String("cwd", "", "Working directory")
	execCmd.Flags().Int("timeout", 0, "Timeout in seconds (0 = no timeout)")
	execCmd.Flags().StringSlice("env", nil, "Environment variables (KEY=VALUE)")
	execCmd.Flags().Bool("wait", false, "Wait for command to exit and print result")

	execKillCmd.Flags().Int("signal", 9, "Signal to send (default SIGKILL=9)")

	execCmd.AddCommand(execListCmd)
	execCmd.AddCommand(execAttachCmd)
	execCmd.AddCommand(execKillCmd)
}
