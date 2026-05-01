package controlplane

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/pkg/types"
)

// Per-sandbox autoscaler. Resizes a sandbox's memory tier (with vCPU bundled
// per types.AllowedResourceTiers) based on observed memory pressure, bounded
// by user-configured min/max.
//
// Design choices:
//   - Tier-aligned. We never pick a half-step size — only values from
//     types.AllowedResourceTiers (1G/4G/8G/16G/32G/64G). This keeps billing
//     predictable and avoids the configuration explosion of a free-form
//     step_gb knob. vCPU follows memory by the tier table.
//   - Asymmetric thresholds and cooldowns. Scale up FAST on a brief 1-min
//     spike (>75% mem) — user notices lag, we want to fix it before they
//     OOM. Scale down SLOW: require all of {1m, 5m, 15m} averages below
//     25% AND at least 5 minutes since the last scale event. This stops
//     the sawtooth pattern from a workload that briefly idles.
//   - In-memory state. Moving averages and last-event timestamps live in
//     CP memory. On CP restart we lose history; the autoscaler self-
//     corrects within ~15 minutes (the longest window). Worth the cost
//     vs adding Redis writes per sandbox per tick.
//   - No live-migrate fallback. If a SetSandboxLimits up-call fails because
//     the host can't fit the new size, we log and try again next tick. A
//     migration fallback is Phase 3 (depends on the fork-reliability work
//     in PR #207 being solid first).
const (
	autoscaleTickInterval = 30 * time.Second

	// Scale-up triggers on a single 1-min sample exceeding the threshold.
	// One window is enough because users feel pressure immediately.
	scaleUpMemPctThreshold = 75.0

	// Scale-down requires ALL three averaging windows below the threshold.
	// 1-min protects against transient idle, 5-min/15-min confirm steady-
	// state. The 15-min window is the dominant constraint — at 30-second
	// ticks that's 30 samples of agreement before we shrink.
	scaleDownMemPctThreshold = 25.0

	// Cooldowns. After scaling up, wait at least 60s before another up.
	// After scaling down, wait 5 min before considering more scaling
	// (either direction). The asymmetry matches user perception: rapid
	// scale-up on demand, conservative shrink after sustained idle.
	scaleUpCooldown   = 60 * time.Second
	scaleDownCooldown = 5 * time.Minute
)

// Sample windows (counts of 30s ticks).
const (
	window1m  = 2  // 60s / 30s
	window5m  = 10 // 300s / 30s
	window15m = 30 // 900s / 30s
)

// AutoscalerSandboxStore is the subset of the DB we need.
type AutoscalerSandboxStore interface {
	// ListAutoscaleEnabled returns running sandboxes with autoscale_enabled=true.
	ListAutoscaleEnabled(ctx context.Context) ([]db.AutoscaleSandbox, error)
	// UpdateAutoscaleLastEvent records a scale event for cooldown tracking.
	UpdateAutoscaleLastEvent(ctx context.Context, sandboxID string, t time.Time) error
}

// AutoscalerRegistry is the small subset of WorkerRegistry the autoscaler
// uses. The autoscaler reads cached per-sandbox stats off the registry —
// stats are populated from worker heartbeats, not pulled per-tick by the
// CP. See redis_registry.go's GetSandboxStats for the cache.
type AutoscalerRegistry interface {
	// GetSandboxStats returns the latest snapshot for a sandbox plus the
	// worker hosting it, or ok=false if no recent heartbeat has carried it.
	GetSandboxStats(sandboxID string) (SandboxStats, string, bool)
}

// AutoscalerScaleSetter applies a scale decision via the existing setLimits
// machinery (worker gRPC SetSandboxLimits). Decoupled so we can test without
// a real worker.
type AutoscalerScaleSetter interface {
	SetSandboxMemoryMB(ctx context.Context, sandboxID string, memoryMB int) error
}

type Autoscaler struct {
	store     AutoscalerSandboxStore
	registry  AutoscalerRegistry
	setter    AutoscalerScaleSetter

	mu     sync.Mutex
	stats  map[string]*sandboxStats // sandboxID → samples
	stop   chan struct{}
}

type sandboxStats struct {
	memSamples []float64 // most recent first; trimmed to window15m
}

// NewAutoscaler wires up the loop. Caller invokes Start to begin ticking.
func NewAutoscaler(store AutoscalerSandboxStore, registry AutoscalerRegistry, setter AutoscalerScaleSetter) *Autoscaler {
	return &Autoscaler{
		store:    store,
		registry: registry,
		setter:   setter,
		stats:    make(map[string]*sandboxStats),
		stop:     make(chan struct{}),
	}
}

// Start runs the autoscaler loop until ctx is cancelled or Stop is called.
func (a *Autoscaler) Start(ctx context.Context) {
	go a.run(ctx)
}

func (a *Autoscaler) Stop() { close(a.stop) }

func (a *Autoscaler) run(ctx context.Context) {
	log.Println("autoscaler: started")
	ticker := time.NewTicker(autoscaleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stop:
			return
		case <-ticker.C:
			a.tick(ctx)
		}
	}
}

func (a *Autoscaler) tick(ctx context.Context) {
	sandboxes, err := a.store.ListAutoscaleEnabled(ctx)
	if err != nil {
		log.Printf("autoscaler: ListAutoscaleEnabled failed: %v", err)
		return
	}

	// Drop stats for sandboxes that are no longer autoscale-enabled (or are gone).
	a.mu.Lock()
	keep := make(map[string]bool, len(sandboxes))
	for _, sb := range sandboxes {
		keep[sb.SandboxID] = true
	}
	for id := range a.stats {
		if !keep[id] {
			delete(a.stats, id)
		}
	}
	a.mu.Unlock()

	for _, sb := range sandboxes {
		a.evaluateOne(ctx, sb)
	}
}

func (a *Autoscaler) evaluateOne(ctx context.Context, sb db.AutoscaleSandbox) {
	// 1. Fetch current stats from the worker.
	memPct, err := a.fetchMemPct(ctx, sb)
	if err != nil {
		// Transient — log at low frequency and skip.
		log.Printf("autoscaler: %s stats fetch failed: %v", sb.SandboxID, err)
		return
	}

	// 2. Record sample.
	a.recordSample(sb.SandboxID, memPct)

	// 3. Decide.
	target := a.decideTarget(sb)
	if target == 0 || target == sb.CurrentMB {
		return
	}

	// 4. Apply.
	if err := a.setter.SetSandboxMemoryMB(ctx, sb.SandboxID, target); err != nil {
		log.Printf("autoscaler: %s scale %d→%d MB failed: %v", sb.SandboxID, sb.CurrentMB, target, err)
		return
	}
	if err := a.store.UpdateAutoscaleLastEvent(ctx, sb.SandboxID, time.Now()); err != nil {
		log.Printf("autoscaler: %s update last_event failed: %v", sb.SandboxID, err)
	}
	log.Printf("autoscaler: %s scaled %d MB → %d MB (mem=%.1f%% min=%d max=%d)",
		sb.SandboxID, sb.CurrentMB, target, memPct, sb.MinMB, sb.MaxMB)
}

// fetchMemPct returns the latest cached mem_pct for a sandbox. Stats are
// populated by worker heartbeats — see internal/qemu/stats_collector.go on
// the worker side and redis_registry.go's handleHeartbeat ingestion.
//
// Returns ok=false (via the err) if no recent heartbeat has carried this
// sandbox. The autoscaler skips the tick for that sandbox; on the next
// heartbeat (~10s later) the data should appear.
func (a *Autoscaler) fetchMemPct(_ context.Context, sb db.AutoscaleSandbox) (float64, error) {
	stats, _, ok := a.registry.GetSandboxStats(sb.SandboxID)
	if !ok {
		return 0, fmt.Errorf("no cached stats for %s — worker heartbeat may be stale", sb.SandboxID)
	}
	return stats.MemPct, nil
}

func (a *Autoscaler) recordSample(sandboxID string, memPct float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.stats[sandboxID]
	if !ok {
		s = &sandboxStats{}
		a.stats[sandboxID] = s
	}
	// Most recent first. Trim to longest window.
	s.memSamples = append([]float64{memPct}, s.memSamples...)
	if len(s.memSamples) > window15m {
		s.memSamples = s.memSamples[:window15m]
	}
}

// decideTarget returns the memory MB to scale to, or 0 if no change.
func (a *Autoscaler) decideTarget(sb db.AutoscaleSandbox) int {
	a.mu.Lock()
	s := a.stats[sb.SandboxID]
	a.mu.Unlock()
	if s == nil || len(s.memSamples) == 0 {
		return 0
	}

	currentMB := sb.CurrentMB
	if currentMB == 0 {
		return 0 // unknown current — don't risk scaling
	}

	avg1 := windowAvg(s.memSamples, window1m)
	avg5 := windowAvg(s.memSamples, window5m)
	avg15 := windowAvg(s.memSamples, window15m)

	now := time.Now()

	// Scale up: 1-min above threshold, current < max, off scale-up cooldown.
	if avg1 > scaleUpMemPctThreshold && currentMB < sb.MaxMB {
		if !sb.LastEventAt.IsZero() && now.Sub(sb.LastEventAt) < scaleUpCooldown {
			return 0
		}
		next := nextTier(currentMB, sb.MaxMB)
		if next > currentMB {
			return next
		}
	}

	// Scale down: ALL three windows below threshold, current > min, off scale-down cooldown.
	// Need at least window15m samples — otherwise the long window is misleading.
	if avg1 < scaleDownMemPctThreshold && avg5 < scaleDownMemPctThreshold && avg15 < scaleDownMemPctThreshold &&
		len(s.memSamples) >= window15m && currentMB > sb.MinMB {
		if !sb.LastEventAt.IsZero() && now.Sub(sb.LastEventAt) < scaleDownCooldown {
			return 0
		}
		prev := prevTier(currentMB, sb.MinMB)
		if prev < currentMB {
			return prev
		}
	}

	return 0
}

// windowAvg returns the mean of the most recent n samples (or all of them if
// fewer exist). Caller must hold no lock — operates on the slice argument.
func windowAvg(samples []float64, n int) float64 {
	if n > len(samples) {
		n = len(samples)
	}
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += samples[i]
	}
	return sum / float64(n)
}

// nextTier returns the next memory tier above current, capped at maxMB.
// Returns current if no higher tier is allowed.
func nextTier(currentMB, maxMB int) int {
	tiers := tierMBs()
	for _, t := range tiers {
		if t > currentMB && t <= maxMB {
			return t
		}
	}
	return currentMB
}

// prevTier returns the next memory tier below current, floored at minMB.
// Returns current if no lower tier is allowed.
func prevTier(currentMB, minMB int) int {
	tiers := tierMBs()
	// Iterate descending.
	for i := len(tiers) - 1; i >= 0; i-- {
		if tiers[i] < currentMB && tiers[i] >= minMB {
			return tiers[i]
		}
	}
	return currentMB
}

// tierMBs returns the allowed memory tier values in ascending order.
// Pulled from the canonical tier list in pkg/types so we can never disagree.
func tierMBs() []int {
	out := make([]int, 0, len(types.AllowedResourceTiers))
	for _, t := range types.AllowedResourceTiers {
		out = append(out, t.MemoryMB)
	}
	sort.Ints(out)
	return out
}
