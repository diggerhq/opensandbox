package sandbox

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/storage"
)

// SandboxState represents the lifecycle state of a sandbox from the router's perspective.
type SandboxState int

const (
	StateRunning    SandboxState = iota
	StateHibernated
	StateWaking
)

func (s SandboxState) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateHibernated:
		return "hibernated"
	case StateWaking:
		return "waking"
	default:
		return "unknown"
	}
}

// sandboxEntry holds per-sandbox routing state.
type sandboxEntry struct {
	mu      sync.Mutex
	state   SandboxState
	timeout time.Duration // configured rolling timeout duration
	timer   *time.Timer   // rolling timeout timer (nil if hibernated)
	wakeCh  chan struct{}  // closed when wake completes; nil if not waking
	wakeErr error         // set if wake failed
}

// Middleware wraps a routed operation. It receives the sandbox ID, the operation
// name (e.g., "exec", "readFile"), and the next function to call. It can
// short-circuit, augment, or observe the operation.
type Middleware func(ctx context.Context, sandboxID string, op string, next func(ctx context.Context) error) error

// RouterConfig holds configuration for the SandboxRouter.
type RouterConfig struct {
	Manager         Manager
	CheckpointStore *storage.CheckpointStore // nil disables auto-wake/hibernate
	Store           *db.Store                // nil disables DB lookups for wake
	WorkerID        string
	DefaultTimeout  time.Duration
	OnHibernate     func(sandboxID string, result *HibernateResult)
	OnKill          func(sandboxID string) // called when a sandbox is killed on timeout (hibernate failed or no checkpoint store)
}

// SandboxRouter routes sandbox interactions through a state machine,
// managing rolling timeouts, auto-wake, and command queueing.
// Every sandbox interaction (exec, file ops, PTY) flows through Route(),
// which ensures the sandbox is running, executes the operation, and
// resets the rolling idle timeout.
type SandboxRouter struct {
	manager         Manager
	checkpointStore *storage.CheckpointStore
	store           *db.Store
	workerID        string
	defaultTimeout  time.Duration
	onHibernate     func(sandboxID string, result *HibernateResult)
	onKill          func(sandboxID string)

	mu        sync.RWMutex
	sandboxes map[string]*sandboxEntry

	middlewares []Middleware
}

// NewSandboxRouter creates a new sandbox router.
func NewSandboxRouter(cfg RouterConfig) *SandboxRouter {
	dt := cfg.DefaultTimeout
	if dt == 0 {
		dt = 300 * time.Second
	}
	return &SandboxRouter{
		manager:         cfg.Manager,
		checkpointStore: cfg.CheckpointStore,
		store:           cfg.Store,
		workerID:        cfg.WorkerID,
		defaultTimeout:  dt,
		onHibernate:     cfg.OnHibernate,
		onKill:          cfg.OnKill,
		sandboxes:       make(map[string]*sandboxEntry),
	}
}

// Use registers middleware that wraps every routed operation.
// Middleware is applied in the order registered (first registered = outermost).
func (r *SandboxRouter) Use(mw Middleware) {
	r.middlewares = append(r.middlewares, mw)
}

// Register is called when a sandbox is created or explicitly woken,
// to begin tracking it with a rolling timeout.
func (r *SandboxRouter) Register(sandboxID string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Stop existing timer if re-registering
	if existing, ok := r.sandboxes[sandboxID]; ok {
		existing.mu.Lock()
		if existing.timer != nil {
			existing.timer.Stop()
		}
		existing.mu.Unlock()
	}

	entry := &sandboxEntry{
		state:   StateRunning,
		timeout: timeout,
	}
	entry.timer = time.AfterFunc(timeout, func() {
		r.onTimeout(sandboxID)
	})
	r.sandboxes[sandboxID] = entry
}

// Unregister removes a sandbox from the router (called on kill/destroy).
func (r *SandboxRouter) Unregister(sandboxID string) {
	r.mu.Lock()
	entry, ok := r.sandboxes[sandboxID]
	if ok {
		delete(r.sandboxes, sandboxID)
	}
	r.mu.Unlock()

	if ok {
		entry.mu.Lock()
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entry.mu.Unlock()
	}
}

// MarkHibernated transitions a sandbox to the hibernated state.
// Called after a successful explicit or timeout-triggered hibernate.
func (r *SandboxRouter) MarkHibernated(sandboxID string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = r.defaultTimeout
	}

	r.mu.Lock()
	entry, ok := r.sandboxes[sandboxID]
	if !ok {
		// Create entry in hibernated state for auto-wake
		entry = &sandboxEntry{
			state:   StateHibernated,
			timeout: timeout,
		}
		r.sandboxes[sandboxID] = entry
		r.mu.Unlock()
		return
	}
	r.mu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	entry.state = StateHibernated
	entry.timeout = timeout
}

// Route is the core method. It ensures the sandbox is running before
// executing the operation. If hibernated, it triggers auto-wake and
// queues the operation. If waking, it waits behind the wake lock.
// Every successful route resets the rolling timeout.
func (r *SandboxRouter) Route(ctx context.Context, sandboxID string, op string, fn func(ctx context.Context) error) error {
	// Apply middleware chain (outermost first)
	wrapped := fn
	for i := len(r.middlewares) - 1; i >= 0; i-- {
		mw := r.middlewares[i]
		next := wrapped
		wrapped = func(ctx context.Context) error {
			return mw(ctx, sandboxID, op, next)
		}
	}

	// Ensure sandbox is ready (running state)
	if err := r.ensureRunning(ctx, sandboxID); err != nil {
		return err
	}

	// Execute the operation
	err := wrapped(ctx)

	// Reset rolling timeout on ANY interaction (success or failure)
	r.resetTimeout(sandboxID)

	return err
}

// Touch resets the rolling timeout without routing an operation.
// Useful for long-lived connections (WebSocket PTY) that indicate activity.
func (r *SandboxRouter) Touch(sandboxID string) {
	r.resetTimeout(sandboxID)
}

// SetTimeout updates the configured timeout duration for a sandbox
// and resets the timer.
func (r *SandboxRouter) SetTimeout(sandboxID string, timeout time.Duration) {
	r.mu.RLock()
	entry, ok := r.sandboxes[sandboxID]
	r.mu.RUnlock()
	if !ok {
		return
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.timeout = timeout
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.state == StateRunning {
		entry.timer = time.AfterFunc(timeout, func() {
			r.onTimeout(sandboxID)
		})
	}
}

// GetState returns the current state of a sandbox in the router.
// Returns (state, tracked). If tracked is false, the sandbox is unknown.
func (r *SandboxRouter) GetState(sandboxID string) (SandboxState, bool) {
	r.mu.RLock()
	entry, ok := r.sandboxes[sandboxID]
	r.mu.RUnlock()
	if !ok {
		return 0, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.state, true
}

// Close stops all timers and cleans up.
func (r *SandboxRouter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.sandboxes {
		entry.mu.Lock()
		if entry.timer != nil {
			entry.timer.Stop()
		}
		entry.mu.Unlock()
	}
	r.sandboxes = make(map[string]*sandboxEntry)
}

// GetManager returns the underlying Manager for direct lifecycle operations
// (create, kill, hibernate, wake) that don't go through routing.
func (r *SandboxRouter) GetManager() Manager {
	return r.manager
}

// ensureRunning checks the sandbox state and handles wake if needed.
func (r *SandboxRouter) ensureRunning(ctx context.Context, sandboxID string) error {
	r.mu.RLock()
	entry, ok := r.sandboxes[sandboxID]
	r.mu.RUnlock()

	if !ok {
		// Not tracked by the router — try to discover state from DB
		return r.discoverAndEnsure(ctx, sandboxID)
	}

	entry.mu.Lock()

	switch entry.state {
	case StateRunning:
		entry.mu.Unlock()
		return nil

	case StateHibernated:
		// Transition to waking, start wake, other requests will queue
		entry.state = StateWaking
		entry.wakeCh = make(chan struct{})
		entry.wakeErr = nil
		entry.mu.Unlock()

		// Perform wake (only one goroutine does this)
		r.doWake(ctx, sandboxID, entry)

		// Check wake result
		entry.mu.Lock()
		err := entry.wakeErr
		entry.mu.Unlock()
		if err != nil {
			return fmt.Errorf("auto-wake failed for sandbox %s: %w", sandboxID, err)
		}
		return nil

	case StateWaking:
		// Another goroutine is already waking — wait for it
		wakeCh := entry.wakeCh
		entry.mu.Unlock()

		select {
		case <-wakeCh:
			// Wake completed — check result
			entry.mu.Lock()
			err := entry.wakeErr
			entry.mu.Unlock()
			if err != nil {
				return fmt.Errorf("auto-wake failed for sandbox %s: %w", sandboxID, err)
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}

	default:
		entry.mu.Unlock()
		return fmt.Errorf("sandbox %s in unexpected state: %v", sandboxID, entry.state)
	}
}

// discoverAndEnsure attempts to look up a sandbox's state from the DB
// (for cases where the router doesn't have it tracked yet, e.g. after restart).
func (r *SandboxRouter) discoverAndEnsure(ctx context.Context, sandboxID string) error {
	if r.store == nil || r.checkpointStore == nil {
		// No DB/S3 — can't auto-wake, let the operation proceed
		// (podman will return an error if the container doesn't exist)
		return nil
	}

	// Check if there's an active checkpoint (meaning it's hibernated)
	checkpoint, err := r.store.GetActiveCheckpoint(ctx, sandboxID)
	if err != nil || checkpoint == nil {
		// No checkpoint found — sandbox might be running or doesn't exist
		return nil
	}

	// Found a checkpoint — register as hibernated and then ensure running
	r.MarkHibernated(sandboxID, r.defaultTimeout)
	return r.ensureRunning(ctx, sandboxID)
}

// doWake performs the actual wake operation and transitions state.
// Only one goroutine calls this per sandbox per wake cycle.
func (r *SandboxRouter) doWake(ctx context.Context, sandboxID string, entry *sandboxEntry) {
	// Use a background context for the wake itself so it completes
	// even if the original request times out
	wakeCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var wakeErr error
	defer func() {
		entry.mu.Lock()
		entry.wakeErr = wakeErr
		if wakeErr != nil {
			entry.state = StateHibernated // revert on failure
		}
		close(entry.wakeCh)
		entry.wakeCh = nil
		entry.mu.Unlock()
	}()

	if r.store == nil || r.checkpointStore == nil {
		wakeErr = fmt.Errorf("hibernation infrastructure not configured")
		return
	}

	checkpoint, err := r.store.GetActiveCheckpoint(wakeCtx, sandboxID)
	if err != nil {
		wakeErr = fmt.Errorf("no active checkpoint: %w", err)
		return
	}

	timeout := int(entry.timeout.Seconds())
	if timeout <= 0 {
		timeout = int(r.defaultTimeout.Seconds())
	}

	sb, err := r.manager.Wake(wakeCtx, sandboxID, checkpoint.CheckpointKey, r.checkpointStore, timeout)
	if err != nil {
		wakeErr = err
		return
	}

	// Update DB records
	_ = r.store.MarkCheckpointRestored(wakeCtx, sandboxID)
	_ = r.store.UpdateSandboxSessionForWake(wakeCtx, sandboxID, r.workerID)

	log.Printf("router: auto-woke sandbox %s (status=%s)", sandboxID, sb.Status)

	// Transition to running and start rolling timeout
	entry.mu.Lock()
	entry.state = StateRunning
	entry.timer = time.AfterFunc(entry.timeout, func() {
		r.onTimeout(sandboxID)
	})
	entry.mu.Unlock()
}

// resetTimeout resets the rolling timeout for a sandbox.
func (r *SandboxRouter) resetTimeout(sandboxID string) {
	r.mu.RLock()
	entry, ok := r.sandboxes[sandboxID]
	r.mu.RUnlock()

	if !ok {
		return
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.state != StateRunning {
		return
	}

	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.timer = time.AfterFunc(entry.timeout, func() {
		r.onTimeout(sandboxID)
	})
}

// onTimeout is called when the rolling timeout fires (no activity for timeout duration).
func (r *SandboxRouter) onTimeout(sandboxID string) {
	r.mu.RLock()
	entry, ok := r.sandboxes[sandboxID]
	r.mu.RUnlock()

	if !ok {
		return
	}

	entry.mu.Lock()
	if entry.state != StateRunning {
		entry.mu.Unlock()
		return
	}
	entry.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Try hibernate if checkpoint store is available
	if r.checkpointStore != nil {
		result, err := r.manager.Hibernate(ctx, sandboxID, r.checkpointStore)
		if err != nil {
			log.Printf("router: hibernate failed for %s, killing: %v", sandboxID, err)
			_ = r.manager.Kill(ctx, sandboxID)
			r.Unregister(sandboxID)
			if r.onKill != nil {
				r.onKill(sandboxID)
			}
			return
		}

		log.Printf("router: sandbox %s hibernated on timeout (key=%s, size=%d)", sandboxID, result.CheckpointKey, result.SizeBytes)

		entry.mu.Lock()
		entry.state = StateHibernated
		entry.timer = nil
		entry.mu.Unlock()

		if r.onHibernate != nil {
			r.onHibernate(sandboxID, result)
		}
		return
	}

	// No checkpoint store — just kill
	_ = r.manager.Kill(ctx, sandboxID)
	r.Unregister(sandboxID)
	if r.onKill != nil {
		r.onKill(sandboxID)
	}
}
