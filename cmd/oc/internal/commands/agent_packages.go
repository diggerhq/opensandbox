package commands

// Package lifecycle: install, uninstall, list. Install is queued and watched
// via agent operation resources.

import (
	"fmt"
	"os"

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

		var result operationSubmissionResponse
		if err := sc.Post(cmd.Context(), "/v1/agents/"+agentID+"/packages/"+pkg, nil, &result); err != nil {
			return err
		}

		if noWait {
			note := ""
			if result.Operation != nil {
				note = "Operation: " + result.Operation.ID
			}
			renderAsyncFallback(os.Stdout, jsonOutput, agentID, "Package install", note)
			return nil
		}

		agent, err := waitForOperation(cmd, sc, agentID, result.Operation, "Package install")
		if err != nil {
			return err
		}
		if agent == nil {
			return nil
		}

		if jsonOutput {
			printer.Print(agent, func() {})
			return nil
		}

		fmt.Fprintf(os.Stderr, "  ✓ %s installed\n", pkg)
		return nil
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
