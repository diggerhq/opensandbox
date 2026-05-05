package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/opensandbox/opensandbox/internal/auth"
)

// allowedSources is the closed set of source values a client may filter
// on. Anything else gets a 400 — never fed into the APL string.
var allowedSources = map[string]struct{}{
	"var_log":     {},
	"exec_stdout": {},
	"exec_stderr": {},
	"agent":       {},
}

// getSandboxLogs streams sandbox session logs as Server-Sent Events.
//
// Flow:
//
//  1. Auth check via existing dashboard middleware (caller has an org).
//  2. Sandbox-ownership check via GetSandboxSessionInOrg — refuses if
//     the caller's org doesn't own this sandbox.
//  3. Initial historical batch: APL with sandbox_id == :id + the
//     caller's filters, sort asc, limit. Emit one SSE event per row.
//  4. If tail=true (default), poll Axiom every 1s with a moving
//     `_time > last_seen` cursor; emit deltas. Send `: keepalive\n\n`
//     every 15s if nothing new arrived to keep proxies happy.
//  5. Stream ends when the client disconnects or the request context
//     is otherwise cancelled.
//
// The query token is held server-side and never reaches the browser —
// this whole endpoint exists to keep that invariant clean.
//
// Query params:
//
//	tail      true|false (default true)
//	since     RFC3339 (default sandbox.StartedAt)
//	until     RFC3339 (default now; ignored when tail=true)
//	q         free-text search (escaped, "where line contains")
//	source    comma-separated subset of allowedSources
//	limit     int, default 1000, max 10000 (historical batch only)
func (s *Server) getSandboxLogs(c echo.Context) error {
	if s.axiomQueryToken == "" || s.axiomDataset == "" {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "sandbox session logs are not configured on this deployment",
		})
	}

	orgUUID, hasOrg := auth.GetOrgID(c)
	if !hasOrg {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}

	sandboxID := c.Param("sandboxId")
	if sandboxID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "missing sandbox id"})
	}

	// Authorization: the caller's org must own this sandbox. Returns
	// 404 (not 403) on mismatch to avoid leaking sandbox existence
	// across orgs — same contract as the rest of /api/sandboxes/:id/*.
	ctx := c.Request().Context()
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "sandbox session logs require a database",
		})
	}
	session, err := s.store.GetSandboxSessionInOrg(ctx, orgUUID, sandboxID)
	if err != nil || session == nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "sandbox not found"})
	}

	q, err := parseLogQuery(sandboxID, session.StartedAt, c.QueryParams())
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	// SSE headers + immediate flush so the browser knows the stream is open.
	c.Response().Header().Set("Content-Type", "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no") // disable nginx buffering
	c.Response().WriteHeader(http.StatusOK)
	c.Response().Flush()

	// Initial historical batch.
	rows, err := s.queryAxiom(ctx, q.toAPL(s.axiomDataset, false))
	if err != nil {
		log.Printf("api: sandbox %s logs: initial query failed: %v", sandboxID, err)
		writeSSEComment(c.Response(), "initial query failed")
		return nil
	}
	for _, ev := range rows {
		writeSSEEvent(c.Response(), ev)
	}
	c.Response().Flush()

	if !q.tail {
		return nil
	}

	// Live tail. Cursor advances as new events arrive. Use a tiny
	// (~1s) overlap on the first tail tick to avoid losing events that
	// were ingested between the historical query and now — the
	// duplicates are harmless because each event has a unique _time +
	// content (and the UI dedupes by _time/content if it cares).
	var cursor time.Time
	if len(rows) > 0 {
		cursor = rows[len(rows)-1].Time
	} else {
		cursor = q.since
	}
	cursor = cursor.Add(-1 * time.Second)

	tick := time.NewTicker(1 * time.Second)
	defer tick.Stop()
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			tailQ := q
			tailQ.since = cursor
			tailQ.until = time.Time{} // open-ended
			newRows, err := s.queryAxiom(ctx, tailQ.toAPL(s.axiomDataset, true))
			if err != nil {
				// Don't kill the stream on a single failed poll — log
				// and try again next tick. Persistent failures will
				// show as a stalled stream which the UI can detect.
				log.Printf("api: sandbox %s logs: tail poll failed: %v", sandboxID, err)
				continue
			}
			for _, ev := range newRows {
				if !ev.Time.After(cursor) {
					continue
				}
				writeSSEEvent(c.Response(), ev)
				cursor = ev.Time
			}
			if len(newRows) > 0 {
				c.Response().Flush()
			}
		case <-keepalive.C:
			writeSSEComment(c.Response(), "keepalive")
			c.Response().Flush()
		}
	}
}

// logQuery is the validated form of incoming query params, ready to
// render into APL.
type logQuery struct {
	sandboxID string
	since     time.Time
	until     time.Time
	text      string   // already-escaped
	sources   []string // already-validated against allowedSources
	limit     int
	tail      bool
}

func parseLogQuery(sandboxID string, sandboxStarted time.Time, qs url.Values) (logQuery, error) {
	q := logQuery{
		sandboxID: sandboxID,
		since:     sandboxStarted,
		limit:     1000,
		tail:      true,
	}

	if v := qs.Get("tail"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return q, fmt.Errorf("tail must be true or false")
		}
		q.tail = b
	}

	if v := qs.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, fmt.Errorf("since must be RFC3339")
		}
		q.since = t
	}

	if v := qs.Get("until"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return q, fmt.Errorf("until must be RFC3339")
		}
		q.until = t
	}

	if v := qs.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("limit must be a positive integer")
		}
		if n > 10000 {
			n = 10000
		}
		q.limit = n
	}

	if v := qs.Get("q"); v != "" {
		// Disallow newlines + control chars; double-escape quotes.
		// APL string literals are double-quoted; embedded `"` is
		// escaped as `\"`. The set we strip here is the surface that
		// could break out of a string literal; everything else passes
		// through unchanged so search behaviour matches user intent.
		if strings.ContainsAny(v, "\r\n\x00") {
			return q, fmt.Errorf("q must not contain control characters")
		}
		q.text = strings.ReplaceAll(v, `"`, `\"`)
	}

	if v := qs.Get("source"); v != "" {
		for _, src := range strings.Split(v, ",") {
			src = strings.TrimSpace(src)
			if src == "" {
				continue
			}
			if _, ok := allowedSources[src]; !ok {
				return q, fmt.Errorf("source %q not allowed", src)
			}
			q.sources = append(q.sources, src)
		}
	}

	return q, nil
}

// toAPL renders the query into a KQL/APL string. The sandbox_id filter
// is the FIRST predicate after the dataset and is unconditionally
// applied — there is no path that omits it. Every interpolated string
// is either: a server-derived constant (sandboxID, dataset name), a
// validated value from allowedSources, or pre-escaped (text search).
//
// `tail` selects an open-ended (no until) variant.
func (q logQuery) toAPL(dataset string, tail bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "['%s']\n", dataset)
	fmt.Fprintf(&b, "  | where sandbox_id == \"%s\"\n", q.sandboxID)
	if !q.since.IsZero() {
		fmt.Fprintf(&b, "  | where _time >= datetime(\"%s\")\n", q.since.UTC().Format(time.RFC3339Nano))
	}
	if !tail && !q.until.IsZero() {
		fmt.Fprintf(&b, "  | where _time <= datetime(\"%s\")\n", q.until.UTC().Format(time.RFC3339Nano))
	}
	if q.text != "" {
		fmt.Fprintf(&b, "  | where line contains \"%s\"\n", q.text)
	}
	if len(q.sources) > 0 {
		quoted := make([]string, len(q.sources))
		for i, s := range q.sources {
			quoted[i] = fmt.Sprintf("\"%s\"", s)
		}
		fmt.Fprintf(&b, "  | where source in (%s)\n", strings.Join(quoted, ", "))
	}
	fmt.Fprintf(&b, "  | sort by _time asc\n")
	if !tail {
		fmt.Fprintf(&b, "  | limit %d\n", q.limit)
	}
	return b.String()
}

// logEvent is the on-the-wire shape we re-emit to SSE clients. Mirrors
// the agent-side schema but only includes fields the UI needs to
// render a row. Unknown extra fields from Axiom are tolerated.
type logEvent struct {
	Time      time.Time `json:"_time"`
	Source    string    `json:"source"`
	Line      string    `json:"line"`
	SandboxID string    `json:"sandbox_id,omitempty"`
	Path      string    `json:"path,omitempty"`
	ExecID    string    `json:"exec_id,omitempty"`
	Command   string    `json:"command,omitempty"`
	Argv      []string  `json:"argv,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
}

// queryAxiom POSTs an APL query and parses the response.
//
// Axiom's APL endpoint is /v1/datasets/_apl/query (with format=tabular
// off, default), and the response shape is:
//
//	{
//	  "matches": [{"data": {<event fields>}}, ...]
//	}
//
// We don't need format=tabular; the default object form is what we want.
func (s *Server) queryAxiom(ctx context.Context, apl string) ([]logEvent, error) {
	body, _ := json.Marshal(map[string]any{"apl": apl})
	req, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.axiom.co/v1/datasets/_apl/query?format=legacy",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.axiomQueryToken)
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("axiom %d: %s", resp.StatusCode, string(raw))
	}

	var parsed struct {
		Matches []struct {
			Data json.RawMessage `json:"data"`
		} `json:"matches"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode axiom response: %w", err)
	}

	out := make([]logEvent, 0, len(parsed.Matches))
	for _, m := range parsed.Matches {
		var ev logEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			continue // skip malformed, don't fail the whole batch
		}
		out = append(out, ev)
	}
	return out, nil
}

// writeSSEEvent writes one event in SSE wire format. Errors here mean
// the client disconnected; the caller's ctx will see Done shortly.
func writeSSEEvent(w io.Writer, ev logEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeSSEComment(w io.Writer, comment string) {
	fmt.Fprintf(w, ": %s\n\n", comment)
}
