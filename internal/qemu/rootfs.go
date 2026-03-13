package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PrepareRootfs copies a base rootfs image to the sandbox directory.
// Uses reflink (--reflink=auto) on XFS/btrfs for instant copy-on-write.
func PrepareRootfs(baseImage, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("mkdir for rootfs: %w", err)
	}

	cmd := exec.Command("cp", "--reflink=auto", baseImage, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy rootfs: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// CreateWorkspace creates a sparse ext4 filesystem for the sandbox workspace.
func CreateWorkspace(path string, sizeMB int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir for workspace: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create workspace file: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("truncate workspace: %w", err)
	}
	f.Close()

	cmd := exec.Command("mkfs.ext4", "-q", "-F",
		"-L", "workspace",
		"-O", "^has_journal",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// ResolveBaseImage finds the base rootfs image for a given template.
func ResolveBaseImage(imagesDir, template string) (string, error) {
	if template == "" {
		template = "ubuntu"
	}

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
