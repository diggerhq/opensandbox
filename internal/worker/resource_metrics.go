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

// StartResourceMetricsTick runs a goroutine that samples disk, memory, allocated
// memory, and CPU pressure every interval and writes them to the worker-side
// Prometheus gauges. Cancel via the provided context.
//
// dataDir is the worker data directory (e.g. /data/sandboxes) — its mountpoint
// is what the disk gauges report on. mount is just the label value, defaulted
// to filepath.Clean(dataDir).
//
// allocator may be nil (e.g. on a non-QEMU backend); allocated-memory gauge is
// then left unwritten.
func StartResourceMetricsTick(ctx context.Context, allocator MemoryAllocator, region, workerID, dataDir string, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	mount := filepath.Clean(dataDir)
	go func() {
		// Emit one sample immediately so the gauges aren't reported empty for
		// the first interval after worker boot.
		collectAndPublish(allocator, region, workerID, dataDir, mount)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				collectAndPublish(allocator, region, workerID, dataDir, mount)
			}
		}
	}()
	log.Printf("opensandbox-worker: resource-stats tick started (interval=%s, mount=%s)", interval, mount)
}

func collectAndPublish(allocator MemoryAllocator, region, workerID, dataDir, mount string) {
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
}
