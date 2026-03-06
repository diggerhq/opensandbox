package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
)

var sandboxCmd = &cobra.Command{
	Use:   "sandbox",
	Short: "Manage sandboxes",
}

var sandboxCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new sandbox",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		timeout, _ := cmd.Flags().GetInt("timeout")
		cpu, _ := cmd.Flags().GetInt("cpu")
		memory, _ := cmd.Flags().GetInt("memory")
		envSlice, _ := cmd.Flags().GetStringSlice("env")
		metaSlice, _ := cmd.Flags().GetStringSlice("metadata")

		config := types.SandboxConfig{
			Timeout:  timeout,
			CpuCount: cpu,
			MemoryMB: memory,
			Envs:     parseKVSlice(envSlice),
			Metadata: parseKVSlice(metaSlice),
		}

		var sandbox types.Sandbox
		if err := c.Post(cmd.Context(), "/sandboxes", config, &sandbox); err != nil {
			return err
		}

		printer.Print(sandbox, func() {
			fmt.Printf("Created sandbox %s (status: %s)\n", sandbox.ID, sandbox.Status)
		})
		return nil
	},
}

var sandboxListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List sandboxes",
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var sandboxes []types.Sandbox
		if err := c.Get(cmd.Context(), "/sandboxes", &sandboxes); err != nil {
			return err
		}

		printer.Print(sandboxes, func() {
			if len(sandboxes) == 0 {
				fmt.Println("No sandboxes found.")
				return
			}
			headers := []string{"ID", "TEMPLATE", "STATUS", "CPU", "MEM", "AGE"}
			var rows [][]string
			for _, s := range sandboxes {
				age := time.Since(s.StartedAt).Truncate(time.Second).String()
				rows = append(rows, []string{
					s.ID,
					s.Template,
					string(s.Status),
					fmt.Sprintf("%d", s.CpuCount),
					fmt.Sprintf("%dMB", s.MemoryMB),
					age,
				})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}

var sandboxGetCmd = &cobra.Command{
	Use:   "get <sandbox-id>",
	Short: "Get sandbox details",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var sandbox types.Sandbox
		if err := c.Get(cmd.Context(), "/sandboxes/"+args[0], &sandbox); err != nil {
			return err
		}

		printer.Print(sandbox, func() {
			fmt.Printf("ID:        %s\n", sandbox.ID)
			fmt.Printf("Template:  %s\n", sandbox.Template)
			fmt.Printf("Status:    %s\n", sandbox.Status)
			fmt.Printf("CPU:       %d\n", sandbox.CpuCount)
			fmt.Printf("Memory:    %dMB\n", sandbox.MemoryMB)
			fmt.Printf("Started:   %s\n", sandbox.StartedAt.Format(time.RFC3339))
			fmt.Printf("Ends:      %s\n", sandbox.EndAt.Format(time.RFC3339))
		})
		return nil
	},
}

var sandboxKillCmd = &cobra.Command{
	Use:   "kill <sandbox-id>",
	Short: "Kill and remove a sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		if err := c.Delete(cmd.Context(), "/sandboxes/"+args[0]); err != nil {
			return err
		}
		fmt.Printf("Sandbox %s killed.\n", args[0])
		return nil
	},
}

var sandboxHibernateCmd = &cobra.Command{
	Use:   "hibernate <sandbox-id>",
	Short: "Hibernate a sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var info types.HibernationInfo
		if err := c.Post(cmd.Context(), "/sandboxes/"+args[0]+"/hibernate", nil, &info); err != nil {
			return err
		}

		printer.Print(info, func() {
			fmt.Printf("Sandbox %s hibernated.\n", args[0])
			if info.SizeBytes > 0 {
				fmt.Printf("Size: %.1f MB\n", float64(info.SizeBytes)/1024/1024)
			}
		})
		return nil
	},
}

var sandboxWakeCmd = &cobra.Command{
	Use:   "wake <sandbox-id>",
	Short: "Wake a hibernated sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		timeout, _ := cmd.Flags().GetInt("timeout")

		req := types.WakeRequest{Timeout: timeout}
		var sandbox types.Sandbox
		if err := c.Post(cmd.Context(), "/sandboxes/"+args[0]+"/wake", req, &sandbox); err != nil {
			return err
		}

		printer.Print(sandbox, func() {
			fmt.Printf("Sandbox %s woke up (status: %s)\n", sandbox.ID, sandbox.Status)
		})
		return nil
	},
}

var sandboxSetTimeoutCmd = &cobra.Command{
	Use:   "set-timeout <sandbox-id> <seconds>",
	Short: "Update sandbox timeout",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var timeout int
		if _, err := fmt.Sscanf(args[1], "%d", &timeout); err != nil {
			return fmt.Errorf("invalid timeout: %s", args[1])
		}

		req := types.TimeoutRequest{Timeout: timeout}
		if err := c.Post(cmd.Context(), "/sandboxes/"+args[0]+"/timeout", req, nil); err != nil {
			return err
		}

		fmt.Printf("Timeout updated to %ds for sandbox %s\n", timeout, args[0])
		return nil
	},
}

// Top-level shortcuts
var createShortcut = &cobra.Command{
	Use:   "create",
	Short: "Create a new sandbox (shortcut for 'sandbox create')",
	RunE:  sandboxCreateCmd.RunE,
}

var lsShortcut = &cobra.Command{
	Use:   "ls",
	Short: "List sandboxes (shortcut for 'sandbox list')",
	RunE:  sandboxListCmd.RunE,
}

func init() {
	// sandbox create flags
	for _, cmd := range []*cobra.Command{sandboxCreateCmd, createShortcut} {
		cmd.Flags().Int("timeout", 300, "Timeout in seconds")
		cmd.Flags().Int("cpu", 0, "CPU count")
		cmd.Flags().Int("memory", 0, "Memory in MB")
		cmd.Flags().StringSlice("env", nil, "Environment variables (KEY=VALUE)")
		cmd.Flags().StringSlice("metadata", nil, "Metadata (KEY=VALUE)")
	}

	// sandbox wake flags
	sandboxWakeCmd.Flags().Int("timeout", 300, "Timeout in seconds after wake")

	sandboxCmd.AddCommand(sandboxCreateCmd)
	sandboxCmd.AddCommand(sandboxListCmd)
	sandboxCmd.AddCommand(sandboxGetCmd)
	sandboxCmd.AddCommand(sandboxKillCmd)
	sandboxCmd.AddCommand(sandboxHibernateCmd)
	sandboxCmd.AddCommand(sandboxWakeCmd)
	sandboxCmd.AddCommand(sandboxSetTimeoutCmd)
}

func parseKVSlice(kvs []string) map[string]string {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]string)
	for _, kv := range kvs {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}
