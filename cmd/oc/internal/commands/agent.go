package commands

// This file holds the parent `oc agent` cobra command, init() for flag +
// subcommand registration, shared response types used across multiple agent
// subcommands, and small formatting helpers. Each subcommand lives in its
// own file: agent_create.go, agent_get.go, agent_list.go, agent_delete.go,
// agent_connect.go, agent_packages.go, agent_events.go. Error rendering
// helpers (LastError, ExitError, codeCatalog, RenderLastError,
// renderAsyncFallback) live in agent_errors.go.

import (
	"fmt"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

// sessionsClient returns the sessions-api client from context, or errors if not configured.
func sessionsClient(cmd *cobra.Command) (*client.Client, error) {
	c := client.SessionsFromContext(cmd.Context())
	if c == nil {
		return nil, fmt.Errorf("sessions-api URL not configured. Set SESSIONS_API_URL or use --sessions-api-url")
	}
	return c, nil
}

// ── Shared response types ──

// agentResponse is the shape returned by sessions-api for both GET /v1/agents
// (list rows) and GET /v1/agents/:id (detail). The list endpoint only
// populates the top half; the detail endpoint additionally populates Status,
// InstanceID, and LastError.
type agentResponse struct {
	ID          string      `json:"id"`
	DisplayName string      `json:"display_name"`
	Core        *string     `json:"core"`
	Channels    interface{} `json:"channels"`
	Packages    interface{} `json:"packages"`
	SecretStore *string     `json:"secret_store"`
	Config      interface{} `json:"config"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`

	// Populated by GET /v1/agents/:id (enriched response); omitted by
	// POST /v1/agents and the list endpoint.
	Status     *string    `json:"status,omitempty"`
	InstanceID *string    `json:"instance_id,omitempty"`
	LastError  *LastError `json:"last_error,omitempty"`
}

type agentListResponse struct {
	Agents []agentResponse `json:"agents"`
}

// ── Parent command ──

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage agents",
	Long:  "Create and manage managed agents on OpenComputer.",
}

// ── Formatting helpers ──

func formatList(v interface{}) string {
	if v == nil {
		return "-"
	}
	switch items := v.(type) {
	case []interface{}:
		if len(items) == 0 {
			return "-"
		}
		strs := make([]string, len(items))
		for i, item := range items {
			strs[i] = fmt.Sprintf("%v", item)
		}
		return strings.Join(strs, ", ")
	case []string:
		if len(items) == 0 {
			return "-"
		}
		return strings.Join(items, ", ")
	default:
		return fmt.Sprintf("%v", v)
	}
}

func formatAge(isoTime string) string {
	t, err := time.Parse(time.RFC3339Nano, isoTime)
	if err != nil {
		return isoTime
	}
	return time.Since(t).Truncate(time.Second).String()
}

// ── Registration ──

func init() {
	// Per-subcommand flags
	agentCreateCmd.Flags().String("core", "", "Managed core (required, e.g. hermes|openclaw)")
	_ = agentCreateCmd.MarkFlagRequired("core")
	agentCreateCmd.Flags().StringSlice("secret", nil, "Secrets (KEY=VALUE)")
	agentCreateCmd.Flags().Bool("no-wait", false, "Don't wait for instance provisioning; exit after agent record is created")

	agentInstallCmd.Flags().Bool("no-wait", false, "Don't wait for install orchestration to finish")

	agentEventsCmd.Flags().Int("limit", 0, "Max events to return (1-200, default 50)")
	agentEventsCmd.Flags().String("before", "", "Return events before this ISO timestamp (for pagination)")

	// Subcommand registration — each is defined in its own file in this package.
	agentCmd.AddCommand(agentCreateCmd)
	agentCmd.AddCommand(agentListCmd)
	agentCmd.AddCommand(agentGetCmd)
	agentCmd.AddCommand(agentDeleteCmd)
	agentCmd.AddCommand(agentConnectCmd)
	agentCmd.AddCommand(agentDisconnectCmd)
	agentCmd.AddCommand(agentChannelsCmd)
	agentCmd.AddCommand(agentInstallCmd)
	agentCmd.AddCommand(agentUninstallCmd)
	agentCmd.AddCommand(agentPackagesCmd)
	agentCmd.AddCommand(agentEventsCmd)
}
