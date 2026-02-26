package worker

import (
	"context"
	"log"
	"sync"
	"time"

	fc "github.com/opensandbox/opensandbox/internal/firecracker"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// WorkspaceAutosaver periodically flushes filesystem buffers inside each
// running VM. This ensures workspace.ext4 on the host NVMe is crash-consistent.
// On hard kill recovery, the worker cold-boots from template + existing
// workspace.ext4 on disk (processes lost, but user files in /workspace are safe).
type WorkspaceAutosaver struct {
	manager     sandbox.Manager
	fcMgr       *fc.Manager
	interval    time.Duration
	concurrency int
	stop        chan struct{}
	done        chan struct{}
}

// NewWorkspaceAutosaver creates a new autosaver.
func NewWorkspaceAutosaver(
	mgr sandbox.Manager,
	fcMgr *fc.Manager,
	interval time.Duration,
) *WorkspaceAutosaver {
	return &WorkspaceAutosaver{
		manager:     mgr,
		fcMgr:       fcMgr,
		interval:    interval,
		concurrency: 10,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Start begins the periodic SyncFS loop.
func (a *WorkspaceAutosaver) Start() {
	go a.loop()
	log.Printf("autosave: started (interval=%s, concurrency=%d)", a.interval, a.concurrency)
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

func (a *WorkspaceAutosaver) syncAll() {
	sandboxes, err := a.manager.List(context.Background())
	if err != nil {
		log.Printf("autosave: failed to list sandboxes: %v", err)
		return
	}
	if len(sandboxes) == 0 {
		return
	}

	sem := make(chan struct{}, a.concurrency)
	var wg sync.WaitGroup
	var successCount, failCount int32
	var mu sync.Mutex

	for _, sb := range sandboxes {
		select {
		case <-a.stop:
			break
		default:
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(sandboxID string) {
			defer wg.Done()
			defer func() { <-sem }()

			syncCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := a.fcMgr.SyncFS(syncCtx, sandboxID); err != nil {
				log.Printf("autosave: syncfs failed for %s: %v", sandboxID, err)
				mu.Lock()
				failCount++
				mu.Unlock()
			} else {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(sb.ID)
	}

	wg.Wait()
	if failCount > 0 {
		log.Printf("autosave: syncfs complete (%d ok, %d failed)", successCount, failCount)
	}
}
