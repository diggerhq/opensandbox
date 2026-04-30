package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// EventForwarder drains the local Redis Stream events:{cell_id} and POSTs
// HMAC-signed batches to the CF events-ingest Worker. It is the only
// back-channel from a regional control plane to the global Cloudflare layer.
//
// Lifecycle:
//   - Start: ensures consumer group "cf-forwarder" exists (XGROUP CREATE MKSTREAM,
//     idempotent — ignores BUSYGROUP).
//   - readLoop: XREADGROUP Count: 500 Block: 5s, dispatch to CFEventClient,
//     XAck on success, leave in PEL on retry-able error.
//   - reclaimLoop: XAUTOCLAIM every 30s with MinIdle: 60s — recovers messages
//     left pending by a crashed instance.
//   - 4xx (non-429): poison pill — XAck, log, drop.
type EventForwarder struct {
	rdb       *redis.Client
	streamKey string // events:{cell_id}
	groupName string // "cf-forwarder"
	consumer  string

	client    *CFEventClient
	batchSize int64
	blockDur  time.Duration

	stopCh chan struct{}
	doneCh chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// EventForwarderConfig configures the forwarder.
type EventForwarderConfig struct {
	Redis    *redis.Client
	CellID   string
	Client   *CFEventClient
	Consumer string // optional, defaults to hostname:pid
}

// NewEventForwarder constructs a forwarder. Caller must Start it.
func NewEventForwarder(cfg EventForwarderConfig) (*EventForwarder, error) {
	if cfg.Redis == nil {
		return nil, errors.New("event_forwarder: Redis client required")
	}
	if cfg.CellID == "" {
		return nil, errors.New("event_forwarder: CellID required")
	}
	if cfg.Client == nil {
		return nil, errors.New("event_forwarder: CFEventClient required")
	}
	consumer := cfg.Consumer
	if consumer == "" {
		host, _ := os.Hostname()
		consumer = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	return &EventForwarder{
		rdb:       cfg.Redis,
		streamKey: "events:" + cfg.CellID,
		groupName: "cf-forwarder",
		consumer:  consumer,
		client:    cfg.Client,
		batchSize: 500,
		blockDur:  5 * time.Second,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}, nil
}

// Start begins read + reclaim loops. Idempotent.
func (f *EventForwarder) Start(ctx context.Context) error {
	// Create the consumer group; ignore BUSYGROUP (already exists).
	if err := f.rdb.XGroupCreateMkStream(ctx, f.streamKey, f.groupName, "$").Err(); err != nil {
		// go-redis returns the raw error string; check for the BUSYGROUP suffix.
		if !isBusyGroup(err) {
			return fmt.Errorf("create consumer group: %w", err)
		}
	}

	f.wg.Add(2)
	go f.readLoop(ctx)
	go f.reclaimLoop(ctx)
	go func() {
		f.wg.Wait()
		close(f.doneCh)
	}()
	log.Printf("event_forwarder: started (stream=%s group=%s consumer=%s)", f.streamKey, f.groupName, f.consumer)
	return nil
}

// Stop gracefully shuts down. Drains in-flight ack on best-effort basis.
func (f *EventForwarder) Stop(ctx context.Context) error {
	f.once.Do(func() { close(f.stopCh) })
	select {
	case <-f.doneCh:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (f *EventForwarder) readLoop(ctx context.Context) {
	defer f.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stopCh:
			return
		default:
		}

		streams, err := f.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    f.groupName,
			Consumer: f.consumer,
			Streams:  []string{f.streamKey, ">"},
			Count:    f.batchSize,
			Block:    f.blockDur,
		}).Result()

		if err != nil {
			if errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			log.Printf("event_forwarder: XREADGROUP error: %v", err)
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			case <-f.stopCh:
				return
			}
			continue
		}

		for _, s := range streams {
			f.processBatch(ctx, s.Messages)
		}
	}
}

func (f *EventForwarder) reclaimLoop(ctx context.Context) {
	defer f.wg.Done()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.stopCh:
			return
		case <-ticker.C:
			f.reclaimOnce(ctx)
		}
	}
}

func (f *EventForwarder) reclaimOnce(ctx context.Context) {
	start := "0-0"
	for {
		msgs, nextStart, err := f.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream:   f.streamKey,
			Group:    f.groupName,
			Consumer: f.consumer,
			MinIdle:  60 * time.Second,
			Start:    start,
			Count:    100,
		}).Result()
		if err != nil {
			log.Printf("event_forwarder: XAUTOCLAIM error: %v", err)
			return
		}
		if len(msgs) == 0 {
			return
		}
		f.processBatch(ctx, msgs)
		if nextStart == "0-0" || nextStart == "" {
			return
		}
		start = nextStart
	}
}

// processBatch serializes a batch and dispatches via the CF client. Acks on
// success or permanent error, leaves in PEL on retryable failure.
//
// Per-entry validation: malformed JSON is poison — acked and dropped immediately
// rather than left in the PEL where it would block progress on every retry.
func (f *EventForwarder) processBatch(ctx context.Context, msgs []redis.XMessage) {
	if len(msgs) == 0 {
		return
	}

	envelopes := make([]json.RawMessage, 0, len(msgs))
	idsBySource := make([]string, 0, len(msgs))
	for _, m := range msgs {
		raw, ok := m.Values["event"]
		if !ok {
			log.Printf("event_forwarder: stream entry %s missing 'event' field — acking and dropping", m.ID)
			f.ack(ctx, m.ID)
			continue
		}
		s, ok := raw.(string)
		if !ok {
			log.Printf("event_forwarder: stream entry %s 'event' field not a string — acking and dropping", m.ID)
			f.ack(ctx, m.ID)
			continue
		}
		// Per-entry JSON validation. Without this, a single malformed entry
		// poisons the whole batch (json.Marshal of []json.RawMessage validates
		// every element) and the forwarder retries forever.
		if !json.Valid([]byte(s)) {
			preview := s
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			log.Printf("event_forwarder: stream entry %s has invalid JSON — acking and dropping; preview=%q", m.ID, preview)
			f.ack(ctx, m.ID)
			continue
		}
		envelopes = append(envelopes, json.RawMessage(s))
		idsBySource = append(idsBySource, m.ID)
	}
	if len(envelopes) == 0 {
		return
	}

	body, err := json.Marshal(envelopes)
	if err != nil {
		// All entries individually validated above, so this should be unreachable.
		// If it ever fires, log loudly and leave entries in PEL for inspection.
		log.Printf("event_forwarder: BUG: marshal batch failed despite per-entry validation: %v — leaving %d msgs in PEL", err, len(envelopes))
		return
	}

	sendCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	err = f.client.SendBatch(sendCtx, body)
	switch {
	case err == nil:
		f.ack(ctx, idsBySource...)
	case errors.Is(err, ErrPermanent):
		log.Printf("event_forwarder: poison-pill batch (%d msgs): %v — acking and dropping", len(envelopes), err)
		f.ack(ctx, idsBySource...)
	default:
		log.Printf("event_forwarder: batch send failed (%d msgs): %v — leaving in PEL", len(envelopes), err)
	}
}

func (f *EventForwarder) ack(ctx context.Context, ids ...string) {
	if len(ids) == 0 {
		return
	}
	if err := f.rdb.XAck(ctx, f.streamKey, f.groupName, ids...).Err(); err != nil {
		log.Printf("event_forwarder: XACK error: %v", err)
	}
}

func isBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}
