package commands

// Error-rendering helpers for agent commands: LastError / ExitError types,
// the code→class catalog (mirrored from sessions-api/src/lib/error-codes.ts),
// and the two renderers used by `agent get`, `agent create`, and
// `agent install` (RenderLastError + renderAsyncFallback).
//
// Scope: agent commands only. If another command family ever needs to render
// classified errors, lift this into a shared package.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// LastError mirrors the JSON shape returned by sessions-api under
// GET /v1/agents/:id when the backing instance is in an error state.
// See ws-gstack/work/agent-error-visibility.md
type LastError struct {
	Phase          string `json:"phase"`
	Message        string `json:"message"`
	Code           string `json:"code"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	At             string `json:"at"`
}

// errorClass determines the CLI exit code. Mirrors the `class` field of
// sessions-api's error-codes catalog. Kept as a static mirror rather than
// fetched from the API: the classification is UX policy, not data.
type errorClass int

const (
	classGeneral     errorClass = 1
	classUpstream4xx errorClass = 3
	classConflict    errorClass = 4
	classTransient   errorClass = 5
)

// codeCatalog maps error codes to a class + suggestion + optional docs URL.
// Keep in sync with sessions-api/src/lib/error-codes.ts. The CLI ships its
// own mirror so offline suggestions render even if the API doesn't return
// them in the response (current API returns only code; suggestion/class
// are client-side concerns).
var codeCatalog = map[string]struct {
	class      errorClass
	suggestion string
	docs       string
}{
	"checkpoint_org_mismatch": {
		class: classUpstream4xx,
		suggestion: "Your OC API key's org doesn't own this core's checkpoint.\n" +
			"Contact an admin to share it, or use an API key from the owning org.",
		docs: "https://docs.opencomputer.dev/errors/checkpoint-org",
	},
	"checkpoint_not_found": {
		class: classUpstream4xx,
		suggestion: "The checkpoint configured for this core no longer exists on OC.\n" +
			"It may have been deleted or replaced. Contact the operator to\n" +
			"rebuild the checkpoint and update the sessions-api config.",
	},
	"telegram_unauthorized": {
		class:      classUpstream4xx,
		suggestion: "Telegram rejected the bot token. Verify the token at https://t.me/BotFather.",
	},
	"webhook_register_failed": {
		class: classGeneral,
		suggestion: "Telegram webhook registration failed.\n" +
			"Verify the bot token and retry. If it persists, inspect agent events\n" +
			"for the upstream Telegram response.",
	},
	"secret_store_failed": {
		class: classGeneral,
		suggestion: "The platform could not write channel or package secrets to OC.\n" +
			"Retry once. If it persists, inspect OC secret-store health and permissions.",
	},
	"core_not_ready": {
		class: classTransient,
		suggestion: "The managed core did not become healthy in time.\n" +
			"Retry once the agent is healthy, or inspect `oc agent get <id>` for readiness details.",
	},
	"channel_not_ready": {
		class: classTransient,
		suggestion: "The channel listener did not become ready in time.\n" +
			"Retry once the agent is healthy, or inspect `oc agent get <id>` for channel readiness.",
	},
	"package_verify_failed": {
		class: classGeneral,
		suggestion: "The package did not pass its verification step.\n" +
			"Inspect the package phase in agent events and retry after fixing the underlying issue.",
	},
	"sandbox_provision_timeout": {
		class: classTransient,
		suggestion: "Sandbox provisioning timed out. The OC worker may be unhealthy.\n" +
			"Try `oc agent delete <id>` then recreate — you may land on a different worker.",
	},
	"git_clone_failed": {
		class: classTransient,
		suggestion: "Package clone failed. The sandbox worker may have no outbound internet.\n" +
			"Delete and recreate the agent; you may land on a working worker.",
	},
}

// ExitError signals the desired process exit code. main.go inspects this
// to choose between 1 (general), 3 (upstream 4xx), 4 (conflict),
// 5 (transient). The error message is expected to already be rendered
// when ExitError is returned — main will not re-print it.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "" }

// ExitCodeFor classifies a LastError into an exit code. Falls back to
// classGeneral (1) for unknown codes.
func ExitCodeFor(le *LastError) int {
	if le == nil {
		return 0
	}
	if entry, ok := codeCatalog[le.Code]; ok {
		return int(entry.class)
	}
	return int(classGeneral)
}

// RenderLastError writes the standard multi-line error block to w.
// Used by both `agent get` (status block) and the poll loop in
// create/install (failure case). One shared renderer keeps the CLI's
// error voice consistent wherever errors appear.
func RenderLastError(w io.Writer, le *LastError) {
	if le == nil {
		return
	}

	title := "Last error"
	if t, err := time.Parse(time.RFC3339Nano, le.At); err == nil {
		title = fmt.Sprintf("Last error (%s ago)", time.Since(t).Truncate(time.Second))
	}

	fmt.Fprintf(w, "%s:\n", title)
	fmt.Fprintf(w, "  Phase:  %s\n", le.Phase)

	// Upstream response bodies frequently carry trailing whitespace / newlines
	// that survive the round-trip. Normalize before appending the upstream
	// status so the block renders on one clean line.
	reason := strings.TrimSpace(le.Message)
	if le.UpstreamStatus != 0 {
		reason = fmt.Sprintf("%s (%d from upstream)", reason, le.UpstreamStatus)
	}
	fmt.Fprintf(w, "  Reason: %s\n", reason)

	if entry, ok := codeCatalog[le.Code]; ok {
		fmt.Fprintln(w)
		for _, line := range strings.Split(entry.suggestion, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
		if entry.docs != "" {
			fmt.Fprintf(w, "  See: %s\n", entry.docs)
		}
	}
}

// asyncFallback is the JSON shape emitted for Mode 3 outcomes (--no-wait,
// poll timeout, poll network error). Scripts branch on the `async` boolean
// to decide whether to poll.
type asyncFallback struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Async     bool   `json:"async"`
	CheckWith string `json:"check_with"`
	Note      string `json:"note,omitempty"`
}

// renderAsyncFallback prints the "work is continuing in background" message
// (Mode 3). Shared across --no-wait, poll timeout, and poll error so the
// user sees the same next step regardless of trigger.
func renderAsyncFallback(stdoutW io.Writer, jsonOut bool, id, op, note string) {
	if jsonOut {
		enc := json.NewEncoder(stdoutW)
		enc.SetIndent("", "  ")
		_ = enc.Encode(asyncFallback{
			ID:        id,
			Status:    "starting",
			Async:     true,
			CheckWith: "oc agent get " + id,
			Note:      note,
		})
		return
	}

	fmt.Fprintf(stdoutW, "  ⋯ %s in background.\n", op)
	if note != "" {
		fmt.Fprintf(stdoutW, "    %s\n", note)
	}
	fmt.Fprintln(stdoutW)
	fmt.Fprintf(stdoutW, "To check status:  oc agent get %s\n", id)
}
