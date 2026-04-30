package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// newTestRegistries creates two RedisWorkerRegistry instances against the same
// test Redis instance, simulating two control planes sharing state. Skips the
// test when Redis is not reachable. Returns the registries and a cleanup
// function that removes any keys created under the supplied workerID.
func newTestRegistries(t *testing.T, workerID string) (*RedisWorkerRegistry, *RedisWorkerRegistry) {
	t.Helper()
	const redisURL = "redis://localhost:6379/15"

	probe := redis.NewClient(&redis.Options{Addr: "localhost:6379", DB: 15})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	if err := probe.Ping(pingCtx).Err(); err != nil {
		probe.Close()
		t.Skipf("skipping: Redis not available at localhost:6379: %v", err)
	}

	cleanupKeys := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		probe.Del(ctx, drainKeyPrefix+workerID)
		probe.Del(ctx, "worker:"+workerID)
		probe.Del(ctx, "routing:count:"+workerID)
	}
	cleanupKeys()

	cp1, err := NewRedisWorkerRegistry(redisURL)
	if err != nil {
		t.Fatalf("cp1 NewRedisWorkerRegistry: %v", err)
	}
	cp2, err := NewRedisWorkerRegistry(redisURL)
	if err != nil {
		cp1.Stop()
		t.Fatalf("cp2 NewRedisWorkerRegistry: %v", err)
	}

	t.Cleanup(func() {
		cp1.Stop()
		cp2.Stop()
		cleanupKeys()
		probe.Close()
	})

	return cp1, cp2
}

func TestSetDrainingPublishesToRedis(t *testing.T) {
	workerID := fmt.Sprintf("test-drain-publish-%d", time.Now().UnixNano())
	cp, _ := newTestRegistries(t, workerID)

	// Bootstrap so SetDraining has a local entry to flip.
	cp.handleHeartbeat(WorkerEntry{ID: workerID, Region: "us-east-1", Capacity: 50})

	cp.SetDraining(workerID, true)

	// Local cache reflects the change immediately.
	if w := cp.GetWorker(workerID); w == nil || !w.Draining {
		t.Fatalf("expected local cache draining=true, got %+v", w)
	}

	// Redis has the key with content "1" and a TTL.
	ctx := context.Background()
	val, err := cp.rdb.Get(ctx, drainKeyPrefix+workerID).Result()
	if err != nil || val != "1" {
		t.Fatalf("expected drain key value \"1\", got val=%q err=%v", val, err)
	}
	ttl, err := cp.rdb.TTL(ctx, drainKeyPrefix+workerID).Result()
	if err != nil {
		t.Fatalf("TTL fetch failed: %v", err)
	}
	if ttl <= 0 || ttl > drainKeyTTL {
		t.Fatalf("expected TTL within (0, %s], got %s", drainKeyTTL, ttl)
	}

	// Clearing removes the key.
	cp.SetDraining(workerID, false)
	if exists, _ := cp.rdb.Exists(ctx, drainKeyPrefix+workerID).Result(); exists != 0 {
		t.Fatalf("expected drain key removed after SetDraining(false)")
	}
	if w := cp.GetWorker(workerID); w == nil || w.Draining {
		t.Fatalf("expected local cache draining=false, got %+v", w)
	}
}

func TestHeartbeatAppliesRedisDrainOverride(t *testing.T) {
	workerID := fmt.Sprintf("test-drain-heartbeat-%d", time.Now().UnixNano())
	cp, _ := newTestRegistries(t, workerID)

	// Worker exists, not draining locally.
	cp.handleHeartbeat(WorkerEntry{ID: workerID, Region: "us-east-1", Capacity: 50})
	if w := cp.GetWorker(workerID); w == nil || w.Draining {
		t.Fatalf("expected initial draining=false, got %+v", w)
	}

	// External actor sets the drain key directly (e.g. another CP).
	ctx := context.Background()
	if err := cp.rdb.Set(ctx, drainKeyPrefix+workerID, "1", drainKeyTTL).Err(); err != nil {
		t.Fatalf("seeding drain key: %v", err)
	}

	// Next heartbeat picks up the override.
	cp.handleHeartbeat(WorkerEntry{ID: workerID, Region: "us-east-1", Capacity: 50})
	if w := cp.GetWorker(workerID); w == nil || !w.Draining {
		t.Fatalf("expected heartbeat to apply drain override, got %+v", w)
	}

	// Clearing the key in Redis lets the next heartbeat reset draining.
	if err := cp.rdb.Del(ctx, drainKeyPrefix+workerID).Err(); err != nil {
		t.Fatalf("deleting drain key: %v", err)
	}
	cp.handleHeartbeat(WorkerEntry{ID: workerID, Region: "us-east-1", Capacity: 50})
	if w := cp.GetWorker(workerID); w == nil || w.Draining {
		t.Fatalf("expected heartbeat to clear drain when key removed, got %+v", w)
	}
}

func TestSetDrainingPropagatesAcrossControlPlanes(t *testing.T) {
	workerID := fmt.Sprintf("test-drain-cross-cp-%d", time.Now().UnixNano())
	cp1, cp2 := newTestRegistries(t, workerID)

	entry := WorkerEntry{ID: workerID, Region: "us-east-1", Capacity: 50}
	cp1.handleHeartbeat(entry)
	cp2.handleHeartbeat(entry)

	// Initially neither CP sees the worker as draining.
	if cp1.GetWorker(workerID).Draining || cp2.GetWorker(workerID).Draining {
		t.Fatal("neither CP should see drain initially")
	}

	// Operator hits CP1 with drain. CP1 reflects immediately; CP2 lags until
	// its next heartbeat.
	cp1.SetDraining(workerID, true)
	if !cp1.GetWorker(workerID).Draining {
		t.Fatal("cp1 should reflect drain immediately on SetDraining")
	}
	if cp2.GetWorker(workerID).Draining {
		t.Fatal("cp2 should not yet reflect drain — its local map hasn't been refreshed")
	}

	// CP2's next heartbeat for that worker pulls the Redis key and converges.
	cp2.handleHeartbeat(entry)
	if !cp2.GetWorker(workerID).Draining {
		t.Fatal("cp2 should converge to draining after heartbeat reads Redis key")
	}

	// Operator clears drain via CP2. CP1 converges on its next heartbeat.
	cp2.SetDraining(workerID, false)
	if cp2.GetWorker(workerID).Draining {
		t.Fatal("cp2 should clear drain immediately on SetDraining(false)")
	}
	cp1.handleHeartbeat(entry)
	if cp1.GetWorker(workerID).Draining {
		t.Fatal("cp1 should converge to not-draining after heartbeat reads cleared key")
	}
}

func TestPlacementSkipsDrainingMarkedViaRedis(t *testing.T) {
	workerID := fmt.Sprintf("test-drain-placement-%d", time.Now().UnixNano())
	cp, _ := newTestRegistries(t, workerID)

	// Single eligible-looking worker entry.
	cp.handleHeartbeat(WorkerEntry{
		ID:       workerID,
		Region:   "us-east-1",
		GRPCAddr: "127.0.0.1:1", // dial will fail async; placement only needs the entry
		Capacity: 50,
		Current:  0,
		MemPct:   10,
	})

	// Without the drain marker, placement finds the worker (or fails on the
	// gRPC client lookup, which still proves it passed the eligibility gate).
	_, _, err := cp.GetLeastLoadedWorker("us-east-1")
	preDrainNoWorkers := err != nil && err.Error() == "no workers available"
	if preDrainNoWorkers {
		t.Fatal("worker should be eligible before drain marker is set")
	}

	// Set the drain marker via the public API.
	cp.SetDraining(workerID, true)

	// Now placement should report no workers available.
	_, _, err = cp.GetLeastLoadedWorker("us-east-1")
	if err == nil || err.Error() != "no workers available" {
		t.Fatalf("expected no-workers error after drain, got err=%v", err)
	}
}
