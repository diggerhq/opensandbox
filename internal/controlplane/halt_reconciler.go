package controlplane

import (
	"context"
	"sync"
	"time"
)

// HaltReconciler is the safety net for missed halt-org / resume-org webhooks.
// Every 60s it pulls the authoritative halt-list from CF
// (GET HaltListURL?cell={cell_id}) and reconciles local state:
//
//   - For each org_id in the returned list: ensure no sandboxes are running.
//     Halt any stragglers via the same code path as /admin/halt-org.
//   - For any org_id NOT in the returned list but with locally hibernated
//     sandboxes whose halt_reason = 'credits_exhausted': resume them.
//
// This catches webhooks lost to network partitions, transient CP outages, or
// CF DO state drift. Primary path remains the push webhooks.
type HaltReconciler struct {
	cellID  string
	listURL string
	secret  []byte // HMAC for the GET request

	admin  *AdminHandlers
	period time.Duration

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// HaltReconcilerConfig configures the reconciler.
type HaltReconcilerConfig struct {
	CellID  string
	ListURL string         // empty disables the reconciler
	Secret  string         // shared HMAC secret with CF
	Admin   *AdminHandlers // delegates to existing halt/resume code paths
	Period  time.Duration  // default 60s
}

// NewHaltReconciler constructs a reconciler. Returns nil if ListURL is empty.
func NewHaltReconciler(cfg HaltReconcilerConfig) *HaltReconciler {
	if cfg.ListURL == "" {
		return nil
	}
	if cfg.Period == 0 {
		cfg.Period = 60 * time.Second
	}
	return &HaltReconciler{
		cellID:  cfg.CellID,
		listURL: cfg.ListURL,
		secret:  []byte(cfg.Secret),
		admin:   cfg.Admin,
		period:  cfg.Period,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}
}

// Start begins the reconcile loop.
func (r *HaltReconciler) Start(ctx context.Context) {
	go r.run(ctx)
}

// Stop gracefully shuts down.
func (r *HaltReconciler) Stop(ctx context.Context) error {
	r.once.Do(func() { close(r.stopCh) })
	select {
	case <-r.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (r *HaltReconciler) run(ctx context.Context) {
	defer close(r.doneCh)
	ticker := time.NewTicker(r.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick performs one reconciliation pass.
func (r *HaltReconciler) tick(ctx context.Context) {
	// TODO:
	// 1. GET listURL + ?cell=cellID with HMAC auth
	// 2. Parse response {"org_ids": [...]}
	// 3. For each org_id: ensure no running sandboxes (else halt)
	// 4. For halted-locally orgs not in list: resume
	// 5. Increment opensandbox_halt_reconciler_corrections_total{cell_id} on changes
	_ = ctx
}
