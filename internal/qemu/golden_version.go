package qemu

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// computeGoldenVersion computes a version hash for a golden snapshot by hashing
// the entire base image. Two workers with byte-identical base images produce
// the same hash; any difference — even bytes past the first megabyte — yields
// a different hash. ~20s for a 4GB base on NVMe, called only on worker startup.
//
// History: previously hashed only the first 1MB, which collided when bases
// were rebuilt with the same superblock/boot region but different userspace.
// That caused ensureCheckpointRebased to miss real mismatches and silently
// serve stale overlays against fresh bases, producing ext4 directory-block
// checksum errors in guests.
func computeGoldenVersion(baseImagePath string) (string, error) {
	f, err := os.Open(baseImagePath)
	if err != nil {
		return "", fmt.Errorf("open base image: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read base image: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16], nil
}

// ComputeGoldenVersion is the exported entry point used by cmd/worker's
// "golden-version" subcommand so Packer invokes the same hash function
// the runtime uses for archive-key lookups.
func ComputeGoldenVersion(baseImagePath string) (string, error) {
	return computeGoldenVersion(baseImagePath)
}
