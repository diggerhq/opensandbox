// Package firecracker implements sandbox.Manager using Firecracker microVMs.
// Each sandbox is a lightweight VM with its own kernel, rootfs, and workspace,
// communicating with the host via gRPC over vsock.
package firecracker

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

// VMInstance holds the state of a running Firecracker microVM.
type VMInstance struct {
	ID          string
	Template    string
	Status      types.SandboxStatus
	StartedAt   time.Time
	EndAt       time.Time
	CpuCount    int
	MemoryMB    int
	HostPort    int
	GuestPort   int

	// VM internals
	pid         int                // Firecracker VMM process PID
	cmd         *exec.Cmd          // Firecracker process
	network     *NetworkConfig
	vsockPath   string             // path to vsock UDS on host
	sandboxDir  string             // /data/sandboxes/{id}/
	agent       *AgentClient       // gRPC client to in-VM agent
	apiSockPath string             // path to Firecracker API socket
	fcClient    *FirecrackerClient // API client for this VM's Firecracker process
	guestMAC    string             // e.g., "AA:FC:00:00:2d:31"
	guestCID    uint32             // vsock CID
	bootArgs    string             // kernel boot args
}

// SandboxMeta is persisted to sandbox-meta.json in each sandbox directory.
// It records VM config so that on hard kill recovery, a sandbox can be
// cold-booted from template + existing workspace without needing DB access.
type SandboxMeta struct {
	SandboxID string `json:"sandboxId"`
	Template  string `json:"template"`
	CpuCount  int    `json:"cpuCount"`
	MemoryMB  int    `json:"memoryMB"`
	GuestPort int    `json:"guestPort"`
}

// Config holds configuration for the Firecracker Manager.
type Config struct {
	DataDir         string // base data directory (e.g., /data)
	KernelPath      string // path to vmlinux (e.g., /data/firecracker/vmlinux-arm64)
	ImagesDir       string // path to base rootfs images (e.g., /data/firecracker/images/)
	FirecrackerBin  string // path to firecracker binary (default: "firecracker")
	DefaultMemoryMB int    // default RAM per VM (default: 512)
	DefaultCPUs     int    // default vCPUs per VM (default: 1)
	DefaultDiskMB   int    // default workspace size in MB (default: 20480 = 20GB)
	DefaultPort     int    // default guest port to expose (default: 80)
}

// Manager implements sandbox.Manager using Firecracker microVMs.
type Manager struct {
	cfg     Config
	subnets *SubnetAllocator

	mu       sync.RWMutex
	vms      map[string]*VMInstance
	nextCID  uint32 // next guest CID to assign (starts at 3, 0-2 are reserved)
	uploadWg sync.WaitGroup // tracks in-flight async S3 uploads
}

// NewManager creates a new Firecracker-backed sandbox manager.
func NewManager(cfg Config) (*Manager, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("DataDir is required")
	}
	if cfg.KernelPath == "" {
		cfg.KernelPath = filepath.Join(cfg.DataDir, "firecracker", "vmlinux-arm64")
	}
	if cfg.ImagesDir == "" {
		cfg.ImagesDir = filepath.Join(cfg.DataDir, "firecracker", "images")
	}
	if cfg.FirecrackerBin == "" {
		cfg.FirecrackerBin = "firecracker"
	}
	if cfg.DefaultMemoryMB == 0 {
		cfg.DefaultMemoryMB = 512
	}
	if cfg.DefaultCPUs == 0 {
		cfg.DefaultCPUs = 1
	}
	if cfg.DefaultDiskMB == 0 {
		cfg.DefaultDiskMB = 20480 // 20GB
	}
	if cfg.DefaultPort == 0 {
		cfg.DefaultPort = 80
	}

	// Verify kernel exists
	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel not found at %s: %w", cfg.KernelPath, err)
	}

	// Verify firecracker binary
	if _, err := exec.LookPath(cfg.FirecrackerBin); err != nil {
		return nil, fmt.Errorf("firecracker binary not found: %w", err)
	}

	// Enable IP forwarding for VM networking
	if err := EnableForwarding(); err != nil {
		log.Printf("firecracker: warning: could not enable IP forwarding: %v", err)
	}

	return &Manager{
		cfg:     cfg,
		subnets: NewSubnetAllocator(),
		vms:     make(map[string]*VMInstance),
		nextCID: 3, // CIDs 0-2 are reserved (hypervisor=0, local=1, host=2)
	}, nil
}

// allocateCID returns a unique guest CID for a new VM.
func (m *Manager) allocateCID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	cid := m.nextCID
	m.nextCID++
	return cid
}

// Create launches a new Firecracker microVM.
func (m *Manager) Create(ctx context.Context, cfg types.SandboxConfig) (*types.Sandbox, error) {
	id := "sb-" + uuid.New().String()[:8]
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", id)

	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// Resolve base image
	template := cfg.Template
	if template == "" {
		template = "ubuntu"
	}
	baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, template)
	if err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("resolve base image: %w", err)
	}

	// Prepare rootfs (reflink copy)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("prepare rootfs: %w", err)
	}

	// Create workspace
	diskMB := m.cfg.DefaultDiskMB
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
	if err := CreateWorkspace(workspacePath, diskMB); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("create workspace: %w", err)
	}

	// Allocate network
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("allocate subnet: %w", err)
	}

	// Create TAP device
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("create TAP: %w", err)
	}

	// Find free host port for port forwarding
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

	// Add DNAT rule
	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	// Configure vCPU and memory
	cpus := cfg.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}

	// Vsock UDS path and unique CID
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")
	guestCID := m.allocateCID()

	// Build kernel boot args
	// The init script in the rootfs reads these to configure networking
	bootArgs := fmt.Sprintf(
		"keep_bootcon console=ttyS0 reboot=k panic=1 pci=off "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	// Generate a deterministic MAC from the sandbox ID
	guestMAC := generateMAC(id)

	// Start Firecracker with API socket (enables snapshot support)
	apiSockPath := filepath.Join(sandboxDir, "firecracker.sock")
	os.Remove(apiSockPath) // clean stale socket

	logPath := filepath.Join(sandboxDir, "firecracker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(m.cfg.FirecrackerBin, "--api-sock", apiSockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	logFile.Close()

	// Configure VM via API socket
	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("wait for API socket: %w", err)
	}

	if err := fcClient.PutMachineConfig(cpus, memMB); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("put machine config: %w", err)
	}
	if err := fcClient.PutBootSource(m.cfg.KernelPath, bootArgs); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("put boot source: %w", err)
	}
	if err := fcClient.PutDrive("rootfs", rootfsPath, true, false); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("put rootfs drive: %w", err)
	}
	if err := fcClient.PutDrive("workspace", workspacePath, false, false); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("put workspace drive: %w", err)
	}
	if err := fcClient.PutNetworkInterface("eth0", guestMAC, netCfg.TAPName); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("put network interface: %w", err)
	}
	if err := fcClient.PutVsock(guestCID, vsockPath); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("put vsock: %w", err)
	}
	if err := fcClient.StartInstance(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start instance: %w", err)
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
		vsockPath:   vsockPath,
		sandboxDir:  sandboxDir,
		apiSockPath: apiSockPath,
		fcClient:    fcClient,
		guestMAC:    guestMAC,
		guestCID:    guestCID,
		bootArgs:    bootArgs,
	}

	// Wait for agent to become available (use background context so gRPC deadline doesn't kill us)
	agentClient, err := m.waitForAgent(context.Background(), vsockPath, 30*time.Second)
	if err != nil {
		log.Printf("firecracker: agent not ready for %s, killing VM: %v", id, err)
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("agent not ready: %w", err)
	}
	vm.agent = agentClient

	// Register VM
	m.mu.Lock()
	m.vms[id] = vm
	m.mu.Unlock()

	// Write sandbox-meta.json for local NVMe recovery after hard kill
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

	log.Printf("firecracker: created VM %s (template=%s, cpu=%d, mem=%dMB, port=%d→%d, tap=%s, mac=%s)",
		id, template, cpus, memMB, hostPort, guestPort, netCfg.TAPName, guestMAC)

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

// waitForAgent polls the agent via gRPC until it responds or times out.
func (m *Manager) waitForAgent(ctx context.Context, vsockPath string, timeout time.Duration) (*AgentClient, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		client, err := NewAgentClient(vsockPath)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err = client.Ping(pingCtx)
		cancel()
		if err != nil {
			client.Close()
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}

		return client, nil
	}

	return nil, fmt.Errorf("agent not ready after %v: %v", timeout, lastErr)
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

	// Kill Firecracker process
	if vm.cmd != nil && vm.cmd.Process != nil {
		vm.cmd.Process.Kill()
		vm.cmd.Wait()
	}

	// Clean up network
	if vm.network != nil {
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	// Clean up API socket
	if vm.apiSockPath != "" {
		os.Remove(vm.apiSockPath)
	}

	// Remove sandbox directory
	if vm.sandboxDir != "" {
		os.RemoveAll(vm.sandboxDir)
	}

	log.Printf("firecracker: destroyed VM %s", vm.ID)
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
	log.Printf("firecracker: manager closed, %d VMs destroyed", len(vms))
}

// WaitUploads blocks until all in-flight async S3 uploads complete,
// or the timeout expires.
func (m *Manager) WaitUploads(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		m.uploadWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Println("firecracker: all S3 uploads complete")
	case <-time.After(timeout):
		log.Printf("firecracker: timed out waiting for S3 uploads after %s", timeout)
	}
}

// HibernateAllResult holds the result of a single VM hibernation during HibernateAll.
type HibernateAllResult struct {
	SandboxID     string
	CheckpointKey string
	Err           error
}

// HibernateAll hibernates all running VMs concurrently.
// The local snapshot (syncfs + pause + dump) is fast (~200ms per VM) and runs in parallel.
// S3 uploads happen asynchronously and are tracked by uploadWg.
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
				log.Printf("firecracker: HibernateAll: %s failed: %v", sandboxID, err)
				results = append(results, HibernateAllResult{SandboxID: sandboxID, Err: err})
			} else {
				results = append(results, HibernateAllResult{SandboxID: sandboxID, CheckpointKey: result.CheckpointKey})
			}
		}(id)
	}

	wg.Wait()
	return results
}

// Exec runs a command in the VM via the agent.
func (m *Manager) Exec(ctx context.Context, sandboxID string, cfg types.ProcessConfig) (*types.ProcessResult, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return nil, err
	}

	timeout := int32(cfg.Timeout)
	if timeout <= 0 {
		timeout = 60
	}

	// If no explicit args, wrap in shell for pipe/redirect/&&/|| support
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
	vm, err := m.getVM(sandboxID)
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
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}
	return vm.agent.WriteFile(ctx, path, []byte(content))
}

// ListDir lists a directory in the VM.
func (m *Manager) ListDir(ctx context.Context, sandboxID, path string) ([]types.EntryInfo, error) {
	vm, err := m.getVM(sandboxID)
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
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}
	return vm.agent.MakeDir(ctx, path)
}

// Remove removes a file/directory in the VM.
func (m *Manager) Remove(ctx context.Context, sandboxID, path string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}
	return vm.agent.Remove(ctx, path)
}

// Exists checks if a path exists in the VM.
func (m *Manager) Exists(ctx context.Context, sandboxID, path string) (bool, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return false, err
	}
	return vm.agent.Exists(ctx, path)
}

// Stat returns file metadata from the VM.
func (m *Manager) Stat(ctx context.Context, sandboxID, path string) (*types.FileInfo, error) {
	vm, err := m.getVM(sandboxID)
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
	vm, err := m.getVM(sandboxID)
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

// DataDir returns the base data directory.
func (m *Manager) DataDir() string {
	return m.cfg.DataDir
}

// ContainerName returns a human-readable name for the sandbox (for logging).
func (m *Manager) ContainerName(id string) string {
	return "fc-" + id
}

// Hibernate snapshots a VM and uploads to S3.
func (m *Manager) Hibernate(ctx context.Context, sandboxID string, checkpointStore *storage.CheckpointStore) (*sandbox.HibernateResult, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return nil, err
	}
	return m.doHibernate(ctx, vm, checkpointStore)
}

// Wake restores a VM from a snapshot.
func (m *Manager) Wake(ctx context.Context, sandboxID string, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	return m.doWake(ctx, sandboxID, checkpointKey, checkpointStore, timeout)
}

// getVM retrieves a VM by ID (read-locked).
func (m *Manager) getVM(id string) (*VMInstance, error) {
	m.mu.RLock()
	vm, ok := m.vms[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sandbox %s not found", id)
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
// Format: AA:FC:00:00:XX:XX where XX:XX are derived from the ID.
func generateMAC(id string) string {
	var b4, b5 byte
	if len(id) > 3 {
		b4 = id[3]
	}
	if len(id) > 0 {
		b5 = id[len(id)-1]
	}
	return fmt.Sprintf("AA:FC:00:00:%02x:%02x", b4, b5)
}

// GetVsockPath returns the vsock UDS path for a sandbox (used by PTY manager).
func (m *Manager) GetVsockPath(sandboxID string) (string, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return "", err
	}
	return vm.vsockPath, nil
}

// GetAgent returns the agent client for a sandbox (used by PTY manager).
func (m *Manager) GetAgent(sandboxID string) (*AgentClient, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return nil, err
	}
	return vm.agent, nil
}

// GetWorkspacePath returns the host path to a sandbox's workspace.ext4 drive.
func (m *Manager) GetWorkspacePath(sandboxID string) (string, error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return "", err
	}
	return filepath.Join(vm.sandboxDir, "workspace.ext4"), nil
}

// SyncFS flushes filesystem buffers inside the VM via the agent.
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

// CleanupOrphanedProcesses kills any Firecracker processes and TAP devices
// left over from a previous worker run. Must be called before RecoverLocalSandboxes
// so TAP devices are free for re-allocation.
func (m *Manager) CleanupOrphanedProcesses() {
	// Kill all firecracker processes not tracked by this manager
	out, err := exec.Command("pgrep", "-f", "firecracker").Output()
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
			log.Printf("firecracker: killed %d orphaned firecracker process(es)", count)
		}
	}

	// Clean up orphaned TAP devices (fc-tap0, fc-tap1, ...)
	out, err = exec.Command("ip", "-o", "link", "show").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			// format: "3: fc-tap0: <flags> ..."
			tapName := strings.TrimSuffix(fields[1], ":")
			if strings.HasPrefix(tapName, "fc-") {
				_ = exec.Command("ip", "link", "del", tapName).Run()
				log.Printf("firecracker: cleaned up orphaned TAP %s", tapName)
			}
		}
	}
}

// LocalRecovery describes a sandbox found on disk that can be recovered.
type LocalRecovery struct {
	SandboxID   string
	HasSnapshot bool        // true = full snapshot (mem+vmstate), false = workspace only
	Meta        SandboxMeta // from sandbox-meta.json or snapshot-meta.json
}

// RecoverLocalSandboxes scans the sandboxes directory for sandbox data left
// on NVMe from a previous run. Returns recoverable sandboxes.
func (m *Manager) RecoverLocalSandboxes() []LocalRecovery {
	sandboxesDir := filepath.Join(m.cfg.DataDir, "sandboxes")
	entries, err := os.ReadDir(sandboxesDir)
	if err != nil {
		log.Printf("firecracker: no sandboxes dir to scan: %v", err)
		return nil
	}

	var recoveries []LocalRecovery
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "sb-") {
			continue
		}
		sandboxID := entry.Name()
		sandboxDir := filepath.Join(sandboxesDir, sandboxID)

		// Check for full snapshot (graceful shutdown completed)
		snapshotMetaPath := filepath.Join(sandboxDir, "snapshot", "snapshot-meta.json")
		if fileExists(filepath.Join(sandboxDir, "snapshot", "mem")) &&
			fileExists(filepath.Join(sandboxDir, "snapshot", "vmstate")) &&
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

		// Check for workspace-only (hard kill, no snapshot)
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
			// No readable meta — skip (can't determine template)
			log.Printf("firecracker: skipping %s: workspace exists but no sandbox-meta.json", sandboxID)
		}
	}
	return recoveries
}
