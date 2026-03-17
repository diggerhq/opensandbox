// Package qemu implements sandbox.Manager using QEMU q35 VMs with KVM acceleration.
// Each sandbox is a full VM with virtio devices, communicating with the host
// via gRPC over AF_VSOCK (kernel vhost-vsock).
package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// Compile-time check that Manager implements sandbox.Manager.
var _ sandbox.Manager = (*Manager)(nil)

// ErrNotImplemented is returned for features not yet ported to the QEMU backend.
var ErrNotImplemented = fmt.Errorf("not implemented in QEMU backend")

// VMInstance holds the state of a running QEMU VM.
type VMInstance struct {
	ID        string
	Template  string
	Status    types.SandboxStatus
	StartedAt time.Time
	EndAt     time.Time
	CpuCount  int
	MemoryMB  int
	HostPort  int
	GuestPort int

	// VM internals
	pid         int
	cmd         *exec.Cmd
	network     *NetworkConfig
	sandboxDir  string
	agent       *AgentClient
	qmpSockPath   string
	agentSockPath string
	qmp           *QMPClient
	guestMAC      string
	guestCID      uint32
	bootArgs      string
	restoring     chan struct{}
	dimmCount     int // number of hotplugged DIMMs (for unique IDs)
}

// SandboxMeta is persisted to sandbox-meta.json for recovery after hard kills.
type SandboxMeta struct {
	SandboxID string `json:"sandboxId"`
	Template  string `json:"template"`
	CpuCount  int    `json:"cpuCount"`
	MemoryMB  int    `json:"memoryMB"`
	GuestPort int    `json:"guestPort"`
}

// Config holds configuration for the QEMU Manager.
type Config struct {
	DataDir         string // base data directory (e.g., /data)
	KernelPath      string // path to vmlinux kernel
	ImagesDir       string // path to base rootfs images
	QEMUBin         string // path to qemu-system-x86_64 binary
	DefaultMemoryMB int
	DefaultCPUs     int
	DefaultDiskMB   int
	DefaultPort     int
}

// Manager implements sandbox.Manager using QEMU VMs.
type Manager struct {
	cfg     Config
	subnets *SubnetAllocator

	mu       sync.RWMutex
	vms      map[string]*VMInstance
	nextCID  uint32
	uploadWg sync.WaitGroup

	// Golden snapshot for fast VM creation
	goldenDir     string // path to golden snapshot dir (empty = not available)
	goldenCID     uint32 // CID used when the golden snapshot was created
	goldenGuestIP string // guest IP baked into the golden snapshot
	goldenHostIP  string // host IP of the golden subnet (for temp addr on TAP)

	// Metadata service callbacks (set via SetMetadataCallbacks)
	onSandboxReady   func(sandboxID, guestIP, template string, startedAt time.Time)
	onSandboxDestroy func(sandboxID string)
}

// NewManager creates a new QEMU-backed sandbox manager.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("DataDir is required")
	}
	if cfg.KernelPath == "" {
		cfg.KernelPath = filepath.Join(cfg.DataDir, "firecracker", "vmlinux-docker-5.10.bin")
	}
	if cfg.ImagesDir == "" {
		cfg.ImagesDir = filepath.Join(cfg.DataDir, "firecracker", "images")
	}
	if cfg.QEMUBin == "" {
		cfg.QEMUBin = "qemu-system-x86_64"
	}
	if cfg.DefaultMemoryMB == 0 {
		cfg.DefaultMemoryMB = 512
	}
	if cfg.DefaultCPUs == 0 {
		cfg.DefaultCPUs = 1
	}
	if cfg.DefaultDiskMB == 0 {
		cfg.DefaultDiskMB = 20480
	}
	if cfg.DefaultPort == 0 {
		cfg.DefaultPort = 80
	}

	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel not found at %s: %w", cfg.KernelPath, err)
	}
	if _, err := exec.LookPath(cfg.QEMUBin); err != nil {
		return nil, fmt.Errorf("QEMU binary not found: %w", err)
	}

	if err := EnableForwarding(); err != nil {
		log.Printf("qemu: warning: could not enable IP forwarding: %v", err)
	}

	return &Manager{
		cfg:     cfg,
		subnets: NewSubnetAllocator(),
		vms:     make(map[string]*VMInstance),
		nextCID: 3,
	}, nil
}

// SetMetadataCallbacks registers callbacks that are invoked when sandboxes
// become ready or are destroyed. Used by the metadata server to track
// guestIP → sandboxID mappings.
func (m *Manager) SetMetadataCallbacks(
	onReady func(sandboxID, guestIP, template string, startedAt time.Time),
	onDestroy func(sandboxID string),
) {
	m.onSandboxReady = onReady
	m.onSandboxDestroy = onDestroy
}

// PrepareGoldenSnapshot boots a temporary VM, waits for the agent, then
// hibernates it to create a reusable snapshot. Subsequent Create() calls
// restore from this snapshot instead of cold-booting, cutting start time
// from ~10s to ~1-2s.
func (m *Manager) PrepareGoldenSnapshot() error {
	goldenDir := filepath.Join(m.cfg.DataDir, "golden")
	memFile := filepath.Join(goldenDir, "mem")
	rootfsFile := filepath.Join(goldenDir, "rootfs.qcow2")

	// If golden snapshot already exists, just use it
	if (fileExists(memFile) || fileExists(memFile+".zst")) && (fileExists(rootfsFile) || fileExists(filepath.Join(goldenDir, "rootfs.ext4"))) {
		m.goldenDir = goldenDir
		// Read saved golden CID + guest IP
		if cidBytes, err := os.ReadFile(filepath.Join(goldenDir, "cid")); err == nil {
			fmt.Sscanf(string(cidBytes), "%d", &m.goldenCID)
		}
		if ipBytes, err := os.ReadFile(filepath.Join(goldenDir, "guest_ip")); err == nil {
			m.goldenGuestIP = string(ipBytes)
		}
		if ipBytes, err := os.ReadFile(filepath.Join(goldenDir, "host_ip")); err == nil {
			m.goldenHostIP = string(ipBytes)
		}
		log.Printf("qemu: golden snapshot already exists at %s (CID=%d, guestIP=%s)", goldenDir, m.goldenCID, m.goldenGuestIP)
		return nil
	}

	log.Printf("qemu: preparing golden snapshot...")
	t0 := time.Now()

	if err := os.MkdirAll(goldenDir, 0755); err != nil {
		return fmt.Errorf("mkdir golden dir: %w", err)
	}

	// Prepare rootfs from default template
	baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, "default")
	if err != nil {
		return fmt.Errorf("resolve base image for golden: %w", err)
	}
	if err := PrepareRootfs(baseImage, rootfsFile); err != nil {
		return fmt.Errorf("prepare golden rootfs: %w", err)
	}

	// Create workspace as qcow2 — must match DefaultDiskMB so the virtio-blk
	// device geometry in the golden migration state matches sandbox workspaces.
	workspaceFile := filepath.Join(goldenDir, "workspace.qcow2")
	if err := CreateWorkspace(workspaceFile, m.cfg.DefaultDiskMB); err != nil {
		return fmt.Errorf("create golden workspace: %w", err)
	}

	// Allocate a temporary network for golden boot
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		return fmt.Errorf("allocate golden subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return fmt.Errorf("create golden TAP: %w", err)
	}
	defer func() {
		RemoveDNAT(netCfg)
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
	}()

	goldenCID := m.allocateCID() // temporary CID for golden VM boot
	goldenMAC := "AA:CE:00:00:FF:FF"
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 "+
			"root=/dev/vda rw "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	qmpSockPath := filepath.Join(goldenDir, "qmp.sock")
	agentSockPath := filepath.Join(goldenDir, "agent.sock")
	os.Remove(qmpSockPath)
	os.Remove(agentSockPath)

	logPath := filepath.Join(goldenDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create golden log: %w", err)
	}

	args := m.buildQEMUArgs(m.cfg.DefaultCPUs, m.cfg.DefaultMemoryMB,
		rootfsFile, workspaceFile, netCfg.TAPName, goldenMAC, agentSockPath, qmpSockPath, bootArgs)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start golden qemu: %w", err)
	}
	logFile.Close()

	// Connect QMP
	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden QMP connect: %w", err)
	}

	// Wait for agent via virtio-serial Unix socket
	agentClient, err := m.waitForAgentSocket(context.Background(), agentSockPath, 30*time.Second)
	if err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden agent not ready: %w", err)
	}
	log.Printf("qemu: golden VM booted, agent ready (%dms)", time.Since(t0).Milliseconds())

	// Unmount /workspace and sync before snapshot — the golden migration state
	// includes virtio-blk device state (ring buffers, pending I/O). If /workspace
	// is mounted when we snapshot, those stale I/O ops will corrupt any fresh
	// workspace.qcow2 that createFromGolden boots with.
	umountCtx, umountCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, umountErr := agentClient.Exec(umountCtx, &pb.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "umount /workspace 2>/dev/null; sync"},
	})
	umountCancel()
	if umountErr != nil {
		log.Printf("qemu: golden: umount /workspace failed (non-fatal): %v", umountErr)
	} else {
		log.Printf("qemu: golden: /workspace unmounted and synced")
	}

	// Close agent connection before migration. Use a timeout because gRPC's
	// graceful close over vsock can hang if vhost-vsock doesn't drain cleanly.
	closeDone := make(chan struct{})
	go func() {
		agentClient.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		log.Printf("qemu: golden: agent close timed out, proceeding anyway")
	}
	time.Sleep(500 * time.Millisecond)

	// QMP stop + migrate
	log.Printf("qemu: golden: sending QMP stop...")
	if err := qmpClient.Stop(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden QMP stop: %w", err)
	}
	log.Printf("qemu: golden: VM stopped, starting migration...")

	migrateURI := fmt.Sprintf("exec:cat > %s", memFile)
	if err := qmpClient.Migrate(migrateURI); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden QMP migrate: %w", err)
	}
	if err := qmpClient.WaitMigration(5 * time.Minute); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden migration wait: %w", err)
	}

	_ = qmpClient.Quit()
	qmpClient.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cmd.Process.Kill()
		<-done
	}

	// Clean up temp files
	os.Remove(workspaceFile)
	os.Remove(qmpSockPath)

	// Compress golden mem with zstd — on EBS volumes, reading less data from disk
	// is faster than raw I/O despite the CPU cost of decompression.
	zstCmd := exec.Command("zstd", "-3", "--rm", memFile, "-o", memFile+".zst")
	if out, err := zstCmd.CombinedOutput(); err != nil {
		log.Printf("qemu: golden zstd compress failed (will use raw): %v (%s)", err, string(out))
	} else {
		log.Printf("qemu: golden mem compressed with zstd")
	}

	m.goldenDir = goldenDir
	m.goldenCID = goldenCID
	m.goldenGuestIP = netCfg.GuestIP
	m.goldenHostIP = netCfg.HostIP
	_ = os.WriteFile(filepath.Join(goldenDir, "cid"), []byte(fmt.Sprintf("%d", goldenCID)), 0644)
	_ = os.WriteFile(filepath.Join(goldenDir, "guest_ip"), []byte(netCfg.GuestIP), 0644)
	_ = os.WriteFile(filepath.Join(goldenDir, "host_ip"), []byte(netCfg.HostIP), 0644)
	log.Printf("qemu: golden snapshot ready (%dms total, mem=%s, CID=%d, guestIP=%s)",
		time.Since(t0).Milliseconds(), memFile, goldenCID, netCfg.GuestIP)
	return nil
}

// createFromGolden creates a new VM by restoring from the golden snapshot.
// This skips kernel boot entirely — the VM resumes with the agent already running.
// After restore, we patch the network config inside the guest.
func (m *Manager) createFromGolden(ctx context.Context, cfg types.SandboxConfig, id string) (*types.Sandbox, error) {
	t0 := time.Now()

	template := cfg.Template
	if template == "" || template == "base" {
		template = "default"
	}

	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", id)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// Copy golden rootfs as qcow2 overlay (golden snapshot was taken with qcow2 drives)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	goldenRootfs := filepath.Join(m.goldenDir, "rootfs.qcow2")
	if err := copyFileReflink(goldenRootfs, rootfsPath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy golden rootfs: %w", err)
	}

	// Create fresh workspace as qcow2 (matching golden snapshot format)
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")
	diskMB := m.cfg.DefaultDiskMB
	if err := CreateWorkspace(workspacePath, diskMB); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	log.Printf("qemu: golden-create %s: rootfs+workspace ready (%dms)", id, time.Since(t0).Milliseconds())

	// Allocate network
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("create TAP: %w", err)
	}

	guestPort := cfg.Port
	if guestPort == 0 {
		guestPort = m.cfg.DefaultPort
	}
	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = guestPort

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	// Add metadata service DNAT (169.254.169.254:80 → host:8888)
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: warning: metadata DNAT failed for %s: %v", netCfg.TAPName, err)
	}

	cpus := cfg.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}

	guestCID := m.allocateCID()
	guestMAC := generateMAC(id)

	// Boot args don't matter for network (we'll patch via agent) but QEMU needs them
	// Use the golden boot args format — the actual IPs will be patched post-restore
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 "+
			"root=/dev/vda rw "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
	os.Remove(qmpSockPath)
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	// Build QEMU args with -incoming to restore from golden snapshot.
	// Use zstd-compressed mem file if available (less EBS I/O despite CPU cost).
	goldenMemZst := filepath.Join(m.goldenDir, "mem.zst")
	goldenMemRaw := filepath.Join(m.goldenDir, "mem")
	var incomingURI string
	if fileExists(goldenMemZst) {
		incomingURI = fmt.Sprintf("exec:zstdcat %s", goldenMemZst)
	} else {
		incomingURI = fmt.Sprintf("exec:cat %s", goldenMemRaw)
	}
	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)
	args = append(args, "-incoming", incomingURI)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start qemu from golden: %w", err)
	}
	logFile.Close()
	log.Printf("qemu: golden-create %s: QEMU started (%dms)", id, time.Since(t0).Milliseconds())

	// Connect QMP
	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("golden QMP connect: %w", err)
	}
	log.Printf("qemu: golden-create %s: QMP connected (%dms)", id, time.Since(t0).Milliseconds())

	// Wait for incoming migration to complete before resuming.
	// With -incoming, QEMU loads the state file and enters "paused" status when done.
	if err := m.waitForMigrationReady(qmpClient, 30*time.Second); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("golden migration load: %w", err)
	}
	log.Printf("qemu: golden-create %s: migration loaded (%dms)", id, time.Since(t0).Milliseconds())

	if err := qmpClient.Cont(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("golden QMP cont: %w", err)
	}
	log.Printf("qemu: golden-create %s: VM resumed (%dms)", id, time.Since(t0).Milliseconds())

	now := time.Now()
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	vm := &VMInstance{
		ID:            id,
		Template:      template,
		Status:        types.SandboxStatusRunning,
		StartedAt:     now,
		EndAt:         now.Add(timeout),
		CpuCount:      cpus,
		MemoryMB:      memMB,
		HostPort:      hostPort,
		GuestPort:     guestPort,
		pid:           cmd.Process.Pid,
		cmd:           cmd,
		network:       netCfg,
		sandboxDir:    sandboxDir,
		qmpSockPath:   qmpSockPath,
		agentSockPath: agentSockPath,
		qmp:           qmpClient,
		guestMAC:      guestMAC,
		guestCID:      guestCID,
		bootArgs:      bootArgs,
	}

	// Connect to agent via Unix socket
	var agentClient *AgentClient
	agentClient, err = m.waitForAgentSocket(context.Background(), agentSockPath, 10*time.Second)
	if err != nil {
		log.Printf("qemu: golden-create %s: agent not ready, falling back to cold boot: %v", id, err)
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, err
	}
	vm.agent = agentClient
	log.Printf("qemu: golden-create %s: agent connected (%dms)", id, time.Since(t0).Milliseconds())

	// Patch network inside the guest — the snapshot had the golden VM's IP
	if err := patchGuestNetwork(context.Background(), agentClient, netCfg); err != nil {
		log.Printf("qemu: golden-create %s: network patch failed: %v", id, err)
	}

	// Sync guest clock — golden snapshot has stale time
	if err := syncGuestClock(context.Background(), agentClient); err != nil {
		log.Printf("qemu: golden-create %s: clock sync failed: %v", id, err)
	}

	// Mount /workspace — the golden snapshot was taken with /workspace unmounted
	// to keep the vdb device state clean for fresh workspace qcow2 files.
	mountCtx, mountCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, mountErr := agentClient.Exec(mountCtx, &pb.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "mount /dev/vdb /workspace 2>/dev/null || true"},
	})
	mountCancel()
	if mountErr != nil {
		log.Printf("qemu: golden-create %s: mount /workspace failed: %v", id, mountErr)
	}
	log.Printf("qemu: golden-create %s: network patched (%dms)", id, time.Since(t0).Milliseconds())

	if len(cfg.Envs) > 0 {
		envCtx, envCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := agentClient.SetEnvs(envCtx, cfg.Envs); err != nil {
			envCancel()
			log.Printf("qemu: warning: SetEnvs failed for %s: %v", id, err)
		}
		envCancel()
	}

	m.mu.Lock()
	m.vms[id] = vm
	m.mu.Unlock()

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(id, netCfg.GuestIP, template, vm.StartedAt)
	}

	sbMeta := SandboxMeta{
		SandboxID: id,
		Template:  template,
		CpuCount:  cpus,
		MemoryMB:  memMB,
		GuestPort: guestPort,
	}
	if metaJSON, err := json.Marshal(sbMeta); err == nil {
		_ = os.WriteFile(filepath.Join(sandboxDir, "sandbox-meta.json"), metaJSON, 0644)
	}

	log.Printf("qemu: golden-create %s: DONE (%dms total, port=%d→%d, tap=%s, cid=%d)",
		id, time.Since(t0).Milliseconds(), hostPort, guestPort, netCfg.TAPName, guestCID)

	return &types.Sandbox{
		ID:        id,
		Template:  template,
		Status:    types.SandboxStatusRunning,
		StartedAt: now,
		EndAt:     now.Add(timeout),
		CpuCount:  cpus,
		MemoryMB:  memMB,
		HostPort:  hostPort,
	}, nil
}

// patchGuestNetwork reconfigures the guest's eth0 with the new IP/gateway.
// This is needed because the golden snapshot was booted with a different IP.
func patchGuestNetwork(ctx context.Context, agent *AgentClient, netCfg *NetworkConfig) error {
	// Calculate prefix length from mask (e.g. "255.255.255.252" → 30)
	prefixLen := maskToPrefixLen(netCfg.Mask)

	script := fmt.Sprintf(
		"ip addr flush dev eth0 && "+
			"ip addr add %s/%d dev eth0 && "+
			"ip link set eth0 up && "+
			"ip route add default via %s",
		netCfg.GuestIP, prefixLen, netCfg.HostIP,
	)

	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := agent.Exec(execCtx, &pb.ExecRequest{
		Command:        "/bin/sh",
		Args:           []string{"-c", script},
		TimeoutSeconds: 5,
	})
	if err != nil {
		return fmt.Errorf("exec network patch: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("network patch failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

// maskToPrefixLen converts a dotted-decimal netmask to a CIDR prefix length.
func maskToPrefixLen(mask string) int {
	switch mask {
	case "255.255.255.252":
		return 30
	case "255.255.255.248":
		return 29
	case "255.255.255.240":
		return 28
	case "255.255.255.224":
		return 27
	case "255.255.255.192":
		return 26
	case "255.255.255.128":
		return 25
	case "255.255.255.0":
		return 24
	default:
		return 30 // safe default for /30 subnets
	}
}

// waitForMigrationReady polls query-status until the VM enters "paused" or "running"
// state, indicating that the incoming migration has finished loading.
func (m *Manager) waitForMigrationReady(qmp *QMPClient, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := qmp.QueryStatus()
		if err != nil {
			// QEMU might not be ready to respond yet during migration load
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// "paused" = migration loaded, waiting for cont
		// "postmigrate" = also valid (some QEMU versions)
		// "inmigrate" = still loading
		switch status.Status {
		case "paused", "postmigrate":
			return nil
		case "running":
			return nil // already resumed somehow
		case "inmigrate", "prelaunch":
			time.Sleep(200 * time.Millisecond)
			continue
		default:
			time.Sleep(200 * time.Millisecond)
			continue
		}
	}
	return fmt.Errorf("migration not ready after %v", timeout)
}

// allocateCID returns a unique guest CID for a new VM.
func (m *Manager) allocateCID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cid := m.nextCID
	m.nextCID++
	return cid
}

// buildQEMUArgs constructs the QEMU command-line arguments.
// agentSock is the Unix socket path for the virtio-serial agent channel.
func (m *Manager) buildQEMUArgs(cpus, memMB int, rootfsPath, workspacePath, tapName, mac, agentSock, qmpSock, bootArgs string) []string {
	// Detect drive format from file extension
	rootfsFmt := "qcow2"
	if strings.HasSuffix(rootfsPath, ".ext4") {
		rootfsFmt = "raw"
	}
	wsFmt := "qcow2"
	if strings.HasSuffix(workspacePath, ".ext4") {
		wsFmt = "raw"
	}
	return []string{
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-m", fmt.Sprintf("%dM,maxmem=16G,slots=8", memMB),
		"-smp", fmt.Sprintf("%d", cpus),
		"-kernel", m.cfg.KernelPath,
		"-append", bootArgs,
		"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio,cache=writethrough", rootfsPath, rootfsFmt),
		"-drive", fmt.Sprintf("file=%s,format=%s,if=virtio,cache=writethrough", workspacePath, wsFmt),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", tapName),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", mac),
		// Agent communication via virtio-serial (survives QEMU migration,
		// unlike vhost-vsock which uses a per-process kernel fd).
		"-device", "virtio-serial-pci-non-transitional",
		"-chardev", fmt.Sprintf("socket,id=agent,path=%s,server=on,wait=off", agentSock),
		"-device", "virtserialport,chardev=agent,name=agent",
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", qmpSock),
		"-nographic",
		"-nodefaults",
		"-serial", "stdio",
	}
}

// Create launches a new QEMU VM.
func (m *Manager) Create(ctx context.Context, cfg types.SandboxConfig) (*types.Sandbox, error) {
	id := cfg.SandboxID
	if id == "" {
		id = "sb-" + uuid.New().String()[:8]
	}

	// Fast path: restore from golden snapshot if available and using default template
	template := cfg.Template
	if template == "" || template == "base" {
		template = "default"
	}
	if m.goldenDir != "" && template == "default" && cfg.TemplateRootfsKey == "" {
		sb, err := m.createFromGolden(ctx, cfg, id)
		if err != nil {
			log.Printf("qemu: golden restore failed for %s, falling back to cold boot: %v", id, err)
			// Fall through to cold boot below
		} else {
			return sb, nil
		}
	}

	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", id)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")

	if cfg.TemplateRootfsKey != "" {
		srcRootfs := strings.TrimPrefix(cfg.TemplateRootfsKey, "local://")
		srcWorkspace := strings.TrimPrefix(cfg.TemplateWorkspaceKey, "local://")
		log.Printf("qemu: create %s from snapshot template (rootfs=%s, workspace=%s)", id, srcRootfs, srcWorkspace)
		if err := copyFileReflink(srcRootfs, rootfsPath); err != nil {
			os.RemoveAll(sandboxDir)
			return nil, fmt.Errorf("copy template rootfs: %w", err)
		}
		if err := copyFileReflink(srcWorkspace, workspacePath); err != nil {
			os.RemoveAll(sandboxDir)
			return nil, fmt.Errorf("copy template workspace: %w", err)
		}
	} else {
		baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, template)
		if err != nil {
			os.RemoveAll(sandboxDir)
			return nil, fmt.Errorf("resolve base image: %w", err)
		}
		if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
			os.RemoveAll(sandboxDir)
			return nil, fmt.Errorf("prepare rootfs: %w", err)
		}

		diskMB := m.cfg.DefaultDiskMB
		if err := CreateWorkspace(workspacePath, diskMB); err != nil {
			os.RemoveAll(sandboxDir)
			return nil, fmt.Errorf("create workspace: %w", err)
		}
	}

	// Allocate network
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("create TAP: %w", err)
	}

	guestPort := cfg.Port
	if guestPort == 0 {
		guestPort = m.cfg.DefaultPort
	}
	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = guestPort

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	// Add metadata service DNAT (169.254.169.254:80 → host:8888)
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: warning: metadata DNAT failed for %s: %v", netCfg.TAPName, err)
	}

	cpus := cfg.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}

	guestCID := m.allocateCID()
	guestMAC := generateMAC(id)

	// Build kernel boot args — no pci=off (QEMU needs PCI for virtio-pci)
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 "+
			"root=/dev/vda rw "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
	os.Remove(qmpSockPath)
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	logFile.Close()

	// Connect QMP
	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("QMP connect: %w", err)
	}

	now := time.Now()
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	vm := &VMInstance{
		ID:            id,
		Template:      template,
		Status:        types.SandboxStatusRunning,
		StartedAt:     now,
		EndAt:         now.Add(timeout),
		CpuCount:      cpus,
		MemoryMB:      memMB,
		HostPort:      hostPort,
		GuestPort:     guestPort,
		pid:           cmd.Process.Pid,
		cmd:           cmd,
		network:       netCfg,
		sandboxDir:    sandboxDir,
		qmpSockPath:   qmpSockPath,
		agentSockPath: agentSockPath,
		qmp:           qmpClient,
		guestMAC:      guestMAC,
		guestCID:      guestCID,
		bootArgs:      bootArgs,
	}

	// Wait for agent via Unix socket
	agentClient, err := m.waitForAgentSocket(context.Background(), agentSockPath, 30*time.Second)
	if err != nil {
		log.Printf("qemu: agent not ready for %s, killing VM: %v", id, err)
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("agent not ready: %w", err)
	}
	vm.agent = agentClient

	if len(cfg.Envs) > 0 {
		envCtx, envCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := agentClient.SetEnvs(envCtx, cfg.Envs); err != nil {
			envCancel()
			log.Printf("qemu: warning: SetEnvs failed for %s: %v", id, err)
		}
		envCancel()
	}

	m.mu.Lock()
	m.vms[id] = vm
	m.mu.Unlock()

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(id, netCfg.GuestIP, template, vm.StartedAt)
	}

	sbMeta := SandboxMeta{
		SandboxID: id,
		Template:  template,
		CpuCount:  cpus,
		MemoryMB:  memMB,
		GuestPort: guestPort,
	}
	if metaJSON, err := json.Marshal(sbMeta); err == nil {
		_ = os.WriteFile(filepath.Join(sandboxDir, "sandbox-meta.json"), metaJSON, 0644)
	}

	log.Printf("qemu: created VM %s (template=%s, cpu=%d, mem=%dMB, port=%d→%d, tap=%s, mac=%s, cid=%d)",
		id, template, cpus, memMB, hostPort, guestPort, netCfg.TAPName, guestMAC, guestCID)

	return &types.Sandbox{
		ID:        id,
		Template:  template,
		Status:    types.SandboxStatusRunning,
		StartedAt: now,
		EndAt:     now.Add(timeout),
		CpuCount:  cpus,
		MemoryMB:  memMB,
		HostPort:  hostPort,
	}, nil
}

// waitForAgent polls the agent via gRPC/AF_VSOCK until it responds or times out.
func (m *Manager) waitForAgent(ctx context.Context, guestCID uint32, timeout time.Duration) (*AgentClient, error) {
	t0 := time.Now()
	deadline := t0.Add(timeout)
	var lastErr error
	attempts := 0

	for time.Now().Before(deadline) {
		attempts++
		tAttempt := time.Now()
		client, err := NewAgentClient(guestCID)
		if err != nil {
			lastErr = err
			if attempts <= 3 || attempts%10 == 0 {
				log.Printf("qemu: waitForAgent: attempt %d dial CID=%d failed (%dms): %v",
					attempts, guestCID, time.Since(tAttempt).Milliseconds(), err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err = client.Ping(pingCtx)
		cancel()
		if err != nil {
			client.Close()
			lastErr = err
			if attempts <= 3 || attempts%10 == 0 {
				log.Printf("qemu: waitForAgent: attempt %d ping CID=%d failed (%dms): %v",
					attempts, guestCID, time.Since(tAttempt).Milliseconds(), err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		log.Printf("qemu: waitForAgent: connected to CID=%d on attempt %d (%dms total)",
			guestCID, attempts, time.Since(t0).Milliseconds())
		return client, nil
	}

	return nil, fmt.Errorf("agent not ready after %v (%d attempts): %v", timeout, attempts, lastErr)
}

// waitForAgentSocket polls the agent via Unix socket (virtio-serial chardev)
// until it responds or times out.
func (m *Manager) waitForAgentSocket(ctx context.Context, socketPath string, timeout time.Duration) (*AgentClient, error) {
	t0 := time.Now()
	deadline := t0.Add(timeout)
	var lastErr error
	attempts := 0

	for time.Now().Before(deadline) {
		attempts++
		tAttempt := time.Now()
		client, err := NewAgentClientSocket(socketPath)
		if err != nil {
			lastErr = err
			if attempts <= 3 || attempts%10 == 0 {
				log.Printf("qemu: waitForAgentSocket: attempt %d dial %s failed (%dms): %v",
					attempts, socketPath, time.Since(tAttempt).Milliseconds(), err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err = client.Ping(pingCtx)
		cancel()
		if err != nil {
			client.Close()
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}

		log.Printf("qemu: waitForAgentSocket: connected to %s on attempt %d (%dms total)",
			socketPath, attempts, time.Since(t0).Milliseconds())
		return client, nil
	}

	return nil, fmt.Errorf("agent not ready after %v (%d attempts): %v", timeout, attempts, lastErr)
}

// Get returns sandbox info by ID.
func (m *Manager) Get(ctx context.Context, id string) (*types.Sandbox, error) {
	m.mu.RLock()
	vm, ok := m.vms[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", id)
	}
	return vmToSandbox(vm), nil
}

// Kill stops a VM and cleans up all resources.
func (m *Manager) Kill(ctx context.Context, id string) error {
	m.mu.Lock()
	vm, ok := m.vms[id]
	if ok {
		delete(m.vms, id)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("sandbox %s not found", id)
	}
	return m.destroyVM(vm)
}

// destroyVM stops a VM and cleans up all resources.
func (m *Manager) destroyVM(vm *VMInstance) error {
	// Try graceful shutdown via agent
	if vm.agent != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = vm.agent.Shutdown(shutCtx)
		cancel()
		vm.agent.Close()
	}

	// Try QMP quit first, fall back to process kill
	if vm.qmp != nil {
		_ = vm.qmp.Quit()
		vm.qmp.Close()
	}

	if vm.cmd != nil && vm.cmd.Process != nil {
		vm.cmd.Process.Kill()
		vm.cmd.Wait()
	}

	if vm.network != nil {
		RemoveMetadataDNAT(vm.network.TAPName, vm.network.HostIP)
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	// Notify metadata server
	if m.onSandboxDestroy != nil {
		m.onSandboxDestroy(vm.ID)
	}

	if vm.qmpSockPath != "" {
		os.Remove(vm.qmpSockPath)
	}

	if vm.sandboxDir != "" {
		os.RemoveAll(vm.sandboxDir)
	}

	log.Printf("qemu: destroyed VM %s", vm.ID)
	return nil
}

// cleanupVM cleans up resources on failed creation.
func (m *Manager) cleanupVM(netCfg *NetworkConfig, sandboxDir string) {
	if netCfg != nil {
		RemoveMetadataDNAT(netCfg.TAPName, netCfg.HostIP)
		RemoveDNAT(netCfg)
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
	}
	if sandboxDir != "" {
		os.RemoveAll(sandboxDir)
	}
}

// List returns all running VMs.
func (m *Manager) List(ctx context.Context) ([]types.Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]types.Sandbox, 0, len(m.vms))
	for _, vm := range m.vms {
		result = append(result, *vmToSandbox(vm))
	}
	return result, nil
}

// Count returns the number of running VMs.
func (m *Manager) Count(ctx context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.vms), nil
}

// Close stops all VMs and cleans up.
func (m *Manager) Close() {
	m.mu.Lock()
	vms := make([]*VMInstance, 0, len(m.vms))
	for _, vm := range m.vms {
		vms = append(vms, vm)
	}
	m.vms = make(map[string]*VMInstance)
	m.mu.Unlock()

	for _, vm := range vms {
		m.destroyVM(vm)
	}
	log.Printf("qemu: manager closed, %d VMs destroyed", len(vms))
}

// WaitUploads blocks until all in-flight async S3 uploads complete.
func (m *Manager) WaitUploads(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		m.uploadWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("qemu: all S3 uploads complete")
	case <-time.After(timeout):
		log.Printf("qemu: timed out waiting for S3 uploads after %s", timeout)
	}
}

// HibernateAllResult holds the result of a single VM hibernation.
type HibernateAllResult struct {
	SandboxID      string
	HibernationKey string
	Err            error
}

// HibernateAll hibernates all running VMs concurrently.
func (m *Manager) HibernateAll(ctx context.Context, checkpointStore *storage.CheckpointStore) []HibernateAllResult {
	m.mu.RLock()
	ids := make([]string, 0, len(m.vms))
	for id := range m.vms {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	if len(ids) == 0 {
		return nil
	}

	var results []HibernateAllResult
	var resultsMu sync.Mutex
	var wg sync.WaitGroup

	for _, id := range ids {
		wg.Add(1)
		go func(sandboxID string) {
			defer wg.Done()
			result, err := m.Hibernate(ctx, sandboxID, checkpointStore)

			resultsMu.Lock()
			defer resultsMu.Unlock()
			if err != nil {
				log.Printf("qemu: HibernateAll: %s failed: %v", sandboxID, err)
				results = append(results, HibernateAllResult{SandboxID: sandboxID, Err: err})
			} else {
				results = append(results, HibernateAllResult{SandboxID: sandboxID, HibernationKey: result.HibernationKey})
			}
		}(id)
	}

	wg.Wait()
	return results
}

// Exec runs a command in the VM via the agent.
func (m *Manager) Exec(ctx context.Context, sandboxID string, cfg types.ProcessConfig) (*types.ProcessResult, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	timeout := int32(cfg.Timeout)
	if timeout <= 0 {
		timeout = 60
	}

	command := cfg.Command
	args := cfg.Args
	if len(args) == 0 {
		args = []string{"-c", command}
		command = "/bin/sh"
	}

	resp, err := vm.agent.Exec(ctx, &pb.ExecRequest{
		Command:        command,
		Args:           args,
		Envs:           cfg.Env,
		Cwd:            cfg.Cwd,
		TimeoutSeconds: timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("exec in %s: %w", sandboxID, err)
	}

	return &types.ProcessResult{
		ExitCode: int(resp.ExitCode),
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}, nil
}

// ReadFile reads a file from the VM.
func (m *Manager) ReadFile(ctx context.Context, sandboxID, path string) (string, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	data, err := vm.agent.ReadFile(ctx, path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteFile writes a file in the VM.
func (m *Manager) WriteFile(ctx context.Context, sandboxID, path, content string) error {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return err
	}
	return vm.agent.WriteFile(ctx, path, []byte(content))
}

// ListDir lists a directory in the VM.
func (m *Manager) ListDir(ctx context.Context, sandboxID, path string) ([]types.EntryInfo, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	entries, err := vm.agent.ListDir(ctx, path)
	if err != nil {
		return nil, err
	}
	result := make([]types.EntryInfo, len(entries))
	for i, e := range entries {
		result[i] = types.EntryInfo{
			Name:  e.Name,
			IsDir: e.IsDir,
			Size:  e.Size,
			Path:  e.Path,
		}
	}
	return result, nil
}

// MakeDir creates a directory in the VM.
func (m *Manager) MakeDir(ctx context.Context, sandboxID, path string) error {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return err
	}
	return vm.agent.MakeDir(ctx, path)
}

// Remove removes a file/directory in the VM.
func (m *Manager) Remove(ctx context.Context, sandboxID, path string) error {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return err
	}
	return vm.agent.Remove(ctx, path)
}

// Exists checks if a path exists in the VM.
func (m *Manager) Exists(ctx context.Context, sandboxID, path string) (bool, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	return vm.agent.Exists(ctx, path)
}

// Stat returns file metadata from the VM.
func (m *Manager) Stat(ctx context.Context, sandboxID, path string) (*types.FileInfo, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	resp, err := vm.agent.Stat(ctx, path)
	if err != nil {
		return nil, err
	}
	return &types.FileInfo{
		Name:    resp.Name,
		IsDir:   resp.IsDir,
		Size:    resp.Size,
		Mode:    resp.Mode,
		ModTime: resp.ModTime,
		Path:    resp.Path,
	}, nil
}

// SetResourceLimits adjusts sandbox cgroup limits at runtime via the agent.
// If the requested memory exceeds the VM's physical RAM, hotplug a DIMM first.
func (m *Manager) SetResourceLimits(ctx context.Context, sandboxID string, maxPids int32, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec int64) error {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return err
	}

	// Memory hotplug: if requested memory > current VM physical RAM, add a DIMM
	if maxMemoryBytes > 0 {
		currentBytes := int64(vm.MemoryMB) * 1024 * 1024
		if maxMemoryBytes > currentBytes && vm.qmp != nil {
			addMB := int(maxMemoryBytes-currentBytes) / (1024 * 1024)
			if addMB > 0 {
				if err := vm.qmp.HotplugMemory(vm.dimmCount, addMB); err != nil {
					log.Printf("qemu: memory hotplug %s: add %dMB failed: %v", sandboxID, addMB, err)
					// Non-fatal — cgroup limit will still be set (capped at physical RAM)
				} else {
					vm.dimmCount++
					vm.MemoryMB += addMB
					log.Printf("qemu: memory hotplug %s: added %dMB (total %dMB)", sandboxID, addMB, vm.MemoryMB)
				}
			}
		}
	}

	return vm.agent.SetResourceLimits(ctx, maxPids, maxMemoryBytes, cpuMaxUsec, cpuPeriodUsec)
}

// Stats returns live resource usage from the VM.
func (m *Manager) Stats(ctx context.Context, sandboxID string) (*sandbox.SandboxStats, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	resp, err := vm.agent.Stats(ctx)
	if err != nil {
		return nil, err
	}
	return &sandbox.SandboxStats{
		CPUPercent: resp.CpuPercent,
		MemUsage:   resp.MemUsage,
		MemLimit:   resp.MemLimit,
		NetInput:   resp.NetInput,
		NetOutput:  resp.NetOutput,
		PIDs:       int(resp.Pids),
	}, nil
}

// HostPort returns the mapped host port for a sandbox.
func (m *Manager) HostPort(ctx context.Context, sandboxID string) (int, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return 0, err
	}
	return vm.HostPort, nil
}

// ContainerAddr returns the VM's guest IP and port.
func (m *Manager) ContainerAddr(ctx context.Context, sandboxID string, port int) (string, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return "", err
	}
	if vm.network == nil {
		return "", fmt.Errorf("sandbox %s has no network config", sandboxID)
	}
	return fmt.Sprintf("%s:%d", vm.network.GuestIP, port), nil
}

// DataDir returns the base data directory.
func (m *Manager) DataDir() string {
	return m.cfg.DataDir
}

// ContainerName returns a human-readable name for the sandbox.
func (m *Manager) ContainerName(id string) string {
	return "qm-" + id
}

// Hibernate snapshots a VM and uploads to S3.
func (m *Manager) Hibernate(ctx context.Context, sandboxID string, checkpointStore *storage.CheckpointStore) (*sandbox.HibernateResult, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return nil, err
	}
	result, err := m.doHibernate(ctx, vm, checkpointStore)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	delete(m.vms, sandboxID)
	m.mu.Unlock()

	return result, nil
}

// Wake restores a VM from a snapshot.
func (m *Manager) Wake(ctx context.Context, sandboxID string, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	return m.doWake(ctx, sandboxID, checkpointKey, checkpointStore, timeout)
}

// TemplateCachePath returns "" — not implemented.
func (m *Manager) TemplateCachePath(templateID, filename string) string {
	return ""
}

// checkpointCacheDir returns the local cache directory for a checkpoint.
func (m *Manager) checkpointCacheDir(checkpointID string) string {
	return filepath.Join(m.cfg.DataDir, "checkpoints", checkpointID)
}

// CheckpointCachePath returns the path to a specific file in the checkpoint cache.
func (m *Manager) CheckpointCachePath(checkpointID, filename string) string {
	p := filepath.Join(m.checkpointCacheDir(checkpointID), filename)
	if fileExists(p) {
		return p
	}
	return ""
}

// CreateCheckpoint creates an internal VM snapshot using QEMU's savevm.
// The snapshot is stored inside the qcow2 drive files — no external migration file needed.
// The VM pauses briefly for the snapshot, then resumes automatically.
func (m *Manager) CreateCheckpoint(ctx context.Context, sandboxID, checkpointID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return "", "", err
	}

	t0 := time.Now()

	if vm.qmp == nil {
		return "", "", fmt.Errorf("no QMP client for VM %s", sandboxID)
	}

	// Sync filesystem and close agent before snapshot. Closing the agent
	// puts it in Accept() mode — when ForkFromCheckpoint restores from this
	// checkpoint, the agent immediately accepts the new connection (~1ms)
	// instead of waiting for the stale gRPC stream to time out (~6s).
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, syncErr := vm.agent.Exec(syncCtx, &pb.ExecRequest{
			Command: "/bin/sh",
			Args:    []string{"-c", "umount /workspace 2>/dev/null; sync"},
		})
		cancel()
		if syncErr != nil {
			log.Printf("qemu: CreateCheckpoint %s/%s: sync failed: %v", sandboxID, checkpointID, syncErr)
		}
		vm.agent.Close()
		vm.agent = nil
	}

	// savevm creates an internal snapshot — memory + device state + disk deltas
	// all stored inside the qcow2 files. The VM pauses during savevm and resumes after.
	snapshotName := "cp-" + checkpointID
	if err := vm.qmp.SaveVM(snapshotName); err != nil {
		return "", "", fmt.Errorf("savevm failed: %w", err)
	}
	log.Printf("qemu: CreateCheckpoint %s/%s: savevm complete (%dms)", sandboxID, checkpointID, time.Since(t0).Milliseconds())

	// Reconnect agent + remount workspace (VM resumed after savevm)
	agentClient, reconnErr := m.waitForAgentSocket(context.Background(), vm.agentSockPath, 10*time.Second)
	if reconnErr != nil {
		log.Printf("qemu: CreateCheckpoint %s/%s: agent reconnect failed: %v", sandboxID, checkpointID, reconnErr)
	} else {
		vm.agent = agentClient
		mountCtx, mountCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = agentClient.Exec(mountCtx, &pb.ExecRequest{
			Command: "/bin/sh",
			Args:    []string{"-c", "echo 3 > /proc/sys/vm/drop_caches; mount /dev/vdb /workspace 2>/dev/null || true"},
		})
		mountCancel()
	}

	// Also cache the drive files for ForkFromCheckpoint (which needs a separate QEMU process)
	cacheDir := m.checkpointCacheDir(checkpointID)
	rootfsKey = fmt.Sprintf("checkpoints/%s/%s/rootfs.tar.zst", sandboxID, checkpointID)
	workspaceKey = fmt.Sprintf("checkpoints/%s/%s/workspace.tar.zst", sandboxID, checkpointID)

	go func() {
		if err := os.MkdirAll(filepath.Join(cacheDir, "snapshot"), 0755); err != nil {
			log.Printf("qemu: checkpoint %s: mkdir cache failed: %v", checkpointID, err)
			if onReady != nil {
				onReady()
			}
			return
		}

		// Copy qcow2 drives (they contain the snapshot data)
		srcRootfs := filepath.Join(vm.sandboxDir, "rootfs.qcow2")
		srcWorkspace := filepath.Join(vm.sandboxDir, "workspace.qcow2")
		_ = copyFileReflink(srcRootfs, filepath.Join(cacheDir, "rootfs.qcow2"))
		_ = copyFileReflink(srcWorkspace, filepath.Join(cacheDir, "workspace.qcow2"))

		// Write metadata
		meta := &SnapshotMeta{
			SandboxID:    vm.ID,
			Network:      vm.network,
			GuestCID:     vm.guestCID,
			GuestMAC:     vm.guestMAC,
			BootArgs:     vm.bootArgs,
			CpuCount:     vm.CpuCount,
			MemoryMB:     vm.MemoryMB,
			Template:     vm.Template,
			GuestPort:    vm.GuestPort,
			SnapshotedAt: time.Now(),
		}
		metaJSON, _ := json.Marshal(meta)
		_ = os.WriteFile(filepath.Join(cacheDir, "snapshot", "snapshot-meta.json"), metaJSON, 0644)
		_ = os.WriteFile(filepath.Join(cacheDir, "snapshot-name"), []byte(snapshotName), 0644)

		log.Printf("qemu: checkpoint %s: cache saved (%dms)", checkpointID, time.Since(t0).Milliseconds())
		if onReady != nil {
			onReady()
		}
	}()

	return rootfsKey, workspaceKey, nil
}

// RestoreFromCheckpoint reverts a running sandbox to a checkpoint using QEMU's loadvm.
// The VM stays running — no process restart needed. ~100-200ms.
func (m *Manager) RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	t0 := time.Now()

	if vm.qmp == nil {
		return fmt.Errorf("no QMP client for VM %s", sandboxID)
	}

	snapshotName := "cp-" + checkpointID

	// Close agent before loadvm (vsock state will revert)
	if vm.agent != nil {
		closeDone := make(chan struct{})
		go func() { vm.agent.Close(); close(closeDone) }()
		select {
		case <-closeDone:
		case <-time.After(2 * time.Second):
		}
		vm.agent = nil
	}

	// Stop the VM before loadvm (QEMU requires it)
	if err := vm.qmp.Stop(); err != nil {
		log.Printf("qemu: RestoreFromCheckpoint %s: stop failed: %v", sandboxID, err)
	}

	// loadvm reverts the entire VM state — memory, devices, and disk contents
	if err := vm.qmp.LoadVM(snapshotName); err != nil {
		// Resume the VM if loadvm fails
		vm.qmp.Cont()
		return fmt.Errorf("loadvm failed: %w", err)
	}

	// Resume after loadvm
	if err := vm.qmp.Cont(); err != nil {
		return fmt.Errorf("cont after loadvm failed: %w", err)
	}

	// Reconnect agent via Unix socket
	agent, err := m.waitForAgentSocket(context.Background(), vm.agentSockPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("agent reconnect after loadvm: %w", err)
	}
	vm.agent = agent

	// Remount workspace + drop caches (loadvm reverted to checkpoint state)
	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = agent.Exec(restoreCtx, &pb.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo 3 > /proc/sys/vm/drop_caches; mount /dev/vdb /workspace 2>/dev/null || true"},
	})
	restoreCancel()

	if err := syncGuestClock(context.Background(), agent); err != nil {
		log.Printf("qemu: RestoreFromCheckpoint %s: clock sync failed: %v", sandboxID, err)
	}

	log.Printf("qemu: RestoreFromCheckpoint %s/%s: loadvm complete (%dms)", sandboxID, checkpointID, time.Since(t0).Milliseconds())
	return nil
}

// ForkFromCheckpoint creates a new sandbox from a checkpoint's saved state.
// The new sandbox gets its own network, CID, and drives (reflinked from cache).
func (m *Manager) ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error) {
	t0 := time.Now()
	cacheDir := m.checkpointCacheDir(checkpointID)
	metaPath := filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")

	// qcow2 drives contain the savevm snapshot data
	cachedRootfs := filepath.Join(cacheDir, "rootfs.qcow2")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.qcow2")
	if !fileExists(cachedRootfs) || !fileExists(cachedWorkspace) {
		return nil, fmt.Errorf("checkpoint %s: qcow2 files not found in cache", checkpointID)
	}

	// Read snapshot name
	snapshotName := "cp-" + checkpointID
	if data, err := os.ReadFile(filepath.Join(cacheDir, "snapshot-name")); err == nil {
		snapshotName = strings.TrimSpace(string(data))
	}

	var meta SnapshotMeta
	if data, err := os.ReadFile(metaPath); err == nil {
		json.Unmarshal(data, &meta)
	}

	id := cfg.SandboxID
	if id == "" {
		id = "sb-" + uuid.New().String()[:8]
	}
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", id)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// Copy qcow2 drives (contain snapshot data)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")
	if err := copyFileReflink(cachedRootfs, rootfsPath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}
	if err := copyFileReflink(cachedWorkspace, workspacePath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy workspace: %w", err)
	}

	// Allocate network
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("create TAP: %w", err)
	}

	guestPort := cfg.Port
	if guestPort == 0 {
		guestPort = m.cfg.DefaultPort
	}
	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = guestPort
	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	// Add metadata service DNAT (169.254.169.254:80 → host:8888)
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: warning: metadata DNAT failed for %s: %v", netCfg.TAPName, err)
	}

	cpus := cfg.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}

	guestCID := m.allocateCID()
	guestMAC := generateMAC(id)
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 "+
			"root=/dev/vda rw "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
	os.Remove(qmpSockPath)
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	// Start QEMU paused — we'll loadvm then cont
	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)
	args = append(args, "-S") // start paused

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start qemu for fork: %w", err)
	}
	logFile.Close()

	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("QMP connect: %w", err)
	}

	// loadvm restores from the internal snapshot in the qcow2 drives
	if err := qmpClient.LoadVM(snapshotName); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("loadvm: %w", err)
	}

	if err := qmpClient.Cont(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("QMP cont: %w", err)
	}
	log.Printf("qemu: ForkFromCheckpoint %s → %s: VM resumed (%dms)", checkpointID, id, time.Since(t0).Milliseconds())

	// Connect agent via Unix socket
	var agent *AgentClient
	agent, err = m.waitForAgentSocket(context.Background(), agentSockPath, 10*time.Second)
	if err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("agent connect: %w", err)
	}

	// Drop caches (checkpoint had different rootfs state) + mount workspace + patch network
	postCtx, postCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, _ = agent.Exec(postCtx, &pb.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo 3 > /proc/sys/vm/drop_caches; mount /dev/vdb /workspace 2>/dev/null || true"},
	})
	postCancel()

	if err := syncGuestClock(context.Background(), agent); err != nil {
		log.Printf("qemu: ForkFromCheckpoint %s: clock sync failed: %v", id, err)
	}

	if err := patchGuestNetwork(context.Background(), agent, netCfg); err != nil {
		log.Printf("qemu: ForkFromCheckpoint %s: network patch failed: %v", id, err)
	}

	// Set env vars
	if len(cfg.Envs) > 0 {
		envCtx, envCancel := context.WithTimeout(context.Background(), 5*time.Second)
		agent.SetEnvs(envCtx, cfg.Envs)
		envCancel()
	}

	now := time.Now()
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	vm := &VMInstance{
		ID:            id,
		Template:      meta.Template,
		Status:        types.SandboxStatusRunning,
		StartedAt:     now,
		EndAt:         now.Add(timeout),
		CpuCount:      cpus,
		MemoryMB:      memMB,
		HostPort:      hostPort,
		GuestPort:     guestPort,
		pid:           cmd.Process.Pid,
		cmd:           cmd,
		network:       netCfg,
		sandboxDir:    sandboxDir,
		qmpSockPath:   qmpSockPath,
		agentSockPath: agentSockPath,
		qmp:           qmpClient,
		guestMAC:      guestMAC,
		guestCID:      guestCID,
		bootArgs:      bootArgs,
		agent:         agent,
	}

	m.mu.Lock()
	m.vms[id] = vm
	m.mu.Unlock()

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(id, netCfg.GuestIP, meta.Template, now)
	}

	log.Printf("qemu: ForkFromCheckpoint %s → %s: complete (%dms, port=%d, tap=%s)",
		checkpointID, id, time.Since(t0).Milliseconds(), hostPort, netCfg.TAPName)

	return &types.Sandbox{
		ID:        id,
		Template:  meta.Template,
		Status:    types.SandboxStatusRunning,
		StartedAt: now,
		EndAt:     now.Add(timeout),
		CpuCount:  cpus,
		MemoryMB:  memMB,
		HostPort:  hostPort,
	}, nil
}

// getVM retrieves a VM by ID.
func (m *Manager) getVM(id string) (*VMInstance, error) {
	m.mu.RLock()
	vm, ok := m.vms[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", id)
	}
	return vm, nil
}

// getReadyVM returns a VM that is ready for agent operations.
func (m *Manager) getReadyVM(ctx context.Context, id string) (*VMInstance, error) {
	vm, err := m.getVM(id)
	if err != nil {
		return nil, err
	}

	if vm.restoring != nil {
		select {
		case <-vm.restoring:
			vm, err = m.getVM(id)
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			return nil, fmt.Errorf("sandbox %s: timed out waiting for restore", id)
		}
	}

	if vm.agent == nil {
		return nil, fmt.Errorf("sandbox %s: agent not available", id)
	}
	return vm, nil
}

// vmToSandbox converts a VMInstance to a types.Sandbox.
func vmToSandbox(vm *VMInstance) *types.Sandbox {
	return &types.Sandbox{
		ID:        vm.ID,
		Template:  vm.Template,
		Status:    vm.Status,
		StartedAt: vm.StartedAt,
		EndAt:     vm.EndAt,
		CpuCount:  vm.CpuCount,
		MemoryMB:  vm.MemoryMB,
		HostPort:  vm.HostPort,
	}
}

// generateMAC creates a deterministic MAC address from a sandbox ID.
// Format: AA:CE:00:00:XX:XX where XX:XX are derived from the ID.
// Uses locally-administered unicast prefix (bit 1 of first octet set).
func generateMAC(id string) string {
	var b4, b5 byte
	if len(id) > 3 {
		b4 = id[3]
	}
	if len(id) > 0 {
		b5 = id[len(id)-1]
	}
	return fmt.Sprintf("AA:CE:00:00:%02x:%02x", b4, b5)
}

// GetGuestCID returns the guest CID for a sandbox (used by PTY manager).
func (m *Manager) GetGuestCID(sandboxID string) (uint32, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return 0, err
	}
	return vm.guestCID, nil
}

// GetAgent returns the agent client for a sandbox (used by PTY manager).
func (m *Manager) GetAgent(sandboxID string) (*AgentClient, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return nil, err
	}
	return vm.agent, nil
}

// GetWorkspacePath returns the host path to a sandbox's workspace.ext4.
func (m *Manager) GetWorkspacePath(sandboxID string) (string, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return "", err
	}
	return filepath.Join(vm.sandboxDir, "workspace.ext4"), nil
}

// SyncFS flushes filesystem buffers inside the VM.
func (m *Manager) SyncFS(ctx context.Context, sandboxID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}
	if vm.agent == nil {
		return fmt.Errorf("no agent connection for %s", sandboxID)
	}
	return vm.agent.SyncFS(ctx)
}

// CleanupOrphanedProcesses kills any QEMU processes and TAP devices
// left over from a previous worker run.
func (m *Manager) CleanupOrphanedProcesses() {
	out, err := exec.Command("pgrep", "-f", "qemu-system").Output()
	if err == nil && len(out) > 0 {
		count := 0
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			_ = exec.Command("kill", "-9", line).Run()
			count++
		}
		if count > 0 {
			log.Printf("qemu: killed %d orphaned qemu process(es)", count)
		}
	}

	out, err = exec.Command("ip", "-o", "link", "show").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			tapName := strings.TrimSuffix(fields[1], ":")
			if strings.HasPrefix(tapName, "qm-") {
				_ = exec.Command("ip", "link", "del", tapName).Run()
				log.Printf("qemu: cleaned up orphaned TAP %s", tapName)
			}
		}
	}
}

// LocalRecovery describes a sandbox found on disk that can be recovered.
type LocalRecovery struct {
	SandboxID   string
	HasSnapshot bool
	Meta        SandboxMeta
}

// RecoverLocalSandboxes scans the sandboxes directory for sandbox data left
// on disk from a previous run.
func (m *Manager) RecoverLocalSandboxes() []LocalRecovery {
	sandboxesDir := filepath.Join(m.cfg.DataDir, "sandboxes")
	entries, err := os.ReadDir(sandboxesDir)
	if err != nil {
		log.Printf("qemu: no sandboxes dir to scan: %v", err)
		return nil
	}

	var recoveries []LocalRecovery
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "sb-") {
			continue
		}
		sandboxID := entry.Name()
		sandboxDir := filepath.Join(sandboxesDir, sandboxID)

		snapshotMetaPath := filepath.Join(sandboxDir, "snapshot", "snapshot-meta.json")
		if fileExists(filepath.Join(sandboxDir, "snapshot", "mem")) &&
			fileExists(snapshotMetaPath) {
			var snapMeta SnapshotMeta
			if data, err := os.ReadFile(snapshotMetaPath); err == nil {
				if json.Unmarshal(data, &snapMeta) == nil {
					recoveries = append(recoveries, LocalRecovery{
						SandboxID:   sandboxID,
						HasSnapshot: true,
						Meta: SandboxMeta{
							SandboxID: sandboxID,
							Template:  snapMeta.Template,
							CpuCount:  snapMeta.CpuCount,
							MemoryMB:  snapMeta.MemoryMB,
							GuestPort: snapMeta.GuestPort,
						},
					})
					continue
				}
			}
		}

		if fileExists(filepath.Join(sandboxDir, "workspace.ext4")) {
			sbMetaPath := filepath.Join(sandboxDir, "sandbox-meta.json")
			var meta SandboxMeta
			if data, err := os.ReadFile(sbMetaPath); err == nil {
				if json.Unmarshal(data, &meta) == nil {
					recoveries = append(recoveries, LocalRecovery{
						SandboxID:   sandboxID,
						HasSnapshot: false,
						Meta:        meta,
					})
					continue
				}
			}
			log.Printf("qemu: skipping %s: workspace exists but no sandbox-meta.json", sandboxID)
		}
	}
	return recoveries
}
