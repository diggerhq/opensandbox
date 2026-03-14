package commands

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secrets in a secret store",
}

var secretSetCmd = &cobra.Command{
	Use:   "set <store-id> <name> [value]",
	Short: "Set a secret in a store (encrypted at rest)",
	Long: `Set a secret in a secret store. The value is encrypted at rest and injected
as an environment variable into sandboxes created with this store.

Value can be provided as:
  oc secret set <store-id> KEY value                          # inline
  oc secret set <store-id> KEY --from-stdin                   # from stdin
  oc secret set <store-id> KEY value --allowed-hosts api.x.com  # with host restriction

When --allowed-hosts is set, the secret is only substituted in outbound
HTTPS requests to matching hosts. Supports wildcards (*.example.com).`,
	Args: cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		storeID := args[0]
		name := args[1]
		fromStdin, _ := cmd.Flags().GetBool("from-stdin")
		allowedHosts, _ := cmd.Flags().GetStringSlice("allowed-hosts")

		var value string
		if fromStdin {
			scanner := bufio.NewScanner(os.Stdin)
			var lines []string
			for scanner.Scan() {
				lines = append(lines, scanner.Text())
			}
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("reading stdin: %w", err)
			}
			value = strings.Join(lines, "\n")
		} else if len(args) == 3 {
			value = args[2]
		} else {
			return fmt.Errorf("provide value as argument or use --from-stdin")
		}

		if value == "" {
			return fmt.Errorf("secret value cannot be empty")
		}

		body := map[string]interface{}{"value": value}
		if len(allowedHosts) > 0 {
			body["allowedHosts"] = allowedHosts
		}

		var result map[string]string
		if err := c.PutJSON(cmd.Context(), fmt.Sprintf("/secret-stores/%s/secrets/%s", storeID, name), body, &result); err != nil {
			return err
		}

		if len(allowedHosts) > 0 {
			fmt.Printf("Secret %s set on store %s (allowed hosts: %s)\n", name, storeID, strings.Join(allowedHosts, ", "))
		} else {
			fmt.Printf("Secret %s set on store %s\n", name, storeID)
		}
		return nil
	},
}

type secretEntryInfo struct {
	Name         string   `json:"name"`
	AllowedHosts []string `json:"allowedHosts"`
}

var secretListCmd = &cobra.Command{
	Use:     "list <store-id>",
	Aliases: []string{"ls"},
	Short:   "List secrets in a store (values are never shown)",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var entries []secretEntryInfo
		if err := c.Get(cmd.Context(), fmt.Sprintf("/secret-stores/%s/secrets", args[0]), &entries); err != nil {
			return err
		}

		printer.Print(entries, func() {
			if len(entries) == 0 {
				fmt.Println("No secrets found.")
				return
			}
			headers := []string{"NAME", "ALLOWED HOSTS"}
			var rows [][]string
			for _, e := range entries {
				hosts := "(all)"
				if len(e.AllowedHosts) > 0 {
					hosts = strings.Join(e.AllowedHosts, ", ")
				}
				rows = append(rows, []string{e.Name, hosts})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <store-id> <name>",
	Short: "Delete a secret from a store",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		if err := c.DeleteIgnoreNotFound(cmd.Context(), fmt.Sprintf("/secret-stores/%s/secrets/%s", args[0], args[1])); err != nil {
			return err
		}
		fmt.Printf("Secret %s deleted from store %s\n", args[1], args[0])
		return nil
	},
}

func init() {
	secretSetCmd.Flags().Bool("from-stdin", false, "Read secret value from stdin")
	secretSetCmd.Flags().StringSlice("allowed-hosts", nil, "Hosts where this secret can be substituted (e.g. api.anthropic.com)")

	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretDeleteCmd)
}
