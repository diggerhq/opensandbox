package agent

import (
	"sync"
	"syscall"
)

// ReapRegistry coordinates the agent's SIGCHLD reaper with explicit-wait
// callers (Exec, PrepareHibernate, PTY). Without coordination the reaper's
// Wait4(-1, ...) races with cmd.Wait() — whichever lands first gets the
// child's exit status, and the other returns ECHILD. The loser's caller
// reports `exec failed: waitid: no child processes`, surfacing to users
// as ~30% fork failures with empty stdout when bench-style workloads hit
// the agent immediately after a fresh fork.
//
// Coordination protocol:
//  1. Caller (Exec handler) calls RegisterReap(pid) BEFORE the child can
//     exit (immediately after cmd.Start()).
//  2. Caller calls cmd.Wait() in the usual way.
//  3a. If cmd.Wait() succeeds, child status is in cmd.ProcessState. Caller
//      should call UnregisterReap(pid) to drop the dangling registration.
//  3b. If cmd.Wait() returns ECHILD, the reaper got there first; pull
//      the WaitStatus from the channel and synthesize the exit code.
//
// The reaper (cmd/agent/reaper_linux.go) calls TryDeliverReap on every
// child it consumes. If the pid is registered, it forwards the status via
// channel; otherwise it logs the orphan reap as before.
var reapRegistry sync.Map // pid (int) → chan syscall.WaitStatus

// RegisterReap claims a pid for explicit-wait. Returns a channel that
// receives the wait status when the reaper picks up the child. Channel is
// closed after the status is delivered.
func RegisterReap(pid int) <-chan syscall.WaitStatus {
	ch := make(chan syscall.WaitStatus, 1)
	reapRegistry.Store(pid, ch)
	return ch
}

// UnregisterReap removes a pid registration without consuming a status.
// Use when cmd.Wait() succeeded and you no longer need the channel.
func UnregisterReap(pid int) {
	reapRegistry.Delete(pid)
}

// TryDeliverReap is called by the SIGCHLD reaper for each child it reaps.
// If the pid is registered, the status is delivered via channel and true
// is returned (so the reaper can skip its orphan-log path). Otherwise
// returns false — caller should log this as an orphan reap.
func TryDeliverReap(pid int, ws syscall.WaitStatus) bool {
	v, ok := reapRegistry.LoadAndDelete(pid)
	if !ok {
		return false
	}
	ch := v.(chan syscall.WaitStatus)
	ch <- ws
	close(ch)
	return true
}
