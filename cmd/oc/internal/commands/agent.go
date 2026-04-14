package commands

import (
	"bufio"
	"fmt"
	"os"
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

// ── Types for sessions-api responses ──

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
}

type agentListResponse struct {
	Agents []agentResponse `json:"agents"`
}

type instanceResponse struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type instanceListResponse struct {
	Instances []instanceResponse `json:"instances"`
}

// ── Commands ──

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage agents",
	Long:  "Create and manage managed agents on OpenComputer.",
}

var agentCreateCmd = &cobra.Command{
	Use:   "create <id>",
	Short: "Create a new managed agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		id := args[0]
		core, _ := cmd.Flags().GetString("core")
		secretSlice, _ := cmd.Flags().GetStringSlice("secret")

		body := map[string]interface{}{
			"id": id,
		}
		if core != "" {
			body["core"] = core
		}

		// Parse --secret KEY=VAL flags into secrets map
		if len(secretSlice) > 0 {
			secrets := make(map[string]string)
			for _, s := range secretSlice {
				parts := strings.SplitN(s, "=", 2)
				if len(parts) == 2 {
					secrets[parts[0]] = parts[1]
				}
			}
			body["secrets"] = secrets
		}

		var agent agentResponse
		if err := sc.Post(cmd.Context(), "/v1/agents", body, &agent); err != nil {
			return err
		}

		printer.Print(agent, func() {
			fmt.Printf("Created agent %s", agent.ID)
			if agent.Core != nil {
				fmt.Printf(" (core: %s)", *agent.Core)
			}
			fmt.Println()
			if agent.Core != nil {
				fmt.Println("Instance is booting...")
			}
		})
		return nil
	},
}

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

		printer.Print(agent, func() {
			fmt.Printf("ID:        %s\n", agent.ID)
			fmt.Printf("Name:      %s\n", agent.DisplayName)
			coreStr := "-"
			if agent.Core != nil {
				coreStr = *agent.Core
			}
			fmt.Printf("Core:      %s\n", coreStr)
			fmt.Printf("Channels:  %s\n", formatList(agent.Channels))
			fmt.Printf("Packages:  %s\n", formatList(agent.Packages))
			fmt.Printf("Created:   %s\n", agent.CreatedAt)
		})

		// Show instance status
		var instResp instanceListResponse
		if err := sc.Get(cmd.Context(), "/v1/agents/"+args[0]+"/instances", &instResp); err == nil && len(instResp.Instances) > 0 {
			inst := instResp.Instances[0]
			fmt.Printf("Instance:  %s (%s)\n", inst.ID, inst.Status)
		}

		return nil
	},
}

var agentDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		if err := sc.Delete(cmd.Context(), "/v1/agents/"+args[0]); err != nil {
			return err
		}

		fmt.Printf("Agent %s deleted.\n", args[0])
		return nil
	},
}

var agentConnectCmd = &cobra.Command{
	Use:   "connect <id> <channel>",
	Short: "Connect a channel to an agent",
	Long:  "Connect a messaging channel (e.g. telegram) to a managed agent.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		agentID := args[0]
		channel := args[1]

		body := map[string]interface{}{}

		if channel == "telegram" {
			fmt.Println("To connect Telegram:")
			fmt.Println("  1. Open Telegram and message @BotFather")
			fmt.Println("  2. Send /newbot, choose a name and username")
			fmt.Println("  3. Copy the bot token")
			fmt.Println()
			fmt.Print("Paste bot token: ")

			reader := bufio.NewReader(os.Stdin)
			token, _ := reader.ReadString('\n')
			token = strings.TrimSpace(token)
			if token == "" {
				return fmt.Errorf("bot token is required")
			}
			body["bot_token"] = token
		}

		var result map[string]interface{}
		if err := sc.Post(cmd.Context(), "/v1/agents/"+agentID+"/channels/"+channel, body, &result); err != nil {
			return err
		}

		fmt.Printf("Telegram connected to %s.\n", agentID)
		if channel == "telegram" {
			fmt.Println("Message your bot on Telegram to start chatting.")
		}
		return nil
	},
}

var agentDisconnectCmd = &cobra.Command{
	Use:   "disconnect <id> <channel>",
	Short: "Disconnect a channel from an agent",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		if err := sc.Delete(cmd.Context(), "/v1/agents/"+args[0]+"/channels/"+args[1]); err != nil {
			return err
		}

		fmt.Printf("Channel %s disconnected from %s.\n", args[1], args[0])
		return nil
	},
}

var agentChannelsCmd = &cobra.Command{
	Use:   "channels <id>",
	Short: "List channels connected to an agent",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		var resp map[string]interface{}
		if err := sc.Get(cmd.Context(), "/v1/agents/"+args[0]+"/channels", &resp); err != nil {
			return err
		}

		printer.Print(resp, func() {
			channels := formatList(resp["channels"])
			if channels == "-" {
				fmt.Println("No channels connected.")
			} else {
				fmt.Printf("Channels: %s\n", channels)
			}
		})
		return nil
	},
}

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

		var result map[string]interface{}
		if err := sc.Post(cmd.Context(), "/v1/agents/"+agentID+"/packages/"+pkg, nil, &result); err != nil {
			return err
		}

		fmt.Printf("Package %s installed on %s.\n", pkg, agentID)
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

// ── Helpers ──

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

func init() {
	// agent create flags
	agentCreateCmd.Flags().String("core", "", "Managed core (e.g. hermes)")
	agentCreateCmd.Flags().StringSlice("secret", nil, "Secrets (KEY=VALUE)")

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
}
