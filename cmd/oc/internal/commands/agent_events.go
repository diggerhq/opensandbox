package commands

// `oc agent events <id>` — time-ordered event history for an agent.
// Primarily surfaces error events today; as Design 003 adds recovered /
// health_check_failed events, they flow through the same table and command.

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var agentEventsCmd = &cobra.Command{
	Use:   "events <id>",
	Short: "Show an agent's event history (errors, recoveries, etc.)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		limit, _ := cmd.Flags().GetInt("limit")
		before, _ := cmd.Flags().GetString("before")

		path := "/v1/agents/" + args[0] + "/events"
		q := []string{}
		if limit > 0 {
			q = append(q, fmt.Sprintf("limit=%d", limit))
		}
		if before != "" {
			q = append(q, "before="+before)
		}
		if len(q) > 0 {
			path += "?" + strings.Join(q, "&")
		}

		var resp agentEventsResponse
		if err := sc.Get(cmd.Context(), path, &resp); err != nil {
			return err
		}

		printer.Print(resp, func() {
			if len(resp.Events) == 0 {
				fmt.Println("No events.")
				return
			}
			headers := []string{"TIMESTAMP", "TYPE", "PHASE", "MESSAGE"}
			var rows [][]string
			for _, e := range resp.Events {
				rows = append(rows, []string{e.At, e.Type, valueOr(e.Phase, "-"), truncate(valueOr(e.Message, ""), 80)})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}

type agentEventRow struct {
	ID             int    `json:"id"`
	InstanceID     string `json:"instance_id"`
	Type           string `json:"type"`
	Phase          string `json:"phase"`
	Message        string `json:"message"`
	Code           string `json:"code"`
	UpstreamStatus int    `json:"upstream_status"`
	At             string `json:"at"`
}

type agentEventsResponse struct {
	Events     []agentEventRow `json:"events"`
	NextBefore *string         `json:"next_before"`
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return s[:n-1] + "…"
}
