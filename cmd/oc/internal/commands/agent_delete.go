package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

var agentDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		if err := confirmDestructive(cmd, fmt.Sprintf("Delete agent %s", args[0])); err != nil {
			return err
		}

		if err := sc.Delete(cmd.Context(), "/v1/agents/"+args[0]); err != nil {
			return err
		}

		fmt.Printf("Agent %s deleted.\n", args[0])
		return nil
	},
}
