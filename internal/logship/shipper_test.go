package logship

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLineWriter_BasicLines: simple newline-separated lines emit
// one Event each with the right source/exec metadata.
func TestLineWriter_BasicLines(t *testing.T) {
	got := captureShipper(t)
	w := NewLineWriter(got.s, SourceExecStdout, "exec-1", "echo", []string{"hi"})

	w.Write([]byte("hello\nworld\n"))
	w.Close(0)

	evs := got.drain(t, 3)

	if evs[0].Line != "hello" || evs[0].Source != SourceExecStdout || evs[0].ExecID != "exec-1" {
		t.Errorf("event[0] = %+v", evs[0])
	}
	if evs[1].Line != "world" {
		t.Errorf("event[1] line = %q, want %q", evs[1].Line, "world")
	}
	// Final EOF event.
	if evs[2].ExitCode == nil || *evs[2].ExitCode != 0 || evs[2].Line != "" {
		t.Errorf("event[2] = %+v, want EOF with exit=0", evs[2])
	}
}

// TestLineWriter_PartialLines: bytes split across multiple Writes
// must be re-assembled on the next newline. No premature emission.
func TestLineWriter_PartialLines(t *testing.T) {
	got := captureShipper(t)
	w := NewLineWriter(got.s, SourceExecStdout, "e", "x", nil)

	w.Write([]byte("hel"))
	w.Write([]byte("lo "))
	w.Write([]byte("world\nrest"))
	w.Close(0)

	evs := got.drain(t, 3)
	if evs[0].Line != "hello world" {
		t.Errorf("expected re-assembled 'hello world', got %q", evs[0].Line)
	}
	// 'rest' is the tail flushed at Close.
	if evs[1].Line != "rest" {
		t.Errorf("expected tail 'rest', got %q", evs[1].Line)
	}
	if evs[2].ExitCode == nil || *evs[2].ExitCode != 0 {
		t.Errorf("expected final EOF, got %+v", evs[2])
	}
}

// TestLineWriter_NoPartialFlushIfTrailingNewline: when Close is
// called and the buffer is already empty (input ended on \n), the
// only synthesised event is the EOF marker — no spurious empty line.
func TestLineWriter_NoPartialFlushIfTrailingNewline(t *testing.T) {
	got := captureShipper(t)
	w := NewLineWriter(got.s, SourceExecStdout, "e", "x", nil)

	w.Write([]byte("a\nb\n"))
	w.Close(7)

	evs := got.drain(t, 3)
	if evs[0].Line != "a" || evs[1].Line != "b" {
		t.Errorf("unexpected lines: %v / %v", evs[0].Line, evs[1].Line)
	}
	if evs[2].Line != "" || evs[2].ExitCode == nil || *evs[2].ExitCode != 7 {
		t.Errorf("expected EOF with exit=7, got %+v", evs[2])
	}
}

// TestLineWriter_NilShipper: a nil shipper must be tolerated (we use
// shipper=nil to disable shipping) — Write/Close must be safe no-ops.
func TestLineWriter_NilShipper(t *testing.T) {
	w := NewLineWriter(nil, SourceExecStdout, "e", "x", nil)
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write returned error with nil shipper: %v", err)
	}
	w.Close(0)
}

// TestLineWriter_LongLines: a single huge line spread across many
// Writes is re-assembled correctly on the trailing newline.
func TestLineWriter_LongLines(t *testing.T) {
	got := captureShipper(t)
	w := NewLineWriter(got.s, SourceExecStdout, "e", "x", nil)

	chunk := strings.Repeat("a", 1024)
	for i := 0; i < 100; i++ {
		w.Write([]byte(chunk))
	}
	w.Write([]byte("\n"))
	w.Close(0)

	evs := got.drain(t, 2)
	if len(evs[0].Line) != 100*1024 {
		t.Errorf("expected re-assembled 100KB line, got %d bytes", len(evs[0].Line))
	}
}

// TestShipper_DropOldest: under flood, the bounded channel drops
// oldest events and reports the dropped count.
func TestShipper_DropOldest(t *testing.T) {
	s := New()
	// Force a tiny buffer to make the test fast.
	s.in = make(chan Event, 4)

	for i := 0; i < 100; i++ {
		s.Send(Event{Line: "x"})
	}
	st := s.Stats()
	if st.Dropped < 90 {
		t.Errorf("expected ~96 drops, got %d", st.Dropped)
	}
	if st.Queued != 4 {
		t.Errorf("expected buffer at capacity 4, got %d", st.Queued)
	}
}

// TestShipper_PreActivateHolds: Send before Activate queues into the
// ring; the flush loop holds the batch (no POST) and discards
// gracefully under cancellation.
func TestShipper_PreActivateHolds(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)

	for i := 0; i < 10; i++ {
		s.Send(Event{Line: "pre"})
	}
	// Let the flush ticker fire a couple of times.
	time.Sleep(500 * time.Millisecond)

	st := s.Stats()
	// No drops yet — buffer is well below capacity.
	if st.Dropped != 0 {
		t.Errorf("unexpected drops pre-activate: %d", st.Dropped)
	}
	cancel()
}

// captureShipper steals events directly off the Shipper's channel and
// stashes them in a slice. No Run/HTTP needed — perfect for unit
// testing the producers (LineWriter, etc.) in isolation.
type fakeShipper struct {
	s   *Shipper
	mu  sync.Mutex
	got []Event
}

func captureShipper(t *testing.T) *fakeShipper {
	t.Helper()
	fs := &fakeShipper{s: New()}
	go func() {
		for ev := range fs.s.in {
			fs.mu.Lock()
			fs.got = append(fs.got, ev)
			fs.mu.Unlock()
		}
	}()
	return fs
}

func (fs *fakeShipper) drain(t *testing.T, want int) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		if len(fs.got) >= want {
			out := make([]Event, want)
			copy(out, fs.got[:want])
			fs.mu.Unlock()
			return out
		}
		fs.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	t.Fatalf("only got %d events, wanted %d", len(fs.got), want)
	return nil
}
