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

// orphanYoungProcessGrace is the minimum age a qemu-system process must have
// before we consider it a reap candidate. Wake / create / fork code starts
// QEMU via cmd.Start() and only registers the sandbox in m.vms *after* QMP
// connect + loadvm + virtio-mem plug + cont + agent connect — 5-30 seconds
// for hibernated wake under chunked download. Reaping a just-started qemu in
// that window kills the in-progress operation: the caller observes
// "QMP cont: write: broken pipe" or "agent not ready after 10s".
//
// 5 min is ~10× the worst observed wake; future slowdowns (bigger archives,
// slower network, more contention) won't re-expose the race. Real orphans
// linger up to ~5 min longer than before the fix — fine, they accrued for
// hours silently before this reaper existed at all.
const orphanYoungProcessGrace = 5 * time.Minute

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

	now := time.Now()
	for sandboxID, p := range pidByID {
		if known[sandboxID] {
			continue
		}
		// Skip too-young processes: wake / create / fork start QEMU before
		// registering the sandbox in m.vms (QMP setup + loadvm + cont + agent
		// connect happen between cmd.Start and m.vms[id] = vm, up to ~30s for
		// hibernated wake under chunked download). Killing one of those mid-
		// flight surfaces as "QMP cont: broken pipe" or "agent not ready" to
		// the caller. If the process is genuinely orphaned, a later sweep
		// catches it once it ages past the grace window.
		if age := now.Sub(p.start); age < orphanYoungProcessGrace {
			log.Printf("qemu: orphan-reaper: skip pid=%d sandbox=%s — process too young (%s < %s)",
				p.pid, sandboxID, age.Round(time.Second), orphanYoungProcessGrace)
			continue
		}
		log.Printf("qemu: orphan-reaper: found leaked qemu pid=%d sandbox=%s (not in vm registry, age=%s), terminating",
			p.pid, sandboxID, now.Sub(p.start).Round(time.Second))
		if err := terminateAndWait(p.pid); err != nil {
			log.Printf("qemu: orphan-reaper: failed to terminate pid=%d: %v", p.pid, err)
			continue
		}
		// Best-effort sandbox dir cleanup. If destroyVM was supposed to remove
		// it but never did, do it now.
		sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", "sandboxes", sandboxID)
		if _, err := os.Stat(sandboxDir); err == nil {
			if err := os.RemoveAll(sandboxDir); err != nil {
				log.Printf("qemu: orphan-reaper: removed pid=%d but failed to clean %s: %v",
					p.pid, sandboxDir, err)
			} else {
				log.Printf("qemu: orphan-reaper: cleaned up sandbox dir %s", sandboxDir)
			}
		}
	}
}

// procInfo carries a qemu-system process's pid + start time. Start time is
// resolved from /proc/PID stat info (the directory's mtime is good enough —
// it's set at process creation and doesn't get touched after).
type procInfo struct {
	pid   int
	start time.Time
}

// scanQEMUProcesses walks /proc and returns a map of sandboxID -> procInfo
// for every qemu-system-x86_64 process whose cmdline references a sandbox dir.
// The start time on each procInfo is the mtime of /proc/PID, which Linux
// stamps at process creation and leaves untouched afterward — close enough
// to a real start time for the orphan-reaper's grace check.
func scanQEMUProcesses() (map[string]procInfo, error) {
	procEntries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[string]procInfo)
	for _, entry := range procEntries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		procDir := filepath.Join("/proc", entry.Name())
		// Read /proc/PID/comm — cheap, only ~16 bytes; skip non-qemu fast.
		commBytes, err := os.ReadFile(filepath.Join(procDir, "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commBytes))
		if !strings.HasPrefix(comm, "qemu-system") {
			continue
		}
		// /proc/PID/cmdline is NUL-separated; we scan it as a single blob.
		cmdlineBytes, err := os.ReadFile(filepath.Join(procDir, "cmdline"))
		if err != nil {
			continue
		}
		cmdline := string(cmdlineBytes)
		match := sandboxIDRe.FindStringSubmatch(cmdline)
		if len(match) < 2 {
			continue
		}
		sandboxID := match[1]
		st, err := os.Stat(procDir)
		if err != nil {
			continue
		}
		// Multiple qemu processes for the same sandbox shouldn't exist —
		// if they do, the first one wins; the duplicate will be reaped on
		// the next pass after the registered one is removed.
		if _, dup := out[sandboxID]; !dup {
			out[sandboxID] = procInfo{pid: pid, start: st.ModTime()}
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
