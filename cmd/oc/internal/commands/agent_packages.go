package commands

// Package lifecycle: install, uninstall, list. Install is the only one that
// goes through the error-visibility render path — orchestration runs
// synchronously in sessions-api, so a 500 from the POST means we can fetch
// the just-persisted last_error and render the block.

import (
	"fmt"
	"os"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

var agentInstallCmd = &cobra.Command{
	Use:   "install <id> <package>",
	Short: "Install a package on an agent",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		agentID := args[0]
		pkg := args[1]
		noWait, _ := cmd.Flags().GetBool("no-wait")

		if !jsonOutput {
			fmt.Fprintf(os.Stderr, "Installing %s on %s\n", pkg, agentID)
		}

		// sessions-api returns 500 when install orchestration fails
		// synchronously. A successful 200 means the whole flow completed.
		// The orchestrator writes per-phase events to agent_events; we
		// surface them by re-fetching and rendering last_error on error.
		var result map[string]interface{}
		postErr := sc.Post(cmd.Context(), "/v1/agents/"+agentID+"/packages/"+pkg, nil, &result)

		if postErr == nil {
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "  ✓ %s installed\n", pkg)
			}
			if jsonOutput {
				printer.PrintJSON(result)
			}
			return nil
		}

		// 500 from the orchestrator — the event is already written. --no-wait
		// callers get the async fallback because they opted out of waiting;
		// otherwise fetch the latest state to render the error block.
		if noWait {
			renderAsyncFallback(os.Stdout, jsonOutput, agentID, "Package install", postErr.Error())
			return nil
		}
		return renderAgentError(cmd, sc, agentID, "Install")
	},
}

var agentUninstallCmd = &cobra.Command{
	Use:   "uninstall <id> <package>",
	Short: "Uninstall a package from an agent",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		if err := confirmDestructive(cmd, fmt.Sprintf("Uninstall %s from %s", args[1], args[0])); err != nil {
			return err
		}

		if err := sc.Delete(cmd.Context(), "/v1/agents/"+args[0]+"/packages/"+args[1]); err != nil {
			return err
		}

		fmt.Printf("Package %s uninstalled from %s.\n", args[1], args[0])
		return nil
	},
}

var agentPackagesCmd = &cobra.Command{
	Use:   "packages <id>",
	Short: "List packages installed on an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		var resp map[string]interface{}
		if err := sc.Get(cmd.Context(), "/v1/agents/"+args[0]+"/packages", &resp); err != nil {
			return err
		}

		printer.Print(resp, func() {
			packages := formatList(resp["packages"])
			if packages == "-" {
				fmt.Println("No packages installed.")
			} else {
				fmt.Printf("Packages: %s\n", packages)
			}
		})
		return nil
	},
}

// renderAgentError fetches the current agent state and renders the last_error
// block. Used by synchronous failure paths (install) where the POST returned
// 500 and the orchestrator has already persisted an error event. If the fetch
// itself fails we return the original network error so the user sees
// something rather than nothing.
func renderAgentError(cmd *cobra.Command, sc *client.Client, agentID, op string) error {
	var agent agentResponse
	if err := sc.Get(cmd.Context(), "/v1/agents/"+agentID, &agent); err != nil {
		return fmt.Errorf("%s failed (unable to fetch agent state: %w)", op, err)
	}
	if agent.LastError == nil {
		// 500 without an event row shouldn't happen post-migration, but guard
		// against it so the user isn't told "everything's fine" after a 500.
		return fmt.Errorf("%s failed (no error detail available — check server logs)", op)
	}
	if !jsonOutput {
		fmt.Fprintln(os.Stderr, "  ✗ "+op+" failed")
		fmt.Fprintln(os.Stderr)
		RenderLastError(os.Stderr, agent.LastError)
	} else {
		printer.Print(agent, func() {})
	}
	return &ExitError{Code: ExitCodeFor(agent.LastError)}
}
