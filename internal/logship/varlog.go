package logship

import (
	"bufio"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// skipFilenamePattern matches files inside /var/log/* that we never
// tail: rotated/compressed archives and binary system logs.
var skipFilenamePattern = regexp.MustCompile(`(^|\.)(wtmp|btmp|lastlog|gz|bz2|xz|zst)$|\.\d+(\.\w+)?$`)

// VarLogTailer watches a root directory (typically /var/log) recursively
// and ships every line appended to any regular file there. New files
// (created during the sandbox's lifetime) are picked up via inotify
// IN_CREATE.
//
// Files are tailed from their *current* end, not from the beginning —
// we don't backfill historical content (it's noisy, often binary, and
// would dump megabytes of irrelevant context every sandbox boot).
type VarLogTailer struct {
	root    string
	shipper *Shipper

	mu      sync.Mutex
	tailing map[string]*tailedFile // path → state
}

type tailedFile struct {
	path   string
	offset int64
}

// NewVarLogTailer creates a tailer rooted at the given directory.
func NewVarLogTailer(shipper *Shipper, root string) *VarLogTailer {
	return &VarLogTailer{
		root:    root,
		shipper: shipper,
		tailing: make(map[string]*tailedFile),
	}
}

// Run starts watching. Returns when ctx is cancelled or the watcher
// fails to initialise. Errors from individual file reads are logged
// but do not stop the loop.
func (t *VarLogTailer) Run(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("logship/varlog: NewWatcher: %v (logs from %s will not be shipped)", err, t.root)
		return
	}
	defer w.Close()

	// Walk root once; register every existing regular file at offset =
	// EOF, register every dir for IN_CREATE notifications.
	if err := filepath.Walk(t.root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			_ = w.Add(path)
			return nil
		}
		if t.skipFile(path, info) {
			return nil
		}
		t.startTail(path, info.Size())
		return nil
	}); err != nil {
		log.Printf("logship/varlog: walk %s: %v", t.root, err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			t.handleEvent(w, ev)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			log.Printf("logship/varlog: watcher error: %v", err)
		}
	}
}

func (t *VarLogTailer) handleEvent(w *fsnotify.Watcher, ev fsnotify.Event) {
	switch {
	case ev.Op&fsnotify.Create != 0:
		info, err := os.Stat(ev.Name)
		if err != nil {
			return
		}
		if info.IsDir() {
			_ = w.Add(ev.Name)
			return
		}
		if t.skipFile(ev.Name, info) {
			return
		}
		// New file: tail from byte 0 (it's brand new).
		t.startTail(ev.Name, 0)
		t.readAvailable(ev.Name)
	case ev.Op&fsnotify.Write != 0:
		t.readAvailable(ev.Name)
	case ev.Op&fsnotify.Remove != 0, ev.Op&fsnotify.Rename != 0:
		t.mu.Lock()
		delete(t.tailing, ev.Name)
		t.mu.Unlock()
	}
}

func (t *VarLogTailer) startTail(path string, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.tailing[path]; exists {
		return
	}
	t.tailing[path] = &tailedFile{path: path, offset: offset}
}

func (t *VarLogTailer) readAvailable(path string) {
	t.mu.Lock()
	tf, ok := t.tailing[path]
	t.mu.Unlock()
	if !ok {
		return
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return
	}

	// Truncation: file shrank. Reset to start.
	if info.Size() < tf.offset {
		tf.offset = 0
	}

	if _, err := f.Seek(tf.offset, io.SeekStart); err != nil {
		return
	}

	br := bufio.NewReader(f)
	var newOffset = tf.offset
	for {
		line, err := br.ReadString('\n')
		newOffset += int64(len(line))
		// Trim the trailing newline before shipping.
		stripped := strings.TrimRight(line, "\r\n")
		if stripped != "" {
			t.shipper.Send(Event{
				Source: SourceVarLog,
				Line:   stripped,
				Path:   path,
			})
		}
		if err != nil {
			break
		}
	}
	tf.offset = newOffset
}

func (t *VarLogTailer) skipFile(path string, info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return true
	}
	name := filepath.Base(path)
	if skipFilenamePattern.MatchString(name) {
		return true
	}
	// Skip files that look binary (first 256 bytes contain a NUL).
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	var sniff [256]byte
	n, _ := f.Read(sniff[:])
	for i := 0; i < n; i++ {
		if sniff[i] == 0 {
			return true
		}
	}
	return false
}
