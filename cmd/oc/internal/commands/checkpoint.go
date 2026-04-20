package commands

import (
	"fmt"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/opensandbox/opensandbox/pkg/types"
	"github.com/spf13/cobra"
)

// CheckpointInfo matches the API response for checkpoints.
type CheckpointInfo struct {
	ID              string    `json:"id"`
	SandboxID       string    `json:"sandboxId"`
	OrgID           string    `json:"orgId"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	RootfsS3Key     string    `json:"rootfsS3Key,omitempty"`
	WorkspaceS3Key  string    `json:"workspaceS3Key,omitempty"`
	SizeBytes       int64     `json:"sizeBytes"`
	IsPublic        bool      `json:"isPublic"`
	CreatedAt       time.Time `json:"createdAt"`
}

var checkpointCmd = &cobra.Command{
	Use:     "checkpoint",
	Aliases: []string{"cp"},
	Short:   "Manage sandbox checkpoints",
}

var checkpointCreateCmd = &cobra.Command{
	Use:   "create <sandbox-id>",
	Short: "Create a checkpoint of a running sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		name, _ := cmd.Flags().GetString("name")

		req := map[string]string{"name": name}
		var cp CheckpointInfo
		if err := c.Post(cmd.Context(), fmt.Sprintf("/sandboxes/%s/checkpoints", args[0]), req, &cp); err != nil {
			return err
		}

		printer.Print(cp, func() {
			fmt.Printf("Checkpoint created: %s (status: %s)\n", cp.ID, cp.Status)
			fmt.Printf("Name: %s\n", cp.Name)
		})
		return nil
	},
}

var checkpointListCmd = &cobra.Command{
	Use:   "list <sandbox-id>",
	Short: "List checkpoints for a sandbox",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var checkpoints []CheckpointInfo
		if err := c.Get(cmd.Context(), fmt.Sprintf("/sandboxes/%s/checkpoints", args[0]), &checkpoints); err != nil {
			return err
		}

		printer.Print(checkpoints, func() {
			if len(checkpoints) == 0 {
				fmt.Println("No checkpoints found.")
				return
			}
			headers := []string{"ID", "NAME", "STATUS", "SIZE", "CREATED"}
			var rows [][]string
			for _, cp := range checkpoints {
				size := fmt.Sprintf("%.1f MB", float64(cp.SizeBytes)/1024/1024)
				if cp.SizeBytes == 0 {
					size = "-"
				}
				rows = append(rows, []string{
					cp.ID,
					cp.Name,
					cp.Status,
					size,
					cp.CreatedAt.Format(time.RFC3339),
				})
			}
			printer.Table(headers, rows)
		})
		return nil
	},
}

var checkpointRestoreCmd = &cobra.Command{
	Use:   "restore <sandbox-id> <checkpoint-id>",
	Short: "Restore a sandbox to a checkpoint (in-place revert)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())

		var result map[string]string
		if err := c.Post(cmd.Context(), fmt.Sprintf("/sandboxes/%s/checkpoints/%s/restore", args[0], args[1]), nil, &result); err != nil {
			return err
		}

		printer.Print(result, func() {
			fmt.Printf("Sandbox %s restored to checkpoint %s\n", args[0], args[1])
		})
		return nil
	},
}

var checkpointSpawnCmd = &cobra.Command{
	Use:   "spawn <checkpoint-id>",
	Short: "Create a new sandbox from a checkpoint",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		timeout, _ := cmd.Flags().GetInt("timeout")

		req := map[string]int{"timeout": timeout}
		var sandbox types.Sandbox
		if err := c.Post(cmd.Context(), fmt.Sprintf("/sandboxes/from-checkpoint/%s", args[0]), req, &sandbox); err != nil {
			return err
		}

		printer.Print(sandbox, func() {
			fmt.Printf("Created sandbox %s from checkpoint (status: %s)\n", sandbox.ID, sandbox.Status)
			if sandbox.ConnectURL != "" {
				fmt.Printf("Connect URL: %s\n", sandbox.ConnectURL)
			}
		})
		return nil
	},
}

var checkpointDeleteCmd = &cobra.Command{
	Use:   "delete <sandbox-id> <checkpoint-id>",
	Short: "Delete a checkpoint",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c := client.FromContext(cmd.Context())
		if err := c.DeleteIgnoreNotFound(cmd.Context(), fmt.Sprintf("/sandboxes/%s/checkpoints/%s", args[0], args[1])); err != nil {
			return err
		}
		fmt.Printf("Checkpoint %s deleted.\n", args[1])
		return nil
	},
}

// checkpointPublishCmd marks a checkpoint as publicly forkable across orgs
// (design 009). The owner org still keeps exclusive control of patches and
// deletion; publish only affects the fork auth gate.
var checkpointPublishCmd = &cobra.Command{
	Use:   "publish <checkpoint-id>",
	Short: "Mark a checkpoint as publicly forkable by any org",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setCheckpointPublic(cmd, args[0], true)
	},
}

var checkpointUnpublishCmd = &cobra.Command{
	Use:   "unpublish <checkpoint-id>",
	Short: "Revoke public forkability of a checkpoint",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return setCheckpointPublic(cmd, args[0], false)
	},
}

func setCheckpointPublic(cmd *cobra.Command, id string, isPublic bool) error {
	c := client.FromContext(cmd.Context())
	verb := "publish"
	if !isPublic {
		verb = "unpublish"
	}
	var cp CheckpointInfo
	if err := c.Post(cmd.Context(), fmt.Sprintf("/sandboxes/checkpoints/%s/%s", id, verb), nil, &cp); err != nil {
		return err
	}
	printer.Print(cp, func() {
		fmt.Printf("Checkpoint %s is_public=%t\n", cp.ID, cp.IsPublic)
	})
	return nil
}

func init() {
	checkpointCreateCmd.Flags().String("name", "", "Checkpoint name (required)")
	checkpointCreateCmd.MarkFlagRequired("name")

	checkpointSpawnCmd.Flags().Int("timeout", 0, "Idle timeout in seconds before auto-hibernate (0 = never hibernate)")

	checkpointCmd.AddCommand(checkpointCreateCmd)
	checkpointCmd.AddCommand(checkpointListCmd)
	checkpointCmd.AddCommand(checkpointRestoreCmd)
	checkpointCmd.AddCommand(checkpointSpawnCmd)
	checkpointCmd.AddCommand(checkpointDeleteCmd)
	checkpointCmd.AddCommand(checkpointPublishCmd)
	checkpointCmd.AddCommand(checkpointUnpublishCmd)
}
