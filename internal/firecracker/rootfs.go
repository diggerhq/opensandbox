package firecracker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RootfsConfig holds paths for VM filesystem images.
type RootfsConfig struct {
	ImagesDir string // directory containing base rootfs images (e.g., /data/firecracker/images/)
}

// PrepareRootfs copies a base rootfs image to the sandbox directory.
// Uses reflink (--reflink=auto) on XFS/btrfs for instant copy-on-write.
func PrepareRootfs(baseImage, destPath string) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("mkdir for rootfs: %w", err)
	}

	// Try reflink copy first (instant on XFS with reflink support)
	cmd := exec.Command("cp", "--reflink=auto", baseImage, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy rootfs: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// CreateWorkspace creates a sparse ext4 filesystem for the sandbox workspace.
// The file is sparse — it only uses disk space as data is written.
func CreateWorkspace(path string, sizeMB int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir for workspace: %w", err)
	}

	// Create sparse file
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create workspace file: %w", err)
	}
	// Truncate to desired size (sparse — no disk used yet)
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("truncate workspace: %w", err)
	}
	f.Close()

	// Format as ext4
	cmd := exec.Command("mkfs.ext4", "-q", "-F",
		"-L", "workspace",
		"-O", "^has_journal",  // no journal for performance (we have S3 as source of truth)
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// ResolveBaseImage finds the base rootfs image for a given template.
// Templates map to image files: "ubuntu" → "ubuntu-base.ext4"
func ResolveBaseImage(imagesDir, template string) (string, error) {
	if template == "" {
		template = "ubuntu"
	}

	// Check for exact match first
	candidates := []string{
		filepath.Join(imagesDir, template+".ext4"),
		filepath.Join(imagesDir, template+"-base.ext4"),
		filepath.Join(imagesDir, template),
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	return "", fmt.Errorf("base image not found for template %q in %s", template, imagesDir)
}
