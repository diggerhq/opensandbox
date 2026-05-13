package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opensandbox/opensandbox/cmd/oc/internal/client"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs <sandbox-id>",
	Short: "Stream a sandbox's session logs (live tail by default)",
	Long: `Stream logs from a sandbox session — /var/log/* and every exec'd
command's stdout/stderr — shipped from inside the sandbox to Axiom and
served back through the control plane.

Default behaviour: tail. Prints the historical batch starting at
the sandbox's createdAt, then polls forever for new lines until you
hit Ctrl-C. Stopped/hibernated sandboxes flip to historical-only
automatically (no new lines will arrive after stop) so --no-tail is
not strictly needed for those.

Examples:
  oc logs sb-abc                              # live tail
  oc logs sb-abc --no-tail                    # one historical batch, then exit
  oc logs sb-abc --since 30m                  # last 30 minutes
  oc logs sb-abc --grep error                 # server-side substring filter
  oc logs sb-abc --source exec_stdout,exec_stderr
  oc logs sb-abc --json                       # raw event JSON, one per line`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		c := client.FromContext(ctx)
		sandboxID := args[0]

		noTail, _ := cmd.Flags().GetBool("no-tail")
		since, _ := cmd.Flags().GetString("since")
		until, _ := cmd.Flags().GetString("until")
		grep, _ := cmd.Flags().GetString("grep")
		sources, _ := cmd.Flags().GetStringSlice("source")
		limit, _ := cmd.Flags().GetInt("limit")
		jsonOut, _ := cmd.Flags().GetBool("json")

		qs := url.Values{}
		if noTail {
			qs.Set("tail", "false")
		}
		if since != "" {
			// Accept both Go duration (e.g. "30m", "2h") and RFC3339.
			if d, err := time.ParseDuration(since); err == nil {
				qs.Set("since", time.Now().UTC().Add(-d).Format(time.RFC3339))
			} else if _, err := time.Parse(time.RFC3339, since); err == nil {
				qs.Set("since", since)
			} else {
				return fmt.Errorf("--since must be a Go duration (e.g. 30m) or RFC3339 timestamp")
			}
		}
		if until != "" {
			if _, err := time.Parse(time.RFC3339, until); err != nil {
				return fmt.Errorf("--until must be an RFC3339 timestamp")
			}
			qs.Set("until", until)
		}
		if grep != "" {
			qs.Set("q", grep)
		}
		if len(sources) > 0 {
			qs.Set("source", strings.Join(sources, ","))
		}
		if limit > 0 {
			qs.Set("limit", strconv.Itoa(limit))
		}

		// client.New() already appends /api to baseURL — use the bare
		// path here, matching the convention in exec/sandbox commands.
		path := "/sandboxes/" + url.PathEscape(sandboxID) + "/logs"
		if encoded := qs.Encode(); encoded != "" {
			path += "?" + encoded
		}

		resp, err := c.GetStream(ctx, path)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			buf, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("logs request failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(buf)))
		}

		return streamSandboxLogs(resp.Body, os.Stdout, jsonOut)
	},
}

// logEvent mirrors the on-the-wire shape from internal/api/sandbox_logs.go.
// Unknown extra fields from server are tolerated by json.Unmarshal.
type logEvent struct {
	Time     time.Time `json:"_time"`
	Source   string    `json:"source"`
	Line     string    `json:"line"`
	Path     string    `json:"path,omitempty"`
	ExecID   string    `json:"exec_id,omitempty"`
	Command  string    `json:"command,omitempty"`
	Argv     []string  `json:"argv,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
}

// streamSandboxLogs parses an SSE stream and writes each event to w.
// Splits out from the cobra RunE so it's unit-testable.
func streamSandboxLogs(r io.Reader, w io.Writer, jsonOut bool) error {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			// SSE comments start with ":" — keepalives, errors. Ignore.
			if strings.HasPrefix(line, ":") {
				if err == io.EOF {
					return nil
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if payload != "" {
					if jsonOut {
						fmt.Fprintln(w, payload)
					} else {
						var ev logEvent
						if jerr := json.Unmarshal([]byte(payload), &ev); jerr == nil {
							renderEvent(w, ev)
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func renderEvent(w io.Writer, ev logEvent) {
	ts := ev.Time.Local().Format("15:04:05.000")
	color := sourceColor(ev.Source)
	fmt.Fprintf(w, "%s %s%-12s\033[0m %s\n", ts, color, ev.Source, ev.Line)
}

func sourceColor(s string) string {
	switch s {
	case "var_log":
		return "\033[36m" // cyan
	case "exec_stdout":
		return "\033[37m" // light gray
	case "exec_stderr":
		return "\033[31m" // red
	case "agent":
		return "\033[33m" // yellow
	default:
		return "\033[37m"
	}
}

func init() {
	logsCmd.Flags().Bool("no-tail", false, "exit after the historical batch instead of tailing")
	logsCmd.Flags().String("since", "", "Go duration (e.g. 30m, 2h) or RFC3339 timestamp")
	logsCmd.Flags().String("until", "", "RFC3339 timestamp; ignored when tailing")
	logsCmd.Flags().String("grep", "", "server-side substring filter on the log line")
	logsCmd.Flags().StringSlice("source", nil, "comma-separated filter: var_log, exec_stdout, exec_stderr, agent")
	logsCmd.Flags().Int("limit", 0, "max historical rows (0 = server default of 1000)")
	logsCmd.Flags().Bool("json", false, "emit raw event JSON instead of formatted lines")
}
