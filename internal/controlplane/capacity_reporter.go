package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// CapacityReporter periodically aggregates worker memory pressure from the
// local RedisWorkerRegistry and pushes a `cell_capacity` event onto the
// events:{cell_id} Redis stream — the same stream EventForwarder drains. The
// events-ingest Worker keys off type=="cell_capacity" to UPSERT the cell's
// row in D1 with healthy_workers / available_workers / running_sandboxes /
// capacity_updated_at, which the api-edge consults in its pickCell() cascade.
//
// "available" = worker whose CommittedMemoryMB / TotalMemoryMB is below the
// pressure threshold (~85%). Single-worker-below-threshold is the right
// placement gate because a sandbox lands on one worker — aggregating across
// the cell would wrongly skip a cell with 1 free worker and 9 loaded ones.
//
// Reuses the existing event pipe so there's no second transport, no new HMAC
// path, no new ingest endpoint. Cost: one extra event per cell per
// ReportInterval, opaque JSON bytes through the same forwarder.

const (
	memPressureThresholdPct = 85
	defaultReportInterval   = 30 * time.Second
	// reporterStreamMaxLen caps the Redis stream so capacity events don't
	// accumulate if the forwarder is down. Sized small — only the most recent
	// sample matters for placement, older ones are stale anyway.
	reporterStreamMaxLen = 10_000
)

// capacityEnvelope mirrors worker.SandboxEventEnvelope on the wire. Defined
// locally to avoid an import of internal/worker from internal/controlplane;
// the forwarder treats stream entries as opaque JSON, so wire-format match is
// all that matters. JSON tags MUST match SandboxEventEnvelope; cross-check
// internal/worker/redis_event_publisher.go if either side changes.
type capacityEnvelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	SandboxID string          `json:"sandbox_id"`
	CellID    string          `json:"cell_id"`
	WorkerID  string          `json:"worker_id"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

type capacityPayload struct {
	HealthyWorkers   int `json:"healthy_workers"`
	AvailableWorkers int `json:"available_workers"`
	RunningSandboxes int `json:"running_sandboxes"`
}

// CapacityReporter periodically XADDs cell_capacity events.
type CapacityReporter struct {
	rdb       *redis.Client
	registry  *RedisWorkerRegistry
	cellID    string
	streamKey string
	interval  time.Duration

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// CapacityReporterConfig configures the reporter.
type CapacityReporterConfig struct {
	Redis    *redis.Client
	Registry *RedisWorkerRegistry
	CellID   string
	Interval time.Duration // default 30s
}

// NewCapacityReporter constructs a reporter. Returns an error if required
// fields are missing.
func NewCapacityReporter(cfg CapacityReporterConfig) (*CapacityReporter, error) {
	if cfg.Redis == nil {
		return nil, errors.New("capacity_reporter: Redis required")
	}
	if cfg.Registry == nil {
		return nil, errors.New("capacity_reporter: Registry required")
	}
	if cfg.CellID == "" {
		return nil, errors.New("capacity_reporter: CellID required")
	}
	iv := cfg.Interval
	if iv == 0 {
		iv = defaultReportInterval
	}
	return &CapacityReporter{
		rdb:       cfg.Redis,
		registry:  cfg.Registry,
		cellID:    cfg.CellID,
		streamKey: "events:" + cfg.CellID,
		interval:  iv,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}, nil
}

// Start launches the report loop. Emits one sample immediately so D1 sees a
// fresh capacity_updated_at without waiting a full interval.
func (r *CapacityReporter) Start(ctx context.Context) {
	go r.runLoop(ctx)
	log.Printf("capacity_reporter: started (cell=%s interval=%s threshold=%d%%)",
		r.cellID, r.interval, memPressureThresholdPct)
}

// Stop signals the loop to exit and waits for it to finish.
func (r *CapacityReporter) Stop() {
	r.once.Do(func() { close(r.stopCh) })
	<-r.doneCh
}

func (r *CapacityReporter) runLoop(ctx context.Context) {
	defer close(r.doneCh)
	r.emit(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stopCh:
			return
		case <-t.C:
			r.emit(ctx)
		}
	}
}

func (r *CapacityReporter) emit(ctx context.Context) {
	workers := r.registry.GetAllWorkers()
	var healthy, available, running int
	for _, w := range workers {
		if w == nil || w.Draining {
			continue
		}
		healthy++
		running += w.Current
		if w.TotalMemoryMB > 0 && (w.CommittedMemoryMB*100)/w.TotalMemoryMB < memPressureThresholdPct {
			available++
		}
	}

	payload, err := json.Marshal(capacityPayload{
		HealthyWorkers:   healthy,
		AvailableWorkers: available,
		RunningSandboxes: running,
	})
	if err != nil {
		log.Printf("capacity_reporter: marshal payload: %v", err)
		return
	}
	body, err := json.Marshal(capacityEnvelope{
		ID:        uuid.NewString(),
		Type:      "cell_capacity",
		CellID:    r.cellID,
		Payload:   payload,
		Timestamp: time.Now(),
	})
	if err != nil {
		log.Printf("capacity_reporter: marshal envelope: %v", err)
		return
	}

	xaddCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := r.rdb.XAdd(xaddCtx, &redis.XAddArgs{
		Stream: r.streamKey,
		MaxLen: reporterStreamMaxLen,
		Approx: true,
		Values: map[string]interface{}{"event": string(body)},
	}).Err(); err != nil {
		log.Printf("capacity_reporter: XADD failed: %v (cell=%s)", err, r.cellID)
		return
	}
}
