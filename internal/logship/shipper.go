// Package logship is the in-VM forwarder that ships sandbox session logs
// (lines from /var/log/* and stdout/stderr from platform-exec'd commands)
// to Axiom.
//
// Lifecycle:
//
//   1. cmd/agent/main starts a Shipper at boot via New(). The shipper is
//      DORMANT: events accepted via Send() queue in a bounded ring but
//      do not POST anywhere yet. The /var/log tailer starts immediately
//      so we don't miss the boot window.
//   2. Worker calls the agent's ConfigureLogship RPC after VM boot. The
//      RPC handler calls Shipper.Activate(cfg), passing the ingest token,
//      dataset, and per-sandbox identifiers.
//   3. Once activated, queued events are flushed (batches of up to 100 or
//      every 200ms) to https://api.axiom.co/v1/datasets/<dataset>/ingest
//      with the activation cfg's identifiers stamped on each event at
//      flush time.
//   4. If Activate is never called (kill-switch: worker had no ingest
//      token), the ring keeps dropping oldest indefinitely. Memory cost
//      is bounded by the ring; CPU cost is one no-op tick per 200ms.
//
// This package only depends on the standard library + fsnotify; no other
// internal packages, so tests are self-contained.
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Source identifies where an event came from.
type Source string

const (
	SourceVarLog     Source = "var_log"
	SourceExecStdout Source = "exec_stdout"
	SourceExecStderr Source = "exec_stderr"
	SourceAgent      Source = "agent"
)

// Event is one log line as serialised to Axiom. The JSON shape matches
// Axiom's ingest API (top-level _time field; arbitrary other fields are
// stored verbatim).
type Event struct {
	Time   time.Time `json:"_time"`
	Source Source    `json:"source"`
	Line   string    `json:"line"`

	// Stamped at flush time from the active config; empty until
	// activated. Sent to Axiom anyway so events captured pre-Activate
	// are still tagged with whatever's set when they finally flush.
	SandboxID string `json:"sandbox_id,omitempty"`
	OrgID     string `json:"org_id,omitempty"`

	// Set when source == "var_log".
	Path string `json:"path,omitempty"`

	// Set when source == "exec_*".
	ExecID   string   `json:"exec_id,omitempty"`
	Command  string   `json:"command,omitempty"`
	Argv     []string `json:"argv,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
}

// Config is the activation payload delivered via the worker's
// ConfigureLogship RPC.
type Config struct {
	IngestToken string
	Dataset     string
	SandboxID   string
	OrgID       string
}

// Shipper buffers events in a bounded ring and flushes batches to Axiom
// once Activate has been called.
type Shipper struct {
	bufSize     int
	batchSize   int
	flushEvery  time.Duration
	httpTimeout time.Duration
	httpClient  *http.Client

	in chan Event // bounded; drop-oldest on full

	mu        sync.RWMutex
	cfg       Config // zero until Activate
	activated bool

	droppedTotal atomic.Uint64
	closed       atomic.Bool
}

// New returns a dormant Shipper. Run it via Run(ctx) (typically in a
// goroutine) to start the flush loop.
func New() *Shipper {
	return &Shipper{
		bufSize:     10_000,
		batchSize:   100,
		flushEvery:  200 * time.Millisecond,
		httpTimeout: 10 * time.Second,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		in:          make(chan Event, 10_000),
	}
}

// Activate enables flushing with the given configuration. Calling
// Activate with an empty IngestToken is a no-op (kill-switch).
// Subsequent calls (e.g. token rotation) replace the config atomically.
func (s *Shipper) Activate(cfg Config) {
	if cfg.IngestToken == "" {
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	s.activated = true
	s.mu.Unlock()
}

// Send enqueues an event. Non-blocking; if the buffer is full, the
// oldest queued event is dropped to make room.
func (s *Shipper) Send(ev Event) {
	if s.closed.Load() {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	select {
	case s.in <- ev:
		return
	default:
		// Full — drop oldest to make room.
		select {
		case <-s.in:
			s.droppedTotal.Add(1)
		default:
		}
		select {
		case s.in <- ev:
		default:
			s.droppedTotal.Add(1)
		}
	}
}

// Run drives the flush loop. Returns when ctx is cancelled. The loop is
// safe to call before Activate; pre-Activate it just keeps draining the
// ring into the local batch but does not POST anywhere — flushing is
// gated on having a valid token.
func (s *Shipper) Run(ctx context.Context) {
	defer s.closed.Store(true)

	// Worker pool for HTTP POSTs. Decoupled from the batch loop so a
	// slow Axiom POST never blocks ingestion.
	postCh := make(chan []Event, 8)
	var workersDone sync.WaitGroup
	workersDone.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer workersDone.Done()
			for batch := range postCh {
				s.post(ctx, batch)
			}
		}()
	}

	var batch []Event
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.mu.RLock()
		cfg := s.cfg
		activated := s.activated
		s.mu.RUnlock()
		if !activated {
			// Hold the batch; it'll get re-flushed once Activate fires.
			// To avoid unbounded growth, cap the held batch at 10× normal.
			if len(batch) > s.batchSize*10 {
				batch = batch[len(batch)-s.batchSize*10:]
			}
			return
		}
		// Stamp identifiers on each event at flush time.
		for i := range batch {
			batch[i].SandboxID = cfg.SandboxID
			batch[i].OrgID = cfg.OrgID
		}
		select {
		case postCh <- batch:
		case <-ctx.Done():
		}
		batch = nil
	}

	tick := time.NewTicker(s.flushEvery)
	defer tick.Stop()

	dropTicker := time.NewTicker(30 * time.Second)
	defer dropTicker.Stop()
	var lastDropped uint64

	for {
		select {
		case <-ctx.Done():
			flush()
			close(postCh)
			workersDone.Wait()
			return
		case ev := <-s.in:
			batch = append(batch, ev)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-dropTicker.C:
			n := s.droppedTotal.Load()
			if n > lastDropped {
				dropped := n - lastDropped
				lastDropped = n
				batch = append(batch, Event{
					Time:   time.Now().UTC(),
					Source: SourceAgent,
					Line:   fmt.Sprintf("logship: dropped %d events in last 30s (total %d)", dropped, n),
				})
			}
		}
	}
}

func (s *Shipper) post(ctx context.Context, batch []Event) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if cfg.IngestToken == "" {
		return // shouldn't happen — flush() gates on activated — defensive
	}

	body, err := json.Marshal(batch)
	if err != nil {
		log.Printf("logship: marshal: %v", err)
		return
	}

	url := fmt.Sprintf("https://api.axiom.co/v1/datasets/%s/ingest", cfg.Dataset)

	// Backoff: 250ms → 500ms → 1s → 2s → 5s, total ~30s before drop.
	delays := []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, d := range delays {
		if d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return
			}
		}

		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			log.Printf("logship: NewRequest: %v", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+cfg.IngestToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.httpClient.Do(req)
		if err != nil {
			if i == len(delays)-1 {
				log.Printf("logship: POST after %d retries: %v (dropping %d events)", i, err, len(batch))
				s.droppedTotal.Add(uint64(len(batch)))
				return
			}
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			return
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// Token's bad — retry won't help.
			log.Printf("logship: POST %d (auth) — dropping %d events", resp.StatusCode, len(batch))
			s.droppedTotal.Add(uint64(len(batch)))
			return
		}
		if i == len(delays)-1 {
			log.Printf("logship: POST %d after %d retries (dropping %d events)", resp.StatusCode, i, len(batch))
			s.droppedTotal.Add(uint64(len(batch)))
			return
		}
	}
}

// Stats returns counters for observability.
type Stats struct {
	Dropped uint64
	Queued  int
}

func (s *Shipper) Stats() Stats {
	return Stats{
		Dropped: s.droppedTotal.Load(),
		Queued:  len(s.in),
	}
}
