package commands

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

const (
	pollInterval = 2 * time.Second
	pollTimeout  = 180 * time.Second
)

func waitForOperation(
	cmd *cobra.Command,
	sc *client.Client,
	agentID string,
	operation *AgentOperation,
	opLabel string,
) (*agentResponse, error) {
	if operation == nil {
		return nil, fmt.Errorf("%s did not return an operation handle", opLabel)
	}

	deadline := time.Now().Add(pollTimeout)
	lastProgress := ""
	consecutiveErrors := 0
	const errorThreshold = 3

	printProgress := func(progress string) {
		if !jsonOutput && progress != "" {
			fmt.Fprintf(os.Stderr, "  ⋯ %s\n", progress)
		}
	}

	if progress := operationProgress(*operation); progress != "" {
		printProgress(progress)
		lastProgress = progress
	}

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		current, err := fetchOperation(cmd, sc, agentID, operation.ID)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= errorThreshold {
				renderAsyncFallback(os.Stdout, jsonOutput, agentID, opLabel+" still in progress", lastProgress)
				return nil, nil
			}
			continue
		}
		consecutiveErrors = 0

		progress := operationProgress(current)
		if progress != "" && progress != lastProgress {
			printProgress(progress)
			lastProgress = progress
		}

		switch current.Status {
		case "queued", "running":
			continue
		case "succeeded":
			agent, err := fetchAgent(cmd, sc, agentID)
			if err != nil {
				return nil, fmt.Errorf("%s completed but failed to fetch agent state: %w", opLabel, err)
			}
			return agent, nil
		case "failed", "canceled":
			agent, err := fetchAgent(cmd, sc, agentID)
			if err != nil {
				return nil, fmt.Errorf("%s failed (unable to fetch agent state: %w)", opLabel, err)
			}
			lastErr := effectiveLastError(agent, &current)
			if !jsonOutput {
				fmt.Fprintln(os.Stderr, "  ✗ "+opLabel+" failed")
				fmt.Fprintln(os.Stderr)
				RenderLastError(os.Stderr, lastErr)
			} else {
				agent.LastError = lastErr
				printer.Print(agent, func() {})
			}
			return nil, &ExitError{Code: ExitCodeFor(lastErr)}
		default:
			continue
		}
	}

	renderAsyncFallback(os.Stdout, jsonOutput, agentID, opLabel+" still in progress", lastProgress)
	return nil, nil
}

func fetchAgent(cmd *cobra.Command, sc *client.Client, agentID string) (*agentResponse, error) {
	var agent agentResponse
	if err := sc.Get(cmd.Context(), "/v1/agents/"+agentID, &agent); err != nil {
		return nil, err
	}
	return &agent, nil
}

func fetchOperation(cmd *cobra.Command, sc *client.Client, agentID, operationID string) (AgentOperation, error) {
	var operation AgentOperation
	err := sc.Get(cmd.Context(), "/v1/agents/"+agentID+"/operations/"+operationID, &operation)
	return operation, err
}

func effectiveLastError(agent *agentResponse, operation *AgentOperation) *LastError {
	if agent != nil && agent.LastError != nil {
		return agent.LastError
	}
	if operation == nil {
		return nil
	}

	phase := ""
	if operation.Phase != nil {
		phase = *operation.Phase
	}
	message := operationProgress(*operation)
	code := ""
	if operation.Code != nil {
		code = *operation.Code
	}
	at := operation.UpdatedAt
	if operation.CompletedAt != nil && *operation.CompletedAt != "" {
		at = *operation.CompletedAt
	}

	return &LastError{
		Phase:   phase,
		Message: message,
		Code:    code,
		At:      at,
	}
}

func operationProgress(operation AgentOperation) string {
	if operation.Message != nil {
		if trimmed := strings.TrimSpace(*operation.Message); trimmed != "" {
			return trimmed
		}
	}
	if operation.Phase != nil {
		if trimmed := strings.TrimSpace(*operation.Phase); trimmed != "" {
			return humanizePhase(trimmed)
		}
	}
	return humanizePhase(operation.Kind)
}

func humanizePhase(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}
