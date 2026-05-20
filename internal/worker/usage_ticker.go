package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// UsageTicker emits a `usage_tick` event into each running sandbox's per-
// sandbox SQLite on a fixed interval. The RedisEventPublisher picks the
// events up on its 2s poll and ships them to the cell's Redis stream; the
// CF events-ingest Worker then fans them out to per-org CreditAccount DOs,
// which decrement the org's free-tier balance and dispatch a halt webhook
// when it hits zero.
//
// Without this ticker the DO never gets debited and free orgs run forever
// at $5 balance. The architectural intent (see events-ingest /debit fan-out)
// is exactly this periodic emit; no other component has the right scope to
// do it because:
//   - The CP doesn't know about per-sandbox uptime granularity.
//   - The worker is the only place that actually owns running VMs.
//   - The DO can't self-schedule per-sandbox debit cadence.
//
// Cost model: every tick contributes a fixed cents amount per sandbox. For
// the dev/test bed this is set so that a free org's $5 seed exhausts in
// ~5 minutes of continuous running at the default interval — short enough
// to validate the halt loop, long enough that quick smoke tests don't
// burn the credit. Production should compute cents from RAM + CPU usage
// (memory_gb_seconds, cpu_seconds) — wired the same way, just a richer
// payload.
type UsageTicker struct {
	manager       sandbox.Manager
	sandboxDBs    *sandbox.SandboxDBManager
	interval      time.Duration
	costPerTickCs int // cents per tick per running sandbox

	stop    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// NewUsageTicker constructs a ticker. interval ≤ 0 defaults to 20s,
// costPerTickCs ≤ 0 defaults to 10 cents (so $5 = 50 ticks ≈ 17 min).
// nil manager or nil sandboxDBs returns nil (ticker disabled).
func NewUsageTicker(manager sandbox.Manager, sandboxDBs *sandbox.SandboxDBManager, interval time.Duration, costPerTickCs int) *UsageTicker {
	if manager == nil || sandboxDBs == nil {
		return nil
	}
	if interval <= 0 {
		interval = 20 * time.Second
	}
	if costPerTickCs <= 0 {
		costPerTickCs = 10
	}
	return &UsageTicker{
		manager:       manager,
		sandboxDBs:    sandboxDBs,
		interval:      interval,
		costPerTickCs: costPerTickCs,
		stop:          make(chan struct{}),
		stopped:       make(chan struct{}),
	}
}

// Start begins the tick loop. Safe to call once; subsequent calls are no-ops.
func (t *UsageTicker) Start(ctx context.Context) {
	go t.run(ctx)
}

// Stop signals the loop to exit and waits for it to drain.
func (t *UsageTicker) Stop(ctx context.Context) error {
	t.once.Do(func() { close(t.stop) })
	select {
	case <-t.stopped:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (t *UsageTicker) run(ctx context.Context) {
	defer close(t.stopped)
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stop:
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

func (t *UsageTicker) tick(ctx context.Context) {
	list, err := t.manager.List(ctx)
	if err != nil {
		log.Printf("usage_ticker: manager.List failed: %v", err)
		return
	}
	if len(list) == 0 {
		log.Printf("usage_ticker: tick: 0 running sandboxes (skip)")
		return
	}
	emitted := 0
	for _, sb := range list {
		sdb, err := t.sandboxDBs.Get(sb.ID)
		if err != nil {
			log.Printf("usage_ticker: %s: Get failed: %v", sb.ID, err)
			continue
		}
		if err := sdb.LogEvent("usage_tick", map[string]interface{}{
			"sandbox_id":  sb.ID,
			"cost_cents":  t.costPerTickCs,
			"interval_s":  int(t.interval.Seconds()),
		}); err != nil {
			log.Printf("usage_ticker: %s: LogEvent failed: %v", sb.ID, err)
			continue
		}
		emitted++
	}
	log.Printf("usage_ticker: tick: emitted %d usage_tick event(s) for %d running sandbox(es)", emitted, len(list))
}
