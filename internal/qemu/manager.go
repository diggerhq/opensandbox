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
	qmpSockPath string
	qmp         *QMPClient
	guestMAC    string
	guestCID    uint32
	bootArgs    string
	restoring   chan struct{}
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
	goldenDir string // path to golden snapshot dir (empty = not available)
	goldenCID uint32 // CID used when the golden snapshot was created
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

// PrepareGoldenSnapshot boots a temporary VM, waits for the agent, then
// hibernates it to create a reusable snapshot. Subsequent Create() calls
// restore from this snapshot instead of cold-booting, cutting start time
// from ~10s to ~1-2s.
func (m *Manager) PrepareGoldenSnapshot() error {
	goldenDir := filepath.Join(m.cfg.DataDir, "golden")
	memFile := filepath.Join(goldenDir, "mem")
	rootfsFile := filepath.Join(goldenDir, "rootfs.ext4")

	// If golden snapshot already exists, just use it
	if (fileExists(memFile) || fileExists(memFile+".zst")) && fileExists(rootfsFile) {
		m.goldenDir = goldenDir
		// Read saved golden CID
		if cidBytes, err := os.ReadFile(filepath.Join(goldenDir, "cid")); err == nil {
			fmt.Sscanf(string(cidBytes), "%d", &m.goldenCID)
		}
		log.Printf("qemu: golden snapshot already exists at %s (CID=%d)", goldenDir, m.goldenCID)
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

	// Create a small workspace (won't be used, just needs to exist for QEMU args)
	workspaceFile := filepath.Join(goldenDir, "workspace.ext4")
	if err := CreateWorkspace(workspaceFile, 64); err != nil {
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
	os.Remove(qmpSockPath)

	logPath := filepath.Join(goldenDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("create golden log: %w", err)
	}

	args := m.buildQEMUArgs(m.cfg.DefaultCPUs, m.cfg.DefaultMemoryMB,
		rootfsFile, workspaceFile, netCfg.TAPName, goldenMAC, goldenCID, qmpSockPath, bootArgs)

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

	// Wait for agent to be ready
	agentClient, err := m.waitForAgent(context.Background(), goldenCID, 30*time.Second)
	if err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden agent not ready: %w", err)
	}
	log.Printf("qemu: golden VM booted, agent ready (%dms)", time.Since(t0).Milliseconds())

	// Close agent connection before migration
	agentClient.Close()
	time.Sleep(500 * time.Millisecond)

	// QMP stop + migrate
	if err := qmpClient.Stop(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("golden QMP stop: %w", err)
	}

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

	// Compress golden mem with zstd for faster restore
	zstCmd := exec.Command("zstd", "-3", "--rm", memFile, "-o", memFile+".zst")
	if out, err := zstCmd.CombinedOutput(); err != nil {
		log.Printf("qemu: golden zstd compress failed (will use raw): %v (%s)", err, string(out))
	} else {
		log.Printf("qemu: golden mem compressed with zstd")
	}

	m.goldenDir = goldenDir
	m.goldenCID = goldenCID
	_ = os.WriteFile(filepath.Join(goldenDir, "cid"), []byte(fmt.Sprintf("%d", goldenCID)), 0644)
	log.Printf("qemu: golden snapshot ready (%dms total, mem=%s, CID=%d)",
		time.Since(t0).Milliseconds(), memFile, goldenCID)
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

	// Copy golden rootfs (this is the base — each VM gets its own copy)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	if err := copyFileReflink(filepath.Join(m.goldenDir, "rootfs.ext4"), rootfsPath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy golden rootfs: %w", err)
	}

	// Create fresh workspace
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
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

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	// Build QEMU args with -incoming to restore from golden snapshot.
	// Use zstd-compressed mem file if available (typically ~40% of raw size).
	goldenMemZst := filepath.Join(m.goldenDir, "mem.zst")
	goldenMemRaw := filepath.Join(m.goldenDir, "mem")
	var incomingURI string
	if fileExists(goldenMemZst) {
		incomingURI = fmt.Sprintf("exec:zstdcat %s", goldenMemZst)
	} else {
		incomingURI = fmt.Sprintf("exec:cat %s", goldenMemRaw)
	}
	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, guestCID, qmpSockPath, bootArgs)
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
		ID:          id,
		Template:    template,
		Status:      types.SandboxStatusRunning,
		StartedAt:   now,
		EndAt:       now.Add(timeout),
		CpuCount:    cpus,
		MemoryMB:    memMB,
		HostPort:    hostPort,
		GuestPort:   guestPort,
		pid:         cmd.Process.Pid,
		cmd:         cmd,
		network:     netCfg,
		sandboxDir:  sandboxDir,
		qmpSockPath: qmpSockPath,
		qmp:         qmpClient,
		guestMAC:    guestMAC,
		guestCID:    guestCID,
		bootArgs:    bootArgs,
	}

	// After migration restore, the vsock CID from the snapshot (golden CID) may
	// override the CLI arg. Try both the new CID and the golden CID.
	var agentClient *AgentClient
	// Try new CID first (quick attempt)
	agentClient, err = m.waitForAgent(context.Background(), guestCID, 3*time.Second)
	if err != nil {
		log.Printf("qemu: golden-create %s: CID=%d failed, trying golden CID=%d", id, guestCID, m.goldenCID)
		agentClient, err = m.waitForAgent(context.Background(), m.goldenCID, 5*time.Second)
		if err == nil {
			log.Printf("qemu: golden-create %s: connected via golden CID=%d (migration preserved CID)", id, m.goldenCID)
			guestCID = m.goldenCID
			vm.guestCID = guestCID
		}
	}
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
		// Non-fatal for now — connectivity might still work via TAP
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
func (m *Manager) buildQEMUArgs(cpus, memMB int, rootfsPath, workspacePath, tapName, mac string, cid uint32, qmpSock, bootArgs string) []string {
	return []string{
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-m", fmt.Sprintf("%dM", memMB),
		"-smp", fmt.Sprintf("%d", cpus),
		"-kernel", m.cfg.KernelPath,
		"-append", bootArgs,
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", rootfsPath),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", workspacePath),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", tapName),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", mac),
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d,id=vsock0", cid),
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

	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")

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

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, guestCID, qmpSockPath, bootArgs)

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
		ID:          id,
		Template:    template,
		Status:      types.SandboxStatusRunning,
		StartedAt:   now,
		EndAt:       now.Add(timeout),
		CpuCount:    cpus,
		MemoryMB:    memMB,
		HostPort:    hostPort,
		GuestPort:   guestPort,
		pid:         cmd.Process.Pid,
		cmd:         cmd,
		network:     netCfg,
		sandboxDir:  sandboxDir,
		qmpSockPath: qmpSockPath,
		qmp:         qmpClient,
		guestMAC:    guestMAC,
		guestCID:    guestCID,
		bootArgs:    bootArgs,
	}

	// Wait for agent via AF_VSOCK
	agentClient, err := m.waitForAgent(context.Background(), guestCID, 30*time.Second)
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
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
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

// SaveAsTemplate is not implemented in the QEMU backend.
func (m *Manager) SaveAsTemplate(ctx context.Context, sandboxID, templateID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error) {
	return "", "", ErrNotImplemented
}

// TemplateCachePath returns "" — not implemented.
func (m *Manager) TemplateCachePath(templateID, filename string) string {
	return ""
}

// CreateCheckpoint is not implemented in the QEMU backend.
func (m *Manager) CreateCheckpoint(ctx context.Context, sandboxID, checkpointID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error) {
	return "", "", ErrNotImplemented
}

// RestoreFromCheckpoint is not implemented in the QEMU backend.
func (m *Manager) RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error {
	return ErrNotImplemented
}

// ForkFromCheckpoint is not implemented in the QEMU backend.
func (m *Manager) ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error) {
	return nil, ErrNotImplemented
}

// CheckpointCachePath returns "" — not implemented.
func (m *Manager) CheckpointCachePath(checkpointID, filename string) string {
	return ""
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
