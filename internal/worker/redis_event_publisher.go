package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/redis/go-redis/v9"
)

// streamMaxLen bounds the Redis stream at ~30 minutes of events at normal load.
// Approximate trimming (~) lets Redis skip work when the stream is short.
const streamMaxLen = 100000

// syncInterval matches the existing per-sandbox SQLite poll cadence.
const syncInterval = 2 * time.Second

// eventBatchPerSandbox caps how many events we pull from each sandbox per tick.
const eventBatchPerSandbox = 100

// RedisEventPublisher drains sandbox-local SQLite event rows into the Redis
// stream events:{cell_id}. The CP's event_forwarder consumes that stream and
// ships batches to the Cloudflare events-ingest Worker. Heartbeats are handled
// separately by redis_heartbeat — this publisher owns events only.
type RedisEventPublisher struct {
	rdb        *redis.Client
	cellID     string
	workerID   string
	streamKey  string
	sandboxDBs *sandbox.SandboxDBManager
	// store is unused today; carried for a future optimization where the
	// publisher enriches envelopes with org_id/plan rather than the forwarder.
	// Keeping the dependency wired avoids churn in cmd/worker/main.go when that
	// change lands.
	store interface{}

	stop chan struct{}
	wg   sync.WaitGroup
}

// sandboxEventEnvelope matches the JSON shape the CP forwarder expects. org_id
// and plan are intentionally absent — the forwarder enriches them.
type sandboxEventEnvelope struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	SandboxID string          `json:"sandbox_id"`
	WorkerID  string          `json:"worker_id"`
	CellID    string          `json:"cell_id"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

// NewRedisEventPublisher connects to Redis and returns a publisher wired to the
// stream for the given cell. Ping validates connectivity at startup; callers
// should treat a non-nil error as "run without event sync" rather than fatal.
func NewRedisEventPublisher(redisURL, cellID, workerID string, sandboxDBs *sandbox.SandboxDBManager, store interface{}) (*RedisEventPublisher, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}
	rdb := redis.NewClient(opts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	return &RedisEventPublisher{
		rdb:        rdb,
		cellID:     cellID,
		workerID:   workerID,
		streamKey:  "events:" + cellID,
		sandboxDBs: sandboxDBs,
		store:      store,
		stop:       make(chan struct{}),
	}, nil
}

// Start begins the sync loop. Safe to call once.
func (p *RedisEventPublisher) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				p.syncEvents()
			case <-p.stop:
				p.syncEvents() // final flush
				return
			}
		}
	}()
}

// Stop halts the sync loop, flushes once more, and closes the Redis connection.
func (p *RedisEventPublisher) Stop() {
	close(p.stop)
	p.wg.Wait()
	_ = p.rdb.Close()
}

func (p *RedisEventPublisher) syncEvents() {
	events, err := p.sandboxDBs.GetAllUnsyncedEventsFlat(eventBatchPerSandbox)
	if err != nil {
		log.Printf("redis_event_publisher: collect unsynced: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Track which local event IDs succeeded per sandbox so we only mark those synced.
	synced := make(map[string][]int64)

	for _, se := range events {
		envelope := sandboxEventEnvelope{
			ID:        uuid.NewString(),
			Type:      se.Event.Type,
			SandboxID: se.SandboxID,
			WorkerID:  p.workerID,
			CellID:    p.cellID,
			Payload:   json.RawMessage(se.Event.Payload),
			Timestamp: se.Timestamp,
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			// Shouldn't happen; payload is already valid JSON. Skip without marking
			// synced so we retry on the next tick.
			log.Printf("redis_event_publisher: marshal event %s: %v", envelope.ID, err)
			continue
		}

		args := &redis.XAddArgs{
			Stream: p.streamKey,
			MaxLen: streamMaxLen,
			Approx: true,
			Values: map[string]interface{}{"event": data},
		}
		if _, err := p.rdb.XAdd(ctx, args).Result(); err != nil {
			log.Printf("redis_event_publisher: XADD to %s failed: %v", p.streamKey, err)
			// Stop this batch so we don't mark anything synced that didn't land.
			break
		}
		synced[se.SandboxID] = append(synced[se.SandboxID], se.Event.ID)
	}

	for sandboxID, ids := range synced {
		if err := p.sandboxDBs.MarkSynced(sandboxID, ids); err != nil {
			log.Printf("redis_event_publisher: MarkSynced sandbox %s: %v", sandboxID, err)
		}
	}
}
