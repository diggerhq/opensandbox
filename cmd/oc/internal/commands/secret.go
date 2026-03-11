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
	Short: "Manage project secrets",
}

var secretSetCmd = &cobra.Command{
	Use:   "set <project-id> <name> [value]",
	Short: "Set a secret on a project (encrypted at rest)",
	Long: `Set a secret on a project. The value is encrypted at rest and injected
as an environment variable into sandboxes created with this project.

Value can be provided as:
  oc secret set <project-id> KEY value          # inline
  oc secret set <project-id> KEY --from-stdin   # from stdin (pipe-friendly)
  echo "val" | oc secret set <project-id> KEY --from-stdin`,
	Args: cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		projectID := args[0]
		name := args[1]
		fromStdin, _ := cmd.Flags().GetBool("from-stdin")

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

		body := map[string]string{"value": value}
		var result map[string]string
		if err := c.PutJSON(cmd.Context(), fmt.Sprintf("/projects/%s/secrets/%s", projectID, name), body, &result); err != nil {
			return err
		}

		fmt.Printf("Secret %s set on project %s\n", name, projectID)
		return nil
	},
}

var secretListCmd = &cobra.Command{
	Use:     "list <project-id>",
	Aliases: []string{"ls"},
	Short:   "List secret names for a project (values are never shown)",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var names []string
		if err := c.Get(cmd.Context(), fmt.Sprintf("/projects/%s/secrets", args[0]), &names); err != nil {
			return err
		}

		printer.Print(names, func() {
			if len(names) == 0 {
				fmt.Println("No secrets found.")
				return
			}
			for _, name := range names {
				fmt.Println(name)
			}
		})
		return nil
	},
}

var secretDeleteCmd = &cobra.Command{
	Use:   "delete <project-id> <name>",
	Short: "Delete a secret from a project",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		if err := c.DeleteIgnoreNotFound(cmd.Context(), fmt.Sprintf("/projects/%s/secrets/%s", args[0], args[1])); err != nil {
			return err
		}
		fmt.Printf("Secret %s deleted from project %s\n", args[1], args[0])
		return nil
	},
}

func init() {
	secretSetCmd.Flags().Bool("from-stdin", false, "Read secret value from stdin")

	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretDeleteCmd)
}
