// Package qemu implements sandbox.Manager using QEMU q35 VMs with KVM acceleration.
// Each sandbox is a full VM with virtio devices, communicating with the host
// via gRPC over AF_VSOCK (kernel vhost-vsock).
package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	opMu          sync.Mutex   // serializes destructive VM ops (checkpoint, restore, hibernate)
	archiveDone   chan struct{} // closed when async hibernate archive completes (nil if no archive in flight)
	baseMemoryMB         int  // initial memory passed to -m (before virtio-mem)
	virtioMemRequestedMB int  // additional memory via virtio-mem (beyond base)
}

// SandboxMeta is persisted to sandbox-meta.json for recovery after hard kills.
type SandboxMeta struct {
	SandboxID string `json:"sandboxId"`
	Template  string `json:"template"`
	CpuCount  int    `json:"cpuCount"`
	MemoryMB  int    `json:"memoryMB"`
	GuestPort int    `json:"guestPort"`
}

// SecretsProxyIntegration provides the interface for the secrets proxy to integrate
// with VM lifecycle.
type SecretsProxyIntegration interface {
	// CreateSealedEnvs tokenizes every entry in secretEnvs, copies plaintextEnvs
	// through verbatim, and returns the full env map to inject into the VM
	// (sealed + plaintext + proxy config vars HTTP_PROXY/CA cert). plaintextEnvs
	// wins on collision (matches the API-layer rule that user envs override
	// store-derived values of the same name).
	CreateSealedEnvs(sandboxID, guestIP, gatewayIP string, plaintextEnvs, secretEnvs map[string]string, allowlist []string, secretAllowedHosts map[string][]string) map[string]string
	// UnregisterSession removes the proxy session for the given guest IP.
	UnregisterSession(guestIP string)
	// GetSessionTokens returns the sealed token → real value map for persisting during hibernate.
	GetSessionTokens(guestIP string) map[string]string
	// GetSessionAllowlist returns the egress allowlist for persisting during hibernate.
	GetSessionAllowlist(guestIP string) []string
	// GetSessionTokenHosts returns the per-token host restrictions for persisting during hibernate.
	GetSessionTokenHosts(guestIP string) map[string][]string
	// ReregisterSession re-creates a proxy session from a persisted token map (used on wake).
	ReregisterSession(sandboxID, guestIP string, tokens map[string]string, allowlist []string, tokenHosts map[string][]string)
	// CACertPEM returns the CA certificate PEM for injection into the VM trust store.
	CACertPEM() []byte
}

// Config holds configuration for the QEMU Manager.
type Config struct {
	DataDir         string // base data directory (e.g., /data)
	KernelPath      string // path to vmlinux kernel
	ImagesDir       string // path to base rootfs images
	QEMUBin         string // path to qemu-system-x86_64 binary
	AgentBinaryPath string // path to osb-agent binary on host (for hot-upgrade)
	AgentVersion    string // expected agent version (for hot-upgrade check)
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

	// Checkpoint cache mutex: write-locked during cache creation, read-locked during fork
	checkpointCacheMu sync.RWMutex

	// Golden snapshot for fast VM creation
	goldenDir     string // path to golden snapshot dir (empty = not available)
	goldenCID     uint32 // CID used when the golden snapshot was created
	goldenGuestIP string // guest IP baked into the golden snapshot
	goldenHostIP  string // host IP of the golden subnet (for temp addr on TAP)

	// Metadata service callbacks (set via SetMetadataCallbacks)
	onSandboxReady   func(sandboxID, guestIP, template string, startedAt time.Time)
	onSandboxDestroy func(sandboxID string)

	secretsProxy SecretsProxyIntegration // nil if secrets proxy is not configured
}

// NewManager creates a new QEMU-backed sandbox manager.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("DataDir is required")
	}
	if cfg.KernelPath == "" {
		cfg.KernelPath = filepath.Join(cfg.DataDir, "vmlinux")
	}
	if cfg.ImagesDir == "" {
		cfg.ImagesDir = filepath.Join(cfg.DataDir, "images")
	}
	if cfg.QEMUBin == "" {
		cfg.QEMUBin = "qemu-system-x86_64"
	}
	if cfg.DefaultMemoryMB == 0 {
		cfg.DefaultMemoryMB = 256
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

	// Verify the data directory supports reflink copy (required for snapshot safety).
	if err := checkReflinkSupport(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("data directory does not support reflink: %w (XFS with reflink=1 required)", err)
	}

	// Clean up stale archive-staging directories from previous crashes
	staleStaging, _ := filepath.Glob(filepath.Join(cfg.DataDir, "sandboxes", "*", "archive-staging"))
	for _, d := range staleStaging {
		os.RemoveAll(d)
		log.Printf("qemu: cleaned up stale archive-staging: %s", d)
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

// SetSecretsProxy configures the secrets proxy integration for token substitution.
// Must be called before any sandboxes are created.
func (m *Manager) SetSecretsProxy(sp SecretsProxyIntegration) {
	m.secretsProxy = sp
}

// sealSandboxEnvs runs cfg.Envs through the secrets proxy to swap real values
// for sealed tokens, registers a proxy session for the guest IP, and writes the
// proxy CA cert into the guest trust store. Returns the env map that should be
// injected into the VM (sealed tokens + HTTP_PROXY/CA env vars), or cfg.Envs
// unchanged if the secrets proxy is not configured.
//
// IMPORTANT: This is the QEMU equivalent of internal/firecracker/manager.go:451.
// The QEMU manager originally shipped without this call, so secrets were
// injected into the guest as plaintext — see git history for f2e64e3.
func (m *Manager) sealSandboxEnvs(ctx context.Context, sandboxID string, netCfg *NetworkConfig, agent *AgentClient, cfg types.SandboxConfig) map[string]string {
	// If the secrets proxy is not configured, just merge the two maps with
	// user (Envs) winning. This keeps non-prod environments working without
	// the proxy registered while preserving the user-wins precedence rule.
	if m.secretsProxy == nil {
		if len(cfg.SecretEnvs) == 0 {
			return cfg.Envs
		}
		merged := make(map[string]string, len(cfg.Envs)+len(cfg.SecretEnvs))
		for k, v := range cfg.SecretEnvs {
			merged[k] = v
		}
		for k, v := range cfg.Envs {
			merged[k] = v
		}
		return merged
	}
	if len(cfg.Envs) == 0 && len(cfg.SecretEnvs) == 0 {
		return cfg.Envs
	}
	sealed := m.secretsProxy.CreateSealedEnvs(sandboxID, netCfg.GuestIP, netCfg.HostIP, cfg.Envs, cfg.SecretEnvs, cfg.EgressAllowlist, cfg.SecretAllowedHosts)
	if sealed == nil {
		return cfg.Envs
	}
	// Inject the proxy CA cert so HTTPS apps inside the VM trust the MITM proxy.
	// CreateSealedEnvs already populates NODE_EXTRA_CA_CERTS / REQUESTS_CA_BUNDLE /
	// SSL_CERT_FILE pointing at this path; without the file the env vars are dead.
	if certPEM := m.secretsProxy.CACertPEM(); len(certPEM) > 0 {
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := agent.WriteFile(writeCtx, "/usr/local/share/ca-certificates/opensandbox-proxy.crt", certPEM); err != nil {
			log.Printf("qemu: warning: write proxy CA cert failed for %s: %v", sandboxID, err)
		}
		cancel()
	}
	return sealed
}

// PrepareGoldenSnapshot boots a temporary VM, waits for the agent, then
// hibernates it to create a reusable snapshot. Subsequent Create() calls
// restore from this snapshot instead of cold-booting, cutting start time
// from ~10s to ~1-2s.
func (m *Manager) PrepareGoldenSnapshot() error {
	goldenDir := filepath.Join(m.cfg.DataDir, "golden")
	memFile := filepath.Join(goldenDir, "mem")
	rootfsFile := filepath.Join(goldenDir, "rootfs.qcow2")

	// If a previous PrepareGoldenSnapshot failed midway, clean up partial files
	preparingMarker := filepath.Join(goldenDir, ".preparing")
	if fileExists(preparingMarker) {
		log.Printf("qemu: golden snapshot has .preparing marker — previous build failed, rebuilding")
		os.RemoveAll(goldenDir)
	}

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

	// Write marker so partial failures are detected on next startup
	if err := os.WriteFile(preparingMarker, []byte("in-progress"), 0644); err != nil {
		return fmt.Errorf("write preparing marker: %w", err)
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

	// Save the workspace ext4 UUID so createFromGolden can stamp new workspaces
	// with the same UUID. The golden kernel caches ext4 metadata (superblock,
	// journal) by UUID — a new workspace with a different UUID triggers checksum
	// errors ("Bad message" / EBADMSG) because the cached metadata doesn't match.
	if wsUUID, uuidErr := getWorkspaceUUID(workspaceFile); uuidErr == nil {
		os.WriteFile(filepath.Join(goldenDir, "workspace_uuid"), []byte(wsUUID), 0644)
		log.Printf("qemu: golden: workspace UUID=%s", wsUUID)
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

	// Upgrade the agent in the golden VM if the rootfs image has an older version.
	// This ensures every sandbox created from golden has the correct agent.
	goldenVM := &VMInstance{
		ID:            "golden",
		agent:         agentClient,
		agentSockPath: agentSockPath,
	}
	m.upgradeAgentIfNeeded(context.Background(), goldenVM)
	agentClient = goldenVM.agent // may have been swapped by upgrade

	// Load virtio_mem kernel module for memory scaling support.
	// The module must be loaded before the golden snapshot so that restored
	// VMs can use virtio-mem for dynamic memory add/remove.
	// Try modprobe first (handles signed modules + dependencies), fall back to insmod.
	modCtx, modCancel := context.WithTimeout(context.Background(), 10*time.Second)
	modResp, modErr := agentClient.Exec(modCtx, &pb.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "modprobe virtio_mem 2>/dev/null || insmod /lib/modules/$(uname -r)/kernel/drivers/virtio/virtio_mem.ko 2>/dev/null; grep -q virtio_mem /proc/modules"},
	})
	modCancel()
	if modErr != nil || (modResp != nil && modResp.ExitCode != 0) {
		return fmt.Errorf("virtio_mem module failed to load (memory scaling will not work) — ensure the rootfs has kmod installed and virtio_mem.ko is present: %v", modErr)
	}
	log.Printf("qemu: golden: virtio_mem module loaded")

	// Unmount /home/sandbox and sync before snapshot — the golden migration state
	// includes virtio-blk device state (ring buffers, pending I/O). If the data disk
	// is mounted when we snapshot, those stale I/O ops will corrupt any fresh
	// workspace.qcow2 that createFromGolden boots with.
	umountCtx, umountCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, umountErr := agentClient.Exec(umountCtx, &pb.ExecRequest{
		Command:   "/bin/sh",
		Args: []string{"-c", "umount -f /home/sandbox 2>/dev/null; sync; echo 3 > /proc/sys/vm/drop_caches; echo 3 > /proc/sys/vm/drop_caches; blockdev --flushbufs /dev/vdb 2>/dev/null; true"},
		RunAsRoot: true,
	})
	umountCancel()
	if umountErr != nil {
		log.Printf("qemu: golden: umount /home/sandbox failed (non-fatal): %v", umountErr)
	} else {
		log.Printf("qemu: golden: /home/sandbox unmounted and synced")
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

	// Remove preparing marker — golden snapshot is complete
	os.Remove(preparingMarker)

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

	// Create fresh workspace as qcow2 with the golden's ext4 UUID.
	// The golden kernel caches ext4 metadata by UUID — mismatched UUIDs cause
	// "Bad message" (EBADMSG) checksum errors on the restored workspace.
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")
	diskMB := m.cfg.DefaultDiskMB
	var goldenWSUUID string
	if data, readErr := os.ReadFile(filepath.Join(m.goldenDir, "workspace_uuid")); readErr == nil {
		goldenWSUUID = strings.TrimSpace(string(data))
	}
	if err := CreateWorkspace(workspacePath, diskMB, goldenWSUUID); err != nil {
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
		baseMemoryMB:  memMB,
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
	agentClient, err = m.waitForAgentSocket(context.Background(), agentSockPath, 30*time.Second)
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

	// Mount /home/sandbox — the data disk is mounted directly as the user's home.
	// The golden snapshot was taken with it unmounted to keep vdb device state clean.
	// Drop caches first: the golden VM's kernel has cached ext4 metadata from the
	// golden workspace. The new sandbox has a DIFFERENT workspace qcow2 on the same
	// virtio-blk device. Without dropping caches, the kernel uses stale superblock/
	// journal data → ext4 checksum errors ("Bad message").
	mountCtx, mountCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, mountErr := agentClient.Exec(mountCtx, &pb.ExecRequest{
		Command: "/bin/sh",
		Args: []string{"-c", strings.Join([]string{
			"echo 3 > /proc/sys/vm/drop_caches",
			"echo 3 > /proc/sys/vm/drop_caches",
			"mount /dev/vdb /home/sandbox 2>/dev/null || true",
			"chown 1000:1000 /home/sandbox",
		}, " && ")},
		RunAsRoot: true,
	})
	mountCancel()
	if mountErr != nil {
		log.Printf("qemu: golden-create %s: mount /home/sandbox failed: %v", id, mountErr)
	}
	log.Printf("qemu: golden-create %s: network patched (%dms)", id, time.Since(t0).Milliseconds())

	envsToInject := m.sealSandboxEnvs(context.Background(), id, netCfg, agentClient, cfg)
	if len(envsToInject) > 0 {
		envCtx, envCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := agentClient.SetEnvs(envCtx, envsToInject); err != nil {
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
		if writeErr := os.WriteFile(filepath.Join(sandboxDir, "sandbox-meta.json"), metaJSON, 0644); writeErr != nil {
			log.Printf("qemu: WARNING: failed to write sandbox-meta.json for %s: %v", sandboxDir, writeErr)
		}
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
			"ip route add default via %s && "+
			"echo 'nameserver 8.8.8.8' > /etc/resolv.conf && "+
			"echo 'nameserver 1.1.1.1' >> /etc/resolv.conf && "+
			"grep -q \"$(hostname)\" /etc/hosts || echo \"127.0.0.1 $(hostname)\" >> /etc/hosts",
		netCfg.GuestIP, prefixLen, netCfg.HostIP,
	)

	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := agent.Exec(execCtx, &pb.ExecRequest{
		Command:        "/bin/sh",
		Args:           []string{"-c", script},
		TimeoutSeconds: 5,
		RunAsRoot:      true,
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
		"-m", fmt.Sprintf("%dM,maxmem=16G", memMB),
		// virtio-mem: pluggable memory pool. Scale via QMP qom-set requested-size.
		// 15GB max + base gives 16GB ceiling. Block size 128MB for granularity.
		"-object", "memory-backend-ram,id=vmem0,size=15360M",
		"-device", "virtio-mem-pci,memdev=vmem0,id=vm0,block-size=128M,requested-size=0",
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
	// Check disk space before creating — refuse if >95% to prevent ENOSPC corruption
	if usage, err := diskUsagePercent(m.cfg.DataDir); err == nil && usage > 95 {
		return nil, fmt.Errorf("disk usage at %d%%, refusing new sandbox (threshold: 95%%)", usage)
	}

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
		baseMemoryMB:  memMB,
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

	envsToInject := m.sealSandboxEnvs(context.Background(), id, netCfg, agentClient, cfg)
	if len(envsToInject) > 0 {
		envCtx, envCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := agentClient.SetEnvs(envCtx, envsToInject); err != nil {
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
		if writeErr := os.WriteFile(filepath.Join(sandboxDir, "sandbox-meta.json"), metaJSON, 0644); writeErr != nil {
			log.Printf("qemu: WARNING: failed to write sandbox-meta.json for %s: %v", sandboxDir, writeErr)
		}
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
	pingFailures := 0

	for time.Now().Before(deadline) {
		attempts++
		tAttempt := time.Now()
		client, err := NewAgentClientSocket(socketPath)
		if err != nil {
			lastErr = err
			pingFailures = 0
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
			pingFailures++
			lastErr = err
			client.Close()
			if pingFailures <= 3 || pingFailures%5 == 0 {
				log.Printf("qemu: waitForAgentSocket: attempt %d ping %s failed (%dms, streak=%d): %v",
					attempts, socketPath, time.Since(tAttempt).Milliseconds(), pingFailures, err)
			}
			// After several ping failures, back off longer to give the guest
			// agent time to fully resume after loadvm.
			if pingFailures >= 5 {
				time.Sleep(500 * time.Millisecond)
			} else {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}

		log.Printf("qemu: waitForAgentSocket: connected to %s on attempt %d (%dms total)",
			socketPath, attempts, time.Since(t0).Milliseconds())
		return client, nil
	}

	return nil, fmt.Errorf("agent not ready after %v (%d attempts, %d ping failures): %v", timeout, attempts, pingFailures, lastErr)
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

	// Try QMP quit first, then wait for QEMU to exit before cleaning up files
	if vm.qmp != nil {
		_ = vm.qmp.Quit()
		vm.qmp.Close()
	}

	if vm.cmd != nil && vm.cmd.Process != nil {
		// Wait for QEMU to exit (with timeout) before removing files it may have open
		waitDone := make(chan error, 1)
		go func() { waitDone <- vm.cmd.Wait() }()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
			vm.cmd.Process.Kill()
			<-waitDone
		}
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

	// Wait for any in-flight hibernate archive to complete before deleting files.
	// Without this, os.RemoveAll races with the archive goroutine reading from
	// archive-staging/ inside sandboxDir.
	if vm.archiveDone != nil {
		select {
		case <-vm.archiveDone:
		case <-time.After(5 * time.Minute):
			log.Printf("qemu: CRITICAL: destroy %s: archive goroutine stuck for 5min, force cleanup", vm.ID)
		}
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

// ReadFileStream returns a streaming reader for a file in the VM.
func (m *Manager) ReadFileStream(ctx context.Context, sandboxID, path string) (io.ReadCloser, int64, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return nil, 0, err
	}
	return vm.agent.ReadFileStream(ctx, path)
}

// WriteFileStream writes a file from a reader in the VM via streaming.
func (m *Manager) WriteFileStream(ctx context.Context, sandboxID, path string, mode uint32, r io.Reader) (int64, error) {
	vm, err := m.getReadyVM(ctx, sandboxID)
	if err != nil {
		return 0, err
	}
	return vm.agent.WriteFileStream(ctx, path, mode, r)
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

	// virtio-mem: adjust pluggable memory to match requested total
	if maxMemoryBytes > 0 && vm.qmp != nil {
		totalDesiredMB := int(maxMemoryBytes) / (1024 * 1024)
		additionalMB := totalDesiredMB - vm.baseMemoryMB
		if additionalMB < 0 {
			additionalMB = 0
		}
		// Round up to 128MB block size
		additionalMB = ((additionalMB + 127) / 128) * 128
		if additionalMB != vm.virtioMemRequestedMB {
			if err := vm.qmp.SetVirtioMemSize(additionalMB); err != nil {
				log.Printf("qemu: virtio-mem %s: set %dMB failed: %v — capping cgroup to current VM memory", sandboxID, additionalMB, err)
				// Cap cgroup memory limit to actual VM memory so we don't set
				// cgroup higher than the physical RAM available to the guest.
				maxMemoryBytes = int64(vm.MemoryMB) * 1024 * 1024
			} else {
				vm.virtioMemRequestedMB = additionalMB
				vm.MemoryMB = vm.baseMemoryMB + additionalMB
				log.Printf("qemu: virtio-mem %s: %dMB additional (total %dMB)", sandboxID, additionalMB, vm.MemoryMB)
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

	if !vm.opMu.TryLock() {
		return nil, fmt.Errorf("another operation is in progress on sandbox %s — try again shortly", sandboxID)
	}
	defer vm.opMu.Unlock()

	// Check if agent needs upgrading before we hibernate.
	// If so, we'll do a background wake→upgrade→re-hibernate after returning
	// the hibernate result to the client, so the next wake is instant.
	needsUpgrade := false
	if m.cfg.AgentVersion != "" && m.cfg.AgentVersion != "dev" && vm.agent != nil {
		vCtx, vCancel := context.WithTimeout(ctx, 3*time.Second)
		ver, err := vm.agent.GetVersion(vCtx)
		vCancel()
		if err == nil && ver != m.cfg.AgentVersion {
			needsUpgrade = true
		}
	}

	result, err := m.doHibernate(ctx, vm, checkpointStore)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	delete(m.vms, sandboxID)
	m.mu.Unlock()

	if needsUpgrade && checkpointStore != nil {
		go func() {
			log.Printf("qemu: post-hibernate upgrade %s: agent outdated, doing wake→upgrade→re-hibernate", sandboxID)
			result := m.rollingUpgradeOne(sandboxID, checkpointStore)
			log.Printf("qemu: post-hibernate upgrade %s: %s", sandboxID, result)
		}()
	}

	return result, nil
}

// Wake restores a VM from a snapshot.
// Guards against double-wake: if the sandbox is already running, returns it.
func (m *Manager) Wake(ctx context.Context, sandboxID string, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	// Prevent double wake — if sandbox is already running, return it
	m.mu.RLock()
	if existing, ok := m.vms[sandboxID]; ok {
		m.mu.RUnlock()
		log.Printf("qemu: wake %s: already running, returning existing VM", sandboxID)
		return vmToSandbox(existing), nil
	}
	m.mu.RUnlock()
	return m.doWake(ctx, sandboxID, checkpointKey, checkpointStore, timeout)
}

// TemplateCachePath returns "" — not implemented.
func (m *Manager) TemplateCachePath(templateID, filename string) string {
	return ""
}

// CleanCheckpointCache removes the local cache for a checkpoint.
// Acquires checkpointCacheMu write lock to ensure no ForkFromCheckpoint is
// reading from the cache concurrently.
func (m *Manager) CleanCheckpointCache(checkpointID string) {
	m.checkpointCacheMu.Lock()
	defer m.checkpointCacheMu.Unlock()
	cacheDir := m.checkpointCacheDir(checkpointID)
	if err := os.RemoveAll(cacheDir); err != nil {
		log.Printf("qemu: clean checkpoint cache %s: %v", checkpointID, err)
	}
}

// checkpointCacheDir returns the local cache directory for a checkpoint's qcow2 files.
// Uses "checkpoint-snapshots/" (not "checkpoints/") to avoid collision with the S3
// checkpoint cache which stores tar.zst files in the "checkpoints/" directory.
func (m *Manager) checkpointCacheDir(checkpointID string) string {
	return filepath.Join(m.cfg.DataDir, "checkpoint-snapshots", checkpointID)
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

	// Reject if another destructive operation (checkpoint, hibernate, restore) is in progress.
	// Without this, rapid-fire checkpoints queue up and the agent gets into a bad state
	// from overlapping SIGUSR1/reconnect cycles.
	if !vm.opMu.TryLock() {
		return "", "", fmt.Errorf("another operation is in progress on sandbox %s — try again shortly", sandboxID)
	}
	defer vm.opMu.Unlock()

	t0 := time.Now()

	if vm.qmp == nil {
		return "", "", fmt.Errorf("QMP connection not available for %s", sandboxID)
	}

	// Sync filesystem before snapshot. Flush dirty pages so the drives are
	// consistent when we copy them while the VM is paused.
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, syncErr := vm.agent.Exec(syncCtx, &pb.ExecRequest{
			Command:   "/bin/sh",
			Args:      []string{"-c", "sync; blockdev --flushbufs /dev/vda 2>/dev/null; blockdev --flushbufs /dev/vdb 2>/dev/null; sync; kill -USR1 1"},
			RunAsRoot: true,
		})
		cancel()
		if syncErr != nil {
			log.Printf("qemu: CreateCheckpoint %s/%s: sync failed: %v", sandboxID, checkpointID, syncErr)
		}
		// Close the agent connection before pausing — the agent's SIGUSR1 handler
		// resets the virtio-serial listener so forks start with a clean Accept state.
		vm.agent.Close()
		vm.agent = nil
		time.Sleep(500 * time.Millisecond) // let guest process SIGUSR1
	}

	// Migration-based checkpoint: pause the VM, dump memory + device state to a
	// file, copy the quiesced drives, then resume. This avoids savevm/loadvm which
	// has a ~0.5% virtio-serial state corruption issue after loadvm.
	if vm.qmp == nil {
		return "", "", fmt.Errorf("QMP connection not available for %s", sandboxID)
	}

	// Pause the VM — all I/O stops, drives are quiesced.
	if err := vm.qmp.Stop(); err != nil {
		return "", "", fmt.Errorf("stop VM: %w", err)
	}

	// Set up the cache staging directory before migration so we can dump directly there.
	cacheDir := m.checkpointCacheDir(checkpointID)
	stagingDir := cacheDir + ".staging"
	if mkErr := os.MkdirAll(filepath.Join(stagingDir, "snapshot"), 0755); mkErr != nil {
		_ = vm.qmp.Cont() // resume on failure
		return "", "", fmt.Errorf("mkdir staging: %w", mkErr)
	}

	// Dump memory + device state via QEMU migration protocol.
	// The migrate command writes the full VM state (RAM, device registers, virtio
	// queue state) to the file. Unlike savevm, this produces a standalone file
	// decoupled from the qcow2 drives.
	memFile := filepath.Join(stagingDir, "mem")
	migrateURI := fmt.Sprintf("exec:cat > %s", memFile)
	if err := vm.qmp.Migrate(migrateURI); err != nil {
		_ = vm.qmp.Cont()
		os.RemoveAll(stagingDir)
		return "", "", fmt.Errorf("migrate: %w", err)
	}
	if err := vm.qmp.WaitMigration(5 * time.Minute); err != nil {
		_ = vm.qmp.Cont()
		os.RemoveAll(stagingDir)
		return "", "", fmt.Errorf("wait migration: %w", err)
	}

	// Copy drives while VM is still paused — guaranteed consistent.
	srcRootfs := filepath.Join(vm.sandboxDir, "rootfs.qcow2")
	srcWorkspace := filepath.Join(vm.sandboxDir, "workspace.qcow2")
	_ = copyFileReflink(srcRootfs, filepath.Join(stagingDir, "rootfs.qcow2"))
	_ = copyFileReflink(srcWorkspace, filepath.Join(stagingDir, "workspace.qcow2"))

	// Compress the memory dump for faster fork I/O.
	memZst := memFile + ".zst"
	compressCmd := exec.Command("zstd", "-3", "--rm", "-q", memFile, "-o", memZst)
	if compErr := compressCmd.Run(); compErr != nil {
		log.Printf("qemu: CreateCheckpoint %s/%s: zstd compress failed: %v (using uncompressed)", sandboxID, checkpointID, compErr)
		// Fall back to uncompressed — memFile still exists if zstd failed
	}

	log.Printf("qemu: CreateCheckpoint %s/%s: migration + drive copy complete (%dms)",
		sandboxID, checkpointID, time.Since(t0).Milliseconds())

	// Resume the source VM.
	if err := vm.qmp.Cont(); err != nil {
		log.Printf("qemu: CreateCheckpoint %s/%s: WARNING: cont failed after migration: %v", sandboxID, checkpointID, err)
	}

	// Reconnect agent (connection was closed before migration).
	agentClient, reconnErr := m.waitForAgentSocket(context.Background(), vm.agentSockPath, 10*time.Second)
	if reconnErr != nil {
		log.Printf("qemu: CreateCheckpoint %s/%s: agent reconnect failed (attempt 1): %v, retrying", sandboxID, checkpointID, reconnErr)
		agentClient, reconnErr = m.waitForAgentSocket(context.Background(), vm.agentSockPath, 30*time.Second)
	}
	if reconnErr == nil {
		vm.agent = agentClient
	} else {
		log.Printf("qemu: CreateCheckpoint %s/%s: CRITICAL: agent reconnect failed, killing orphan VM", sandboxID, checkpointID)
		if vm.qmp != nil {
			_ = vm.qmp.Quit()
			vm.qmp.Close()
			vm.qmp = nil
		}
		m.mu.Lock()
		delete(m.vms, sandboxID)
		m.mu.Unlock()
	}

	// Write metadata and finalize cache.
	rootfsKey = fmt.Sprintf("checkpoints/%s/%s/rootfs.tar.zst", sandboxID, checkpointID)
	workspaceKey = fmt.Sprintf("checkpoints/%s/%s/workspace.tar.zst", sandboxID, checkpointID)

	meta := &SnapshotMeta{
		SandboxID:    vm.ID,
		Network:      vm.network,
		GuestCID:     vm.guestCID,
		GuestMAC:     vm.guestMAC,
		BootArgs:     vm.bootArgs,
		CpuCount:     vm.CpuCount,
		MemoryMB:     vm.MemoryMB,
		BaseMemoryMB: vm.baseMemoryMB,
		Template:     vm.Template,
		GuestPort:    vm.GuestPort,
		SnapshotedAt: time.Now(),
	}
	// Persist secrets proxy state so RestoreFromCheckpoint can re-register the session.
	if m.secretsProxy != nil && vm.network != nil {
		meta.SealedTokens = m.secretsProxy.GetSessionTokens(vm.network.GuestIP)
		meta.EgressAllowlist = m.secretsProxy.GetSessionAllowlist(vm.network.GuestIP)
		meta.TokenHosts = m.secretsProxy.GetSessionTokenHosts(vm.network.GuestIP)
	}
	metaJSON, _ := json.Marshal(meta)
	_ = os.WriteFile(filepath.Join(stagingDir, "snapshot", "snapshot-meta.json"), metaJSON, 0644)

	// Atomic rename into cache under write lock.
	m.checkpointCacheMu.Lock()
	os.RemoveAll(cacheDir)
	if renameErr := os.Rename(stagingDir, cacheDir); renameErr != nil {
		log.Printf("qemu: checkpoint %s: rename staging to cache failed: %v", checkpointID, renameErr)
	}
	m.checkpointCacheMu.Unlock()

	log.Printf("qemu: checkpoint %s: cache saved (%dms)", checkpointID, time.Since(t0).Milliseconds())

	// Upload full checkpoint to S3 in the background so cross-worker forks can download it.
	// The archive includes drives + memory dump + metadata — everything ForkFromCheckpoint needs.
	// Image builder waits for upload to finish (via WaitUploads) before forking.
	if checkpointStore != nil {
		// Build list of files to archive
		var archiveFiles []string
		archiveFiles = append(archiveFiles, "rootfs.qcow2", "workspace.qcow2")
		if fileExists(filepath.Join(cacheDir, "mem.zst")) {
			archiveFiles = append(archiveFiles, "mem.zst")
		} else if fileExists(filepath.Join(cacheDir, "mem")) {
			archiveFiles = append(archiveFiles, "mem")
		}
		if fileExists(filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")) {
			archiveFiles = append(archiveFiles, filepath.Join("snapshot", "snapshot-meta.json"))
		}

		m.uploadWg.Add(1)
		go func() {
			defer m.uploadWg.Done()
			t1 := time.Now()

			archivePath := filepath.Join(cacheDir, "checkpoint.tar.zst")
			if err := createArchive(archivePath, cacheDir, archiveFiles); err != nil {
				log.Printf("qemu: checkpoint %s: archive failed: %v", checkpointID, err)
				return
			}
			defer os.Remove(archivePath)

			uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if _, err := checkpointStore.Upload(uploadCtx, rootfsKey, archivePath); err != nil {
				log.Printf("qemu: checkpoint %s: S3 upload failed: %v", checkpointID, err)
				return
			}

			log.Printf("qemu: checkpoint %s: S3 upload complete (%dms, files=%v)",
				checkpointID, time.Since(t1).Milliseconds(), archiveFiles)
		}()
	}

	if onReady != nil {
		onReady()
	}

	return rootfsKey, workspaceKey, nil
}

// RestoreFromCheckpoint reverts a sandbox to a checkpoint by killing the current
// QEMU process and starting a fresh one from the checkpoint's cached qcow2 drives.
// In-place loadvm corrupts the qcow2 COW layer because blocks written after the
// checkpoint aren't cleanly reverted. Fresh drives from the cache are always consistent.
func (m *Manager) RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	if !vm.opMu.TryLock() {
		return fmt.Errorf("another operation is in progress on sandbox %s — try again shortly", sandboxID)
	}
	defer vm.opMu.Unlock()

	t0 := time.Now()

	// Step 1: Kill the current VM
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}
	if vm.qmp != nil {
		_ = vm.qmp.Quit()
		vm.qmp.Close()
		vm.qmp = nil
	}
	if vm.cmd != nil && vm.cmd.Process != nil {
		done := make(chan error, 1)
		go func() { done <- vm.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			vm.cmd.Process.Kill()
			<-done
		}
	}

	// Step 2: Tear down old network
	if vm.network != nil {
		RemoveMetadataDNAT(vm.network.TAPName, vm.network.HostIP)
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	// Step 3: Copy fresh qcow2 drives from checkpoint cache
	m.checkpointCacheMu.RLock()
	cacheDir := m.checkpointCacheDir(checkpointID)
	cachedRootfs := filepath.Join(cacheDir, "rootfs.qcow2")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.qcow2")
	if !fileExists(cachedRootfs) || !fileExists(cachedWorkspace) {
		m.checkpointCacheMu.RUnlock()
		return fmt.Errorf("checkpoint %s: qcow2 files not found in cache", checkpointID)
	}

	// Read checkpoint metadata for base topology.
	var cpMeta SnapshotMeta
	if metaData, err := os.ReadFile(filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")); err == nil {
		json.Unmarshal(metaData, &cpMeta)
	}

	// Determine restore mode: prefer migration-based (-incoming) over savevm (loadvm).
	// CreateCheckpoint uses QEMU migrate which produces a standalone mem dump file.
	// loadvm only works with savevm-based internal snapshots in the qcow2.
	memZst := filepath.Join(cacheDir, "mem.zst")
	memRaw := filepath.Join(cacheDir, "mem")
	var incomingURI string
	if fileExists(memZst) {
		incomingURI = fmt.Sprintf("exec:zstdcat %s", memZst)
	} else if fileExists(memRaw) {
		incomingURI = fmt.Sprintf("exec:cat %s", memRaw)
	}

	snapshotName := "cp-" + checkpointID
	if data, err := os.ReadFile(filepath.Join(cacheDir, "snapshot-name")); err == nil {
		snapshotName = strings.TrimSpace(string(data))
	}

	sandboxDir := vm.sandboxDir
	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")

	// Remove old drives and copy fresh ones
	os.Remove(rootfsPath)
	os.Remove(workspacePath)
	if err := copyFileReflink(cachedRootfs, rootfsPath); err != nil {
		m.checkpointCacheMu.RUnlock()
		return fmt.Errorf("copy rootfs from cache: %w", err)
	}
	if err := copyFileReflink(cachedWorkspace, workspacePath); err != nil {
		m.checkpointCacheMu.RUnlock()
		return fmt.Errorf("copy workspace from cache: %w", err)
	}
	m.checkpointCacheMu.RUnlock()

	// Step 4: Allocate new network
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		return fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return fmt.Errorf("create TAP: %w", err)
	}
	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = vm.GuestPort
	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return fmt.Errorf("add DNAT: %w", err)
	}
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: RestoreFromCheckpoint %s: metadata DNAT failed: %v", sandboxID, err)
	}

	// Step 5: Start fresh QEMU
	guestMAC := generateMAC(sandboxID)
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 root=/dev/vda rw ip=%s::%s:%s::eth0:off init=/sbin/init osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(qmpSockPath)
	os.Remove(agentSockPath)

	// Boot with checkpoint's base topology so restore succeeds.
	bootCpus := cpMeta.CpuCount
	if bootCpus <= 0 {
		bootCpus = vm.CpuCount
	}
	bootMemMB := cpMeta.BaseMemoryMB
	if bootMemMB <= 0 {
		bootMemMB = vm.baseMemoryMB
	}
	if bootMemMB <= 0 {
		bootMemMB = m.cfg.DefaultMemoryMB
	}
	// Remember what the user had so we can re-scale after restore
	desiredMemMB := vm.MemoryMB

	logFile, _ := os.Create(filepath.Join(sandboxDir, "qemu.log"))
	args := m.buildQEMUArgs(bootCpus, bootMemMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)

	if incomingURI != "" {
		// Migration-based restore: QEMU loads state from the mem dump file.
		args = append(args, "-incoming", incomingURI)
	} else {
		// Savevm-based fallback: start paused, then loadvm.
		args = append(args, "-S")
	}

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("start QEMU: %w", err)
	}
	if logFile != nil {
		logFile.Close()
	}

	// Step 6: QMP connect + restore + cont
	qmpClient, err := waitForQMP(qmpSockPath, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("QMP connect: %w", err)
	}

	if incomingURI != "" {
		// Migration-based: wait for incoming migration to finish loading, then resume.
		if err := m.waitForMigrationReady(qmpClient, 30*time.Second); err != nil {
			qmpClient.Close()
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, "")
			return fmt.Errorf("migration load: %w", err)
		}
	} else {
		// Savevm fallback: load the internal snapshot.
		if err := qmpClient.LoadVM(snapshotName); err != nil {
			qmpClient.Close()
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, "")
			return fmt.Errorf("loadvm: %w", err)
		}
	}

	// Re-scale virtio-mem BEFORE cont — the VM is paused, so the kernel sees full
	// memory immediately on resume. Without this, restored processes that were using
	// >baseMemMB would OOM before the post-resume re-scale completes.
	if desiredMemMB > bootMemMB {
		additionalMB := desiredMemMB - bootMemMB
		additionalMB = ((additionalMB + 127) / 128) * 128 // align to 128MB block size
		if err := qmpClient.SetVirtioMemSize(additionalMB); err != nil {
			log.Printf("qemu: RestoreFromCheckpoint %s: pre-resume scale to %dMB failed: %v (continuing with base %dMB)",
				sandboxID, desiredMemMB, err, bootMemMB)
		} else {
			log.Printf("qemu: RestoreFromCheckpoint %s: pre-resume scale to %dMB (base=%d + virtio-mem=%d)",
				sandboxID, bootMemMB+additionalMB, bootMemMB, additionalMB)
		}
	}

	if err := qmpClient.Cont(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("cont: %w", err)
	}

	// Step 7: Reconnect agent + patch network
	agentClient, err := m.waitForAgentSocket(context.Background(), agentSockPath, 30*time.Second)
	if err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("agent connect: %w", err)
	}

	if err := patchGuestNetwork(context.Background(), agentClient, netCfg); err != nil {
		log.Printf("qemu: RestoreFromCheckpoint %s: network patch failed: %v", sandboxID, err)
	}
	if err := syncGuestClock(context.Background(), agentClient); err != nil {
		log.Printf("qemu: RestoreFromCheckpoint %s: clock sync failed: %v", sandboxID, err)
	}

	// Re-register secrets proxy session from checkpoint metadata.
	if m.secretsProxy != nil && len(cpMeta.SealedTokens) > 0 {
		m.secretsProxy.ReregisterSession(sandboxID, netCfg.GuestIP, cpMeta.SealedTokens, cpMeta.EgressAllowlist, cpMeta.TokenHosts)
		log.Printf("qemu: RestoreFromCheckpoint %s: re-registered secrets proxy session (%d tokens)", sandboxID, len(cpMeta.SealedTokens))
	}

	// Step 8: Update VM instance
	vm.cmd = cmd
	vm.qmp = qmpClient
	vm.agent = agentClient
	vm.network = netCfg
	vm.HostPort = hostPort
	vm.qmpSockPath = qmpSockPath
	vm.agentSockPath = agentSockPath
	vm.guestMAC = guestMAC
	vm.bootArgs = bootArgs
	vm.pid = cmd.Process.Pid
	vm.CpuCount = bootCpus
	vm.baseMemoryMB = bootMemMB
	if desiredMemMB > bootMemMB {
		additionalMB := ((desiredMemMB - bootMemMB + 127) / 128) * 128
		vm.MemoryMB = bootMemMB + additionalMB
		vm.virtioMemRequestedMB = additionalMB
	} else {
		vm.MemoryMB = bootMemMB
		vm.virtioMemRequestedMB = 0
	}

	// Don't upgrade agent during restore — the checkpoint was created from the
	// same rootfs, and the upgrade's syscall.Exec + reconnect cycle is fragile.
	// Agent upgrades happen on golden snapshot creation and wake instead.

	log.Printf("qemu: RestoreFromCheckpoint %s/%s: complete (%dms, port=%d, tap=%s)",
		sandboxID, checkpointID, time.Since(t0).Milliseconds(), hostPort, netCfg.TAPName)
	return nil
}

// ForkFromCheckpoint creates a new sandbox from a checkpoint's saved state.
// The new sandbox gets its own network, CID, and drives (reflinked from cache).
func (m *Manager) ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error) {
	t0 := time.Now()

	// Lock checkpoint cache for reading — prevents race with CreateCheckpoint writing cache
	m.checkpointCacheMu.RLock()
	cacheDir := m.checkpointCacheDir(checkpointID)
	metaPath := filepath.Join(cacheDir, "snapshot", "snapshot-meta.json")

	cachedRootfs := filepath.Join(cacheDir, "rootfs.qcow2")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.qcow2")
	if !fileExists(cachedRootfs) || !fileExists(cachedWorkspace) {
		m.checkpointCacheMu.RUnlock()
		return nil, fmt.Errorf("checkpoint %s: qcow2 files not found in cache", checkpointID)
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
		m.checkpointCacheMu.RUnlock()
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// Copy qcow2 drives (contain snapshot data)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")
	if err := copyFileReflink(cachedRootfs, rootfsPath); err != nil {
		m.checkpointCacheMu.RUnlock()
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy rootfs: %w", err)
	}
	if err := copyFileReflink(cachedWorkspace, workspacePath); err != nil {
		m.checkpointCacheMu.RUnlock()
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy workspace: %w", err)
	}
	m.checkpointCacheMu.RUnlock()

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

	// Use checkpoint's CPU/memory for loadvm topology match.
	// savevm captures a fixed CPU topology — loadvm fails silently
	// if the new QEMU has a different core count.
	cpus := meta.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := meta.BaseMemoryMB
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
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")

	// Determine the migration memory file — prefer zstd-compressed.
	memZst := filepath.Join(cacheDir, "mem.zst")
	memRaw := filepath.Join(cacheDir, "mem")
	var incomingURI string
	if fileExists(memZst) {
		incomingURI = fmt.Sprintf("exec:zstdcat %s", memZst)
	} else if fileExists(memRaw) {
		incomingURI = fmt.Sprintf("exec:cat %s", memRaw)
	} else {
		// Backward compat: no mem file means this is a savevm-based checkpoint.
		// Fall back to the old loadvm path.
		incomingURI = ""
	}

	os.Remove(qmpSockPath)
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)

	if incomingURI != "" {
		// Migration-based restore: QEMU loads state from the mem dump file.
		args = append(args, "-incoming", incomingURI)
	} else {
		// Savevm-based fallback: start paused, then loadvm.
		args = append(args, "-S")
	}

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start qemu for fork: %w", err)
	}
	logFile.Close()

	log.Printf("qemu: ForkFromCheckpoint %s → %s: QEMU started (pid=%d, migration=%v)",
		checkpointID, id, cmd.Process.Pid, incomingURI != "")

	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("QMP connect: %w", err)
	}

	if incomingURI != "" {
		// Migration-based: wait for incoming migration to finish loading, then resume.
		if err := m.waitForMigrationReady(qmpClient, 30*time.Second); err != nil {
			qmpClient.Close()
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, sandboxDir)
			return nil, fmt.Errorf("migration load: %w", err)
		}
	} else {
		// Savevm fallback: load the internal snapshot.
		snapshotName := "cp-" + checkpointID
		if data, readErr := os.ReadFile(filepath.Join(cacheDir, "snapshot-name")); readErr == nil {
			snapshotName = strings.TrimSpace(string(data))
		}
		if err := qmpClient.LoadVM(snapshotName); err != nil {
			qmpClient.Close()
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, sandboxDir)
			return nil, fmt.Errorf("loadvm: %w", err)
		}
	}

	if err := qmpClient.Cont(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("QMP cont: %w", err)
	}
	log.Printf("qemu: ForkFromCheckpoint %s → %s: VM resumed (%dms), connecting agent...",
		checkpointID, id, time.Since(t0).Milliseconds())

	// Connect agent — migration-based restores are reliable (no virtio-serial
	// state issues), so use a shorter timeout.
	agentTimeout := 30 * time.Second
	if incomingURI != "" {
		agentTimeout = 10 * time.Second
	}
	var agent *AgentClient
	agent, err = m.waitForAgentSocket(context.Background(), agentSockPath, agentTimeout)
	if err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("agent connect: %w", err)
	}

	log.Printf("qemu: ForkFromCheckpoint %s → %s: agent connected, patching network...", checkpointID, id)

	// Patch network (fork gets new IPs) + sync clock
	if err := patchGuestNetwork(context.Background(), agent, netCfg); err != nil {
		log.Printf("qemu: ForkFromCheckpoint %s: network patch failed: %v", id, err)
	}
	if err := syncGuestClock(context.Background(), agent); err != nil {
		log.Printf("qemu: ForkFromCheckpoint %s: clock sync failed: %v", id, err)
	}

	// Set env vars (sealed via secrets proxy if configured)
	envsToInject := m.sealSandboxEnvs(context.Background(), id, netCfg, agent, cfg)
	if len(envsToInject) > 0 {
		envCtx, envCancel := context.WithTimeout(context.Background(), 5*time.Second)
		agent.SetEnvs(envCtx, envsToInject)
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
		baseMemoryMB:  memMB,
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

// upgradeAgentIfNeeded checks the agent version inside a running VM and
// hot-upgrades it if the version doesn't match. Transfers the binary in
// 256KB chunks, then tells the agent to re-exec. Blocks until complete.
func (m *Manager) upgradeAgentIfNeeded(ctx context.Context, vm *VMInstance) {
	if m.cfg.AgentVersion == "" || m.cfg.AgentVersion == "dev" || m.cfg.AgentBinaryPath == "" || vm.agent == nil {
		return
	}

	versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	agentVersion, err := vm.agent.GetVersion(versionCtx)
	if err != nil {
		log.Printf("qemu: agent %s: GetVersion unavailable (pre-upgrade agent, skipping)", vm.ID)
		return
	}
	if agentVersion == m.cfg.AgentVersion {
		return
	}
	log.Printf("qemu: agent %s: version mismatch (agent=%s, expected=%s), upgrading", vm.ID, agentVersion, m.cfg.AgentVersion)

	agentData, err := os.ReadFile(m.cfg.AgentBinaryPath)
	if err != nil {
		log.Printf("qemu: agent %s: upgrade failed (read binary): %v", vm.ID, err)
		return
	}

	t0 := time.Now()
	const chunkSize = 512 * 1024 // 512KB chunks
	tmpPath := "/tmp/osb-agent-new"

	createCtx, createCancel := context.WithTimeout(ctx, 5*time.Second)
	vm.agent.WriteFileBinary(createCtx, tmpPath, []byte{}, 0755)
	createCancel()

	for offset := 0; offset < len(agentData); offset += chunkSize {
		end := offset + chunkSize
		if end > len(agentData) {
			end = len(agentData)
		}

		chunkCtx, chunkCancel := context.WithTimeout(ctx, 120*time.Second)
		err := vm.agent.WriteFileBinary(chunkCtx, tmpPath+".chunk", agentData[offset:end], 0644)
		chunkCancel()
		if err != nil {
			log.Printf("qemu: agent %s: upgrade aborted (chunk at %d): %v", vm.ID, offset, err)
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
			vm.agent.Exec(cleanCtx, &pb.ExecRequest{
				Command:   "/bin/sh",
				Args:      []string{"-c", fmt.Sprintf("rm -f %s %s.chunk", tmpPath, tmpPath)},
				RunAsRoot: true,
			})
			cleanCancel()
			return
		}

		appendCtx, appendCancel := context.WithTimeout(ctx, 5*time.Second)
		_, _ = vm.agent.Exec(appendCtx, &pb.ExecRequest{
			Command:   "/bin/sh",
			Args:      []string{"-c", fmt.Sprintf("cat %s.chunk >> %s", tmpPath, tmpPath)},
			RunAsRoot: true,
		})
		appendCancel()
	}

	chmodCtx, chmodCancel := context.WithTimeout(ctx, 5*time.Second)
	_, _ = vm.agent.Exec(chmodCtx, &pb.ExecRequest{
		Command:   "/bin/sh",
		Args:      []string{"-c", fmt.Sprintf("chmod +x %s && rm -f %s.chunk", tmpPath, tmpPath)},
		RunAsRoot: true,
	})
	chmodCancel()

	log.Printf("qemu: agent %s: binary written (%d bytes, %d chunks, %dms)",
		vm.ID, len(agentData), (len(agentData)+chunkSize-1)/chunkSize, time.Since(t0).Milliseconds())

	upgradeCtx, upgradeCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := vm.agent.Upgrade(upgradeCtx, tmpPath); err != nil {
		upgradeCancel()
		log.Printf("qemu: agent %s: upgrade RPC failed: %v", vm.ID, err)
		return
	}
	upgradeCancel()

	log.Printf("qemu: agent %s: upgrade initiated, waiting for new version...", vm.ID)

	// Poll the existing connection until either:
	// 1. GetVersion returns the new version (re-exec completed, new agent up)
	// 2. The connection breaks (re-exec killed the old process)
	// 3. Timeout (30s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		pollCtx, pollCancel := context.WithTimeout(ctx, 2*time.Second)
		ver, err := vm.agent.GetVersion(pollCtx)
		pollCancel()
		if err != nil {
			// Connection broke — old agent is gone, new one starting
			break
		}
		if ver == m.cfg.AgentVersion {
			// New agent is already up on the same connection
			log.Printf("qemu: agent %s: upgraded to %s (%dms total)", vm.ID, m.cfg.AgentVersion, time.Since(t0).Milliseconds())
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Old connection is dead — reconnect to the new agent
	vm.agent.Close()
	newAgent, err := m.waitForAgentSocket(ctx, vm.agentSockPath, 30*time.Second)
	if err != nil {
		log.Printf("qemu: agent %s: reconnect after upgrade failed: %v, retrying...", vm.ID, err)
		fallback, fbErr := m.waitForAgentSocket(ctx, vm.agentSockPath, 15*time.Second)
		if fbErr == nil {
			vm.agent = fallback
			log.Printf("qemu: agent %s: fallback reconnect succeeded", vm.ID)
		} else {
			// Both reconnect attempts failed — agent is dead.
			// Set to nil so callers get "agent not available" instead of using a closed connection.
			vm.agent = nil
			log.Printf("qemu: agent %s: CRITICAL: all reconnect attempts failed after upgrade, agent unavailable", vm.ID)
		}
		return
	}
	vm.agent = newAgent
	log.Printf("qemu: agent %s: upgraded to %s (%dms total)", vm.ID, m.cfg.AgentVersion, time.Since(t0).Milliseconds())
}

// RollingUpgradeHibernated wakes each hibernated sandbox on disk, upgrades the
// agent if needed, and re-hibernates it. This runs in the background on worker
// startup so that future client wakes never hit a version mismatch.
// concurrency controls how many VMs are upgraded simultaneously.
func (m *Manager) RollingUpgradeHibernated(checkpointStore *storage.CheckpointStore, concurrency int) {
	if m.cfg.AgentVersion == "" || m.cfg.AgentVersion == "dev" || m.cfg.AgentBinaryPath == "" {
		return
	}
	if concurrency <= 0 {
		concurrency = 2
	}

	sandboxesDir := filepath.Join(m.cfg.DataDir, "sandboxes")
	entries, err := os.ReadDir(sandboxesDir)
	if err != nil {
		log.Printf("qemu: rolling-upgrade: cannot read %s: %v", sandboxesDir, err)
		return
	}

	// Find hibernated sandboxes (have snapshot-meta.json, not currently running)
	var candidates []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "sb-") {
			continue
		}
		sid := e.Name()
		metaPath := filepath.Join(sandboxesDir, sid, "snapshot", "snapshot-meta.json")
		if !fileExists(metaPath) {
			continue
		}
		// Skip if already running
		m.mu.RLock()
		_, running := m.vms[sid]
		m.mu.RUnlock()
		if running {
			continue
		}
		candidates = append(candidates, sid)
	}

	if len(candidates) == 0 {
		log.Printf("qemu: rolling-upgrade: no hibernated sandboxes to upgrade")
		return
	}
	log.Printf("qemu: rolling-upgrade: found %d hibernated sandboxes, upgrading (concurrency=%d)", len(candidates), concurrency)

	sem := make(chan struct{}, concurrency)
	var upgraded, skipped, failed int
	var mu sync.Mutex

	var wg sync.WaitGroup
	for _, sid := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(sandboxID string) {
			defer wg.Done()
			defer func() { <-sem }()

			result := m.rollingUpgradeOne(sandboxID, checkpointStore)
			mu.Lock()
			switch result {
			case "upgraded":
				upgraded++
			case "skipped":
				skipped++
			default:
				failed++
			}
			mu.Unlock()
		}(sid)
	}
	wg.Wait()

	log.Printf("qemu: rolling-upgrade: complete (%d upgraded, %d skipped, %d failed)", upgraded, skipped, failed)
}

// rollingUpgradeOne wakes a single hibernated sandbox, upgrades the agent
// (handled by upgradeAgentIfNeeded inside doWake), and re-hibernates to
// bake the new agent into the snapshot. Returns "upgraded", "skipped", or "failed".
func (m *Manager) rollingUpgradeOne(sandboxID string, checkpointStore *storage.CheckpointStore) string {
	t0 := time.Now()

	// Wake — upgradeAgentIfNeeded runs inside doWake if version mismatches
	_, err := m.doWake(context.Background(), sandboxID, "local://rolling-upgrade", checkpointStore, 60)
	if err != nil {
		log.Printf("qemu: rolling-upgrade %s: wake failed: %v", sandboxID, err)
		return "failed"
	}

	m.mu.RLock()
	vm, ok := m.vms[sandboxID]
	m.mu.RUnlock()
	if !ok {
		log.Printf("qemu: rolling-upgrade %s: VM not found after wake", sandboxID)
		return "failed"
	}

	// Re-hibernate to bake the upgraded agent into the snapshot
	_, err = m.doHibernate(context.Background(), vm, checkpointStore)
	if err != nil {
		log.Printf("qemu: rolling-upgrade %s: re-hibernate failed: %v", sandboxID, err)
		m.destroyVM(vm)
		m.mu.Lock()
		delete(m.vms, sandboxID)
		m.mu.Unlock()
		return "failed"
	}

	m.mu.Lock()
	delete(m.vms, sandboxID)
	m.mu.Unlock()

	log.Printf("qemu: rolling-upgrade %s: done (%dms)", sandboxID, time.Since(t0).Milliseconds())
	return "upgraded"
}

// dropPageCache evicts a file's pages from the kernel page cache.
// After loadvm reverts qcow2 internal state, the host page cache may hold
// stale blocks. POSIX_FADV_DONTNEED tells the kernel to release them.
func dropPageCache(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	// POSIX_FADV_DONTNEED = 4 on Linux
	const FADV_DONTNEED = 4
	// SYS_FADVISE64 = 221 on x86_64
	const SYS_FADVISE64 = 221
	_, _, errno := syscall.Syscall6(SYS_FADVISE64, f.Fd(), 0, uintptr(info.Size()), FADV_DONTNEED, 0, 0)
	if errno != 0 {
		log.Printf("qemu: dropPageCache %s: fadvise failed: %v", path, errno)
	}
}
