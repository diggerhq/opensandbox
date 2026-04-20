package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// ScalerStateStore persists scaler state across restarts.
// All methods use short timeouts internally so the scaler loop doesn't block.
type ScalerStateStore interface {
	// Cooldowns
	GetLastScaleUp(region string) (time.Time, bool)
	SetLastScaleUp(region string, t time.Time, cooldown time.Duration)

	// Pending launches (VMs launched but not yet registered)
	GetPendingLaunches(region string) []pendingLaunch
	AddPendingLaunch(region string, p pendingLaunch)
	RemovePendingLaunch(region, machineID string)
	SetPendingLaunches(region string, launches []pendingLaunch)

	// Draining workers
	GetDraining(machineID string) (*drainState, bool)
	SetDraining(machineID string, state *drainState)
	RemoveDraining(machineID string)
	IsDraining(machineID string) bool
	AllDraining() map[string]*drainState

	// Evacuation cooldowns
	GetLastEvacuation(workerID string) (time.Time, bool)
	SetLastEvacuation(workerID string, t time.Time)

	// Migration dedup
	AcquireMigrationLock(sandboxID string) bool
	ReleaseMigrationLock(sandboxID string)

	// In-flight migration tracking
	IncrInFlight(workerID string)
	DecrInFlight(workerID string)
	GetInFlight(workerID string) int

	// Rate-of-change tracking
	GetLastSandboxCount(region string) (int, bool)
	SetLastSandboxCount(region string, count int)

	// Creation failure backoff
	IncrCreationFailures(region string) int
	ResetCreationFailures(region string)
	GetCreationBackoffUntil(region string) (time.Time, bool)
	SetCreationBackoffUntil(region string, until time.Time)
}

// RedisScalerState implements ScalerStateStore backed by Redis.
type RedisScalerState struct {
	rdb *redis.Client
}

// NewRedisScalerState creates a new Redis-backed scaler state store.
func NewRedisScalerState(rdb *redis.Client) *RedisScalerState {
	return &RedisScalerState{rdb: rdb}
}

func (r *RedisScalerState) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

// --- Cooldowns ---

func (r *RedisScalerState) GetLastScaleUp(region string) (time.Time, bool) {
	ctx, cancel := r.ctx()
	defer cancel()
	val, err := r.rdb.Get(ctx, "scaler:lastscaleup:"+region).Result()
	if err != nil {
		return time.Time{}, false
	}
	var t time.Time
	if err := t.UnmarshalText([]byte(val)); err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (r *RedisScalerState) SetLastScaleUp(region string, t time.Time, cooldown time.Duration) {
	ctx, cancel := r.ctx()
	defer cancel()
	data, _ := t.MarshalText()
	r.rdb.Set(ctx, "scaler:lastscaleup:"+region, data, cooldown+60*time.Second)
}

// --- Pending Launches ---

func (r *RedisScalerState) GetPendingLaunches(region string) []pendingLaunch {
	ctx, cancel := r.ctx()
	defer cancel()
	vals, err := r.rdb.HGetAll(ctx, "scaler:pending:"+region).Result()
	if err != nil {
		return nil
	}
	var result []pendingLaunch
	for _, v := range vals {
		var p pendingLaunch
		if json.Unmarshal([]byte(v), &p) == nil {
			result = append(result, p)
		}
	}
	return result
}

func (r *RedisScalerState) AddPendingLaunch(region string, p pendingLaunch) {
	ctx, cancel := r.ctx()
	defer cancel()
	data, _ := json.Marshal(p)
	r.rdb.HSet(ctx, "scaler:pending:"+region, p.MachineID, data)
	r.rdb.Expire(ctx, "scaler:pending:"+region, pendingWorkerTTL+time.Minute)
}

func (r *RedisScalerState) RemovePendingLaunch(region, machineID string) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.HDel(ctx, "scaler:pending:"+region, machineID)
}

func (r *RedisScalerState) SetPendingLaunches(region string, launches []pendingLaunch) {
	ctx, cancel := r.ctx()
	defer cancel()
	key := "scaler:pending:" + region
	r.rdb.Del(ctx, key)
	for _, p := range launches {
		data, _ := json.Marshal(p)
		r.rdb.HSet(ctx, key, p.MachineID, data)
	}
	if len(launches) > 0 {
		r.rdb.Expire(ctx, key, pendingWorkerTTL+time.Minute)
	}
}

// --- Draining ---

func (r *RedisScalerState) GetDraining(machineID string) (*drainState, bool) {
	ctx, cancel := r.ctx()
	defer cancel()
	val, err := r.rdb.Get(ctx, "scaler:draining:"+machineID).Result()
	if err != nil {
		return nil, false
	}
	var ds drainState
	if json.Unmarshal([]byte(val), &ds) != nil {
		return nil, false
	}
	return &ds, true
}

func (r *RedisScalerState) SetDraining(machineID string, state *drainState) {
	ctx, cancel := r.ctx()
	defer cancel()
	data, _ := json.Marshal(state)
	r.rdb.Set(ctx, "scaler:draining:"+machineID, data, drainTimeout+5*time.Minute)
}

func (r *RedisScalerState) RemoveDraining(machineID string) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Del(ctx, "scaler:draining:" + machineID)
}

func (r *RedisScalerState) IsDraining(machineID string) bool {
	ctx, cancel := r.ctx()
	defer cancel()
	n, _ := r.rdb.Exists(ctx, "scaler:draining:"+machineID).Result()
	return n > 0
}

func (r *RedisScalerState) AllDraining() map[string]*drainState {
	ctx, cancel := r.ctx()
	defer cancel()
	result := make(map[string]*drainState)
	var cursor uint64
	for {
		keys, nextCursor, err := r.rdb.Scan(ctx, cursor, "scaler:draining:*", 100).Result()
		if err != nil {
			break
		}
		for _, key := range keys {
			machineID := key[len("scaler:draining:"):]
			val, err := r.rdb.Get(ctx, key).Result()
			if err == nil {
				var ds drainState
				if json.Unmarshal([]byte(val), &ds) == nil {
					result[machineID] = &ds
				}
			}
		}
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}
	return result
}

// --- Evacuation ---

func (r *RedisScalerState) GetLastEvacuation(workerID string) (time.Time, bool) {
	ctx, cancel := r.ctx()
	defer cancel()
	val, err := r.rdb.Get(ctx, "scaler:evacuation:"+workerID).Result()
	if err != nil {
		return time.Time{}, false
	}
	var t time.Time
	if err := t.UnmarshalText([]byte(val)); err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (r *RedisScalerState) SetLastEvacuation(workerID string, t time.Time) {
	ctx, cancel := r.ctx()
	defer cancel()
	data, _ := t.MarshalText()
	r.rdb.Set(ctx, "scaler:evacuation:"+workerID, data, evacuationCooldown+30*time.Second)
}

// --- Migration dedup ---

func (r *RedisScalerState) AcquireMigrationLock(sandboxID string) bool {
	ctx, cancel := r.ctx()
	defer cancel()
	ok, err := r.rdb.SetArgs(ctx, "scaler:migrating:"+sandboxID, "1", redis.SetArgs{
		Mode: "NX",
		TTL:  5 * time.Minute,
	}).Result()
	return err == nil && ok == "OK"
}

func (r *RedisScalerState) ReleaseMigrationLock(sandboxID string) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Del(ctx, "scaler:migrating:"+sandboxID)
}

// --- In-flight ---

func (r *RedisScalerState) IncrInFlight(workerID string) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Incr(ctx, "scaler:inflight:"+workerID)
	r.rdb.Expire(ctx, "scaler:inflight:"+workerID, 10*time.Minute)
}

func (r *RedisScalerState) DecrInFlight(workerID string) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Decr(ctx, "scaler:inflight:"+workerID)
}

func (r *RedisScalerState) GetInFlight(workerID string) int {
	ctx, cancel := r.ctx()
	defer cancel()
	n, err := r.rdb.Get(ctx, "scaler:inflight:"+workerID).Int()
	if err != nil {
		return 0
	}
	return n
}

// --- Rate of change ---

func (r *RedisScalerState) GetLastSandboxCount(region string) (int, bool) {
	ctx, cancel := r.ctx()
	defer cancel()
	n, err := r.rdb.Get(ctx, "scaler:sandboxcount:"+region).Int()
	if err != nil {
		return 0, false
	}
	return n, true
}

func (r *RedisScalerState) SetLastSandboxCount(region string, count int) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Set(ctx, "scaler:sandboxcount:"+region, fmt.Sprintf("%d", count), 2*time.Minute)
}

// --- Creation failure backoff (Redis) ---

func (r *RedisScalerState) IncrCreationFailures(region string) int {
	ctx, cancel := r.ctx()
	defer cancel()
	n, err := r.rdb.Incr(ctx, "scaler:createfail:"+region).Result()
	if err != nil {
		return 0
	}
	r.rdb.Expire(ctx, "scaler:createfail:"+region, 30*time.Minute)
	return int(n)
}

func (r *RedisScalerState) ResetCreationFailures(region string) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Del(ctx, "scaler:createfail:"+region)
	r.rdb.Del(ctx, "scaler:createbackoff:"+region)
}

func (r *RedisScalerState) GetCreationBackoffUntil(region string) (time.Time, bool) {
	ctx, cancel := r.ctx()
	defer cancel()
	val, err := r.rdb.Get(ctx, "scaler:createbackoff:"+region).Result()
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, val)
	if err != nil || time.Now().After(t) {
		return time.Time{}, false
	}
	return t, true
}

func (r *RedisScalerState) SetCreationBackoffUntil(region string, until time.Time) {
	ctx, cancel := r.ctx()
	defer cancel()
	r.rdb.Set(ctx, "scaler:createbackoff:"+region, until.Format(time.RFC3339), time.Until(until))
}

// --- In-memory fallback (for dev/combined mode) ---

// InMemoryScalerState provides the same interface backed by Go maps (current behavior).
type InMemoryScalerState struct {
	mu              sync.Mutex
	lastScaleUp     map[string]time.Time
	pending         map[string][]pendingLaunch
	draining        map[string]*drainState
	lastEvacuation  map[string]time.Time
	migrating       sync.Map
	inFlight        map[string]int
	sandboxCount    map[string]int
	createFailures  map[string]int
	createBackoff   map[string]time.Time
}

// NewInMemoryScalerState creates an in-memory state store.
func NewInMemoryScalerState() *InMemoryScalerState {
	return &InMemoryScalerState{
		lastScaleUp:    make(map[string]time.Time),
		pending:        make(map[string][]pendingLaunch),
		draining:       make(map[string]*drainState),
		lastEvacuation: make(map[string]time.Time),
		inFlight:       make(map[string]int),
		sandboxCount:   make(map[string]int),
		createFailures: make(map[string]int),
		createBackoff:  make(map[string]time.Time),
	}
}

func (m *InMemoryScalerState) GetLastScaleUp(region string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastScaleUp[region]
	return t, ok
}
func (m *InMemoryScalerState) SetLastScaleUp(region string, t time.Time, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastScaleUp[region] = t
}
func (m *InMemoryScalerState) GetPendingLaunches(region string) []pendingLaunch {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pendingLaunch(nil), m.pending[region]...)
}
func (m *InMemoryScalerState) AddPendingLaunch(region string, p pendingLaunch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[region] = append(m.pending[region], p)
}
func (m *InMemoryScalerState) RemovePendingLaunch(region, machineID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var remaining []pendingLaunch
	for _, p := range m.pending[region] {
		if p.MachineID != machineID {
			remaining = append(remaining, p)
		}
	}
	m.pending[region] = remaining
}
func (m *InMemoryScalerState) SetPendingLaunches(region string, launches []pendingLaunch) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[region] = launches
}
func (m *InMemoryScalerState) GetDraining(machineID string) (*drainState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ds, ok := m.draining[machineID]
	return ds, ok
}
func (m *InMemoryScalerState) SetDraining(machineID string, state *drainState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining[machineID] = state
}
func (m *InMemoryScalerState) RemoveDraining(machineID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.draining, machineID)
}
func (m *InMemoryScalerState) IsDraining(machineID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.draining[machineID]
	return ok
}
func (m *InMemoryScalerState) AllDraining() map[string]*drainState {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]*drainState, len(m.draining))
	for k, v := range m.draining {
		result[k] = v
	}
	return result
}
func (m *InMemoryScalerState) GetLastEvacuation(workerID string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.lastEvacuation[workerID]
	return t, ok
}
func (m *InMemoryScalerState) SetLastEvacuation(workerID string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEvacuation[workerID] = t
}
func (m *InMemoryScalerState) AcquireMigrationLock(sandboxID string) bool {
	_, loaded := m.migrating.LoadOrStore(sandboxID, struct{}{})
	return !loaded
}
func (m *InMemoryScalerState) ReleaseMigrationLock(sandboxID string) {
	m.migrating.Delete(sandboxID)
}
func (m *InMemoryScalerState) IncrInFlight(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight[workerID]++
}
func (m *InMemoryScalerState) DecrInFlight(workerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight[workerID]--
	if m.inFlight[workerID] <= 0 {
		delete(m.inFlight, workerID)
	}
}
func (m *InMemoryScalerState) GetInFlight(workerID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inFlight[workerID]
}
func (m *InMemoryScalerState) GetLastSandboxCount(region string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.sandboxCount[region]
	return n, ok
}
func (m *InMemoryScalerState) SetLastSandboxCount(region string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxCount[region] = count
}

// --- Creation failure backoff (in-memory) ---

func (m *InMemoryScalerState) IncrCreationFailures(region string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createFailures[region]++
	return m.createFailures[region]
}

func (m *InMemoryScalerState) ResetCreationFailures(region string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.createFailures, region)
	delete(m.createBackoff, region)
}

func (m *InMemoryScalerState) GetCreationBackoffUntil(region string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.createBackoff[region]
	if !ok || time.Now().After(t) {
		return time.Time{}, false
	}
	return t, true
}

func (m *InMemoryScalerState) SetCreationBackoffUntil(region string, until time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createBackoff[region] = until
}

// Ensure both implementations satisfy the interface.
var _ ScalerStateStore = (*RedisScalerState)(nil)
var _ ScalerStateStore = (*InMemoryScalerState)(nil)
