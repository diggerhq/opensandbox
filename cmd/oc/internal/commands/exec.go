package commands

import (
	"fmt"
	"os"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
)

var execCmd = &cobra.Command{
	Use:   "exec <sandbox-id> -- <command> [args...]",
	Short: "Execute a command in a sandbox",
	Long: `Execute a command in a running sandbox and print the output.

The command exits with the same exit code as the remote process.

Examples:
  oc exec abc123 -- echo hello
  oc exec abc123 --cwd /app -- ls -la
  oc exec abc123 --timeout 120 -- npm install`,
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

		req := types.ProcessConfig{
			Command: command,
			Args:    cmdArgs,
			Cwd:     cwd,
			Timeout: timeout,
			Env:     parseKVSlice(envSlice),
		}

		var result types.ProcessResult
		if err := c.Post(cmd.Context(), "/sandboxes/"+sandboxID+"/commands", req, &result); err != nil {
			return err
		}

		if jsonOutput {
			printer.PrintJSON(result)
		} else {
			if result.Stdout != "" {
				fmt.Fprint(os.Stdout, result.Stdout)
			}
			if result.Stderr != "" {
				fmt.Fprint(os.Stderr, result.Stderr)
			}
		}

		if result.ExitCode != 0 {
			os.Exit(result.ExitCode)
		}
		return nil
	},
}

func init() {
	execCmd.Flags().String("cwd", "", "Working directory")
	execCmd.Flags().Int("timeout", 60, "Timeout in seconds")
	execCmd.Flags().StringSlice("env", nil, "Environment variables (KEY=VALUE)")
}
