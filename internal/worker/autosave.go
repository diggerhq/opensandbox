package worker

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// SyncFSer is implemented by any VM manager that can flush filesystem buffers
// and report the workspace path so we can gate sync on mtime.
type SyncFSer interface {
	SyncFS(ctx context.Context, sandboxID string) error
	GetWorkspacePath(sandboxID string) (string, error)
}

// maxConsecutiveFailures: after this many failed sync attempts in a row for
// the same sandbox, we stop trying. Prevents a wedged sandbox (e.g. agent
// stuck after savevm/loadvm protocol confusion) from emitting an autosave
// failure log every 5 minutes forever, and stops us churning the broken
// gRPC channel — which is itself part of the post-loadvm flake surface.
// Operator can re-enable by reviving the sandbox or restarting the worker.
const maxConsecutiveFailures = 5

// WorkspaceAutosaver periodically flushes filesystem buffers inside each
// running VM so workspace.qcow2 on the host NVMe is crash-consistent for
// cold-boot recovery on worker death.
//
// Two correctness/cost gates:
//   - skip a sandbox if its workspace.qcow2 hasn't been modified since the
//     last successful sync (idle sandboxes are common and a sync per tick
//     is wasted work and gRPC roundtrip).
//   - circuit-breaker after N consecutive failures: stop trying for a
//     sandbox where the agent is wedged. Prevents log flood and churn on
//     the broken control channel.
type WorkspaceAutosaver struct {
	manager     sandbox.Manager
	syncer      SyncFSer
	interval    time.Duration
	concurrency int
	stop        chan struct{}
	done        chan struct{}

	mu       sync.Mutex
	state    map[string]*sandboxSyncState // keyed by sandboxID
}

type sandboxSyncState struct {
	lastSyncedMtime time.Time // mtime of workspace.qcow2 at last successful sync
	consecutiveFails int
	disabled         bool // circuit-breaker tripped — skip until restart
}

// NewWorkspaceAutosaver creates a new autosaver.
func NewWorkspaceAutosaver(
	mgr sandbox.Manager,
	syncer SyncFSer,
	interval time.Duration,
) *WorkspaceAutosaver {
	return &WorkspaceAutosaver{
		manager:     mgr,
		syncer:      syncer,
		interval:    interval,
		concurrency: 10,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		state:       map[string]*sandboxSyncState{},
	}
}

// Start begins the periodic SyncFS loop.
func (a *WorkspaceAutosaver) Start() {
	go a.loop()
	log.Printf("autosave: started (interval=%s, concurrency=%d, fail-threshold=%d)",
		a.interval, a.concurrency, maxConsecutiveFailures)
}

// Stop signals the loop to exit and waits for it to finish.
func (a *WorkspaceAutosaver) Stop() {
	close(a.stop)
	<-a.done
	log.Println("autosave: stopped")
}

func (a *WorkspaceAutosaver) loop() {
	defer close(a.done)
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			a.syncAll()
		case <-a.stop:
			return
		}
	}
}

func (a *WorkspaceAutosaver) getOrCreateState(sandboxID string) *sandboxSyncState {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.state[sandboxID]
	if !ok {
		s = &sandboxSyncState{}
		a.state[sandboxID] = s
	}
	return s
}

// pruneState removes per-sandbox state for sandboxes no longer in the active
// set. Without this the map grows unbounded across the worker's lifetime.
func (a *WorkspaceAutosaver) pruneState(active map[string]struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.state {
		if _, ok := active[id]; !ok {
			delete(a.state, id)
		}
	}
}

func (a *WorkspaceAutosaver) syncAll() {
	sandboxes, err := a.manager.List(context.Background())
	if err != nil {
		log.Printf("autosave: failed to list sandboxes: %v", err)
		return
	}
	if len(sandboxes) == 0 {
		a.pruneState(map[string]struct{}{})
		return
	}

	active := make(map[string]struct{}, len(sandboxes))
	for _, sb := range sandboxes {
		active[sb.ID] = struct{}{}
	}
	a.pruneState(active)

	sem := make(chan struct{}, a.concurrency)
	var wg sync.WaitGroup
	var successCount, skipCount, failCount, disabledCount int32
	var mu sync.Mutex

	for _, sb := range sandboxes {
		select {
		case <-a.stop:
			return
		default:
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(sandboxID string) {
			defer wg.Done()
			defer func() { <-sem }()

			outcome := a.syncOne(sandboxID)
			mu.Lock()
			switch outcome {
			case syncOK:
				successCount++
			case syncSkipped:
				skipCount++
			case syncFailed:
				failCount++
			case syncDisabled:
				disabledCount++
			}
			mu.Unlock()
		}(sb.ID)
	}

	wg.Wait()
	if failCount > 0 || disabledCount > 0 {
		log.Printf("autosave: tick complete (ok=%d skip=%d fail=%d disabled=%d)",
			successCount, skipCount, failCount, disabledCount)
	}
}

type syncOutcome int

const (
	syncOK syncOutcome = iota
	syncSkipped     // workspace unchanged since last sync
	syncFailed      // sync attempted, returned error
	syncDisabled    // circuit-breaker tripped — skipped without attempting
)

func (a *WorkspaceAutosaver) syncOne(sandboxID string) syncOutcome {
	state := a.getOrCreateState(sandboxID)

	a.mu.Lock()
	if state.disabled {
		a.mu.Unlock()
		return syncDisabled
	}
	lastMtime := state.lastSyncedMtime
	a.mu.Unlock()

	// Gate on workspace mtime — skip if nothing changed since last successful sync.
	wsPath, err := a.syncer.GetWorkspacePath(sandboxID)
	if err == nil && wsPath != "" {
		if stat, statErr := os.Stat(wsPath); statErr == nil {
			if !lastMtime.IsZero() && !stat.ModTime().After(lastMtime) {
				return syncSkipped
			}
		}
	}

	syncCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.syncer.SyncFS(syncCtx, sandboxID); err != nil {
		a.mu.Lock()
		state.consecutiveFails++
		hitThreshold := state.consecutiveFails >= maxConsecutiveFailures
		if hitThreshold {
			state.disabled = true
		}
		fails := state.consecutiveFails
		a.mu.Unlock()

		if hitThreshold {
			log.Printf("autosave: %s disabled after %d consecutive failures (last=%v) — operator must intervene to re-enable",
				sandboxID, fails, err)
		} else if fails == 1 {
			log.Printf("autosave: syncfs failed for %s: %v", sandboxID, err)
		}
		return syncFailed
	}

	// Success — record current mtime to gate next tick.
	a.mu.Lock()
	state.consecutiveFails = 0
	if wsPath != "" {
		if stat, err := os.Stat(wsPath); err == nil {
			state.lastSyncedMtime = stat.ModTime()
		}
	}
	a.mu.Unlock()
	return syncOK
}
