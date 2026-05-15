package logship

import (
	"bytes"
	"sync"
)

// LineWriter adapts a stream of bytes (e.g. exec stdout/stderr) into
// line-segmented Events fed to a Shipper. Implements io.Writer so it
// drops cleanly into io.MultiWriter alongside the existing per-exec
// pipes.
//
// Lines are split on '\n'. A trailing partial line (no terminating
// newline) is held in an internal buffer and emitted on Close, along
// with one synthetic "EOF" event carrying the exit code so the UI can
// render "command X exited 0/1".
type LineWriter struct {
	shipper *Shipper
	source  Source
	execID  string
	command string
	argv    []string

	mu    sync.Mutex
	buf   bytes.Buffer
	final bool // Close() called
}

// NewLineWriter returns a LineWriter that ships to the given Shipper
// under the given Source. execID is a unique-per-exec identifier (the
// caller generates it; ULID/UUID both fine). command + argv are
// stamped on every line so the UI can drill into "all output of this
// exec" without joining tables.
func NewLineWriter(shipper *Shipper, source Source, execID, command string, argv []string) *LineWriter {
	return &LineWriter{
		shipper: shipper,
		source:  source,
		execID:  execID,
		command: command,
		argv:    argv,
	}
}

// Write splits incoming bytes on '\n' and ships each completed line as
// an Event. A trailing partial is held in the buffer.
func (w *LineWriter) Write(p []byte) (int, error) {
	if w.shipper == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.final {
		// Close already called — don't ship more.
		return len(p), nil
	}

	w.buf.Write(p)

	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx])
		// Drop the consumed bytes (including the newline).
		w.buf.Next(idx + 1)

		w.shipper.Send(Event{
			Source:  w.source,
			Line:    line,
			ExecID:  w.execID,
			Command: w.command,
			Argv:    w.argv,
		})
	}
	return len(p), nil
}

// Close flushes any partial-line residue and emits one final synthetic
// event carrying the exit code. After Close, further Writes are
// silently dropped (defensive — a well-behaved caller should not Write
// after Close, but stdout/stderr pipes can race with Wait).
//
// Pass exitCode = -1 if the process did not exit cleanly (signal /
// timeout / killed); the EOF event records it as -1 unchanged so the
// UI can distinguish "exited" from "killed".
func (w *LineWriter) Close(exitCode int) {
	if w.shipper == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.final {
		return
	}
	w.final = true

	// Flush any trailing partial line.
	if w.buf.Len() > 0 {
		w.shipper.Send(Event{
			Source:  w.source,
			Line:    w.buf.String(),
			ExecID:  w.execID,
			Command: w.command,
			Argv:    w.argv,
		})
		w.buf.Reset()
	}

	// Final synthetic event marking the exec's end.
	ec := exitCode
	w.shipper.Send(Event{
		Source:   w.source,
		Line:     "",
		ExecID:   w.execID,
		Command:  w.command,
		Argv:     w.argv,
		ExitCode: &ec,
	})
}
