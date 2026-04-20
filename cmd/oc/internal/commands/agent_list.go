package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

var agentListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List agents",
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		var resp agentListResponse
		if err := sc.Get(cmd.Context(), "/v1/agents", &resp); err != nil {
			return err
		}

		printer.Print(resp.Agents, func() {
			if len(resp.Agents) == 0 {
				fmt.Println("No agents found.")
				return
			}
			headers := []string{"ID", "CORE", "CHANNELS", "PACKAGES", "CREATED"}
			var rows [][]string
			for _, a := range resp.Agents {
				coreStr := "-"
				if a.Core != nil {
					coreStr = *a.Core
				}
				channels := formatList(a.Channels)
				packages := formatList(a.Packages)
				created := formatAge(a.CreatedAt)
				rows = append(rows, []string{a.ID, coreStr, channels, packages, created})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}
