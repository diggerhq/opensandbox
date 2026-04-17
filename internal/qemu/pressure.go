package qemu

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// PressureLevel represents the current host resource pressure state.
type PressureLevel int32

const (
	PressureNormal    PressureLevel = iota // >10% RAM, >200GB disk
	PressureWarm                           // <10% RAM OR <200GB disk
	PressurePressure                       // <5% RAM OR <100GB disk
	PressureCritical                       // <3% RAM OR <50GB disk
	PressureEmergency                      // <1.3% RAM OR <20GB disk
)

func (l PressureLevel) String() string {
	switch l {
	case PressureNormal:
		return "normal"
	case PressureWarm:
		return "warm"
	case PressurePressure:
		return "pressure"
	case PressureCritical:
		return "critical"
	case PressureEmergency:
		return "emergency"
	}
	return "unknown"
}

// PressureThresholds defines the resource thresholds for each level.
// All values are in bytes.
type PressureThresholds struct {
	// Disk thresholds (available bytes)
	DiskWarmBytes      uint64 // < this = warm (default 200GB)
	DiskPressureBytes  uint64 // < this = pressure (default 100GB)
	DiskCriticalBytes  uint64 // < this = critical (default 50GB)
	DiskEmergencyBytes uint64 // < this = emergency (default 20GB)

	// RAM thresholds (percentage available)
	RAMWarmPct      float64 // < this = warm (default 10%)
	RAMPressurePct  float64 // < this = pressure (default 5%)
	RAMCriticalPct  float64 // < this = critical (default 3%)
	RAMEmergencyPct float64 // < this = emergency (default 1.3%)
}

// DefaultThresholds returns production thresholds.
func DefaultThresholds() PressureThresholds {
	gb := uint64(1024 * 1024 * 1024)
	return PressureThresholds{
		DiskWarmBytes:      200 * gb,
		DiskPressureBytes:  100 * gb,
		DiskCriticalBytes:  50 * gb,
		DiskEmergencyBytes: 20 * gb,
		RAMWarmPct:         10.0,
		RAMPressurePct:     5.0,
		RAMCriticalPct:     3.0,
		RAMEmergencyPct:    1.3,
	}
}

// PressureCallbacks are invoked when the pressure level changes.
type PressureCallbacks struct {
	// OnLevelChange is called when the pressure level transitions.
	OnLevelChange func(from, to PressureLevel)

	// OnMigrateIdle is called at Warm+ with sandbox IDs to migrate (longest idle first).
	OnMigrateIdle func(sandboxIDs []string)

	// OnHibernateIdle is called at Pressure+ to hibernate idle sandboxes.
	OnHibernateIdle func(sandboxIDs []string)

	// OnHibernateAll is called at Critical+ to hibernate everything.
	OnHibernateAll func()

	// OnDeregister is called at Emergency to remove worker from pool.
	OnDeregister func()
}

// PressureMonitor monitors host RAM and disk and triggers actions at thresholds.
type PressureMonitor struct {
	dataDir    string
	thresholds PressureThresholds
	callbacks  PressureCallbacks
	manager    *Manager
	interval   time.Duration

	level      atomic.Int32 // PressureLevel
	mu         sync.Mutex
	lastAction time.Time
	stopCh     chan struct{}
	doneCh     chan struct{}
}

// NewPressureMonitor creates a monitor that checks every 10 seconds.
func NewPressureMonitor(manager *Manager, dataDir string, thresholds PressureThresholds, callbacks PressureCallbacks) *PressureMonitor {
	return &PressureMonitor{
		dataDir:    dataDir,
		thresholds: thresholds,
		callbacks:  callbacks,
		manager:    manager,
		interval:   10 * time.Second,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Start begins monitoring.
func (pm *PressureMonitor) Start() {
	go pm.loop()
	log.Printf("pressure-monitor: started (interval=%v, data=%s)", pm.interval, pm.dataDir)
}

// Stop halts monitoring.
func (pm *PressureMonitor) Stop() {
	close(pm.stopCh)
	<-pm.doneCh
	log.Printf("pressure-monitor: stopped")
}

// Level returns the current pressure level.
func (pm *PressureMonitor) Level() PressureLevel {
	return PressureLevel(pm.level.Load())
}

func (pm *PressureMonitor) loop() {
	defer close(pm.doneCh)
	ticker := time.NewTicker(pm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			pm.check()
		case <-pm.stopCh:
			return
		}
	}
}

func (pm *PressureMonitor) check() {
	ramPct := sampleRAMPercent()
	diskAvail := sampleDiskAvail(pm.dataDir)

	newLevel := pm.classify(ramPct, diskAvail)
	oldLevel := PressureLevel(pm.level.Load())

	if newLevel != oldLevel {
		pm.level.Store(int32(newLevel))
		log.Printf("pressure-monitor: level changed %s → %s (RAM=%.1f%% free, disk=%dGB free)",
			oldLevel, newLevel, ramPct, diskAvail/(1024*1024*1024))

		if pm.callbacks.OnLevelChange != nil {
			pm.callbacks.OnLevelChange(oldLevel, newLevel)
		}
	}

	// Throttle actions: at most once per 30 seconds
	pm.mu.Lock()
	if time.Since(pm.lastAction) < 30*time.Second {
		pm.mu.Unlock()
		return
	}
	pm.mu.Unlock()

	pm.act(newLevel)
}

func (pm *PressureMonitor) classify(ramPct float64, diskAvail uint64) PressureLevel {
	t := pm.thresholds

	if ramPct < t.RAMEmergencyPct || diskAvail < t.DiskEmergencyBytes {
		return PressureEmergency
	}
	if ramPct < t.RAMCriticalPct || diskAvail < t.DiskCriticalBytes {
		return PressureCritical
	}
	if ramPct < t.RAMPressurePct || diskAvail < t.DiskPressureBytes {
		return PressurePressure
	}
	if ramPct < t.RAMWarmPct || diskAvail < t.DiskWarmBytes {
		return PressureWarm
	}
	return PressureNormal
}

func (pm *PressureMonitor) act(level PressureLevel) {
	if level == PressureNormal {
		return
	}

	pm.mu.Lock()
	pm.lastAction = time.Now()
	pm.mu.Unlock()

	ctx := context.Background()

	switch level {
	case PressureWarm:
		// Migrate/hibernate idle sandboxes (inactive > 2 min)
		idle := pm.idleSandboxes(2 * time.Minute)
		if len(idle) > 5 {
			idle = idle[:5] // max 5 per cycle
		}
		if len(idle) > 0 {
			log.Printf("pressure-monitor: warm — hibernating %d idle sandboxes", len(idle))
			if pm.callbacks.OnHibernateIdle != nil {
				pm.callbacks.OnHibernateIdle(idle)
			}
		}

	case PressurePressure:
		// Hibernate idle sandboxes more aggressively (inactive > 30s)
		idle := pm.idleSandboxes(30 * time.Second)
		if len(idle) > 10 {
			idle = idle[:10]
		}
		if len(idle) > 0 {
			log.Printf("pressure-monitor: pressure — hibernating %d sandboxes (idle >30s)", len(idle))
			if pm.callbacks.OnHibernateIdle != nil {
				pm.callbacks.OnHibernateIdle(idle)
			}
		}

	case PressureCritical:
		log.Printf("pressure-monitor: critical — hibernating all sandboxes")
		if pm.callbacks.OnHibernateAll != nil {
			pm.callbacks.OnHibernateAll()
		}

	case PressureEmergency:
		log.Printf("pressure-monitor: EMERGENCY — hibernating all + deregistering")
		if pm.callbacks.OnHibernateAll != nil {
			pm.callbacks.OnHibernateAll()
		}
		if pm.callbacks.OnDeregister != nil {
			pm.callbacks.OnDeregister()
		}
	}

	_ = ctx // used by future live migration calls
}

// idleSandboxes returns sandbox IDs that have been idle longer than threshold.
func (pm *PressureMonitor) idleSandboxes(threshold time.Duration) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sandboxes, err := pm.manager.List(ctx)
	if err != nil {
		return nil
	}

	cutoff := time.Now().Add(-threshold)
	var idle []string
	for _, sb := range sandboxes {
		// Use StartedAt as a proxy for last activity
		// TODO: track actual last-exec timestamp per sandbox
		if sb.StartedAt.Before(cutoff) {
			idle = append(idle, sb.ID)
		}
	}
	return idle
}

// sampleRAMPercent and sampleDiskAvail are in pressure_linux.go / pressure_other.go.
