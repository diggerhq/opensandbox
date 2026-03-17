package qemu

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// PrepareRootfs creates a qcow2 overlay backed by the base rootfs image.
// This is instant — no data is copied. Writes go to the qcow2 overlay,
// reads fall through to the raw backing file. Works on any filesystem.
func PrepareRootfs(baseImage, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("mkdir for rootfs: %w", err)
	}

	// Use absolute path for backing file
	absBase, err := filepath.Abs(baseImage)
	if err != nil {
		return fmt.Errorf("abs path for base image: %w", err)
	}

	// qemu-img create -f qcow2 -b /abs/path/base.ext4 -F raw dest.qcow2
	cmd := exec.Command("qemu-img", "create",
		"-f", "qcow2",
		"-b", absBase,
		"-F", "raw",
		destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create qcow2 overlay: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// PrepareRootfsRaw copies a base rootfs image using reflink (for golden snapshot).
func PrepareRootfsRaw(baseImage, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("mkdir for rootfs: %w", err)
	}
	cmd := exec.Command("cp", "--reflink=auto", baseImage, destPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copy rootfs: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateWorkspaceRaw creates a raw ext4 workspace (for golden snapshot path).
func CreateWorkspaceRaw(path string, sizeMB int) error {
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
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-L", "workspace", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("mkfs.ext4: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateWorkspace creates a qcow2 workspace disk with an ext4 filesystem.
// First creates a raw ext4 image, then converts to qcow2 for snapshot support.
func CreateWorkspace(path string, sizeMB int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir for workspace: %w", err)
	}

	// Create raw ext4 first
	rawPath := path + ".raw"
	f, err := os.Create(rawPath)
	if err != nil {
		return fmt.Errorf("create workspace file: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		os.Remove(rawPath)
		return fmt.Errorf("truncate workspace: %w", err)
	}
	f.Close()

	mkfsCmd := exec.Command("mkfs.ext4", "-q", "-F",
		"-L", "workspace",
		rawPath)
	if out, err := mkfsCmd.CombinedOutput(); err != nil {
		os.Remove(rawPath)
		return fmt.Errorf("mkfs.ext4: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Convert to qcow2
	cmd := exec.Command("qemu-img", "convert",
		"-f", "raw", "-O", "qcow2",
		rawPath, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(rawPath)
		return fmt.Errorf("convert workspace to qcow2: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	os.Remove(rawPath)

	return nil
}

// detectDrivePath returns the actual path for a drive file (rootfs or workspace),
// preferring qcow2 if it exists, falling back to ext4.
func detectDrivePath(sandboxDir, prefix string) string {
	qcow2 := filepath.Join(sandboxDir, prefix+".qcow2")
	if _, err := os.Stat(qcow2); err == nil {
		return qcow2
	}
	return filepath.Join(sandboxDir, prefix+".ext4")
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
