package worker

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
)

// --- classifyState ---

func TestClassifyState(t *testing.T) {
	tests := []struct {
		memGB float64
		want  string
	}{
		{0.0, "idle"},
		{0.1, "idle"},
		{0.49, "idle"},
		{0.5, "light"},
		{1.0, "light"},
		{1.49, "light"},
		{1.5, "medium"},
		{3.0, "medium"},
		{3.99, "medium"},
		{4.0, "heavy"},
		{8.0, "heavy"},
		{16.0, "heavy"},
	}
	for _, tt := range tests {
		got := classifyState(tt.memGB)
		if got != tt.want {
			t.Errorf("classifyState(%.2f) = %q, want %q", tt.memGB, got, tt.want)
		}
	}
}

// --- applyWeightedFairShare ---

// helper to create a PressureMonitor with pre-populated usages for testing fair-share.
// Since applyWeightedFairShare reads from pm.usages and writes CgroupCapGB (but also
// calls firecracker.UpdateCgroupMemoryLimit which will fail on non-Linux), we capture
// the caps from pm.usages after the call. The cgroup write errors are non-fatal (logged).
func newTestMonitor(workerRAMGB float64, usages map[string]*SandboxUsage) *PressureMonitor {
	return &PressureMonitor{
		workerRAMBytes: int64(workerRAMGB * float64(gbBytes)),
		usages:         usages,
		stop:           make(chan struct{}),
	}
}

func sandboxInfos(ids ...string) []SandboxInfo {
	var infos []SandboxInfo
	for _, id := range ids {
		infos = append(infos, SandboxInfo{ID: id, CpuCount: 1, MemoryMB: 8192})
	}
	return infos
}

func TestFairShare_SingleVM(t *testing.T) {
	usages := map[string]*SandboxUsage{
		"a": {SandboxID: "a", MemoryGB: 1.0, CpuCount: 1},
	}
	pm := newTestMonitor(64, usages)
	pm.applyWeightedFairShare(sandboxInfos("a"))

	// Single VM on 64GB worker: base=2.0, budget=60.8 (95%), surplus=58.8
	// want=6.0, gets all surplus → cap = min(2.0+58.8, 8.0) = 8.0
	cap := usages["a"].CgroupCapGB
	if cap != vmCeilingGB {
		t.Errorf("single VM cap = %.2f, want %.2f (ceiling)", cap, vmCeilingGB)
	}
}

func TestFairShare_TwoVMs_IdleAndHeavy(t *testing.T) {
	usages := map[string]*SandboxUsage{
		"idle":  {SandboxID: "idle", MemoryGB: 0.1, CpuCount: 1},
		"heavy": {SandboxID: "heavy", MemoryGB: 5.0, CpuCount: 2},
	}
	pm := newTestMonitor(64, usages)
	pm.applyWeightedFairShare(sandboxInfos("idle", "heavy"))

	// budget = 64 * 0.95 = 60.8 GB
	// idle: base = max(0.2, 0.5) = 0.5, want = 8.0 - 0.5 = 7.5
	// heavy: base = min(10.0, 8.0) = 8.0, want = 0.0
	// totalBase = 8.5, surplus = 52.3
	// idle gets: 0.5 + 52.3*(7.5/7.5) = 0.5 + 52.3 → clamped to 8.0
	// heavy gets: 8.0 + 0 = 8.0
	if usages["heavy"].CgroupCapGB != vmCeilingGB {
		t.Errorf("heavy cap = %.2f, want %.2f", usages["heavy"].CgroupCapGB, vmCeilingGB)
	}
	if usages["idle"].CgroupCapGB != vmCeilingGB {
		t.Errorf("idle cap = %.2f, want %.2f (plenty of headroom)", usages["idle"].CgroupCapGB, vmCeilingGB)
	}
}

func TestFairShare_ManyVMs_EvenLoad(t *testing.T) {
	// 50 VMs each using 1GB on a 64GB worker
	usages := make(map[string]*SandboxUsage)
	var ids []string
	for i := 0; i < 50; i++ {
		id := string(rune('A'+i/26)) + string(rune('a'+i%26))
		ids = append(ids, id)
		usages[id] = &SandboxUsage{SandboxID: id, MemoryGB: 1.0, CpuCount: 1}
	}
	pm := newTestMonitor(64, usages)
	pm.applyWeightedFairShare(sandboxInfos(ids...))

	// budget = 60.8 GB, each base = 2.0, totalBase = 100 > budget
	// Over budget: each cap = 2.0 * (60.8/100) = 1.216 GB
	for _, id := range ids {
		cap := usages[id].CgroupCapGB
		if cap < 1.0 || cap > 1.5 {
			t.Errorf("VM %s cap = %.3f, expected ~1.216 (proportional scale-down)", id, cap)
		}
	}
}

func TestFairShare_OverBudget_MinCapFloor(t *testing.T) {
	// 200 VMs each using 0.5GB on 16GB worker — extreme pressure
	usages := make(map[string]*SandboxUsage)
	var ids []string
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("vm%03d", i)
		ids = append(ids, id)
		usages[id] = &SandboxUsage{SandboxID: id, MemoryGB: 0.5, CpuCount: 1}
	}
	pm := newTestMonitor(16, usages)
	pm.applyWeightedFairShare(sandboxInfos(ids...))

	// budget = 15.2 GB, each base = 1.0, totalBase = 200 > budget
	// Proportional: 1.0 * (15.2/200) = 0.076 → clamped to minCapGB (0.5)
	for _, id := range ids {
		cap := usages[id].CgroupCapGB
		if cap < minCapGB {
			t.Errorf("VM %s cap = %.3f, below min cap %.1f", id, cap, minCapGB)
		}
	}
}

func TestFairShare_NoSandboxes(t *testing.T) {
	pm := newTestMonitor(64, make(map[string]*SandboxUsage))
	// Should not panic
	pm.applyWeightedFairShare(nil)
	pm.applyWeightedFairShare([]SandboxInfo{})
}

func TestFairShare_SurplusGoesToHungryVMs(t *testing.T) {
	// 3 idle VMs + 1 heavy VM on 64GB — heavy VM should get most surplus
	usages := map[string]*SandboxUsage{
		"i1":    {SandboxID: "i1", MemoryGB: 0.1, CpuCount: 1},
		"i2":    {SandboxID: "i2", MemoryGB: 0.1, CpuCount: 1},
		"i3":    {SandboxID: "i3", MemoryGB: 0.1, CpuCount: 1},
		"heavy": {SandboxID: "heavy", MemoryGB: 3.5, CpuCount: 2},
	}
	pm := newTestMonitor(64, usages)
	pm.applyWeightedFairShare(sandboxInfos("i1", "i2", "i3", "heavy"))

	// All get ceiling since there's tons of headroom on 64GB with 4 VMs
	// But verify heavy gets at least its base (7.0)
	if usages["heavy"].CgroupCapGB < 7.0 {
		t.Errorf("heavy cap = %.2f, expected >= 7.0", usages["heavy"].CgroupCapGB)
	}
}

// --- FinalizeUsage ---

func TestFinalizeUsage(t *testing.T) {
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"sb1": {
			SandboxID:        "sb1",
			AccumVCPUSeconds: 120.0,
			AccumGBSeconds:   60.0,
		},
	})

	vcpu, gb := pm.FinalizeUsage("sb1")
	if vcpu != 120.0 || gb != 60.0 {
		t.Errorf("FinalizeUsage = (%.1f, %.1f), want (120.0, 60.0)", vcpu, gb)
	}

	// Should be removed after finalize
	if pm.GetUsage("sb1") != nil {
		t.Error("sandbox should be removed after FinalizeUsage")
	}
}

func TestFinalizeUsage_NotFound(t *testing.T) {
	pm := newTestMonitor(64, make(map[string]*SandboxUsage))
	vcpu, gb := pm.FinalizeUsage("nonexistent")
	if vcpu != 0 || gb != 0 {
		t.Errorf("FinalizeUsage for missing sandbox = (%.1f, %.1f), want (0, 0)", vcpu, gb)
	}
}

// --- GetUsage ---

func TestGetUsage_ReturnsCopy(t *testing.T) {
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"sb1": {SandboxID: "sb1", MemoryGB: 2.5, State: "medium"},
	})

	u := pm.GetUsage("sb1")
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.MemoryGB != 2.5 || u.State != "medium" {
		t.Errorf("got MemoryGB=%.1f State=%s, want 2.5 medium", u.MemoryGB, u.State)
	}

	// Mutating the copy should not affect the original
	u.MemoryGB = 999
	orig := pm.GetUsage("sb1")
	if orig.MemoryGB == 999 {
		t.Error("GetUsage returned a reference, not a copy")
	}
}

func TestGetUsage_NotFound(t *testing.T) {
	pm := newTestMonitor(64, make(map[string]*SandboxUsage))
	if pm.GetUsage("nope") != nil {
		t.Error("expected nil for missing sandbox")
	}
}

// --- TotalMemoryUsed ---

func TestTotalMemoryUsed(t *testing.T) {
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"a": {MemoryBytes: 1 * gbBytes},
		"b": {MemoryBytes: 2 * gbBytes},
		"c": {MemoryBytes: 500 * 1024 * 1024}, // 500MB
	})

	total := pm.TotalMemoryUsed()
	expected := int64(1*gbBytes + 2*gbBytes + 500*1024*1024)
	if total != expected {
		t.Errorf("TotalMemoryUsed = %d, want %d", total, expected)
	}
}

func TestTotalMemoryUsed_Empty(t *testing.T) {
	pm := newTestMonitor(64, make(map[string]*SandboxUsage))
	if pm.TotalMemoryUsed() != 0 {
		t.Error("expected 0 for empty usages")
	}
}

// --- MemoryUsagePct ---

func TestMemoryUsagePct(t *testing.T) {
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"a": {MemoryBytes: 32 * gbBytes}, // 32GB of 64GB = 50%
	})

	pct := pm.MemoryUsagePct()
	if math.Abs(pct-50.0) > 0.01 {
		t.Errorf("MemoryUsagePct = %.2f, want 50.0", pct)
	}
}

// --- SandboxesOverPressure ---

func TestSandboxesOverPressure_BelowThreshold(t *testing.T) {
	// 10GB used on 64GB = 15.6% — well below 55%
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"a": {SandboxID: "a", MemoryBytes: 10 * gbBytes, State: "idle"},
	})

	result := pm.SandboxesOverPressure()
	if result != nil {
		t.Errorf("expected nil below threshold, got %v", result)
	}
}

func TestSandboxesOverPressure_AboveThreshold(t *testing.T) {
	// 40GB used on 64GB = 62.5% — above 55% threshold
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"idle1": {SandboxID: "idle1", MemoryBytes: 10 * gbBytes, State: "idle"},
		"light": {SandboxID: "light", MemoryBytes: 10 * gbBytes, State: "light"},
		"heavy": {SandboxID: "heavy", MemoryBytes: 20 * gbBytes, State: "heavy"},
	})

	result := pm.SandboxesOverPressure()
	sort.Strings(result)

	if len(result) != 2 {
		t.Fatalf("expected 2 candidates, got %d: %v", len(result), result)
	}
	if result[0] != "idle1" || result[1] != "light" {
		t.Errorf("expected [idle1, light], got %v", result)
	}
}

func TestSandboxesOverPressure_NoIdleVMs(t *testing.T) {
	// Above threshold but only heavy VMs — nothing to hibernate
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"h1": {SandboxID: "h1", MemoryBytes: 20 * gbBytes, State: "heavy"},
		"h2": {SandboxID: "h2", MemoryBytes: 20 * gbBytes, State: "heavy"},
	})

	result := pm.SandboxesOverPressure()
	if len(result) != 0 {
		t.Errorf("expected no candidates (all heavy), got %v", result)
	}
}

// --- flushBilling ---

func TestFlushBilling(t *testing.T) {
	var flushed []UsageFlush
	pm := &PressureMonitor{
		workerRAMBytes: 64 * gbBytes,
		usages: map[string]*SandboxUsage{
			"sb1": {SandboxID: "sb1", AccumVCPUSeconds: 100, AccumGBSeconds: 50},
			"sb2": {SandboxID: "sb2", AccumVCPUSeconds: 0, AccumGBSeconds: 0}, // no usage, should be skipped
			"sb3": {SandboxID: "sb3", AccumVCPUSeconds: 200, AccumGBSeconds: 75},
		},
		onFlush: func(f []UsageFlush) { flushed = f },
		stop:    make(chan struct{}),
	}

	pm.flushBilling()

	if len(flushed) != 2 {
		t.Fatalf("expected 2 flushes (skip zero-usage), got %d", len(flushed))
	}

	sort.Slice(flushed, func(i, j int) bool { return flushed[i].SandboxID < flushed[j].SandboxID })
	if flushed[0].SandboxID != "sb1" || flushed[0].VCPUSeconds != 100 || flushed[0].GBSeconds != 50 {
		t.Errorf("flush[0] = %+v, unexpected", flushed[0])
	}
	if flushed[1].SandboxID != "sb3" || flushed[1].VCPUSeconds != 200 || flushed[1].GBSeconds != 75 {
		t.Errorf("flush[1] = %+v, unexpected", flushed[1])
	}
}

func TestFlushBilling_NilCallback(t *testing.T) {
	pm := &PressureMonitor{
		usages: map[string]*SandboxUsage{
			"sb1": {SandboxID: "sb1", AccumVCPUSeconds: 100, AccumGBSeconds: 50},
		},
		onFlush: nil,
		stop:    make(chan struct{}),
	}
	// Should not panic
	pm.flushBilling()
}

// --- Concurrency ---

func TestConcurrentAccess(t *testing.T) {
	pm := newTestMonitor(64, map[string]*SandboxUsage{
		"sb1": {SandboxID: "sb1", MemoryBytes: 1 * gbBytes, MemoryGB: 1.0, State: "light", AccumVCPUSeconds: 10},
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_ = pm.GetUsage("sb1")
		}()
		go func() {
			defer wg.Done()
			_ = pm.TotalMemoryUsed()
		}()
		go func() {
			defer wg.Done()
			_ = pm.SandboxesOverPressure()
		}()
	}
	wg.Wait()
}
