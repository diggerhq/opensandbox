package worker

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SystemStats returns current CPU, memory, and disk usage percentages.
// On Linux, reads from /proc. On other platforms, returns 0.0 gracefully.
// Disk usage uses syscall.Statfs which works on both Linux and macOS.
func SystemStats() (cpuPct, memPct, diskPct float64) {
	if runtime.GOOS == "linux" {
		memPct = linuxMemoryPercent()
		cpuPct = linuxCPUPercent()
	}
	diskPct = diskPercent()
	return cpuPct, memPct, diskPct
}

func linuxMemoryPercent() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0.0
	}
	defer f.Close()

	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMeminfoKB(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMeminfoKB(line)
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}
	if memTotal == 0 {
		return 0.0
	}
	return float64(memTotal-memAvailable) / float64(memTotal) * 100.0
}

func parseMeminfoKB(line string) uint64 {
	// Format: "MemTotal:       16384000 kB"
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return val
}

// CPU delta tracking for accurate current-load measurement.
// Without delta tracking, /proc/stat gives cumulative averages since boot.
var (
	cpuPrevTotal uint64
	cpuPrevIdle  uint64
	cpuMu        sync.Mutex
)

func linuxCPUPercent() float64 {
	cpuMu.Lock()
	defer cpuMu.Unlock()

	total1, idle1 := readProcStat()
	if total1 == 0 {
		return 0.0
	}

	if cpuPrevTotal > 0 {
		// Delta from previous call (typically ~10s ago from heartbeat interval)
		dTotal := total1 - cpuPrevTotal
		dIdle := idle1 - cpuPrevIdle
		cpuPrevTotal = total1
		cpuPrevIdle = idle1
		if dTotal == 0 {
			return 0.0
		}
		return float64(dTotal-dIdle) / float64(dTotal) * 100.0
	}

	// First call: take two samples 500ms apart to get an initial reading
	time.Sleep(500 * time.Millisecond)
	total2, idle2 := readProcStat()
	cpuPrevTotal = total2
	cpuPrevIdle = idle2

	dTotal := total2 - total1
	dIdle := idle2 - idle1
	if dTotal == 0 {
		return 0.0
	}
	return float64(dTotal-dIdle) / float64(dTotal) * 100.0
}

// readProcStat reads the aggregate CPU line from /proc/stat and returns total and idle jiffies.
func readProcStat() (total, idle uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0
	}
	line := scanner.Text() // "cpu  user nice system idle iowait irq softirq steal"
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}

	for i := 1; i < len(fields); i++ {
		val, _ := strconv.ParseUint(fields[i], 10, 64)
		total += val
		if i == 4 { // idle is the 4th value (index 4 after "cpu")
			idle = val
		}
	}
	return total, idle
}

// diskPercent returns the disk usage percentage for the root filesystem.
// Uses syscall.Statfs which works on both Linux and macOS.
func diskPercent() float64 {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0.0
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return 0.0
	}
	return float64(total-free) / float64(total) * 100.0
}

// DiskBytes returns total / used / available bytes for the filesystem
// containing path. Used for the per-worker disk metrics.
// Available uses Bavail (free to non-privileged users) rather than Bfree —
// matches what `df` reports and is the number that actually constrains us.
func DiskBytes(path string) (total, used, available uint64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0, err
	}
	total = stat.Blocks * uint64(stat.Bsize)
	available = stat.Bavail * uint64(stat.Bsize)
	if free := stat.Bfree * uint64(stat.Bsize); total >= free {
		used = total - free
	}
	return total, used, available, nil
}

// MemoryBytes returns total and available bytes on Linux from /proc/meminfo.
// Returns (0, 0, nil) on non-Linux platforms so callers can skip the metric
// without treating it as an error.
func MemoryBytes() (total, available uint64, err error) {
	if runtime.GOOS != "linux" {
		return 0, 0, nil
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMeminfoKB(line) * 1024
		case strings.HasPrefix(line, "MemAvailable:"):
			available = parseMeminfoKB(line) * 1024
		}
		if total > 0 && available > 0 {
			break
		}
	}
	return total, available, nil
}

// CPUPressure reports stall percentages for the host CPU across three
// rolling windows (avg10, avg60, avg300).
//
// On modern Linux kernels (4.20+) with CONFIG_PSI enabled, source="psi" and
// values come from /proc/pressure/cpu's "some" line — percent of wall time
// during which at least one task was stalled on CPU. This is the right CPU
// pressure signal: it captures CPU saturation independent of utilization
// (a worker at 95% CPU with 0% PSI is fine; one at 60% with rising PSI is
// already in trouble).
//
// On older kernels or non-Linux dev hosts the fallback is source="loadavg":
// /proc/loadavg's 1/5/15-minute values divided by nproc and scaled to a
// percent. Different semantics — loadavg measures runqueue depth, not stall
// time — but it's the same 0..100+ scale and dashboards can flag the source
// label to interpret accordingly.
type CPUPressure struct {
	Avg10, Avg60, Avg300 float64
	Source               string // "psi" or "loadavg"
}

func ReadCPUPressure() CPUPressure {
	if runtime.GOOS == "linux" {
		if p, ok := readPSI("/proc/pressure/cpu"); ok {
			p.Source = "psi"
			return p
		}
	}
	return readLoadavgFallback()
}

// readPSI parses /proc/pressure/cpu format:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=...
//	full avg10=...
//
// We use the "some" line — "full" doesn't apply to CPU pressure anyway
// (it's always 0 for CPU, only meaningful for memory/io).
func readPSI(path string) (CPUPressure, bool) {
	f, err := os.Open(path)
	if err != nil {
		return CPUPressure{}, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		var p CPUPressure
		for _, field := range strings.Fields(line)[1:] {
			kv := strings.SplitN(field, "=", 2)
			if len(kv) != 2 {
				continue
			}
			v, err := strconv.ParseFloat(kv[1], 64)
			if err != nil {
				continue
			}
			switch kv[0] {
			case "avg10":
				p.Avg10 = v
			case "avg60":
				p.Avg60 = v
			case "avg300":
				p.Avg300 = v
			}
		}
		return p, true
	}
	return CPUPressure{}, false
}

// readLoadavgFallback reads /proc/loadavg (Linux) or returns zero on macOS.
// Scales by 100/nproc so values are roughly comparable to PSI percent
// (loadavg == nproc → ~100%).
func readLoadavgFallback() CPUPressure {
	p := CPUPressure{Source: "loadavg"}
	if runtime.GOOS != "linux" {
		return p
	}
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return p
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return p
	}
	nproc := float64(runtime.NumCPU())
	if nproc < 1 {
		nproc = 1
	}
	parse := func(s string) float64 {
		v, _ := strconv.ParseFloat(s, 64)
		return v / nproc * 100.0
	}
	// /proc/loadavg windows: 1min, 5min, 15min. Map onto avg10/avg60/avg300
	// purely so the same label set works — the windows aren't equivalent but
	// the source="loadavg" label tells dashboards not to mix them with PSI.
	p.Avg10 = parse(fields[0])
	p.Avg60 = parse(fields[1])
	p.Avg300 = parse(fields[2])
	return p
}
