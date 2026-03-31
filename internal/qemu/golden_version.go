package qemu

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// computeGoldenVersion computes a version hash for a golden snapshot by hashing
// the first 1MB of the base image. Two workers with the same base image will
// produce the same hash. This is fast (1MB read) and sufficient to detect
// different base images (kernel, OS, packages).
func computeGoldenVersion(baseImagePath string) (string, error) {
	f, err := os.Open(baseImagePath)
	if err != nil {
		return "", fmt.Errorf("open base image: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.CopyN(h, f, 1024*1024); err != nil && err != io.EOF {
		return "", fmt.Errorf("read base image: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16], nil
}
