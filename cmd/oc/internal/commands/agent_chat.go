package commands

// `oc agent send` (one-shot) and `oc agent chat` (interactive REPL).
//
// Both stream the SSE response from sessions-api's instance-message endpoint
// (POST /v1/agents/:id/instances/:instId/messages) chunk-by-chunk. The
// endpoint emits OpenAI-style frames: data: {"type":"text","content":"..."}
// per chunk, and data: {"type":"done"} on the last frame.
//
// `chat` keeps the full message history in memory and resends it every turn
// — OpenClaw's chat-completions is stateless, so context only persists if
// the client supplies it.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
)

type chatTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// resolveAgentInstance fetches the agent and returns its current instance
// ID. Returns a friendly error when the agent has no running instance.
func resolveAgentInstance(ctx context.Context, sc *client.Client, agentID string) (string, error) {
	var agent agentResponse
	if err := sc.Get(ctx, "/v1/agents/"+agentID, &agent); err != nil {
		return "", err
	}
	if agent.InstanceID == nil || *agent.InstanceID == "" {
		return "", fmt.Errorf("agent %s has no running instance yet (status=%s) — wait for it to come up and retry",
			agentID, derefOrDash(agent.Status))
	}
	return *agent.InstanceID, nil
}

func derefOrDash(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// streamChatTurn posts to /messages and prints text chunks as they arrive.
// Returns the assembled assistant text so callers can append it to a
// running history (used by `oc agent chat`).
func streamChatTurn(
	ctx context.Context,
	sc *client.Client,
	agentID, instanceID string,
	history []chatTurn,
	w io.Writer,
) (string, error) {
	body := map[string]interface{}{
		"messages": history,
	}
	resp, err := sc.PostStream(ctx, "/v1/agents/"+agentID+"/instances/"+instanceID+"/messages", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}

	var assembled strings.Builder
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(line, "data:") {
				if err == io.EOF {
					break
				}
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				if err == io.EOF {
					break
				}
				continue
			}
			var ev struct {
				Type    string `json:"type"`
				Content string `json:"content"`
				Message string `json:"message"`
				Code    string `json:"code"`
			}
			if jerr := json.Unmarshal([]byte(payload), &ev); jerr != nil {
				if err == io.EOF {
					break
				}
				continue
			}
			switch ev.Type {
			case "text":
				if ev.Content != "" {
					assembled.WriteString(ev.Content)
					_, _ = io.WriteString(w, ev.Content)
				}
			case "error":
				return assembled.String(), fmt.Errorf("agent error: %s (%s)", ev.Message, ev.Code)
			case "done":
				return assembled.String(), nil
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return assembled.String(), err
		}
	}
	return assembled.String(), nil
}

var agentSendCmd = &cobra.Command{
	Use:   "send <agent-id> <message>",
	Short: "Send one message to an agent and stream the reply",
	Long: "Send a single message to an agent and stream the reply to stdout.\n" +
		"Convenient for smoke-testing core/gateway changes from a shell:\n\n" +
		"  oc agent send my-bot 'hello'\n" +
		"  echo 'translate this to french: hello' | oc agent send my-bot",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}
		agentID := args[0]

		var message string
		if len(args) >= 2 {
			message = args[1]
		} else if !stdinIsTTY() {
			buf, rerr := io.ReadAll(os.Stdin)
			if rerr != nil {
				return rerr
			}
			message = strings.TrimRight(string(buf), "\n")
		}
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("message is required (positional arg or piped stdin)")
		}

		instanceID, err := resolveAgentInstance(cmd.Context(), sc, agentID)
		if err != nil {
			return err
		}

		history := []chatTurn{{Role: "user", Content: message}}
		_, err = streamChatTurn(cmd.Context(), sc, agentID, instanceID, history, os.Stdout)
		// Trailing newline so shell prompts return cleanly.
		fmt.Println()
		return err
	},
}

var agentChatCmd = &cobra.Command{
	Use:   "chat <agent-id>",
	Short: "Open an interactive chat with an agent",
	Long: "Open an interactive REPL chat with an agent. Each turn streams\n" +
		"the assistant reply live. Type ':exit' or send EOF (Ctrl-D) to quit.\n" +
		"History is kept client-side and resent every turn (OpenClaw's chat-\n" +
		"completions is stateless).",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sc, err := sessionsClient(cmd)
		if err != nil {
			return err
		}
		agentID := args[0]

		instanceID, err := resolveAgentInstance(cmd.Context(), sc, agentID)
		if err != nil {
			return err
		}

		fmt.Fprintf(os.Stderr, "Chat with %s — type ':exit' or Ctrl-D to quit.\n\n", agentID)

		history := []chatTurn{}
		reader := bufio.NewReader(os.Stdin)
		for {
			fmt.Fprint(os.Stderr, "you> ")
			line, rerr := reader.ReadString('\n')
			if rerr == io.EOF {
				if strings.TrimSpace(line) == "" {
					fmt.Fprintln(os.Stderr)
					return nil
				}
			} else if rerr != nil {
				return rerr
			}
			msg := strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(msg) == "" {
				if rerr == io.EOF {
					return nil
				}
				continue
			}
			if msg == ":exit" || msg == ":quit" {
				return nil
			}

			history = append(history, chatTurn{Role: "user", Content: msg})
			fmt.Fprint(os.Stderr, "bot> ")
			reply, serr := streamChatTurn(cmd.Context(), sc, agentID, instanceID, history, os.Stdout)
			fmt.Println()
			if serr != nil {
				// Drop the unanswered user turn so the next try doesn't
				// re-send the failed pair as context.
				history = history[:len(history)-1]
				fmt.Fprintf(os.Stderr, "  error: %v\n", serr)
				continue
			}
			history = append(history, chatTurn{Role: "assistant", Content: reply})

			if rerr == io.EOF {
				return nil
			}
		}
	},
}
