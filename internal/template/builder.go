package template

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opensandbox/opensandbox/internal/podman"
)

// Builder builds ext4 rootfs images from Dockerfiles using Podman as a build tool.
// The workflow: Dockerfile → podman build → podman create → podman export → tar2ext4 → images dir.
// The osb-agent binary and init script are injected into the image during build.
type Builder struct {
	podman    *podman.Client
	imagesDir string // target directory for ext4 images (e.g., /data/firecracker/images/)
	agentPath string // path to osb-agent binary to inject into images
}

// NewBuilder creates a new template builder.
// imagesDir is where completed ext4 images are stored.
// agentPath is the osb-agent binary to inject (if empty, uses /usr/local/bin/osb-agent).
func NewBuilder(client *podman.Client, imagesDir, agentPath string) *Builder {
	if agentPath == "" {
		agentPath = "/usr/local/bin/osb-agent"
	}
	return &Builder{
		podman:    client,
		imagesDir: imagesDir,
		agentPath: agentPath,
	}
}

// Build builds an ext4 rootfs image from a Dockerfile.
// The resulting image is placed at {imagesDir}/{name}.ext4.
// Returns the image path and the build log.
func (b *Builder) Build(ctx context.Context, dockerfile, name, tag, _ string) (string, string, error) {
	if tag == "" {
		tag = "latest"
	}

	localImage := fmt.Sprintf("localhost/opensandbox-template/%s:%s", name, tag)

	// Write Dockerfile to temp directory
	tmpDir, err := os.MkdirTemp("", "opensandbox-build-*")
	if err != nil {
		return "", "", fmt.Errorf("failed to create temp dir for build: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Inject osb-agent and init script into the Dockerfile
	augmentedDockerfile := b.augmentDockerfile(dockerfile)

	dockerfilePath := filepath.Join(tmpDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(augmentedDockerfile), 0644); err != nil {
		return "", "", fmt.Errorf("failed to write Dockerfile: %w", err)
	}

	// Copy agent binary into build context so COPY instruction works
	if _, err := os.Stat(b.agentPath); err == nil {
		agentDest := filepath.Join(tmpDir, "osb-agent")
		if err := copyFile(b.agentPath, agentDest); err != nil {
			return "", "", fmt.Errorf("failed to copy agent binary to build context: %w", err)
		}
	}

	// Copy init script into build context
	initScript := generateInitScript()
	initPath := filepath.Join(tmpDir, "init")
	if err := os.WriteFile(initPath, []byte(initScript), 0755); err != nil {
		return "", "", fmt.Errorf("failed to write init script: %w", err)
	}

	// Build image with podman
	log.Printf("template: building image %s from Dockerfile...", localImage)
	result, err := b.podman.Run(ctx, "build", "-t", localImage, "-f", dockerfilePath, tmpDir)
	if err != nil {
		return "", "", fmt.Errorf("failed to build template %s: %w", name, err)
	}
	if result.ExitCode != 0 {
		return "", "", fmt.Errorf("podman build failed (exit %d): %s", result.ExitCode, result.Stderr)
	}
	buildLog := result.Stdout + result.Stderr

	// Export container filesystem as tar
	log.Printf("template: exporting filesystem for %s...", name)
	tarPath := filepath.Join(tmpDir, "rootfs.tar")
	if err := b.exportImage(ctx, localImage, tarPath); err != nil {
		return "", buildLog, fmt.Errorf("failed to export image: %w", err)
	}

	// Convert tar to ext4
	log.Printf("template: converting to ext4 for %s...", name)
	ext4Path := filepath.Join(tmpDir, "rootfs.ext4")
	if err := tarToExt4(tarPath, ext4Path, 4096); err != nil {
		return "", buildLog, fmt.Errorf("failed to convert to ext4: %w", err)
	}

	// Move to images directory
	if err := os.MkdirAll(b.imagesDir, 0755); err != nil {
		return "", buildLog, fmt.Errorf("failed to create images dir: %w", err)
	}
	destPath := filepath.Join(b.imagesDir, name+".ext4")
	if err := os.Rename(ext4Path, destPath); err != nil {
		// Cross-device: copy + remove
		if err := copyFile(ext4Path, destPath); err != nil {
			return "", buildLog, fmt.Errorf("failed to move ext4 to images dir: %w", err)
		}
		os.Remove(ext4Path)
	}

	// Clean up local image
	_, _ = b.podman.Run(ctx, "rmi", "-f", localImage)

	log.Printf("template: built %s (%s)", name, destPath)
	return destPath, buildLog, nil
}

// augmentDockerfile appends instructions to inject osb-agent and init script.
func (b *Builder) augmentDockerfile(dockerfile string) string {
	// Append agent + init injection after the user's Dockerfile
	injection := `

# --- OpenSandbox agent injection ---
COPY osb-agent /usr/local/bin/osb-agent
RUN chmod +x /usr/local/bin/osb-agent
COPY init /sbin/init
RUN chmod +x /sbin/init
# Ensure workspace mount point exists
RUN mkdir -p /workspace
`
	return dockerfile + injection
}

// exportImage creates a container from the image, exports its filesystem as a tar.
func (b *Builder) exportImage(ctx context.Context, image, tarPath string) error {
	// Create a container (don't start it)
	containerName := "osb-export-" + filepath.Base(tarPath)
	result, err := b.podman.Run(ctx, "create", "--name", containerName, image, "/bin/true")
	if err != nil {
		return fmt.Errorf("podman create: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman create failed (exit %d): %s", result.ExitCode, result.Stderr)
	}
	containerID := strings.TrimSpace(result.Stdout)
	if containerID == "" {
		containerID = containerName
	}

	// Export container filesystem
	defer func() {
		_, _ = b.podman.Run(ctx, "rm", "-f", containerID)
	}()

	result, err = b.podman.Run(ctx, "export", "-o", tarPath, containerID)
	if err != nil {
		return fmt.Errorf("podman export: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("podman export failed (exit %d): %s", result.ExitCode, result.Stderr)
	}

	return nil
}

// tarToExt4 converts a tar archive to an ext4 filesystem image.
// sizeMB is the target size of the ext4 image.
func tarToExt4(tarPath, ext4Path string, sizeMB int) error {
	// Create sparse ext4 image
	f, err := os.Create(ext4Path)
	if err != nil {
		return fmt.Errorf("create ext4 file: %w", err)
	}
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		f.Close()
		return fmt.Errorf("truncate ext4: %w", err)
	}
	f.Close()

	// Format as ext4
	cmd := exec.Command("mkfs.ext4", "-q", "-F",
		"-L", "rootfs",
		"-d", "/dev/null", // dummy, we'll populate via mount
		ext4Path)
	if out, err := cmd.CombinedOutput(); err != nil {
		// mkfs.ext4 -d might not be available on older versions, try without
		cmd2 := exec.Command("mkfs.ext4", "-q", "-F", "-L", "rootfs", ext4Path)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return fmt.Errorf("mkfs.ext4: %w (%s / %s)", err2, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}

	// Mount the ext4 image and extract tar into it
	mountDir, err := os.MkdirTemp("", "osb-mount-*")
	if err != nil {
		return fmt.Errorf("create mount dir: %w", err)
	}
	defer os.RemoveAll(mountDir)

	// Mount using loop device
	mountCmd := exec.Command("mount", "-o", "loop", ext4Path, mountDir)
	if out, err := mountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount ext4: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	defer func() {
		exec.Command("umount", mountDir).Run()
	}()

	// Extract tar into mounted filesystem
	tarCmd := exec.Command("tar", "xf", tarPath, "-C", mountDir)
	if out, err := tarCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract tar: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Ensure key directories exist
	for _, dir := range []string{"proc", "sys", "dev", "tmp", "workspace", "run"} {
		os.MkdirAll(filepath.Join(mountDir, dir), 0755)
	}

	// Sync and unmount
	exec.Command("sync").Run()
	umountCmd := exec.Command("umount", mountDir)
	if out, err := umountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("umount: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Shrink the ext4 image to minimum size + headroom
	resizeCmd := exec.Command("resize2fs", "-M", ext4Path)
	if out, err := resizeCmd.CombinedOutput(); err != nil {
		// Non-fatal — image just stays at full size
		log.Printf("template: resize2fs -M warning: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	return nil
}

// generateInitScript creates the /sbin/init script for Firecracker VMs.
func generateInitScript() string {
	return `#!/bin/busybox sh
# OpenSandbox VM init script
# Runs as PID 1 inside the Firecracker microVM

# Mount virtual filesystems
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /run

# Create device nodes if devtmpfs didn't
[ -c /dev/null ] || mknod -m 666 /dev/null c 1 3
[ -c /dev/zero ] || mknod -m 666 /dev/zero c 1 5
[ -c /dev/random ] || mknod -m 444 /dev/random c 1 8
[ -c /dev/urandom ] || mknod -m 444 /dev/urandom c 1 9
[ -c /dev/tty ] || mknod -m 666 /dev/tty c 5 0
[ -c /dev/console ] || mknod -m 600 /dev/console c 5 1
[ -d /dev/pts ] || mkdir -p /dev/pts
mount -t devpts devpts /dev/pts
[ -d /dev/shm ] || mkdir -p /dev/shm
mount -t tmpfs tmpfs /dev/shm

# Mount workspace from vdb
mkdir -p /workspace
mount /dev/vdb /workspace 2>/dev/null || {
    echo "init: warning: could not mount /dev/vdb, trying /dev/vdb1"
    mount /dev/vdb1 /workspace 2>/dev/null || echo "init: warning: workspace mount failed"
}

# Configure networking from kernel command line
# Format: ip=GUEST_IP::GATEWAY:NETMASK::IFACE:off osb.gateway=GATEWAY
for param in $(cat /proc/cmdline); do
    case "$param" in
        ip=*)
            IP_CONFIG="${param#ip=}"
            GUEST_IP=$(echo "$IP_CONFIG" | cut -d: -f1)
            GATEWAY=$(echo "$IP_CONFIG" | cut -d: -f3)
            NETMASK=$(echo "$IP_CONFIG" | cut -d: -f4)
            IFACE=$(echo "$IP_CONFIG" | cut -d: -f6)
            ;;
        osb.gateway=*)
            GATEWAY="${param#osb.gateway=}"
            ;;
    esac
done

if [ -n "$GUEST_IP" ] && [ -n "$IFACE" ]; then
    ip link set lo up
    ip addr add "${GUEST_IP}/30" dev "$IFACE"
    ip link set "$IFACE" up
    if [ -n "$GATEWAY" ]; then
        ip route add default via "$GATEWAY" dev "$IFACE"
    fi
fi

# Set up DNS
echo "nameserver 8.8.8.8" > /etc/resolv.conf
echo "nameserver 8.8.4.4" >> /etc/resolv.conf

# Set hostname
hostname sandbox

# Start the agent
exec /usr/local/bin/osb-agent
`
}

// copyFile copies a file from src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := out.ReadFrom(in); err != nil {
		return err
	}

	// Preserve execute permission
	srcInfo, err := os.Stat(src)
	if err == nil {
		out.Chmod(srcInfo.Mode())
	}

	return nil
}
