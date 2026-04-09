package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/analytics"
	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// UsageCollector periodically samples resource usage from running sandboxes
// and batch-inserts the data into Postgres for billing.
type UsageCollector struct {
	manager  sandbox.Manager
	store    *db.Store
	segment  *analytics.Client // optional; nil = no Segment shipping
	interval time.Duration     // how often to sample (default 60s)
	flushN   int               // flush to DB every N samples (default 5 = every 5 min)

	mu       sync.Mutex
	buffer   []db.UsageSample
	count    int
	stopCh   chan struct{}
	stopped  chan struct{}
}

// NewUsageCollector creates a collector that samples every interval.
// segment may be nil to disable analytics shipping.
func NewUsageCollector(manager sandbox.Manager, store *db.Store, segment *analytics.Client) *UsageCollector {
	return &UsageCollector{
		manager:  manager,
		store:    store,
		segment:  segment,
		interval: 60 * time.Second,
		flushN:   5,
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// Start begins the collection loop.
func (c *UsageCollector) Start() {
	go c.loop()
	log.Printf("usage-collector: started (sample every %v, flush every %d samples)", c.interval, c.flushN)
}

// Stop halts collection and flushes remaining samples.
func (c *UsageCollector) Stop() {
	close(c.stopCh)
	<-c.stopped
	c.flush()
	log.Printf("usage-collector: stopped")
}

func (c *UsageCollector) loop() {
	defer close(c.stopped)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.sample()
		case <-c.stopCh:
			return
		}
	}
}

func (c *UsageCollector) sample() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sandboxes, err := c.manager.List(ctx)
	if err != nil {
		log.Printf("usage-collector: list failed: %v", err)
		return
	}

	now := time.Now()
	intervalSec := c.interval.Seconds()
	var samples []db.UsageSample

	for _, sb := range sandboxes {
		stats, err := c.manager.Stats(ctx, sb.ID)
		if err != nil {
			continue // sandbox might be shutting down
		}

		owner, err := c.store.GetSandboxOwner(ctx, sb.ID)
		if err != nil || owner.OrgID == "" {
			continue // no billable owner, skip
		}

		samples = append(samples, db.UsageSample{
			SandboxID:   sb.ID,
			OrgID:       owner.OrgID,
			SampledAt:   now,
			MemoryMB:    sb.MemoryMB,
			CPUUsec:     0, // TODO: parse from cgroup cpu.stat
			MemoryBytes: int64(stats.MemUsage),
			PIDs:        int(stats.PIDs),
		})

		// Ship to Segment: GB-seconds based on PROVISIONED memory (the tier the
		// customer reserved), not actual RSS. Reservation is what we bill for —
		// actual usage is gameable and punishes well-behaved tenants.
		gbSeconds := (float64(sb.MemoryMB) / 1024.0) * intervalSec
		c.segment.TrackGBSeconds(analytics.UsageEvent{
			OrgID:        owner.OrgID,
			UserID:       owner.UserID,
			UserEmail:    owner.UserEmail,
			WorkosUserID: owner.WorkosUserID,
			WorkosOrgID:  owner.WorkosOrgID,
			SandboxID:    sb.ID,
			GBSeconds:    gbSeconds,
		})
	}

	if len(samples) == 0 {
		return
	}

	c.mu.Lock()
	c.buffer = append(c.buffer, samples...)
	c.count++
	shouldFlush := c.count >= c.flushN
	c.mu.Unlock()

	if shouldFlush {
		c.flush()
	}
}

func (c *UsageCollector) flush() {
	c.mu.Lock()
	samples := c.buffer
	c.buffer = nil
	c.count = 0
	c.mu.Unlock()

	if len(samples) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.store.InsertUsageSamples(ctx, samples); err != nil {
		log.Printf("usage-collector: flush %d samples failed: %v", len(samples), err)
		// Put them back
		c.mu.Lock()
		c.buffer = append(samples, c.buffer...)
		c.mu.Unlock()
		return
	}

	log.Printf("usage-collector: flushed %d samples", len(samples))
}
