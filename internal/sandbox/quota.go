package sandbox

import (
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SetDiskQuota enforces a disk quota on a sandbox's data directory using XFS project quotas.
// This requires the filesystem to be mounted with -o prjquota and xfs_quota to be available.
// If quotas are not supported (e.g., dev mode on non-XFS), the error is logged and ignored.
func (m *Manager) SetDiskQuota(sandboxID string, limitMB int) {
	if m.dataDir == "" || limitMB <= 0 {
		return
	}

	projectID := sandboxProjectID(sandboxID)
	sandboxDir := filepath.Join(m.dataDir, sandboxID)

	// Register the project in /etc/projects and /etc/projid (idempotent)
	if err := registerXFSProject(projectID, sandboxDir, sandboxID); err != nil {
		log.Printf("quota: failed to register project for %s: %v (quotas disabled for this sandbox)", sandboxID, err)
		return
	}

	// Initialize the project directory
	initCmd := exec.Command("xfs_quota", "-x", "-c",
		fmt.Sprintf("project -s %d", projectID),
		m.dataDir)
	if out, err := initCmd.CombinedOutput(); err != nil {
		log.Printf("quota: failed to init project %d for %s: %v (%s)", projectID, sandboxID, err, strings.TrimSpace(string(out)))
		return
	}

	// Set the hard block limit
	limitCmd := exec.Command("xfs_quota", "-x", "-c",
		fmt.Sprintf("limit -p bhard=%dm %d", limitMB, projectID),
		m.dataDir)
	if out, err := limitCmd.CombinedOutput(); err != nil {
		log.Printf("quota: failed to set limit for %s: %v (%s)", sandboxID, err, strings.TrimSpace(string(out)))
		return
	}

	log.Printf("quota: set %dMB disk limit for sandbox %s (project %d)", limitMB, sandboxID, projectID)
}

// RemoveDiskQuota removes the XFS project quota entries for a sandbox.
func (m *Manager) RemoveDiskQuota(sandboxID string) {
	if m.dataDir == "" {
		return
	}

	projectID := sandboxProjectID(sandboxID)

	// Remove limit (set to 0 = unlimited)
	limitCmd := exec.Command("xfs_quota", "-x", "-c",
		fmt.Sprintf("limit -p bhard=0 %d", projectID),
		m.dataDir)
	_ = limitCmd.Run()

	// Clean up /etc/projects and /etc/projid entries
	removeXFSProject(projectID, sandboxID)
}

// sandboxProjectID generates a stable project ID from a sandbox ID.
// XFS project IDs are uint32; we use FNV-1a hash and avoid 0 (reserved).
func sandboxProjectID(sandboxID string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(sandboxID))
	id := h.Sum32()
	if id == 0 {
		id = 1
	}
	return id
}

// registerXFSProject adds entries to /etc/projects and /etc/projid if not already present.
func registerXFSProject(projectID uint32, dir, sandboxID string) error {
	idStr := strconv.FormatUint(uint64(projectID), 10)
	projectLine := fmt.Sprintf("%s:%s", idStr, dir)
	projidLine := fmt.Sprintf("%s:sandbox-%s", idStr, sandboxID)

	if err := appendLineIfMissing("/etc/projects", projectLine); err != nil {
		return fmt.Errorf("failed to update /etc/projects: %w", err)
	}
	if err := appendLineIfMissing("/etc/projid", projidLine); err != nil {
		return fmt.Errorf("failed to update /etc/projid: %w", err)
	}
	return nil
}

// removeXFSProject removes entries from /etc/projects and /etc/projid.
func removeXFSProject(projectID uint32, sandboxID string) {
	idStr := strconv.FormatUint(uint64(projectID), 10)
	removeLineByPrefix("/etc/projects", idStr+":")
	removeLineByPrefix("/etc/projid", idStr+":")
}

func appendLineIfMissing(filePath, line string) error {
	data, err := os.ReadFile(filePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	content := string(data)
	if strings.Contains(content, line) {
		return nil
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(line + "\n")
	return err
}

func removeLineByPrefix(filePath, prefix string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return
	}

	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			kept = append(kept, line)
		}
	}

	_ = os.WriteFile(filePath, []byte(strings.Join(kept, "\n")), 0644)
}
