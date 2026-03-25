package qemu

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// MigrationCoordinator orchestrates live migration of VMs between workers.
// Flow: pre-copy drives to S3 → target prepares incoming QEMU → QMP live migrate → cleanup.
type MigrationCoordinator struct {
	manager         *Manager
	checkpointStore *storage.CheckpointStore
	workerID        string

	mu         sync.Mutex
	migrations map[string]*MigrationState
}

// MigrationState tracks a single in-flight migration.
type MigrationState struct {
	SandboxID  string
	TargetAddr string
	Phase      string
	StartedAt  time.Time
	Err        error
}

// NewMigrationCoordinator creates a coordinator.
func NewMigrationCoordinator(manager *Manager, checkpointStore *storage.CheckpointStore, workerID string) *MigrationCoordinator {
	return &MigrationCoordinator{
		manager:         manager,
		checkpointStore: checkpointStore,
		workerID:        workerID,
		migrations:      make(map[string]*MigrationState),
	}
}

// MigrateToS3 pre-copies a sandbox's drives to S3 for cross-worker migration.
// This is Phase 1: no live migration yet, just prepare for hibernate+wake on another worker.
// The sandbox continues running during the copy.
func (mc *MigrationCoordinator) MigrateToS3(ctx context.Context, sandboxID string) (rootfsKey, workspaceKey string, err error) {
	mc.mu.Lock()
	if _, ok := mc.migrations[sandboxID]; ok {
		mc.mu.Unlock()
		return "", "", fmt.Errorf("migration already in progress for %s", sandboxID)
	}
	state := &MigrationState{
		SandboxID: sandboxID,
		Phase:     "pre-copy",
		StartedAt: time.Now(),
	}
	mc.migrations[sandboxID] = state
	mc.mu.Unlock()

	defer func() {
		mc.mu.Lock()
		delete(mc.migrations, sandboxID)
		mc.mu.Unlock()
	}()

	vm, err := mc.manager.getVM(sandboxID)
	if err != nil {
		return "", "", fmt.Errorf("vm not found: %w", err)
	}

	// Sync guest filesystems before copying
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = vm.agent.Exec(syncCtx, &pb.ExecRequest{Command: "sync", RunAsRoot: true})
		cancel()
	}

	sandboxDir := vm.sandboxDir
	rootfsPath := detectDrivePath(sandboxDir, "rootfs")
	workspacePath := detectDrivePath(sandboxDir, "workspace")

	rootfsKey = fmt.Sprintf("migrations/%s/rootfs.qcow2", sandboxID)
	workspaceKey = fmt.Sprintf("migrations/%s/workspace.qcow2", sandboxID)

	t0 := time.Now()

	// Upload rootfs
	state.Phase = "upload-rootfs"
	rootfsSize, err := mc.uploadFile(ctx, rootfsPath, rootfsKey)
	if err != nil {
		return "", "", fmt.Errorf("upload rootfs: %w", err)
	}

	// Upload workspace
	state.Phase = "upload-workspace"
	wsSize, err := mc.uploadFile(ctx, workspacePath, workspaceKey)
	if err != nil {
		return "", "", fmt.Errorf("upload workspace: %w", err)
	}

	log.Printf("qemu: migration pre-copy %s: rootfs=%.1fMB workspace=%.1fMB (%dms)",
		sandboxID,
		float64(rootfsSize)/(1024*1024),
		float64(wsSize)/(1024*1024),
		time.Since(t0).Milliseconds())

	return rootfsKey, workspaceKey, nil
}

// LiveMigrate performs a full QEMU live migration to a target worker.
// Prerequisites: target worker has called PrepareIncomingMigration and returned the incoming address.
func (mc *MigrationCoordinator) LiveMigrate(ctx context.Context, sandboxID, incomingAddr string) error {
	vm, err := mc.manager.getVM(sandboxID)
	if err != nil {
		return fmt.Errorf("vm not found: %w", err)
	}

	if vm.qmp == nil {
		return fmt.Errorf("no QMP client for %s", sandboxID)
	}

	mc.mu.Lock()
	state := &MigrationState{
		SandboxID:  sandboxID,
		TargetAddr: incomingAddr,
		Phase:      "qmp-migrate",
		StartedAt:  time.Now(),
	}
	mc.migrations[sandboxID] = state
	mc.mu.Unlock()

	defer func() {
		mc.mu.Lock()
		delete(mc.migrations, sandboxID)
		mc.mu.Unlock()
	}()

	t0 := time.Now()

	// Set migration parameters for fast cutover
	_ = vm.qmp.Execute("migrate-set-parameters", map[string]interface{}{
		"max-bandwidth":  int64(1024 * 1024 * 1024), // 1 GB/s
		"downtime-limit": int64(100),                 // 100ms max downtime
	})

	// Start live migration
	state.Phase = "migrating"
	if err := vm.qmp.Migrate("tcp:" + incomingAddr); err != nil {
		return fmt.Errorf("QMP migrate: %w", err)
	}

	// Wait for migration to complete
	if err := vm.qmp.WaitMigration(5 * time.Minute); err != nil {
		return fmt.Errorf("migration wait: %w", err)
	}

	state.Phase = "cleanup"
	log.Printf("qemu: live migration %s → %s complete (%dms)",
		sandboxID, incomingAddr, time.Since(t0).Milliseconds())

	// Source cleanup: quit QEMU, release network
	_ = vm.qmp.Quit()
	vm.qmp.Close()
	vm.qmp = nil

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

	if vm.network != nil {
		RemoveMetadataDNAT(vm.network.TAPName, vm.network.HostIP)
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		mc.manager.subnets.Release(vm.network.TAPName)
	}

	// Remove from manager
	mc.manager.mu.Lock()
	delete(mc.manager.vms, sandboxID)
	mc.manager.mu.Unlock()

	// Notify metadata server
	if mc.manager.onSandboxDestroy != nil {
		mc.manager.onSandboxDestroy(sandboxID)
	}

	return nil
}

// PrepareIncomingMigration sets up QEMU on the target worker to receive a live migration.
// Returns the TCP address for the source to connect to.
func (m *Manager) PrepareIncomingMigration(ctx context.Context, sandboxID, rootfsPath, workspacePath string, cpus, memMB, guestPort int, template string) (incomingAddr string, hostPort int, err error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return "", 0, fmt.Errorf("mkdir: %w", err)
	}

	netCfg, err := m.subnets.Allocate()
	if err != nil {
		return "", 0, fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return "", 0, fmt.Errorf("create TAP: %w", err)
	}

	hp, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return "", 0, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hp
	netCfg.GuestPort = guestPort
	if netCfg.GuestPort == 0 {
		netCfg.GuestPort = 80
	}

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return "", 0, fmt.Errorf("add DNAT: %w", err)
	}
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: warning: metadata DNAT failed: %v", err)
	}

	guestMAC := generateMAC(sandboxID)
	guestCID := m.allocateCID()
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 "+
			"root=/dev/vda rw "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	// Find a free TCP port for incoming migration
	migrationPort, err := FindFreePort()
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return "", 0, fmt.Errorf("find migration port: %w", err)
	}

	qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(qmpSockPath)
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return "", 0, fmt.Errorf("create log: %w", err)
	}

	// Start QEMU with -incoming tcp (paused, waiting for migration data)
	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)
	args = append(args, "-incoming", fmt.Sprintf("tcp:0:%d", migrationPort))

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return "", 0, fmt.Errorf("start qemu: %w", err)
	}
	logFile.Close()

	// Connect QMP
	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return "", 0, fmt.Errorf("QMP connect: %w", err)
	}

	// Store the VM (will be completed after migration arrives)
	now := time.Now()
	vm := &VMInstance{
		ID:            sandboxID,
		Template:      template,
		Status:        types.SandboxStatusRunning,
		StartedAt:     now,
		EndAt:         now.Add(300 * time.Second),
		CpuCount:      cpus,
		MemoryMB:      memMB,
		baseMemoryMB:  memMB,
		HostPort:      hp,
		GuestPort:     netCfg.GuestPort,
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

	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	// Return the private IP + port for the source to connect
	// The source will call: migrate tcp:PRIVATE_IP:PORT
	return fmt.Sprintf("%s:%d", netCfg.HostIP, migrationPort), hp, nil
}

// CompleteIncomingMigration is called after QMP migration finishes on the target.
// It reconnects the agent, patches the network, and marks the VM as ready.
func (m *Manager) CompleteIncomingMigration(ctx context.Context, sandboxID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	// Wait for agent via virtio-serial
	agentClient, err := m.waitForAgentSocket(ctx, vm.agentSockPath, 10*time.Second)
	if err != nil {
		return fmt.Errorf("agent connect after migration: %w", err)
	}
	vm.agent = agentClient

	// Patch network (source had different IP)
	if err := patchGuestNetwork(ctx, agentClient, vm.network); err != nil {
		log.Printf("qemu: migration %s: network patch failed: %v", sandboxID, err)
	}

	if err := syncGuestClock(ctx, agentClient); err != nil {
		log.Printf("qemu: migration %s: clock sync failed: %v", sandboxID, err)
	}

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(sandboxID, vm.network.GuestIP, vm.Template, vm.StartedAt)
	}

	log.Printf("qemu: incoming migration %s complete (port=%d, tap=%s)",
		sandboxID, vm.HostPort, vm.network.TAPName)
	return nil
}

// LiveMigrate on Manager delegates to a MigrationCoordinator.
func (m *Manager) LiveMigrate(ctx context.Context, sandboxID, incomingAddr string) error {
	mc := &MigrationCoordinator{
		manager:    m,
		migrations: make(map[string]*MigrationState),
	}
	return mc.LiveMigrate(ctx, sandboxID, incomingAddr)
}

// uploadFile uploads a local file to S3.
func (mc *MigrationCoordinator) uploadFile(ctx context.Context, localPath, s3Key string) (int64, error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return 0, err
	}
	_, err = mc.checkpointStore.Upload(ctx, s3Key, localPath)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// ensure imports are used
var _ = pb.ExecRequest{}
var _ = types.SandboxStatusRunning
var _ = exec.Command
