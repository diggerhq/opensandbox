package qemu

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// orphanReapInterval is how often we scan /proc for leaked qemu processes.
// 60s is short enough that a leak doesn't accumulate for hours, long enough
// that the scan cost is negligible (one sweep of /proc per minute).
const orphanReapInterval = 60 * time.Second

// orphanGraceTermDelay is how long we wait between SIGTERM and SIGKILL when
// reaping an orphan. QEMU usually exits cleanly on SIGTERM within ~1s; 5s is
// generous and bounds the reap pass at ~5s per orphan in the worst case.
const orphanGraceTermDelay = 5 * time.Second

// sandboxIDRe extracts an sb-xxxxxxxx sandbox ID from a qemu command line.
// QEMU's cmdline embeds the sandboxDir as a -drive file=… and -chardev path=…
// argument, so the ID always appears in the form /data/sandboxes/sandboxes/sb-XXX/.
var sandboxIDRe = regexp.MustCompile(`/sandboxes/(sb-[a-z0-9]+)/`)

// StartOrphanReaper launches a background goroutine that periodically scans
// /proc for qemu-system processes whose sandbox ID is not in the worker's VM
// registry, and kills them.
//
// Why this is needed: when destroyVM races with a state inconsistency (e.g.,
// a hibernate-on-timeout that ran without the VM being in m.vms, a panic in
// a sandbox goroutine that exits before cleanup, or a worker crash that left
// children behind), a qemu-system-x86_64 process can survive past the worker's
// knowledge of it. Those orphans hold a TAP, an agent.sock, and a vCPU — they
// silently shrink real worker capacity until the worker is restarted. We saw
// this in the field: after a load test, Worker A had 2 zombie + 1 live qemu
// from a session that ended 30 minutes earlier, which then made every new
// fork on that worker fail.
func (m *Manager) StartOrphanReaper(ctx context.Context) {
	go m.orphanReaperLoop(ctx)
}

func (m *Manager) orphanReaperLoop(ctx context.Context) {
	ticker := time.NewTicker(orphanReapInterval)
	defer ticker.Stop()
	// First scan happens after one interval — gives the worker time to
	// register VMs at startup before we start judging them as orphans.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reapOrphans()
		}
	}
}

// reapOrphans scans /proc for qemu-system processes belonging to sandboxes
// that are no longer in m.vms, and kills them.
func (m *Manager) reapOrphans() {
	pidByID, err := scanQEMUProcesses()
	if err != nil {
		log.Printf("qemu: orphan-reaper: scan failed: %v", err)
		return
	}
	if len(pidByID) == 0 {
		return
	}

	m.mu.RLock()
	known := make(map[string]bool, len(m.vms))
	for id := range m.vms {
		known[id] = true
	}
	m.mu.RUnlock()

	for sandboxID, pid := range pidByID {
		if known[sandboxID] {
			continue
		}
		log.Printf("qemu: orphan-reaper: found leaked qemu pid=%d sandbox=%s (not in vm registry), terminating",
			pid, sandboxID)
		if err := terminateAndWait(pid); err != nil {
			log.Printf("qemu: orphan-reaper: failed to terminate pid=%d: %v", pid, err)
			continue
		}
		// Best-effort sandbox dir cleanup. If destroyVM was supposed to remove
		// it but never did, do it now.
		sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", "sandboxes", sandboxID)
		if _, err := os.Stat(sandboxDir); err == nil {
			if err := os.RemoveAll(sandboxDir); err != nil {
				log.Printf("qemu: orphan-reaper: removed pid=%d but failed to clean %s: %v",
					pid, sandboxDir, err)
			} else {
				log.Printf("qemu: orphan-reaper: cleaned up sandbox dir %s", sandboxDir)
			}
		}
	}
}

// scanQEMUProcesses walks /proc and returns a map of sandboxID -> pid for
// every qemu-system-x86_64 process whose cmdline references a sandbox dir.
func scanQEMUProcesses() (map[string]int, error) {
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[string]int)
	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// Read /proc/PID/comm — cheap, only ~16 bytes; skip non-qemu fast.
		commBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commBytes))
		if !strings.HasPrefix(comm, "qemu-system") {
			continue
		}
		// /proc/PID/cmdline is NUL-separated; we scan it as a single blob.
		cmdlineBytes, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmdline := string(cmdlineBytes)
		match := sandboxIDRe.FindStringSubmatch(cmdline)
		if len(match) < 2 {
			continue
		}
		sandboxID := match[1]
		// Multiple qemu processes for the same sandbox shouldn't exist —
		// if they do, the first one wins; the duplicate will be reaped on
		// the next pass after the registered one is removed.
		if _, dup := out[sandboxID]; !dup {
			out[sandboxID] = pid
		}
	}
	return out, nil
}

// terminateAndWait sends SIGTERM, waits up to orphanGraceTermDelay, then
// SIGKILLs if the process is still alive. Returns nil if the process is
// gone (whatever the path).
func terminateAndWait(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// SIGTERM lets QEMU shut down its devices cleanly, which avoids leaving
	// stale qcow2 locks. If it's already wedged it won't help; we'll SIGKILL
	// after the grace window.
	_ = proc.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(orphanGraceTermDelay)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pidAlive(pid) {
		_ = proc.Kill()
		// Brief wait for kernel to flush the kill.
		for i := 0; i < 25 && pidAlive(pid); i++ {
			time.Sleep(40 * time.Millisecond)
		}
	}
	return nil
}

// pidAlive returns true if /proc/PID still exists. We don't use kill(0)
// because that fails with EPERM if the worker isn't root for some reason —
// /proc is more permissive.
func pidAlive(pid int) bool {
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil
}
