package qemu

import (
	"context"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// SandboxStatsCache holds the latest per-sandbox resource snapshots gathered
// by a periodic background collector. The autoscaler in the control plane
// reads these via the worker's heartbeat — see internal/worker/redis_heartbeat.go.
//
// Why a cache: collecting stats requires a gRPC round-trip to the agent in
// each VM (over virtio-serial). Doing that lazily inside the heartbeat publish
// path would block heartbeats for ~5-10ms × N sandboxes. A separate goroutine
// polls in parallel and the heartbeat reads from a snapshot map — heartbeats
// stay fast and stats are at most one collection interval stale.
type SandboxStatsCache struct {
	mu    sync.RWMutex
	stats map[string]sandbox.SandboxStats // sandboxID → latest snapshot
}

// StartStatsCollector launches a goroutine that polls every running sandbox's
// agent.Stats and caches the response. The heartbeat path reads the cached
// snapshot. Stops when ctx is cancelled.
//
// Concurrency: limited to 8 in-flight collections so a worker hosting 100+
// sandboxes doesn't spike its virtio-serial channel pool. 100 sandboxes × 10ms
// per call ÷ 8 concurrent ≈ 125ms per collection round. Comfortable inside
// the 10s heartbeat cadence.
func (m *Manager) StartStatsCollector(ctx context.Context, interval time.Duration) {
	if m.statsCache == nil {
		m.statsCache = &SandboxStatsCache{stats: make(map[string]sandbox.SandboxStats)}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Collect once immediately so the first heartbeat after worker boot
		// has data instead of an empty map.
		m.collectAllStats(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.collectAllStats(ctx)
			}
		}
	}()
}

func (m *Manager) collectAllStats(ctx context.Context) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.vms))
	for id := range m.vms {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	const maxConcurrent = 8
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(sandboxID string) {
			defer wg.Done()
			defer func() { <-sem }()

			statsCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			stats, err := m.Stats(statsCtx, sandboxID)
			if err != nil {
				// Stats call can fail transiently (agent restarting, etc.)
				// We just skip this sandbox this round; next collection picks
				// it up. Stale entry stays in cache for one cycle which is
				// fine for the autoscaler — it's looking at moving averages
				// over a 1-15 min window.
				return
			}
			m.statsCache.mu.Lock()
			m.statsCache.stats[sandboxID] = *stats
			m.statsCache.mu.Unlock()
		}(id)
	}
	wg.Wait()

	// Prune entries for vanished sandboxes so the cache doesn't leak across
	// destroy events.
	m.mu.RLock()
	live := make(map[string]bool, len(m.vms))
	for id := range m.vms {
		live[id] = true
	}
	m.mu.RUnlock()

	m.statsCache.mu.Lock()
	for id := range m.statsCache.stats {
		if !live[id] {
			delete(m.statsCache.stats, id)
		}
	}
	m.statsCache.mu.Unlock()
}

// GetAllSandboxStats returns a snapshot map of the latest cached stats. Safe
// to call concurrently with the collector. Used by the heartbeat to populate
// the per-sandbox stats portion of the worker:* Redis entry.
func (m *Manager) GetAllSandboxStats() map[string]sandbox.SandboxStats {
	if m.statsCache == nil {
		return nil
	}
	m.statsCache.mu.RLock()
	defer m.statsCache.mu.RUnlock()
	out := make(map[string]sandbox.SandboxStats, len(m.statsCache.stats))
	for id, s := range m.statsCache.stats {
		out[id] = s
	}
	return out
}
