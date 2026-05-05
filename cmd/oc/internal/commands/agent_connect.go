package commands

// Channel lifecycle: connect, disconnect, list.

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/cmd/oc/internal/config"
)

// dashboardAgentURL builds the dashboard URL for an agent, used in the
// CLI's paywall-redirect message. APIURL doubles as the dashboard origin
// (the OC server serves /api/dashboard/* and the SPA from the same host).
func dashboardAgentURL(cmd *cobra.Command, agentID string) string {
	cfg := config.Load(cmd)
	base := strings.TrimRight(cfg.APIURL, "/")
	if base == "" {
		base = config.DefaultAPIURL
	}
	return base + "/agents/" + url.PathEscape(agentID)
}

var agentConnectCmd = &cobra.Command{
	Use:   "connect <id> <channel>",
	Short: "Connect a channel to an agent",
	Long: "Connect a messaging channel to a managed agent.\n\n" +
		"Supported channels:\n" +
		"  telegram   requires --bot-token (or interactive prompt if TTY)",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		agentID := args[0]
		channel := args[1]
		noWait, _ := cmd.Flags().GetBool("no-wait")

		body := map[string]interface{}{}

		if channel == "telegram" {
			// Pre-flight entitlement check — bail with a clean upgrade
			// message before asking for a bot token if the agent isn't
			// subscribed. The actual connect endpoint also gates, but
			// hitting that path first means the user has to paste a token
			// just to see the paywall, which is jarring.
			var ent struct {
				Entitled          bool   `json:"entitled"`
				Reason            string `json:"reason"`
				PriceMonthlyCents int64  `json:"price_monthly_cents"`
			}
			err := sc.Get(cmd.Context(), "/v1/agents/"+agentID+"/channels/"+channel+"/entitlement", &ent)
			if err != nil {
				if apiErr, ok := err.(*client.APIError); ok && apiErr.StatusCode == 402 {
					upgradeURL := dashboardAgentURL(cmd, agentID)
					price := "$20/mo"
					if ent.PriceMonthlyCents > 0 {
						price = fmt.Sprintf("$%.2f/mo", float64(ent.PriceMonthlyCents)/100)
					}
					fmt.Fprintf(os.Stderr,
						"\nThis agent isn't subscribed to %s yet (%s per agent).\n"+
							"Subscribe in the dashboard, then re-run this command:\n\n"+
							"  %s\n\n",
						channel, price, upgradeURL,
					)
					return fmt.Errorf("subscription required for %s on %s", channel, agentID)
				}
				// Soft-fail on other lookup errors — fall through to the
				// real connect, which will surface a more specific error.
			}

			token, _ := cmd.Flags().GetString("bot-token")
			if token == "" {
				if !stdinIsTTY() {
					return fmt.Errorf("--bot-token is required when stdin is not a terminal")
				}
				fmt.Println("To connect Telegram:")
				fmt.Println("  1. Open Telegram and message @BotFather")
				fmt.Println("  2. Send /newbot, choose a name and username")
				fmt.Println("  3. Copy the bot token")
				fmt.Println()
				fmt.Print("Paste bot token: ")

				reader := bufio.NewReader(os.Stdin)
				line, err := reader.ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("reading bot token: %w", err)
				}
				token = strings.TrimSpace(line)
			}
			if token == "" {
				return fmt.Errorf("bot token is required")
			}
			body["bot_token"] = token
		}

		if !jsonOutput {
			fmt.Fprintf(os.Stderr, "Connecting %s to %s\n", channel, agentID)
		}

		var result operationSubmissionResponse
		if err := sc.Post(cmd.Context(), "/v1/agents/"+agentID+"/channels/"+channel, body, &result); err != nil {
			// HTTP 402 → paywalled feature. The dashboard owns
			// subscription flow (Stripe Checkout needs a browser);
			// surface a clean message and the upgrade URL instead of
			// a raw API error.
			if apiErr, ok := err.(*client.APIError); ok && apiErr.StatusCode == 402 {
				upgradeURL := dashboardAgentURL(cmd, agentID)
				fmt.Fprintf(os.Stderr,
					"\nThis agent is not subscribed to %s ($20/mo per agent).\n"+
						"Subscribe in the dashboard, then re-run this command:\n"+
						"  %s\n\n",
					channel, upgradeURL,
				)
				return fmt.Errorf("subscription required for %s on %s", channel, agentID)
			}
			return err
		}

		if noWait || result.Operation == nil {
			note := ""
			if result.Operation != nil {
				note = "Operation: " + result.Operation.ID
			}
			renderAsyncFallback(os.Stdout, jsonOutput, agentID, channel+" connect", note)
			return nil
		}

		agent, err := waitForOperation(cmd, sc, agentID, result.Operation, channel+" connect")
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

		fmt.Printf("%s connected to %s.\n", channel, agentID)
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

		if err := confirmDestructive(cmd, fmt.Sprintf("Disconnect %s from %s", args[1], args[0])); err != nil {
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
