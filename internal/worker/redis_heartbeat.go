package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisHeartbeatPayload is the JSON structure published to Redis.
type redisHeartbeatPayload struct {
	WorkerID  string  `json:"worker_id"`
	MachineID string  `json:"machine_id,omitempty"` // EC2 instance ID (e.g. i-099088f8ac4a34ef3)
	Region    string  `json:"region"`
	GRPCAddr  string  `json:"grpc_addr"`
	HTTPAddr  string  `json:"http_addr"`
	Capacity  int     `json:"capacity"`
	Current   int     `json:"current"`
	CPUPct    float64 `json:"cpu_pct"`
	MemPct    float64 `json:"mem_pct"`
	DiskPct       float64 `json:"disk_pct"`
	GoldenVersion string  `json:"golden_version,omitempty"`
	WorkerVersion string  `json:"worker_version,omitempty"`
}

// RedisHeartbeat publishes periodic heartbeats to Redis for worker discovery.
// Each heartbeat:
//  1. SETs worker:{id} with a 30s TTL (auto-expires if worker dies)
//  2. PUBLISHes to workers:heartbeat for real-time server notification
type RedisHeartbeat struct {
	rdb       *redis.Client
	workerID  string
	machineID string
	region    string
	grpcAddr  string
	httpAddr  string
	getStats      func() (capacity, current int, cpuPct, memPct, diskPct float64)
	onReconnect   func() // called when heartbeat succeeds after a previous failure
	goldenVersion string
	workerVersion string
	wasDown       bool   // true if the last publish failed (used to detect reconnect)
	stop          chan struct{}
}

// NewRedisHeartbeat creates a new heartbeat publisher.
func NewRedisHeartbeat(redisURL, workerID, region, grpcAddr, httpAddr string) (*RedisHeartbeat, error) {
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

	return &RedisHeartbeat{
		rdb:      rdb,
		workerID: workerID,
		region:   region,
		grpcAddr: grpcAddr,
		httpAddr: httpAddr,
		stop:     make(chan struct{}),
	}, nil
}

// SetMachineID sets the EC2 instance ID for the heartbeat (used by scaler for drain/terminate).
func (h *RedisHeartbeat) SetMachineID(id string) {
	h.machineID = id
}

// SetGoldenVersion sets the golden snapshot version hash for the heartbeat.
func (h *RedisHeartbeat) SetGoldenVersion(v string) {
	h.goldenVersion = v
}

// SetWorkerVersion sets the worker binary version (git SHA) for the heartbeat.
func (h *RedisHeartbeat) SetWorkerVersion(v string) {
	h.workerVersion = v
}

// OnReconnect sets a callback that fires when heartbeat succeeds after a failure.
// Used to reconcile sandbox state after a network outage.
func (h *RedisHeartbeat) OnReconnect(fn func()) {
	h.onReconnect = fn
}

// Start begins publishing heartbeats every 10 seconds.
func (h *RedisHeartbeat) Start(getStats func() (int, int, float64, float64, float64)) {
	h.getStats = getStats

	go func() {
		// Publish immediately on start
		h.publish()

		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				h.publish()
			case <-h.stop:
				return
			}
		}
	}()
}

func (h *RedisHeartbeat) publish() {
	capacity, current, cpuPct, memPct, diskPct := h.getStats()

	payload := redisHeartbeatPayload{
		WorkerID:  h.workerID,
		MachineID: h.machineID,
		Region:    h.region,
		GRPCAddr:  h.grpcAddr,
		HTTPAddr:  h.httpAddr,
		Capacity:  capacity,
		Current:   current,
		CPUPct:    cpuPct,
		MemPct:    memPct,
		DiskPct:       diskPct,
		GoldenVersion: h.goldenVersion,
		WorkerVersion: h.workerVersion,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("redis_heartbeat: marshal error: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// SET with 30s TTL — key auto-expires if worker dies
	key := "worker:" + h.workerID
	setErr := h.rdb.Set(ctx, key, data, 30*time.Second).Err()
	if setErr != nil {
		log.Printf("redis_heartbeat: SET failed: %v", setErr)
		h.wasDown = true
	} else if h.wasDown {
		// Heartbeat succeeded after previous failure — we reconnected
		h.wasDown = false
		log.Printf("redis_heartbeat: reconnected after outage, triggering reconciliation")
		if h.onReconnect != nil {
			go h.onReconnect()
		}
	}

	// PUBLISH for real-time notification
	if err := h.rdb.Publish(ctx, "workers:heartbeat", data).Err(); err != nil {
		log.Printf("redis_heartbeat: PUBLISH failed: %v", err)
	}
}

// Stop stops the heartbeat publisher and closes the Redis connection.
func (h *RedisHeartbeat) Stop() {
	close(h.stop)

	// Remove the key so the server knows we're gone immediately
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.rdb.Del(ctx, "worker:"+h.workerID)

	h.rdb.Close()
	log.Println("redis_heartbeat: stopped")
}
