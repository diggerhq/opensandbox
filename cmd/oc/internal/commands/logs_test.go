package commands

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamSandboxLogs_FormattedMode(t *testing.T) {
	sse := "" +
		": keepalive\n\n" +
		"data: {\"_time\":\"2026-05-13T12:00:00Z\",\"source\":\"var_log\",\"line\":\"hello\"}\n\n" +
		": ping\n\n" +
		"data: {\"_time\":\"2026-05-13T12:00:01Z\",\"source\":\"exec_stdout\",\"line\":\"world\"}\n\n"

	var out bytes.Buffer
	if err := streamSandboxLogs(strings.NewReader(sse), &out, false); err != nil {
		t.Fatalf("stream: %v", err)
	}
	s := out.String()
	for _, want := range []string{"hello", "world", "var_log", "exec_stdout"} {
		if !strings.Contains(s, want) {
			t.Errorf("formatted output missing %q:\n%s", want, s)
		}
	}
}

func TestStreamSandboxLogs_JSONMode(t *testing.T) {
	payload := `{"_time":"2026-05-13T12:00:00Z","source":"var_log","line":"hello"}`
	sse := "data: " + payload + "\n\n"

	var out bytes.Buffer
	if err := streamSandboxLogs(strings.NewReader(sse), &out, true); err != nil {
		t.Fatalf("stream: %v", err)
	}
	// In JSON mode, the payload is printed verbatim. Verify it parses back
	// to the same shape — protects against accidental wrapping/quoting.
	got := strings.TrimSpace(out.String())
	var rt map[string]any
	if err := json.Unmarshal([]byte(got), &rt); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, got)
	}
	if rt["line"] != "hello" {
		t.Errorf("round-tripped line = %v, want hello", rt["line"])
	}
}

func TestStreamSandboxLogs_SkipsCommentsAndBlanks(t *testing.T) {
	sse := ": just a keepalive\n\n: another\n\n\n\n"
	var out bytes.Buffer
	if err := streamSandboxLogs(strings.NewReader(sse), &out, false); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output for comment-only stream, got: %q", out.String())
	}
}

func TestStreamSandboxLogs_TolerantOfMalformedPayload(t *testing.T) {
	// A bad payload shouldn't kill the whole stream — subsequent good
	// events should still render.
	sse := "" +
		"data: not-json\n\n" +
		"data: {\"_time\":\"2026-05-13T12:00:00Z\",\"source\":\"var_log\",\"line\":\"after-bad\"}\n\n"
	var out bytes.Buffer
	if err := streamSandboxLogs(strings.NewReader(sse), &out, false); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if !strings.Contains(out.String(), "after-bad") {
		t.Errorf("good event after malformed payload was dropped:\n%s", out.String())
	}
}
