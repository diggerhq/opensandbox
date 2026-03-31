package worker

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/opensandbox/opensandbox/internal/sandbox"
)

// EventPublisher publishes sandbox events from local SQLite to NATS JetStream.
type EventPublisher struct {
	nc         *nats.Conn
	js         nats.JetStreamContext
	sandboxDBs *sandbox.SandboxDBManager
	region     string
	workerID      string
	goldenVersion string
	stop          chan struct{}
	wg            sync.WaitGroup
}

// NATSEvent is the JSON payload published to NATS.
type NATSEvent struct {
	Type      string          `json:"type"`
	SandboxID string          `json:"sandbox_id"`
	WorkerID  string          `json:"worker_id"`
	Region    string          `json:"region"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp time.Time       `json:"timestamp"`
}

// NewEventPublisher creates a new NATS event publisher.
func NewEventPublisher(natsURL, region, workerID string, sandboxDBs *sandbox.SandboxDBManager) (*EventPublisher, error) {
	nc, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS: %w", err)
	}

	js, err := nc.JetStream()
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("failed to get JetStream context: %w", err)
	}

	// Ensure the stream exists
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "SANDBOX_EVENTS",
		Subjects: []string{"sandbox.events.>"},
		MaxAge:   7 * 24 * time.Hour, // Retain for 7 days
	})
	if err != nil {
		// Stream may already exist, that's OK
		log.Printf("event_publisher: stream setup: %v", err)
	}

	return &EventPublisher{
		nc:         nc,
		js:         js,
		sandboxDBs: sandboxDBs,
		region:     region,
		workerID:   workerID,
		stop:       make(chan struct{}),
	}, nil
}

// Start begins the event sync loop (every 2 seconds).
func (p *EventPublisher) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				p.syncEvents()
			case <-p.stop:
				// Final flush
				p.syncEvents()
				return
			}
		}
	}()
}

// Stop stops the event sync loop and closes the NATS connection.
func (p *EventPublisher) Stop() {
	close(p.stop)
	p.wg.Wait()
	p.nc.Close()
}

// SetGoldenVersion sets the golden snapshot version hash for heartbeats.
func (p *EventPublisher) SetGoldenVersion(v string) {
	p.goldenVersion = v
}

// PublishHeartbeat sends a worker heartbeat to NATS.
func (p *EventPublisher) PublishHeartbeat(capacity, current int, cpuPct, memPct, diskPct float64) {
	subject := fmt.Sprintf("workers.heartbeat.%s.%s", p.region, p.workerID)
	payload := map[string]interface{}{
		"worker_id": p.workerID,
		"region":    p.region,
		"capacity":  capacity,
		"current":   current,
		"cpu_pct":   cpuPct,
		"mem_pct":   memPct,
		"disk_pct":        diskPct,
		"golden_version":  p.goldenVersion,
	}
	data, _ := json.Marshal(payload)
	if err := p.nc.Publish(subject, data); err != nil {
		log.Printf("event_publisher: heartbeat publish error: %v", err)
	}
}

// StartHeartbeat begins sending heartbeats every 5 seconds.
func (p *EventPublisher) StartHeartbeat(getStats func() (capacity, current int, cpuPct, memPct, diskPct float64)) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				cap, cur, cpu, mem, disk := getStats()
				p.PublishHeartbeat(cap, cur, cpu, mem, disk)
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *EventPublisher) syncEvents() {
	events, err := p.sandboxDBs.GetAllUnsyncedEventsFlat(100)
	if err != nil || len(events) == 0 {
		return
	}

	// Group by sandbox for batch marking
	synced := make(map[string][]int64)

	for _, se := range events {
		subject := fmt.Sprintf("sandbox.events.%s.%s", p.region, p.workerID)
		natsEvent := NATSEvent{
			Type:      se.Event.Type,
			SandboxID: se.SandboxID,
			WorkerID:  p.workerID,
			Region:    p.region,
			Payload:   json.RawMessage(se.Event.Payload),
			Timestamp: se.Timestamp,
		}
		data, _ := json.Marshal(natsEvent)

		if _, err := p.js.Publish(subject, data); err != nil {
			log.Printf("event_publisher: publish error for sandbox %s: %v", se.SandboxID, err)
			continue
		}

		synced[se.SandboxID] = append(synced[se.SandboxID], se.Event.ID)
	}

	// Mark synced events
	for sandboxID, ids := range synced {
		if err := p.sandboxDBs.MarkSynced(sandboxID, ids); err != nil {
			log.Printf("event_publisher: mark synced error for sandbox %s: %v", sandboxID, err)
		}
	}

	if total := len(events); total > 0 {
		log.Printf("event_publisher: synced %d events to NATS", total)
	}
}
