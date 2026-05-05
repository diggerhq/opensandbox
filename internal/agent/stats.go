package agent

import (
	"bufio"
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// Ping responds with the agent version and uptime.
func (s *Server) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	return &pb.PingResponse{
		Version:       s.version,
		UptimeSeconds: int64(time.Since(s.startTime).Seconds()),
	}, nil
}

// Shutdown syncs disks and signals for clean shutdown.
func (s *Server) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*pb.ShutdownResponse, error) {
	// Sync all filesystems
	_ = syncFS()

	// Clean shutdown — the init process will handle the rest
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()
	return &pb.ShutdownResponse{}, nil
}

// Stats reads live resource usage from /proc.
func (s *Server) Stats(ctx context.Context, req *pb.StatsRequest) (*pb.StatsResponse, error) {
	resp := &pb.StatsResponse{}

	// Memory from /proc/meminfo. Note we use MemAvailable, not MemFree —
	// MemAvailable is the kernel's own working-set estimate that subtracts
	// reclaimable page cache + slab. So MemTotal-MemAvailable already
	// excludes file-cache pressure (e.g. a `dd` workload won't make this
	// climb to 100% even though page cache fills RAM). The autoscaler reads
	// this value via mem_pct; treating it as a working-set proxy is correct.
	// It is also an upper bound on resident anon memory, so the worker's
	// scale-down OOM floor uses MemUsage * 1.05 as a safe lower bound.
	memTotal, memAvail := readMemInfo()
	resp.MemLimit = memTotal
	resp.MemUsage = memTotal - memAvail

	// CPU from /proc/stat (instantaneous — two samples)
	resp.CpuPercent = readCPUPercent()

	// PIDs from /proc
	resp.Pids = int32(countPIDs())

	// Network from /proc/net/dev
	resp.NetInput, resp.NetOutput = readNetStats()

	return resp, nil
}

// readMemInfo parses /proc/meminfo for MemTotal and MemAvailable.
func readMemInfo() (total, available uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			total = parseMemLine(line) * 1024 // kB → bytes
		} else if strings.HasPrefix(line, "MemAvailable:") {
			available = parseMemLine(line) * 1024
		}
	}
	return total, available
}

// parseMemLine extracts the numeric value from "MemTotal:   123456 kB".
func parseMemLine(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// readCPUPercent takes two snapshots of /proc/stat 100ms apart and
// computes the CPU usage percentage.
func readCPUPercent() float64 {
	idle1, total1 := readCPUSample()
	time.Sleep(100 * time.Millisecond)
	idle2, total2 := readCPUSample()

	totalDelta := float64(total2 - total1)
	idleDelta := float64(idle2 - idle1)

	if totalDelta == 0 {
		return 0
	}
	return (1.0 - idleDelta/totalDelta) * 100.0
}

// readCPUSample reads the aggregate CPU line from /proc/stat.
func readCPUSample() (idle, total uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	// First line: "cpu  user nice system idle iowait irq softirq steal ..."
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}

	for i := 1; i < len(fields); i++ {
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		total += v
		if i == 4 { // idle is the 4th value after "cpu"
			idle = v
		}
	}
	return idle, total
}

// countPIDs counts /proc/[0-9]* directories.
func countPIDs() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) > 0 && e.Name()[0] >= '0' && e.Name()[0] <= '9' {
			count++
		}
	}
	return count
}

// readNetStats sums rx/tx bytes across all interfaces (excluding lo).
func readNetStats() (rxBytes, txBytes uint64) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // skip headers
		}
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 10 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64)
		tx, _ := strconv.ParseUint(fields[8], 10, 64)
		rxBytes += rx
		txBytes += tx
	}
	return rxBytes, txBytes
}

// SyncFS flushes all filesystem buffers without exiting the agent.
func (s *Server) SyncFS(ctx context.Context, req *pb.SyncFSRequest) (*pb.SyncFSResponse, error) {
	if err := syncFS(); err != nil {
		return nil, err
	}
	return &pb.SyncFSResponse{}, nil
}

// PrepareHibernate does all the pre-hibernate work synchronously:
// sync filesystems, flush block device buffers, and reset the virtio-serial
// listener so a clean Accept happens on wake/fork. Returns only after all
// work completes — the host does not need to sleep after this call.
func (s *Server) PrepareHibernate(ctx context.Context, req *pb.PrepareHibernateRequest) (*pb.PrepareHibernateResponse, error) {
	syscall.Sync()
	flushBlockDevices("/dev/vda", "/dev/vdb")
	syscall.Sync()
	if s.OnPrepareHibernate != nil {
		s.OnPrepareHibernate()
	}
	return &pb.PrepareHibernateResponse{}, nil
}

// flushBlockDevices issues BLKFLSBUF on each device (equivalent to `blockdev --flushbufs`).
// Ignores errors — not all devices may be present (e.g., /dev/vdb).
func flushBlockDevices(paths ...string) {
	const BLKFLSBUF = 0x1261
	for _, path := range paths {
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(BLKFLSBUF), 0)
		f.Close()
	}
}

// syncFS calls sync(2) to flush all filesystem buffers.
// This syncs ALL mounted filesystems (rootfs + workspace), ensuring dirty pages
// are written to their backing ext4 images before snapshot/checkpoint.
func syncFS() error {
	syscall.Sync()
	return nil
}
