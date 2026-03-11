package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

type projectInfo struct {
	ID              string   `json:"id"`
	OrgID           string   `json:"orgId"`
	Name            string   `json:"name"`
	Template        string   `json:"template"`
	CpuCount        int      `json:"cpuCount"`
	MemoryMB        int      `json:"memoryMB"`
	TimeoutSec      int      `json:"timeoutSec"`
	EgressAllowlist []string `json:"egressAllowlist"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
}

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

var projectCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new project",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		name, _ := cmd.Flags().GetString("name")
		template, _ := cmd.Flags().GetString("template")
		cpu, _ := cmd.Flags().GetInt("cpu")
		memory, _ := cmd.Flags().GetInt("memory")
		timeout, _ := cmd.Flags().GetInt("timeout")
		allowlist, _ := cmd.Flags().GetStringSlice("egress-allowlist")

		if name == "" {
			return fmt.Errorf("--name is required")
		}

		body := map[string]interface{}{"name": name}
		if template != "" {
			body["template"] = template
		}
		if cpu > 0 {
			body["cpuCount"] = cpu
		}
		if memory > 0 {
			body["memoryMB"] = memory
		}
		if timeout > 0 {
			body["timeoutSec"] = timeout
		}
		if len(allowlist) > 0 {
			body["egressAllowlist"] = allowlist
		}

		var project projectInfo
		if err := c.Post(cmd.Context(), "/projects", body, &project); err != nil {
			return err
		}

		printer.Print(project, func() {
			fmt.Printf("Created project %s (id: %s)\n", project.Name, project.ID)
		})
		return nil
	},
}

var projectListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var projects []projectInfo
		if err := c.Get(cmd.Context(), "/projects", &projects); err != nil {
			return err
		}

		printer.Print(projects, func() {
			if len(projects) == 0 {
				fmt.Println("No projects found.")
				return
			}
			headers := []string{"ID", "NAME", "TEMPLATE", "CPU", "MEM", "TIMEOUT", "CREATED"}
			var rows [][]string
			for _, p := range projects {
				created := p.CreatedAt
				if t, err := time.Parse(time.RFC3339Nano, p.CreatedAt); err == nil {
					created = time.Since(t).Truncate(time.Second).String() + " ago"
				}
				rows = append(rows, []string{
					p.ID,
					p.Name,
					p.Template,
					fmt.Sprintf("%d", p.CpuCount),
					fmt.Sprintf("%dMB", p.MemoryMB),
					fmt.Sprintf("%ds", p.TimeoutSec),
					created,
				})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}

var projectGetCmd = &cobra.Command{
	Use:   "get <project-id>",
	Short: "Get project details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var project projectInfo
		if err := c.Get(cmd.Context(), "/projects/"+args[0], &project); err != nil {
			return err
		}

		printer.Print(project, func() {
			fmt.Printf("ID:        %s\n", project.ID)
			fmt.Printf("Name:      %s\n", project.Name)
			fmt.Printf("Template:  %s\n", project.Template)
			fmt.Printf("CPU:       %d\n", project.CpuCount)
			fmt.Printf("Memory:    %dMB\n", project.MemoryMB)
			fmt.Printf("Timeout:   %ds\n", project.TimeoutSec)
			if len(project.EgressAllowlist) > 0 {
				fmt.Printf("Egress:    %s\n", strings.Join(project.EgressAllowlist, ", "))
			}
		})
		return nil
	},
}

var projectUpdateCmd = &cobra.Command{
	Use:   "update <project-id>",
	Short: "Update a project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		body := map[string]interface{}{}
		if cmd.Flags().Changed("name") {
			v, _ := cmd.Flags().GetString("name")
			body["name"] = v
		}
		if cmd.Flags().Changed("template") {
			v, _ := cmd.Flags().GetString("template")
			body["template"] = v
		}
		if cmd.Flags().Changed("cpu") {
			v, _ := cmd.Flags().GetInt("cpu")
			body["cpuCount"] = v
		}
		if cmd.Flags().Changed("memory") {
			v, _ := cmd.Flags().GetInt("memory")
			body["memoryMB"] = v
		}
		if cmd.Flags().Changed("timeout") {
			v, _ := cmd.Flags().GetInt("timeout")
			body["timeoutSec"] = v
		}
		if cmd.Flags().Changed("egress-allowlist") {
			v, _ := cmd.Flags().GetStringSlice("egress-allowlist")
			body["egressAllowlist"] = v
		}

		if len(body) == 0 {
			return fmt.Errorf("no fields to update (use --name, --template, --cpu, --memory, --timeout, --egress-allowlist)")
		}

		var project projectInfo
		if err := c.PutJSON(cmd.Context(), "/projects/"+args[0], body, &project); err != nil {
			return err
		}

		printer.Print(project, func() {
			fmt.Printf("Updated project %s\n", project.Name)
		})
		return nil
	},
}

var projectDeleteCmd = &cobra.Command{
	Use:   "delete <project-id>",
	Short: "Delete a project and all its secrets",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		if err := c.Delete(cmd.Context(), "/projects/"+args[0]); err != nil {
			return err
		}
		fmt.Printf("Project %s deleted.\n", args[0])
		return nil
	},
}

func init() {
	// project create flags
	projectCreateCmd.Flags().String("name", "", "Project name (required)")
	projectCreateCmd.Flags().String("template", "", "Default template")
	projectCreateCmd.Flags().Int("cpu", 0, "Default CPU count")
	projectCreateCmd.Flags().Int("memory", 0, "Default memory in MB")
	projectCreateCmd.Flags().Int("timeout", 0, "Default timeout in seconds")
	projectCreateCmd.Flags().StringSlice("egress-allowlist", nil, "Allowed egress hosts")

	// project update flags (same as create)
	projectUpdateCmd.Flags().String("name", "", "Project name")
	projectUpdateCmd.Flags().String("template", "", "Default template")
	projectUpdateCmd.Flags().Int("cpu", 0, "Default CPU count")
	projectUpdateCmd.Flags().Int("memory", 0, "Default memory in MB")
	projectUpdateCmd.Flags().Int("timeout", 0, "Default timeout in seconds")
	projectUpdateCmd.Flags().StringSlice("egress-allowlist", nil, "Allowed egress hosts")

	projectCmd.AddCommand(projectCreateCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectGetCmd)
	projectCmd.AddCommand(projectUpdateCmd)
	projectCmd.AddCommand(projectDeleteCmd)
}
