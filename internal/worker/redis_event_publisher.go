package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// SandboxEventEnvelope is the wire format flowing worker → CP forwarder → CF
// events-ingest. org_id and plan are denormalized so the CF Worker can route
// without a D1 lookup per event.
type SandboxEventEnvelope struct {
	ID        string          `json:"id"` // UUID, idempotency key (KV seen:{id})
	Type      string          `json:"type"`
	SandboxID string          `json:"sandbox_id"`
	OrgID     string          `json:"org_id,omitempty"`
	Plan      string          `json:"plan,omitempty"`
	WorkerID  string          `json:"worker_id"`
	CellID    string          `json:"cell_id"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

// MetadataResolver returns org_id + plan for a given sandbox, used to fill
// out the envelope. The worker keeps these on its in-memory sandbox table
// (set at create time from the capability token); a nil resolver leaves the
// fields blank, which the CF ingest treats as "unknown — log only, no debit."
type MetadataResolver func(sandboxID string) (orgID, plan string, ok bool)

// RedisEventPublisher polls per-sandbox SQLite every poll interval for unsynced
// events and XADDs them to events:{cell_id} with MaxLen approx 100k. Marks
// events synced on successful XADD.
//
// Runs parallel to the legacy NATS publisher during cutover — see
// docs/dev-cutover-runbook.md.
type RedisEventPublisher struct {
	rdb        *redis.Client
	sandboxDBs *sandbox.SandboxDBManager
	resolver   MetadataResolver

	cellID    string
	workerID  string
	streamKey string
	maxLen    int64

	pollInterval time.Duration
	batchSize    int

	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

// RedisEventPublisherConfig configures the publisher.
type RedisEventPublisherConfig struct {
	RedisURL     string
	SandboxDBs   *sandbox.SandboxDBManager
	Resolver     MetadataResolver // optional — if nil, org_id/plan fields are blank
	CellID       string
	WorkerID     string
	MaxLen       int64
	PollInterval time.Duration // default 2s
	BatchSize    int           // GetAllUnsyncedEventsFlat limitPerDB; default 100
}

// NewRedisEventPublisher constructs a publisher. CellID, WorkerID,
// SandboxDBs and RedisURL are required.
func NewRedisEventPublisher(cfg RedisEventPublisherConfig) (*RedisEventPublisher, error) {
	if cfg.RedisURL == "" {
		return nil, errors.New("redis_event_publisher: RedisURL required")
	}
	if cfg.CellID == "" {
		return nil, errors.New("redis_event_publisher: CellID required")
	}
	if cfg.WorkerID == "" {
		return nil, errors.New("redis_event_publisher: WorkerID required")
	}
	if cfg.SandboxDBs == nil {
		return nil, errors.New("redis_event_publisher: SandboxDBs required")
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}
	opts.PoolSize = 3
	opts.MinIdleConns = 1
	opts.ConnMaxIdleTime = 5 * time.Minute
	opts.ConnMaxLifetime = 30 * time.Minute
	opts.MaxRetries = 3

	rdb := redis.NewClient(opts)
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxLen <= 0 {
		cfg.MaxLen = 100_000
	}

	return &RedisEventPublisher{
		rdb:          rdb,
		sandboxDBs:   cfg.SandboxDBs,
		resolver:     cfg.Resolver,
		cellID:       cfg.CellID,
		workerID:     cfg.WorkerID,
		streamKey:    "events:" + cfg.CellID,
		maxLen:       cfg.MaxLen,
		pollInterval: cfg.PollInterval,
		batchSize:    cfg.BatchSize,
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
	}, nil
}

// Start begins the poll-and-publish loop.
func (p *RedisEventPublisher) Start(ctx context.Context) {
	go p.run(ctx)
}

// Stop gracefully shuts down. Drains a final batch.
func (p *RedisEventPublisher) Stop(ctx context.Context) error {
	p.once.Do(func() { close(p.stopCh) })
	select {
	case <-p.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (p *RedisEventPublisher) run(ctx context.Context) {
	defer close(p.doneCh)
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			p.flush(ctx) // final drain
			return
		case <-ticker.C:
			p.flush(ctx)
		}
	}
}

// flush reads up to BatchSize unsynced events per sandbox DB, XADDs each,
// then marks them synced. On XADD failure, leaves them unsynced for retry.
func (p *RedisEventPublisher) flush(ctx context.Context) {
	events, err := p.sandboxDBs.GetAllUnsyncedEventsFlat(p.batchSize)
	if err != nil {
		log.Printf("redis_event_publisher: GetAllUnsyncedEventsFlat error: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	// Group successful sandbox-id → event-ids for batch MarkSynced.
	synced := make(map[string][]int64)
	xaddCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for _, se := range events {
		envelope := SandboxEventEnvelope{
			ID:        uuid.NewString(),
			Type:      se.Event.Type,
			SandboxID: se.SandboxID,
			WorkerID:  p.workerID,
			CellID:    p.cellID,
			Payload:   json.RawMessage(se.Event.Payload),
			Timestamp: se.Timestamp,
		}
		if p.resolver != nil {
			if orgID, plan, ok := p.resolver(se.SandboxID); ok {
				envelope.OrgID = orgID
				envelope.Plan = plan
			}
		}

		body, err := json.Marshal(envelope)
		if err != nil {
			log.Printf("redis_event_publisher: marshal failed for sandbox %s: %v", se.SandboxID, err)
			continue
		}

		err = p.rdb.XAdd(xaddCtx, &redis.XAddArgs{
			Stream: p.streamKey,
			MaxLen: p.maxLen,
			Approx: true,
			Values: map[string]interface{}{"event": string(body)},
		}).Err()
		if err != nil {
			log.Printf("redis_event_publisher: XADD failed for sandbox %s: %v — will retry", se.SandboxID, err)
			continue
		}
		synced[se.SandboxID] = append(synced[se.SandboxID], se.Event.ID)
	}

	for sandboxID, ids := range synced {
		if err := p.sandboxDBs.MarkSynced(sandboxID, ids); err != nil {
			log.Printf("redis_event_publisher: MarkSynced %s: %v", sandboxID, err)
		}
	}

	if total := 0; true {
		for _, ids := range synced {
			total += len(ids)
		}
		if total > 0 {
			log.Printf("redis_event_publisher: published %d events to %s", total, p.streamKey)
		}
	}
}
