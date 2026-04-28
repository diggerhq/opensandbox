//go:build linux

package main

import (
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
)

// startReaper spawns a goroutine that waits on SIGCHLD and reaps any
// available zombie children. This is mandatory when osb-agent runs as PID 1
// (always true inside the sandbox VM) — Linux re-parents orphaned processes
// to PID 1, which is then responsible for calling wait() on them. If we
// don't, zombies accumulate in the sandbox cgroup's pids.max budget and
// eventually every fork() returns EAGAIN.
//
// Also calls prctl(PR_SET_CHILD_SUBREAPER) defensively so reaping still
// works even if some future setup parents us to PID > 1.
func startReaper() {
	// Best-effort: PR_SET_CHILD_SUBREAPER (option 36). If running as PID 1
	// this is a no-op; if not, it makes us inherit orphans of our subtree.
	const PR_SET_CHILD_SUBREAPER = 36
	_, _, _ = syscall.Syscall(syscall.SYS_PRCTL, PR_SET_CHILD_SUBREAPER, 1, 0)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for range ch {
			drainZombies()
		}
	}()
}

var reapedTotal atomic.Int64

// drainZombies repeatedly waits for any child in non-blocking mode until
// none are available. Multiple SIGCHLDs that arrive while we're already
// reaping coalesce into one delivery, so each notification must drain all.
func drainZombies() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 {
			// 0 = children exist but none ready; ECHILD = no children.
			if err != nil && err != syscall.ECHILD {
				log.Printf("agent: reaper: wait4 error: %v", err)
			}
			return
		}
		n := reapedTotal.Add(1)
		// Log first few + every 100th so the journal stays readable.
		if n <= 5 || n%100 == 0 {
			log.Printf("agent: reaped pid=%d (status=%d, total=%d)", pid, ws.ExitStatus(), n)
		}
	}
}
