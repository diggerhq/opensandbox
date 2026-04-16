package commands

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

var agentCreateCmd = &cobra.Command{
	Use:   "create <id>",
	Short: "Create a new managed agent",
	Long: "Create a new managed agent. A core (e.g. --core hermes) is required:\n" +
		"without one, the agent has no runtime and cannot connect channels\n" +
		"or install packages.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}

		id := args[0]
		core, _ := cmd.Flags().GetString("core")
		secretSlice, _ := cmd.Flags().GetStringSlice("secret")
		noWait, _ := cmd.Flags().GetBool("no-wait")

		body := map[string]interface{}{
			"id":   id,
			"core": core,
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

		// Text-mode preamble (suppressed in --json mode — scripts only want
		// the final JSON object).
		if !jsonOutput {
			fmt.Fprintf(os.Stderr, "Creating agent %s", agent.ID)
			if agent.Core != nil {
				fmt.Fprintf(os.Stderr, " (core: %s)", *agent.Core)
			}
			fmt.Fprintln(os.Stderr)
			fmt.Fprintln(os.Stderr, "  ✓ Agent record created")
		}

		// --no-wait short-circuits into Mode 3 (async fallback). Scripts
		// that don't want to block use this path.
		if noWait {
			renderAsyncFallback(os.Stdout, jsonOutput, id, "Instance provisioning", "")
			return nil
		}

		return pollUntilTerminal(cmd, sc, id, "Instance creation")
	},
}

// ── Polling ──

// Poll cadence for async operations. 2s is fast enough that humans don't
// notice, slow enough not to hammer the API. The 180s cap is a generous
// bound on "it's almost certainly still working" before falling back to
// the async message.
const (
	pollInterval = 2 * time.Second
	pollTimeout  = 180 * time.Second
)

// pollUntilTerminal polls GET /v1/agents/:id until status reaches a terminal
// state (running / error), the deadline hits, or a persistent network error
// occurs. One of three Mode outcomes results:
//   - Mode 1 (running):       success block, exit 0
//   - Mode 2 (error):         error block, ExitError with class-based code
//   - Mode 3 (timeout / err): async-fallback message, exit 0
//
// See ws-gstack/work/agent-error-visibility.md — "Three outcome modes".
func pollUntilTerminal(cmd *cobra.Command, sc *client.Client, agentID, op string) error {
	deadline := time.Now().Add(pollTimeout)

	// Print phase progress only in text mode. JSON mode suppresses stderr
	// progress so consumers get exactly one object on stdout.
	printProgress := func(phase string) {
		if !jsonOutput {
			fmt.Fprintf(os.Stderr, "  ⋯ %s\n", phase)
		}
	}

	lastPhase := ""
	consecutiveErrors := 0
	const errorThreshold = 3 // tolerate transient blips before declaring the poll dead

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		var agent agentResponse
		if err := sc.Get(cmd.Context(), "/v1/agents/"+agentID, &agent); err != nil {
			consecutiveErrors++
			if consecutiveErrors >= errorThreshold {
				// Poll lost connection; Mode 3 fallback. Work may still be
				// running in sessions-api — the user just can't observe it
				// from here.
				renderAsyncFallback(os.Stdout, jsonOutput, agentID, op+" still in progress", "")
				return nil
			}
			continue
		}
		consecutiveErrors = 0

		status := ""
		if agent.Status != nil {
			status = *agent.Status
		}

		switch status {
		case "running":
			// Mode 1 — success.
			if !jsonOutput {
				fmt.Fprintln(os.Stderr, "  ✓ Ready")
			}
			printer.Print(agent, func() {})
			return nil

		case "error":
			// Mode 2 — failure. Render the error block and exit with the
			// class-mapped code.
			if !jsonOutput {
				fmt.Fprintln(os.Stderr, "  ✗ "+op+" failed")
				fmt.Fprintln(os.Stderr)
				RenderLastError(os.Stderr, agent.LastError)
			} else {
				printer.Print(agent, func() {})
			}
			return &ExitError{Code: ExitCodeFor(agent.LastError)}

		case "creating", "":
			// Still working. Report phase progress from packageStatus /
			// channelStatus in a future pass; for now the instance status
			// alone is enough to reassure the user something's happening.
			phase := "Provisioning instance"
			if phase != lastPhase {
				printProgress(phase)
				lastPhase = phase
			}
		}
	}

	// Mode 3 — poll hit the cap. Work is likely still running.
	renderAsyncFallback(os.Stdout, jsonOutput, agentID, op+" still in progress", "")
	return nil
}
