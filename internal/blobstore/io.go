package blobstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// writeAtomic streams r to destPath via destPath+".tmp" + rename. The
// temp file is removed on any failure. Parent directories are created if
// missing.
func writeAtomic(destPath string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("blobstore: mkdir parent: %w", err)
	}
	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("blobstore: create tmp: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("blobstore: copy: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("blobstore: close tmp: %w", err)
	}
	if err := os.Rename(tmp, destPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("blobstore: rename: %w", err)
	}
	return nil
}

// openSized opens srcPath and returns the file along with its size, ready
// for streaming Put.
func openSized(srcPath string) (*os.File, int64, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, 0, fmt.Errorf("blobstore: open src: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("blobstore: stat src: %w", err)
	}
	return f, st.Size(), nil
}
