package commands

import (
	"fmt"
	"sort"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

var sandboxAllowedHostsCmd = &cobra.Command{
	Use:   "allowed-hosts <sandbox-id>",
	Short: "Show the egress allowlist + per-secret allowed hosts for a sandbox",
	Long: `Show the egress allowlist (and any per-secret host restrictions) the
sandbox's secrets proxy enforces. Useful for debugging "why is my
outbound HTTP call being blocked" without having to cross-reference the
secret store config separately.

Sandboxes created without --secret-store have no per-store egress
restriction; this command reports an empty list for those.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		var resp struct {
			SandboxID             string              `json:"sandboxID"`
			SecretStore           string              `json:"secretStore"`
			EgressAllowlist       []string            `json:"egressAllowlist"`
			PerSecretAllowedHosts map[string][]string `json:"perSecretAllowedHosts"`
		}
		if err := c.Get(cmd.Context(), "/sandboxes/"+args[0]+"/allowed-hosts", &resp); err != nil {
			return err
		}

		printer.Print(resp, func() {
			if resp.SecretStore == "" {
				fmt.Printf("Sandbox %s has no secret store attached — no per-store egress restriction.\n", resp.SandboxID)
				return
			}
			fmt.Printf("Sandbox %s (secret store: %s)\n", resp.SandboxID, resp.SecretStore)

			fmt.Println()
			fmt.Println("Egress allowlist:")
			if len(resp.EgressAllowlist) == 0 {
				fmt.Println("  (empty — store has no allowlist; egress is governed by platform-level policy)")
			} else {
				for _, h := range resp.EgressAllowlist {
					fmt.Printf("  • %s\n", h)
				}
			}

			if len(resp.PerSecretAllowedHosts) > 0 {
				fmt.Println()
				fmt.Println("Per-secret allowed hosts:")
				// Sort keys for deterministic output
				names := make([]string, 0, len(resp.PerSecretAllowedHosts))
				for k := range resp.PerSecretAllowedHosts {
					names = append(names, k)
				}
				sort.Strings(names)
				for _, name := range names {
					hosts := resp.PerSecretAllowedHosts[name]
					fmt.Printf("  %s:\n", name)
					for _, h := range hosts {
						fmt.Printf("    • %s\n", h)
					}
				}
			}
		})
		return nil
	},
}

func init() {
	sandboxCmd.AddCommand(sandboxAllowedHostsCmd)
}
