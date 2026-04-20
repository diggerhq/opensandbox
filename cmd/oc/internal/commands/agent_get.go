package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var agentGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get agent details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		var agent agentResponse
		if err := sc.Get(cmd.Context(), "/v1/agents/"+args[0], &agent); err != nil {
			return err
		}

		// /v1/agents/:id returns status + last_error inline so we no longer
		// need a second call to /instances.
		printer.Print(agent, func() {
			fmt.Printf("ID:        %s\n", agent.ID)
			fmt.Printf("Name:      %s\n", agent.DisplayName)
			coreStr := "-"
			if agent.Core != nil {
				coreStr = *agent.Core
			}
			fmt.Printf("Core:      %s\n", coreStr)
			if agent.Status != nil {
				fmt.Printf("Status:    %s\n", *agent.Status)
			}
			if agent.InstanceStatus != nil {
				fmt.Printf("Instance:  %s\n", *agent.InstanceStatus)
			}
			if agent.CurrentOperation != nil {
				fmt.Printf("Op:        %s (%s)\n", humanizePhase(agent.CurrentOperation.Kind), agent.CurrentOperation.Status)
				if progress := operationProgress(*agent.CurrentOperation); progress != "" {
					fmt.Printf("Progress:  %s\n", progress)
				}
			}
			fmt.Printf("Channels:  %s\n", formatList(agent.Channels))
			fmt.Printf("Packages:  %s\n", formatList(agent.Packages))
			fmt.Printf("Created:   %s\n", agent.CreatedAt)

			if pending := pendingConditions(agent.Conditions); len(pending) > 0 {
				fmt.Println("Conditions:")
				for _, cond := range pending {
					label := cond.Type
					if cond.Subject != "" {
						label += "[" + cond.Subject + "]"
					}
					line := fmt.Sprintf("  %s=%s (%s)", label, cond.Status, cond.Reason)
					if msg := strings.TrimSpace(cond.Message); msg != "" {
						line += ": " + msg
					}
					fmt.Println(line)
				}
			}

			if agent.LastError != nil {
				fmt.Println()
				RenderLastError(os.Stdout, agent.LastError)
			}
		})

		return nil
	},
}

func pendingConditions(conditions []AgentCondition) []AgentCondition {
	var pending []AgentCondition
	for _, cond := range conditions {
		if cond.Status != "True" {
			pending = append(pending, cond)
		}
	}
	return pending
}
