package worker

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/opensandbox/opensandbox/internal/metrics"
)

// MemoryAllocator is the slice of the QEMU manager interface needed to report
// per-worker memory allocation. Duck-typed so this package doesn't depend on
// internal/qemu directly.
type MemoryAllocator interface {
	MemoryAllocatedBytes() uint64
}

// SandboxCounter reports the count of currently-active sandboxes grouped by
// template, used to drive the opensandbox_sandboxes_active gauge.
type SandboxCounter interface {
	ActiveSandboxesByTemplate() map[string]int
}

// StartResourceMetricsTick runs a goroutine that samples disk, memory, allocated
// memory, CPU pressure, and active-sandbox count every interval and writes them
// to the worker-side Prometheus gauges. Cancel via the provided context.
//
// dataDir is the worker data directory (e.g. /data/sandboxes) — its mountpoint
// is what the disk gauges report on. mount is just the label value, defaulted
// to filepath.Clean(dataDir).
//
// allocator may be nil (non-QEMU backend); allocated-memory gauge is then left
// unwritten. counter may be nil for the same reason; sandboxes_active gauge is
// then left unwritten.
func StartResourceMetricsTick(ctx context.Context, allocator MemoryAllocator, counter SandboxCounter, region, workerID, dataDir string, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	mount := filepath.Clean(dataDir)
	go func() {
		// Emit one sample immediately so the gauges aren't reported empty for
		// the first interval after worker boot.
		collectAndPublish(allocator, counter, region, workerID, dataDir, mount)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				collectAndPublish(allocator, counter, region, workerID, dataDir, mount)
			}
		}
	}()
	log.Printf("opensandbox-worker: resource-stats tick started (interval=%s, mount=%s)", interval, mount)
}

func collectAndPublish(allocator MemoryAllocator, counter SandboxCounter, region, workerID, dataDir, mount string) {
	if total, used, avail, err := DiskBytes(dataDir); err == nil {
		metrics.WorkerDiskTotalBytes.WithLabelValues(region, workerID, mount).Set(float64(total))
		metrics.WorkerDiskUsedBytes.WithLabelValues(region, workerID, mount).Set(float64(used))
		metrics.WorkerDiskAvailableBytes.WithLabelValues(region, workerID, mount).Set(float64(avail))
	}

	if total, avail, err := MemoryBytes(); err == nil && total > 0 {
		metrics.WorkerMemoryTotalBytes.WithLabelValues(region, workerID).Set(float64(total))
		metrics.WorkerMemoryAvailableBytes.WithLabelValues(region, workerID).Set(float64(avail))
	}

	if allocator != nil {
		metrics.WorkerMemoryAllocatedBytes.WithLabelValues(region, workerID).Set(float64(allocator.MemoryAllocatedBytes()))
	}

	psi := ReadCPUPressure()
	metrics.WorkerCPUPressure.WithLabelValues(region, workerID, "avg10", psi.Source).Set(psi.Avg10)
	metrics.WorkerCPUPressure.WithLabelValues(region, workerID, "avg60", psi.Source).Set(psi.Avg60)
	metrics.WorkerCPUPressure.WithLabelValues(region, workerID, "avg300", psi.Source).Set(psi.Avg300)

	if counter != nil {
		// Reset clears stale template label combos (sandbox stopped → its
		// template would otherwise stay at its last value forever). The
		// Reset/re-emit window is sub-microsecond and Vector scrapes on a
		// 15-30s cadence, so the race is negligible.
		metrics.SandboxesActive.Reset()
		counts := counter.ActiveSandboxesByTemplate()
		if len(counts) == 0 {
			// Heartbeat so the dashboard query has at least one row to
			// summarize on (otherwise APL trips on the by-tags.worker_id
			// group-by with "field not found").
			metrics.SandboxesActive.WithLabelValues(region, workerID, "").Set(0)
		} else {
			for tmpl, n := range counts {
				metrics.SandboxesActive.WithLabelValues(region, workerID, tmpl).Set(float64(n))
			}
		}
	}
}
