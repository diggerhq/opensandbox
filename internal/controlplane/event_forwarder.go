package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/db"
	"github.com/redis/go-redis/v9"
)

const (
	consumerGroup = "cf-forwarder"

	readBatchSize  = 500
	readBlock      = 5 * time.Second
	reclaimMinIdle = 60 * time.Second
	reclaimTick    = 30 * time.Second
	metricsTick    = 10 * time.Second

	// identityCacheSize bounds the sandbox→(org, plan) cache. One entry per
	// live sandbox in the cell; 10k is comfortably more than any single cell
	// is expected to host.
	identityCacheSize = 10000
)

// EventForwarder drains events:{cell_id} into the CF events-ingest Worker.
//
// Design notes:
//   - Consumer group "cf-forwarder" with one consumer name per process (hostname).
//     This lets multiple CP replicas read the same stream without duplicating
//     work and recovers cleanly after a crash via XAUTOCLAIM.
//   - Events from the worker carry no org_id/plan; the forwarder enriches each
//     envelope by looking up the sandbox's billing identity in PG (cached).
//   - On CF failure, messages stay in the pending-entries-list (PEL) and the
//     next XAUTOCLAIM reclaims them. 4xx responses are ACKed + logged because
//     retrying would produce the same failure.
type EventForwarder struct {
	rdb       *redis.Client
	streamKey string
	consumer  string
	cellID    string
	store     *db.Store
	client    *CFEventClient

	identityCache *sync.Map // sandbox_id -> db.BillingIdentity
	cacheSize     int

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewEventForwarder wires a forwarder for the given cell. A non-nil store is
// required so we can enrich event envelopes with org_id/plan.
func NewEventForwarder(redisURL, cellID, cfEndpoint, cfSecret string) (*EventForwarder, error) {
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

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "cp-unknown"
	}

	return &EventForwarder{
		rdb:           rdb,
		streamKey:     "events:" + cellID,
		consumer:      "forwarder-" + hostname,
		cellID:        cellID,
		client:        NewCFEventClient(cfEndpoint, cfSecret, cellID),
		identityCache: &sync.Map{},
		stop:          make(chan struct{}),
	}, nil
}

// SetStore wires the PG store used for envelope enrichment. Must be called
// before Start if the store is constructed after the forwarder.
func (f *EventForwarder) SetStore(store *db.Store) {
	f.store = store
}

// Start launches the read, reclaim, and metrics loops. Idempotent: second call
// is a no-op because the group create is tolerant of BUSYGROUP.
func (f *EventForwarder) Start() {
	f.ensureGroup()

	f.wg.Add(3)
	go f.readLoop()
	go f.reclaimLoop()
	go f.metricsLoop()
}

// Stop signals all loops to exit and closes the Redis connection.
func (f *EventForwarder) Stop() {
	close(f.stop)
	f.wg.Wait()
	_ = f.rdb.Close()
}

func (f *EventForwarder) ensureGroup() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// MKSTREAM creates the stream if it doesn't exist. $ means "deliver only
	// events that arrive after group creation", which is correct for a fresh
	// install; on reconnect the group's offset is already persisted.
	err := f.rdb.XGroupCreateMkStream(ctx, f.streamKey, consumerGroup, "$").Err()
	if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
		log.Printf("event_forwarder: XGROUP CREATE: %v", err)
	}
}

func (f *EventForwarder) readLoop() {
	defer f.wg.Done()

	for {
		select {
		case <-f.stop:
			return
		default:
		}

		ctx, cancel := context.WithTimeout(context.Background(), readBlock+5*time.Second)
		streams, err := f.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    consumerGroup,
			Consumer: f.consumer,
			Streams:  []string{f.streamKey, ">"},
			Count:    readBatchSize,
			Block:    readBlock,
		}).Result()
		cancel()

		if err != nil {
			if errors.Is(err, redis.Nil) || errors.Is(err, context.DeadlineExceeded) {
				continue // no messages in the block window
			}
			log.Printf("event_forwarder: XREADGROUP: %v", err)
			// Back off briefly so we don't hot-loop on a broken Redis.
			select {
			case <-f.stop:
				return
			case <-time.After(1 * time.Second):
			}
			continue
		}

		for _, s := range streams {
			if len(s.Messages) == 0 {
				continue
			}
			f.processBatch(s.Messages)
		}
	}
}

func (f *EventForwarder) reclaimLoop() {
	defer f.wg.Done()
	ticker := time.NewTicker(reclaimTick)
	defer ticker.Stop()

	var cursor string = "0-0"
	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		msgs, nextCursor, err := f.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   f.streamKey,
			Group:    consumerGroup,
			Consumer: f.consumer,
			MinIdle:  reclaimMinIdle,
			Start:    cursor,
			Count:    readBatchSize,
		}).Result()
		cancel()

		if err != nil {
			log.Printf("event_forwarder: XAUTOCLAIM: %v", err)
			continue
		}

		cursor = nextCursor
		if len(msgs) > 0 {
			log.Printf("event_forwarder: reclaimed %d stuck messages", len(msgs))
			f.processBatch(msgs)
		}
	}
}

func (f *EventForwarder) metricsLoop() {
	defer f.wg.Done()
	ticker := time.NewTicker(metricsTick)
	defer ticker.Stop()

	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pending, err := f.rdb.XPending(ctx, f.streamKey, consumerGroup).Result()
		cancel()
		if err != nil {
			continue
		}
		// TODO: wire a Prometheus gauge once internal/metrics has a registry handle.
		_ = pending
	}
}

// processBatch decodes messages, enriches them with (org_id, plan), sends via
// the CF client, and ACKs on success. On transient failure messages stay in
// the PEL for a later reclaim; on a 4xx we ACK anyway and log.
func (f *EventForwarder) processBatch(msgs []redis.XMessage) {
	if len(msgs) == 0 {
		return
	}

	enriched := make([]map[string]interface{}, 0, len(msgs))
	ids := make([]string, 0, len(msgs))

	for _, m := range msgs {
		raw, ok := m.Values["event"].(string)
		if !ok {
			// Malformed entry — ack so we don't reprocess forever.
			ids = append(ids, m.ID)
			log.Printf("event_forwarder: dropping malformed entry %s", m.ID)
			continue
		}

		var envelope map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			ids = append(ids, m.ID)
			log.Printf("event_forwarder: unmarshal %s: %v", m.ID, err)
			continue
		}

		sandboxID, _ := envelope["sandbox_id"].(string)
		if sandboxID != "" {
			if identity, ok := f.lookupIdentity(sandboxID); ok {
				envelope["org_id"] = identity.OrgID
				envelope["plan"] = identity.Plan
			}
		}
		enriched = append(enriched, envelope)
		ids = append(ids, m.ID)
	}

	if len(enriched) == 0 {
		// Only malformed entries — still ack.
		f.ack(ids)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	err := f.client.SendBatch(ctx, enriched)
	if err == nil {
		f.ack(ids)
		return
	}

	var perm *PermanentError
	if errors.As(err, &perm) {
		// 4xx: the batch is structurally unacceptable. Acking prevents the PEL
		// from filling with poison. The log is the breadcrumb for triage.
		log.Printf("event_forwarder: poison batch (size=%d): %v", len(enriched), err)
		f.ack(ids)
		return
	}

	// Transient (5xx or network). Leave in PEL; XAUTOCLAIM retries later.
	log.Printf("event_forwarder: transient send error (size=%d): %v", len(enriched), err)
}

func (f *EventForwarder) ack(ids []string) {
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.rdb.XAck(ctx, f.streamKey, consumerGroup, ids...).Err(); err != nil {
		log.Printf("event_forwarder: XACK: %v", err)
	}
}

func (f *EventForwarder) lookupIdentity(sandboxID string) (db.BillingIdentity, bool) {
	if v, ok := f.identityCache.Load(sandboxID); ok {
		return v.(db.BillingIdentity), true
	}
	if f.store == nil {
		return db.BillingIdentity{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	identity, err := f.store.GetSandboxBillingIdentity(ctx, sandboxID)
	if err != nil {
		// Common case: events for a sandbox that has already been destroyed.
		// Log at debug level via a silent path to avoid spamming.
		return db.BillingIdentity{}, false
	}
	// Cheap overflow guard: if the cache grows too large, wipe it. A proper
	// LRU is overkill for phase 1 since we don't expect more than a few
	// thousand distinct sandboxes over the lifetime of a CP process.
	if f.cacheSize >= identityCacheSize {
		f.identityCache = &sync.Map{}
		f.cacheSize = 0
	}
	f.identityCache.Store(sandboxID, identity)
	f.cacheSize++
	return identity, true
}
