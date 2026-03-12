package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

type secretStoreInfo struct {
	ID              string   `json:"id"`
	OrgID           string   `json:"orgId"`
	Name            string   `json:"name"`
	EgressAllowlist []string `json:"egressAllowlist"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
}

var secretStoreCmd = &cobra.Command{
	Use:   "secret-store",
	Short: "Manage secret stores",
}

var secretStoreCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new secret store",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		name, _ := cmd.Flags().GetString("name")
		allowlist, _ := cmd.Flags().GetStringSlice("egress-allowlist")

		if name == "" {
			return fmt.Errorf("--name is required")
		}

		body := map[string]interface{}{"name": name}
		if len(allowlist) > 0 {
			body["egressAllowlist"] = allowlist
		}

		var store secretStoreInfo
		if err := c.Post(cmd.Context(), "/secret-stores", body, &store); err != nil {
			return err
		}

		printer.Print(store, func() {
			fmt.Printf("Created secret store %s (id: %s)\n", store.Name, store.ID)
		})
		return nil
	},
}

var secretStoreListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List secret stores",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var stores []secretStoreInfo
		if err := c.Get(cmd.Context(), "/secret-stores", &stores); err != nil {
			return err
		}

		printer.Print(stores, func() {
			if len(stores) == 0 {
				fmt.Println("No secret stores found.")
				return
			}
			headers := []string{"ID", "NAME", "EGRESS ALLOWLIST", "CREATED"}
			var rows [][]string
			for _, s := range stores {
				created := s.CreatedAt
				if t, err := time.Parse(time.RFC3339Nano, s.CreatedAt); err == nil {
					created = time.Since(t).Truncate(time.Second).String() + " ago"
				}
				egress := "(all)"
				if len(s.EgressAllowlist) > 0 {
					egress = strings.Join(s.EgressAllowlist, ", ")
				}
				rows = append(rows, []string{
					s.ID,
					s.Name,
					egress,
					created,
				})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}

var secretStoreGetCmd = &cobra.Command{
	Use:   "get <store-id>",
	Short: "Get secret store details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var store secretStoreInfo
		if err := c.Get(cmd.Context(), "/secret-stores/"+args[0], &store); err != nil {
			return err
		}

		printer.Print(store, func() {
			fmt.Printf("ID:      %s\n", store.ID)
			fmt.Printf("Name:    %s\n", store.Name)
			if len(store.EgressAllowlist) > 0 {
				fmt.Printf("Egress:  %s\n", strings.Join(store.EgressAllowlist, ", "))
			}
		})
		return nil
	},
}

var secretStoreUpdateCmd = &cobra.Command{
	Use:   "update <store-id>",
	Short: "Update a secret store",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		body := map[string]interface{}{}
		if cmd.Flags().Changed("name") {
			v, _ := cmd.Flags().GetString("name")
			body["name"] = v
		}
		if cmd.Flags().Changed("egress-allowlist") {
			v, _ := cmd.Flags().GetStringSlice("egress-allowlist")
			body["egressAllowlist"] = v
		}

		if len(body) == 0 {
			return fmt.Errorf("no fields to update (use --name, --egress-allowlist)")
		}

		var store secretStoreInfo
		if err := c.PutJSON(cmd.Context(), "/secret-stores/"+args[0], body, &store); err != nil {
			return err
		}

		printer.Print(store, func() {
			fmt.Printf("Updated secret store %s\n", store.Name)
		})
		return nil
	},
}

var secretStoreDeleteCmd = &cobra.Command{
	Use:   "delete <store-id>",
	Short: "Delete a secret store and all its secrets",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		if err := c.Delete(cmd.Context(), "/secret-stores/"+args[0]); err != nil {
			return err
		}
		fmt.Printf("Secret store %s deleted.\n", args[0])
		return nil
	},
}

func init() {
	secretStoreCreateCmd.Flags().String("name", "", "Store name (required)")
	secretStoreCreateCmd.Flags().StringSlice("egress-allowlist", nil, "Allowed egress hosts")

	secretStoreUpdateCmd.Flags().String("name", "", "Store name")
	secretStoreUpdateCmd.Flags().StringSlice("egress-allowlist", nil, "Allowed egress hosts")

	secretStoreCmd.AddCommand(secretStoreCreateCmd)
	secretStoreCmd.AddCommand(secretStoreListCmd)
	secretStoreCmd.AddCommand(secretStoreGetCmd)
	secretStoreCmd.AddCommand(secretStoreUpdateCmd)
	secretStoreCmd.AddCommand(secretStoreDeleteCmd)
}
