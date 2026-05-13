package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestFilter_AlwaysFlatSandboxID is the load-bearing security test:
// no matter what the user supplies in query params, the filter sent
// to Axiom must be a FLAT `sandbox_id == <url-path>` predicate.
//
// Axiom's per-dataset /query endpoint silently drops AND-wrapped
// filters and returns the unfiltered dataset (verified empirically
// against api.axiom.co), so a wrapped shape == a tenant leak. The
// flat shape is the only one Axiom actually honors.
func TestFilter_AlwaysFlatSandboxID(t *testing.T) {
	cases := []struct {
		name string
		qs   url.Values
	}{
		{"empty", url.Values{}},
		{"with text", url.Values{"q": {"hello"}}},
		{"with sources", url.Values{"source": {"exec_stdout,exec_stderr"}}},
		{"with limit", url.Values{"limit": {"500"}}},
		{
			"injection attempt — quotes in q",
			url.Values{"q": {`" or sandbox_id != "anything`}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parseLogQuery("sb-target", time.Now().Add(-time.Hour), tc.qs)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			f := q.toFilter()

			if f.Op != "==" {
				t.Errorf("filter op = %q, want == (NEVER and/or — Axiom drops those)", f.Op)
			}
			if f.Field != "sandbox_id" {
				t.Errorf("filter field = %q, want sandbox_id", f.Field)
			}
			if f.Value != "sb-target" {
				t.Errorf("filter value = %v, want sb-target", f.Value)
			}
			if len(f.Filters) != 0 {
				t.Errorf("filter must be flat (no nested .Filters), got %d sub-filters", len(f.Filters))
			}
		})
	}
}

// TestFilter_RejectsBadSource: source values not in allowedSources are a
// 400 at parse time, never interpolated.
func TestFilter_RejectsBadSource(t *testing.T) {
	for _, bad := range []string{"system", "stdout", "exec_stdout'); drop --"} {
		t.Run(bad, func(t *testing.T) {
			_, err := parseLogQuery("sb-x", time.Now(), url.Values{"source": {bad}})
			if err == nil {
				t.Errorf("expected error for source=%q", bad)
			}
		})
	}
}

// TestFilter_RejectsControlCharsInQ: newlines / NULs in `q` are rejected
// at parse time. We rely on this since q.text is applied client-side
// via strings.Contains — any operator-side parsing here is moot, but
// defending against control chars protects future code paths.
func TestFilter_RejectsControlCharsInQ(t *testing.T) {
	for _, bad := range []string{"hello\nworld", "x\x00y", "\r"} {
		_, err := parseLogQuery("sb-x", time.Now(), url.Values{"q": {bad}})
		if err == nil {
			t.Errorf("expected error for q=%q", bad)
		}
	}
}

// TestRequest_TailOmitsLimit: tail polls don't impose a row cap (every
// poll wants every new event since the cursor).
func TestRequest_TailOmitsLimit(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now().Add(-time.Hour), url.Values{"limit": {"50"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tailReq := q.toRequest(time.Now().Add(-time.Hour), time.Now(), true)
	if tailReq.Limit != 0 {
		t.Errorf("tail request limit = %d, want 0 (omitted)", tailReq.Limit)
	}
	histReq := q.toRequest(time.Now().Add(-time.Hour), time.Now(), false)
	if histReq.Limit != 50 {
		t.Errorf("historical request limit = %d, want 50", histReq.Limit)
	}
}

// TestRequest_BodyShape — full marshaled-JSON smoke test. Failure here
// likely means the body shape has drifted in a way Axiom won't
// understand; verify with an actual Axiom probe before changing.
func TestRequest_BodyShape(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now().Add(-time.Hour), url.Values{
		"q":      {"oops"},
		"source": {"exec_stdout"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 1, 0, 0, 0, time.UTC)
	req := q.toRequest(start, end, false)
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, must := range []string{
		`"startTime":"2026-05-13T00:00:00Z"`,
		`"endTime":"2026-05-13T01:00:00Z"`,
		// Flat filter — exactly what Axiom honors.
		`"filter":{"op":"==","field":"sandbox_id","value":"sb-x"}`,
		`"limit":1000`,
		`"order":[{"field":"_time"}]`,
	} {
		if !strings.Contains(string(body), must) {
			t.Errorf("body missing %q:\n%s", must, body)
		}
	}
	// Regression: the body must NOT carry an `apl` field (legacy bug
	// fixed in #242) AND must NOT use the AND wrapper (broken in #242,
	// fixed by this change — Axiom silently drops AND-wrapped filter
	// trees and returns every event in the dataset, see commit message).
	for _, mustNot := range []string{`"apl"`, `"op":"and"`, `"op":"or"`} {
		if strings.Contains(string(body), mustNot) {
			t.Errorf("body contains %q — Axiom drops this shape:\n%s", mustNot, body)
		}
	}
}

// TestParseLogQuery_LimitClamp: caller-supplied limit > 10000 is silently
// clamped to 10000 (the cap is a defense, not a contract).
func TestParseLogQuery_LimitClamp(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now(), url.Values{"limit": {"99999"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.limit != 10000 {
		t.Errorf("expected limit clamped to 10000, got %d", q.limit)
	}
}

// TestApplyClientFilters_NoOp: when the query has no text/source
// narrowing, all rows pass through.
func TestApplyClientFilters_NoOp(t *testing.T) {
	rows := []logEvent{
		{Line: "alpha", Source: "exec_stdout"},
		{Line: "beta", Source: "var_log"},
	}
	got := applyClientFilters(rows, logQuery{})
	if len(got) != 2 {
		t.Errorf("got %d rows, want 2 (no narrowing)", len(got))
	}
}

// TestApplyClientFilters_TextSubstring: q.text narrows by substring on
// the line field, case-sensitive (matches Axiom's `contains` operator
// semantics — we just don't trust Axiom to apply it).
func TestApplyClientFilters_TextSubstring(t *testing.T) {
	rows := []logEvent{
		{Line: "hello world", Source: "exec_stdout"},
		{Line: "goodbye", Source: "exec_stdout"},
		{Line: "Hello again", Source: "exec_stdout"},
	}
	got := applyClientFilters(rows, logQuery{text: "hello"})
	if len(got) != 1 || got[0].Line != "hello world" {
		t.Errorf("got %v, want only 'hello world'", got)
	}
}

// TestApplyClientFilters_SourceList: q.sources narrows to the listed
// sources; events with other source values are dropped.
func TestApplyClientFilters_SourceList(t *testing.T) {
	rows := []logEvent{
		{Line: "a", Source: "exec_stdout"},
		{Line: "b", Source: "exec_stderr"},
		{Line: "c", Source: "var_log"},
		{Line: "d", Source: "agent"},
	}
	got := applyClientFilters(rows, logQuery{sources: []string{"exec_stdout", "exec_stderr"}})
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2 (exec_stdout + exec_stderr only)", len(got))
	}
	if got[0].Source != "exec_stdout" || got[1].Source != "exec_stderr" {
		t.Errorf("unexpected sources: %v", got)
	}
}

// TestApplyClientFilters_TextAndSourceComposed: both predicates apply
// (AND semantics — row must satisfy both).
func TestApplyClientFilters_TextAndSourceComposed(t *testing.T) {
	rows := []logEvent{
		{Line: "error in foo", Source: "exec_stderr"},
		{Line: "error in bar", Source: "var_log"},
		{Line: "ok in foo", Source: "exec_stderr"},
	}
	got := applyClientFilters(rows, logQuery{
		text:    "error",
		sources: []string{"exec_stderr"},
	})
	if len(got) != 1 || got[0].Line != "error in foo" {
		t.Errorf("got %v, want only 'error in foo'", got)
	}
}

// TestApplySessionState_RunningPassthrough: a running sandbox keeps the
// query exactly as parsed — no narrowing.
func TestApplySessionState_RunningPassthrough(t *testing.T) {
	q := logQuery{tail: true, until: time.Time{}}
	got := applySessionState(q, "running", nil)
	if !got.tail {
		t.Errorf("running sandbox: tail = false, want true (caller-set default)")
	}
	if !got.until.IsZero() {
		t.Errorf("running sandbox: until set to %v, want zero", got.until)
	}
}

// TestApplySessionState_PreservesTail: q.tail reflects the client's
// explicit intent (URL ?tail=false / CLI --no-tail) and is NOT
// rewritten by applySessionState. The handler reads session.Status
// separately to decide whether to actually poll Axiom; on non-running
// sandboxes it holds the SSE connection open with keepalives.
//
// History: an earlier version flipped q.tail=false for non-running
// sandboxes. The handler then `return nil`-ed after the historical
// batch, closing the SSE stream. Browsers' EventSource auto-reconnects
// on close → server re-sent the same historical batch → events
// visibly duplicated in the UI (one copy per reconnect). Reverted.
func TestApplySessionState_PreservesTail(t *testing.T) {
	for _, status := range []string{"running", "stopped", "error", "hibernated", "migrating"} {
		t.Run(status, func(t *testing.T) {
			for _, clientTail := range []bool{true, false} {
				q := logQuery{tail: clientTail}
				got := applySessionState(q, status, nil)
				if got.tail != clientTail {
					t.Errorf("status=%s clientTail=%v: tail = %v, want preserved", status, clientTail, got.tail)
				}
			}
		})
	}
}

// TestApplySessionState_CapsUntilAtStoppedAtGrace: non-running sandboxes
// with a stoppedAt timestamp get `until` bounded to stoppedAt + 30s. If
// the caller already passed a tighter `until`, theirs wins.
func TestApplySessionState_CapsUntilAtStoppedAtGrace(t *testing.T) {
	stopped := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	t.Run("unset until gets capped", func(t *testing.T) {
		q := logQuery{tail: true}
		got := applySessionState(q, "stopped", &stopped)
		want := stopped.Add(30 * time.Second)
		if !got.until.Equal(want) {
			t.Errorf("until = %v, want %v", got.until, want)
		}
	})
	t.Run("tighter until kept", func(t *testing.T) {
		tighter := stopped.Add(-1 * time.Hour)
		q := logQuery{tail: true, until: tighter}
		got := applySessionState(q, "stopped", &stopped)
		if !got.until.Equal(tighter) {
			t.Errorf("until = %v, want caller-set %v", got.until, tighter)
		}
	})
	t.Run("until in future gets clamped", func(t *testing.T) {
		future := stopped.Add(1 * time.Hour)
		q := logQuery{tail: true, until: future}
		got := applySessionState(q, "stopped", &stopped)
		want := stopped.Add(30 * time.Second)
		if !got.until.Equal(want) {
			t.Errorf("until = %v, want %v (capped at stoppedAt+30s)", got.until, want)
		}
	})
	t.Run("no stoppedAt → no until set", func(t *testing.T) {
		q := logQuery{tail: true}
		got := applySessionState(q, "error", nil)
		if !got.until.IsZero() {
			t.Errorf("error+no-stoppedAt: until = %v, want zero", got.until)
		}
	})
}
