package api

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestAPL_AlwaysFiltersSandboxID is the load-bearing security test:
// no matter what the user supplies in query params, the rendered APL
// must filter on sandbox_id == the URL-path value. This is the
// invariant that makes the auth model work — break this and a user
// who can hit /api/sandboxes/X/logs but only owns Y could read X's
// logs by sneaking past the filter.
func TestAPL_AlwaysFiltersSandboxID(t *testing.T) {
	cases := []struct {
		name string
		qs   url.Values
	}{
		{"empty", url.Values{}},
		{"with text", url.Values{"q": {"hello"}}},
		{"with sources", url.Values{"source": {"exec_stdout,exec_stderr"}}},
		{"with limit", url.Values{"limit": {"500"}}},
		{
			"injection attempt — quote in q",
			url.Values{"q": {`" or sandbox_id != "anything`}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := parseLogQuery("sb-target", time.Now().Add(-time.Hour), tc.qs)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			apl := q.toAPL("oc-sandbox-logs", false)
			if !strings.Contains(apl, `where sandbox_id == "sb-target"`) {
				t.Errorf("APL missing sandbox_id filter:\n%s", apl)
			}
			// Verify the filter line is the FIRST predicate after the
			// dataset, not buried somewhere a search OR could subvert.
			lines := strings.Split(strings.TrimSpace(apl), "\n")
			if len(lines) < 2 {
				t.Fatalf("APL too short:\n%s", apl)
			}
			if !strings.Contains(lines[1], `where sandbox_id == "sb-target"`) {
				t.Errorf("sandbox_id filter not first predicate:\n%s", apl)
			}
		})
	}
}

// TestAPL_RejectsBadSource: source values not in allowedSources are a
// 400 at parse time, never interpolated.
func TestAPL_RejectsBadSource(t *testing.T) {
	for _, bad := range []string{"system", "stdout", "exec_stdout'); drop --", ""} {
		if bad == "" {
			continue // empty values are stripped, not rejected
		}
		t.Run(bad, func(t *testing.T) {
			_, err := parseLogQuery("sb-x", time.Now(), url.Values{"source": {bad}})
			if err == nil {
				t.Errorf("expected error for source=%q", bad)
			}
		})
	}
}

// TestAPL_RejectsControlCharsInQ: newlines / NULs in `q` would let a
// user break out of the string literal and append APL of their own.
// They must be rejected at parse time.
func TestAPL_RejectsControlCharsInQ(t *testing.T) {
	for _, bad := range []string{"hello\nworld", "x\x00y", "\r"} {
		_, err := parseLogQuery("sb-x", time.Now(), url.Values{"q": {bad}})
		if err == nil {
			t.Errorf("expected error for q=%q", bad)
		}
	}
}

// TestAPL_QuoteEscapeInQ: legitimate double quotes in `q` are escaped
// to `\"` so the APL parses as a single string literal.
func TestAPL_QuoteEscapeInQ(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now(), url.Values{"q": {`hello "world"`}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	apl := q.toAPL("d", false)
	if !strings.Contains(apl, `where line contains "hello \"world\""`) {
		t.Errorf("expected escaped quotes in APL:\n%s", apl)
	}
}

// TestAPL_TailVariantOmitsLimitAndUntil: the live-tail render path
// shouldn't bound by `until` or apply a row cap — every poll wants
// every new event since the cursor.
func TestAPL_TailVariantOmitsLimitAndUntil(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now().Add(-time.Hour), url.Values{
		"until": {time.Now().Format(time.RFC3339)},
		"limit": {"50"},
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tailAPL := q.toAPL("d", true)
	if strings.Contains(tailAPL, "_time <=") {
		t.Errorf("tail APL must not bound by until:\n%s", tailAPL)
	}
	if strings.Contains(tailAPL, "limit") {
		t.Errorf("tail APL must not apply limit:\n%s", tailAPL)
	}

	// And the historical render path DOES include them.
	histAPL := q.toAPL("d", false)
	if !strings.Contains(histAPL, "_time <=") {
		t.Errorf("historical APL missing until:\n%s", histAPL)
	}
	if !strings.Contains(histAPL, "limit") {
		t.Errorf("historical APL missing limit:\n%s", histAPL)
	}
}

// TestAPL_LimitClamp: caller-supplied limit > 10000 is silently
// clamped to 10000 (we don't 400 here — the cap is a defense, not a
// contract).
func TestAPL_LimitClamp(t *testing.T) {
	q, err := parseLogQuery("sb-x", time.Now(), url.Values{"limit": {"99999"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.limit != 10000 {
		t.Errorf("expected limit clamped to 10000, got %d", q.limit)
	}
}
