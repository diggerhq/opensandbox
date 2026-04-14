package commands

import (
	"fmt"
	"os"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/cmd/oc/internal/config"
	"github.com/opensandbox/opensandbox/cmd/oc/internal/output"
	"github.com/spf13/cobra"
)

// Version is set at build time via ldflags.
var Version = "dev"

var (
	jsonOutput bool
	printer    *output.Printer
)

var rootCmd = &cobra.Command{
	Use:     "oc",
	Short:   "OpenComputer CLI — manage cloud sandboxes",
	Long:    "Command-line interface for creating and managing OpenComputer sandboxes.",
	Version: Version,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Load(cmd)
		if cfg.APIKey == "" && cmd.Name() != "config" && cmd.Name() != "help" && cmd.Name() != "set" && cmd.Name() != "show" {
			fmt.Fprintln(os.Stderr, "Warning: no API key configured. Set OPENCOMPUTER_API_KEY or run 'oc config set api-key <key>'")
		}
		c := client.New(cfg.APIURL, cfg.APIKey)
		cmd.SetContext(client.WithClient(cmd.Context(), c))
		printer = output.New(jsonOutput)
		return nil
	},
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output as JSON")
	rootCmd.PersistentFlags().String("api-key", "", "API key (overrides OPENCOMPUTER_API_KEY)")
	rootCmd.PersistentFlags().String("api-url", "", "API URL (overrides OPENCOMPUTER_API_URL)")

	// Register command groups
	rootCmd.AddCommand(sandboxCmd)
	rootCmd.AddCommand(execCmd)
	rootCmd.AddCommand(shellCmd)
	rootCmd.AddCommand(checkpointCmd)
	rootCmd.AddCommand(patchCmd)
	rootCmd.AddCommand(previewCmd)
	rootCmd.AddCommand(secretStoreCmd)
	rootCmd.AddCommand(secretCmd)
	rootCmd.AddCommand(configCmd)

	// Top-level shortcuts
	rootCmd.AddCommand(createShortcut)
	rootCmd.AddCommand(lsShortcut)
}

// Execute runs the root command.
func Execute() error {
	return rootCmd.Execute()
}
