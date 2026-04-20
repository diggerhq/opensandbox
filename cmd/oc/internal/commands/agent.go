package commands

// This file holds the parent `oc agent` cobra command, init() for flag +
// subcommand registration, shared response types used across multiple agent
// subcommands, and small formatting helpers. Each subcommand lives in its
// own file: agent_create.go, agent_get.go, agent_list.go, agent_delete.go,
// agent_connect.go, agent_packages.go, agent_events.go. Error rendering
// helpers (LastError, ExitError, codeCatalog, RenderLastError,
// renderAsyncFallback) live in agent_errors.go.

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

// stdinIsTTY reports whether stdin is attached to a terminal. Used to gate
// interactive prompts: if stdin isn't a TTY we refuse to prompt, since the
// caller is a script or agent that can't respond.
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// confirmDestructive gates delete/disconnect/uninstall on explicit consent.
// With --yes: proceed. Without --yes and a TTY: prompt. Without --yes and
// no TTY: refuse — scripts and agents must pass --yes so accidental
// invocation can't quietly destroy state.
func confirmDestructive(cmd *cobra.Command, action string) error {
	if yes, _ := cmd.Flags().GetBool("yes"); yes {
		return nil
	}
	if !stdinIsTTY() {
		return fmt.Errorf("refusing to %s without --yes (stdin is not a terminal)", action)
	}
	fmt.Fprintf(os.Stderr, "%s? [y/N] ", action)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return fmt.Errorf("aborted")
	}
}

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
	Model       *string     `json:"model,omitempty"`
	Channels    interface{} `json:"channels"`
	Packages    interface{} `json:"packages"`
	SecretStore *string     `json:"secret_store"`
	Config      interface{} `json:"config"`
	CreatedAt   string      `json:"created_at"`
	UpdatedAt   string      `json:"updated_at"`

	// Populated by GET /v1/agents/:id (enriched response); omitted by
	// POST /v1/agents and the list endpoint.
	Status           *string                   `json:"status,omitempty"`
	InstanceID       *string                   `json:"instance_id,omitempty"`
	InstanceStatus   *string                   `json:"instance_status,omitempty"`
	CoreStatus       map[string]interface{}    `json:"core_status,omitempty"`
	ChannelStatus    map[string]map[string]any `json:"channel_status,omitempty"`
	PackageStatus    map[string]map[string]any `json:"package_status,omitempty"`
	CurrentOperation *AgentOperation           `json:"current_operation,omitempty"`
	Conditions       []AgentCondition          `json:"conditions,omitempty"`
	LastError        *LastError                `json:"last_error,omitempty"`
}

type agentListResponse struct {
	Agents []agentResponse `json:"agents"`
}

type AgentOperation struct {
	ID          string  `json:"id"`
	AgentID     string  `json:"agent_id"`
	InstanceID  *string `json:"instance_id,omitempty"`
	Kind        string  `json:"kind"`
	TargetType  *string `json:"target_type,omitempty"`
	TargetKey   *string `json:"target_key,omitempty"`
	Status      string  `json:"status"`
	Phase       *string `json:"phase,omitempty"`
	Code        *string `json:"code,omitempty"`
	Message     *string `json:"message,omitempty"`
	CreatedBy   *string `json:"created_by,omitempty"`
	StartedAt   *string `json:"started_at,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type AgentCondition struct {
	Type    string `json:"type"`
	Subject string `json:"subject,omitempty"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

type operationSubmissionResponse struct {
	AgentID   string          `json:"agent_id"`
	Status    string          `json:"status"`
	Channel   string          `json:"channel,omitempty"`
	Package   string          `json:"package,omitempty"`
	Operation *AgentOperation `json:"operation,omitempty"`
}

// ── Parent command ──

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage agents",
	Long: "Create and manage managed agents on OpenComputer.\n\n" +
		"Exit codes:\n" +
		"  0  Success\n" +
		"  1  General error (unclassified failures, bad args/flags)\n" +
		"  3  Upstream 4xx (not found, unauthorized, org mismatch)\n" +
		"  4  Conflict (already exists, invalid state)\n" +
		"  5  Transient error (timeout, retry-safe)\n\n" +
		"Classes 3-5 are emitted by create/install/get when the failure\n" +
		"carries a known code. Other commands exit 0 on success, 1 on\n" +
		"failure. Agent callers can branch on the class to decide retry\n" +
		"vs surface-to-user vs give-up.",
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

func listEntries(v interface{}) []string {
	switch items := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out
	case []string:
		return append([]string(nil), items...)
	default:
		return nil
	}
}

func listIsEmpty(v interface{}) bool {
	return len(listEntries(v)) == 0
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

	agentConnectCmd.Flags().String("bot-token", "", "Telegram bot token (required for channel=telegram when stdin is not a TTY)")
	agentConnectCmd.Flags().Bool("no-wait", false, "Don't wait for channel orchestration to finish")

	agentInstallCmd.Flags().Bool("no-wait", false, "Don't wait for install orchestration to finish")

	// Destructive ops — require explicit --yes in non-TTY callers.
	agentDeleteCmd.Flags().Bool("yes", false, "Skip confirmation (required for non-interactive callers)")
	agentDisconnectCmd.Flags().Bool("yes", false, "Skip confirmation (required for non-interactive callers)")
	agentUninstallCmd.Flags().Bool("yes", false, "Skip confirmation (required for non-interactive callers)")

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
