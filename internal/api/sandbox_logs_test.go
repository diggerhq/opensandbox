package api

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestFilter_AlwaysFiltersSandboxID is the load-bearing security test:
// no matter what the user supplies in query params, the rendered filter
// must have sandbox_id == the URL-path value as the first conjunct under
// the top-level AND. Break this and a user who can hit
// /api/sandboxes/X/logs but only owns Y could read X's logs.
func TestFilter_AlwaysFiltersSandboxID(t *testing.T) {
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

			if f.Op != "and" {
				t.Errorf("top-level op = %q, want and", f.Op)
			}
			if len(f.Filters) == 0 {
				t.Fatalf("top-level filter has no sub-filters")
			}
			first := f.Filters[0]
			if first.Op != "==" || first.Field != "sandbox_id" || first.Value != "sb-target" {
				t.Errorf("first conjunct = %+v, want sandbox_id == sb-target", first)
			}
		})
	}
}

// TestFilter_QInjectionStaysInValue: a malicious `q` parameter ends up as
// a plain string in the JSON value, not as a sibling predicate.
func TestFilter_QInjectionStaysInValue(t *testing.T) {
	mal := `" or sandbox_id != "anything`
	q, err := parseLogQuery("sb-target", time.Now(), url.Values{"q": {mal}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := q.toFilter()
	// The text predicate is the second conjunct.
	if len(f.Filters) < 2 {
		t.Fatalf("expected at least 2 sub-filters, got %d", len(f.Filters))
	}
	txt := f.Filters[1]
	if txt.Op != "contains" || txt.Field != "line" || txt.Value != mal {
		t.Errorf("text predicate = %+v, expected contains(line, %q)", txt, mal)
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
// at parse time to avoid any operator-side quirk with embedded control
// characters.
func TestFilter_RejectsControlCharsInQ(t *testing.T) {
	for _, bad := range []string{"hello\nworld", "x\x00y", "\r"} {
		_, err := parseLogQuery("sb-x", time.Now(), url.Values{"q": {bad}})
		if err == nil {
			t.Errorf("expected error for q=%q", bad)
		}
	}
}

// TestFilter_SourceList_Or: multiple sources become an OR subtree, not a
// list-in-value.
func TestFilter_SourceList_Or(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now(), url.Values{"source": {"exec_stdout,exec_stderr"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	f := q.toFilter()
	if len(f.Filters) < 2 {
		t.Fatalf("expected sandbox_id + source-or, got %d sub-filters", len(f.Filters))
	}
	srcOr := f.Filters[1]
	if srcOr.Op != "or" {
		t.Errorf("source subtree op = %q, want or", srcOr.Op)
	}
	if len(srcOr.Filters) != 2 {
		t.Fatalf("source OR has %d filters, want 2", len(srcOr.Filters))
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
// understand; cross-reference against the API docs before changing.
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
		`"filter":{"op":"and",`,
		`"op":"==","field":"sandbox_id","value":"sb-x"`,
		`"op":"contains","field":"line","value":"oops"`,
		`"op":"==","field":"source","value":"exec_stdout"`,
		`"limit":1000`,
		`"order":[{"field":"_time"}]`,
	} {
		if !strings.Contains(string(body), must) {
			t.Errorf("body missing %q:\n%s", must, body)
		}
	}
	// And it must NOT contain an `apl` field — the regression that
	// caused the cross-tenant leak. The per-dataset endpoint silently
	// ignores unknown body fields, so {"apl": "..."} comes back with
	// every event in the dataset.
	if strings.Contains(string(body), `"apl"`) {
		t.Errorf("body contains an `apl` field — this is the bug we're guarding against:\n%s", body)
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
