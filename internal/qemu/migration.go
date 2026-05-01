package qemu

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/opensandbox/opensandbox/internal/sandbox"
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

	// Sync guest + reflink-copy drives under opMu to prevent concurrent
	// checkpoint/destroy from modifying files during the copy.
	vm.opMu.Lock()
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = vm.agent.Exec(syncCtx, &pb.ExecRequest{Command: "sync"})
		cancel()
	}

	sandboxDir := vm.sandboxDir
	rootfsPath := detectDrivePath(sandboxDir, "rootfs")
	workspacePath := detectDrivePath(sandboxDir, "workspace")

	stagingDir, stageErr := os.MkdirTemp(mc.manager.cfg.DataDir, "migration-staging-")
	if stageErr != nil {
		vm.opMu.Unlock()
		return "", "", fmt.Errorf("create staging dir: %w", stageErr)
	}

	stagedRootfs := filepath.Join(stagingDir, filepath.Base(rootfsPath))
	stagedWorkspace := filepath.Join(stagingDir, filepath.Base(workspacePath))
	if cpErr := copyFileReflink(rootfsPath, stagedRootfs); cpErr != nil {
		vm.opMu.Unlock()
		os.RemoveAll(stagingDir)
		return "", "", fmt.Errorf("stage rootfs: %w", cpErr)
	}
	if cpErr := copyFileReflink(workspacePath, stagedWorkspace); cpErr != nil {
		vm.opMu.Unlock()
		os.RemoveAll(stagingDir)
		return "", "", fmt.Errorf("stage workspace: %w", cpErr)
	}
	vm.opMu.Unlock() // release — uploads read from staging copies, not originals
	defer os.RemoveAll(stagingDir)

	rootfsKey = fmt.Sprintf("migrations/%s/rootfs.qcow2", sandboxID)
	workspaceKey = fmt.Sprintf("migrations/%s/workspace.qcow2.zst", sandboxID)

	t0 := time.Now()

	// Compress workspace with zstd before upload — typically 2-4x smaller,
	// proportionally faster to upload/download over S3.
	state.Phase = "compress-workspace"
	compressedWorkspace := stagedWorkspace + ".zst"
	compressCmd := exec.CommandContext(ctx, "zstd", "-1", "--rm", "-q", stagedWorkspace, "-o", compressedWorkspace)
	if out, err := compressCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("compress workspace: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Upload rootfs + compressed workspace in parallel
	state.Phase = "upload"
	type uploadResult struct {
		size int64
		err  error
	}
	rootfsCh := make(chan uploadResult, 1)
	workspaceCh := make(chan uploadResult, 1)

	go func() {
		sz, err := mc.uploadFile(ctx, stagedRootfs, rootfsKey)
		rootfsCh <- uploadResult{sz, err}
	}()
	go func() {
		sz, err := mc.uploadFile(ctx, compressedWorkspace, workspaceKey)
		workspaceCh <- uploadResult{sz, err}
	}()

	rRes := <-rootfsCh
	wRes := <-workspaceCh
	if rRes.err != nil {
		return "", "", fmt.Errorf("upload rootfs: %w", rRes.err)
	}
	if wRes.err != nil {
		return "", "", fmt.Errorf("upload workspace: %w", wRes.err)
	}

	log.Printf("qemu: migration pre-copy %s: rootfs=%.1fMB workspace=%.1fMB(compressed) (%dms)",
		sandboxID,
		float64(rRes.size)/(1024*1024),
		float64(wRes.size)/(1024*1024),
		time.Since(t0).Milliseconds())

	return rootfsKey, workspaceKey, nil
}

// MigrateToS3Flatten is like MigrateToS3 but flattens the rootfs qcow2 overlay
// before uploading, merging the backing file (base ext4 image) into the qcow2.
// This makes the uploaded rootfs self-contained for cross-golden-version migration.
// Uses `qemu-img rebase -b ""` which preserves internal snapshots.
func (mc *MigrationCoordinator) MigrateToS3Flatten(ctx context.Context, sandboxID string) (rootfsKey, workspaceKey string, err error) {
	mc.mu.Lock()
	state := &MigrationState{
		SandboxID: sandboxID,
		Phase:     "pre-copy-flatten",
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

	// Sync guest + reflink-copy drives under opMu
	vm.opMu.Lock()
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = vm.agent.Exec(syncCtx, &pb.ExecRequest{Command: "sync", RunAsRoot: true})
		cancel()
	}

	sandboxDir := vm.sandboxDir
	rootfsPath := detectDrivePath(sandboxDir, "rootfs")
	workspacePath := detectDrivePath(sandboxDir, "workspace")

	stagingDir, stageErr := os.MkdirTemp(mc.manager.cfg.DataDir, "migration-staging-")
	if stageErr != nil {
		vm.opMu.Unlock()
		return "", "", fmt.Errorf("create staging dir: %w", stageErr)
	}

	stagedRootfs := filepath.Join(stagingDir, filepath.Base(rootfsPath))
	stagedWorkspace := filepath.Join(stagingDir, filepath.Base(workspacePath))
	if cpErr := copyFileReflink(rootfsPath, stagedRootfs); cpErr != nil {
		vm.opMu.Unlock()
		os.RemoveAll(stagingDir)
		return "", "", fmt.Errorf("stage rootfs: %w", cpErr)
	}
	if cpErr := copyFileReflink(workspacePath, stagedWorkspace); cpErr != nil {
		vm.opMu.Unlock()
		os.RemoveAll(stagingDir)
		return "", "", fmt.Errorf("stage workspace: %w", cpErr)
	}
	vm.opMu.Unlock()
	defer os.RemoveAll(stagingDir)

	// Flatten rootfs: merge backing file into overlay so it's self-contained.
	// `rebase -b ""` preserves internal savevm snapshots unlike `convert`.
	rebaseCmd := exec.Command("qemu-img", "rebase", "-b", "", stagedRootfs)
	if out, err := rebaseCmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("flatten rootfs: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	rootfsKey = fmt.Sprintf("migrations/%s/rootfs.qcow2", sandboxID)
	workspaceKey = fmt.Sprintf("migrations/%s/workspace.qcow2", sandboxID)

	t0 := time.Now()

	state.Phase = "upload-rootfs-flat"
	rootfsSize, err := mc.uploadFile(ctx, stagedRootfs, rootfsKey)
	if err != nil {
		return "", "", fmt.Errorf("upload rootfs: %w", err)
	}

	state.Phase = "upload-workspace"
	wsSize, err := mc.uploadFile(ctx, stagedWorkspace, workspaceKey)
	if err != nil {
		return "", "", fmt.Errorf("upload workspace: %w", err)
	}

	log.Printf("qemu: migration pre-copy-flatten %s: rootfs=%.1fMB workspace=%.1fMB (%dms)",
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
	log.Printf("qemu: live migration %s: sending migrate tcp:%s", sandboxID, incomingAddr)
	if err := vm.qmp.Migrate("tcp:" + incomingAddr); err != nil {
		return fmt.Errorf("QMP migrate: %w", err)
	}

	// Wait for migration to complete
	if err := vm.qmp.WaitMigration(5 * time.Minute); err != nil {
		// Query detailed status for debugging
		status, qErr := vm.qmp.QueryMigrate()
		if qErr == nil {
			log.Printf("qemu: live migration %s FAILED: status=%s error=%s", sandboxID, status.Status, status.ErrorDesc)
		}
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

	// Tear down the secrets-proxy session for this sandbox on the source.
	// The destination has already re-registered it during
	// PrepareMigrationIncoming, so leaving it here would leak plaintext
	// secrets in the source worker's process memory until restart.
	if mc.manager.secretsProxy != nil && vm.network != nil && vm.network.GuestIP != "" {
		mc.manager.secretsProxy.UnregisterSession(vm.network.GuestIP)
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

	// Resolve rootfs from template if not provided
	if rootfsPath == "" {
		if template == "" {
			template = "default"
		}
		baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, template)
		if err != nil {
			return "", 0, fmt.Errorf("resolve base image for template %q: %w", template, err)
		}
		rootfsPath = filepath.Join(sandboxDir, "rootfs.qcow2")
		if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
			return "", 0, fmt.Errorf("prepare rootfs: %w", err)
		}
		log.Printf("qemu: migration %s: prepared rootfs from template %q: %s", sandboxID, template, rootfsPath)
	}
	// Create workspace if not provided
	if workspacePath == "" {
		workspacePath = filepath.Join(sandboxDir, "workspace.ext4")
		if err := CreateWorkspace(workspacePath, 4096); err != nil {
			return "", 0, fmt.Errorf("create workspace: %w", err)
		}
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

	// Migration targets must have the exact same device configuration as the source.
	// Keep -serial stdio but redirect stdout to /dev/null to avoid blocking.

	log.Printf("qemu: migration-prepare %s: starting QEMU (mem=%dMB, cpu=%d, rootfs=%s, workspace=%s, qmp=%s)",
		sandboxID, memMB, cpus, rootfsPath, workspacePath, qmpSockPath)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	// For migration targets: send stderr to log, but stdout to /dev/null.
	// -serial stdio outputs kernel console to stdout which can block if piped to a file.
	devNull, _ := os.Open(os.DevNull)
	cmd.Stdout = devNull
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		if devNull != nil { devNull.Close() }
		m.cleanupVM(netCfg, sandboxDir)
		return "", 0, fmt.Errorf("start qemu: %w", err)
	}
	logFile.Close()
	if devNull != nil { devNull.Close() }
	log.Printf("qemu: migration-prepare %s: QEMU started (pid=%d), waiting for QMP", sandboxID, cmd.Process.Pid)

	// Connect QMP — allow up to 60s for large-memory VMs (virtio-mem init takes time)
	qmpClient, err := waitForQMP(qmpSockPath, 60*time.Second)
	if err != nil {
		// Check if QEMU exited
		var exitErr string
		if cmd.ProcessState != nil {
			exitErr = cmd.ProcessState.String()
		}
		qemuLog, _ := os.ReadFile(filepath.Join(sandboxDir, "qemu.log"))
		log.Printf("qemu: migration-prepare %s: QMP failed — exit=%s, log tail: %s", sandboxID, exitErr, string(qemuLog[max(0, len(qemuLog)-500):]))
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return "", 0, fmt.Errorf("QMP connect: %w", err)
	}

	// Store the VM (will be completed after migration arrives).
	// goldenVersion is set to this worker's current golden — after migration,
	// PrepareIncomingMigrationWithS3 rebased the overlay to point at this
	// worker's base, so the VM is effectively running against that golden.
	// Without this, any checkpoint taken after migration would record an
	// empty goldenVersion and subsequent cross-golden forks of that
	// checkpoint can't locate the correct old base to rebase from.
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
		goldenVersion: m.GoldenVersion(),
	}

	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	// Return the worker's VNet IP + migration port for the source to connect.
	// We use the advertise address (from OPENSANDBOX_GRPC_ADVERTISE) which is the
	// worker's private IP on the VNet — reachable from other workers.
	// netCfg.HostIP is the TAP host IP (172.16.x.x) which is NOT routable between workers.
	workerIP := m.getWorkerIP()
	return fmt.Sprintf("%s:%d", workerIP, migrationPort), hp, nil
}

// CompleteIncomingMigration is called after QMP migration finishes on the target.
// It reconnects the agent, patches the network, and marks the VM as ready.
func (m *Manager) CompleteIncomingMigration(ctx context.Context, sandboxID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	// Resume the VM — after live migration the target is paused
	if vm.qmp != nil {
		if err := vm.qmp.Cont(); err != nil {
			log.Printf("qemu: migration %s: QMP continue failed: %v", sandboxID, err)
		} else {
			log.Printf("qemu: migration %s: VM resumed", sandboxID)
		}
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

	// Sync VM memory tracking with actual QEMU state.
	// After migration, the QEMU process has the source's virtio-mem hotplugged memory,
	// but the Go struct still has the prep values (virtioMemRequestedMB=0, MemoryMB=baseMem).
	// This causes totalCommittedMemoryMB() to underreport, leading to overcommit.
	if vm.qmp != nil {
		if actualVirtioMB := vm.qmp.GetVirtioMemSize(); actualVirtioMB > 0 {
			vm.virtioMemRequestedMB = actualVirtioMB
			vm.MemoryMB = vm.baseMemoryMB + actualVirtioMB
			log.Printf("qemu: migration %s: synced memory tracking: virtio-mem=%dMB, total=%dMB",
				sandboxID, actualVirtioMB, vm.MemoryMB)
		}
	}

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(sandboxID, vm.network.GuestIP, vm.Template, vm.StartedAt)
	}

	log.Printf("qemu: incoming migration %s complete (port=%d, tap=%s)",
		sandboxID, vm.HostPort, vm.network.TAPName)
	return nil
}

// PreCopyDrives uploads a sandbox's drives to S3 for cross-worker migration.
// Always uploads the thin overlay (never flattens). The target worker rebases
// to its own base image if golden versions differ.
// Returns S3 keys, golden version, and base CPU/memory for QEMU matching.
// See sandbox.MigrationSecrets for the canonical type. We use the shared
// type so the worker gRPC layer can refer to it without an import cycle.

func (m *Manager) PreCopyDrives(ctx context.Context, sandboxID string, checkpointStore *storage.CheckpointStore) (rootfsKey, workspaceKey, goldenVer string, baseCPU, baseMem, actualMem int, secrets sandbox.MigrationSecrets, err error) {
	m.mu.RLock()
	vm, exists := m.vms[sandboxID]
	var pid int
	var guestIP string
	if exists {
		goldenVer = vm.goldenVersion
		baseCPU = vm.CpuCount
		baseMem = vm.baseMemoryMB
		pid = vm.pid
		if vm.network != nil {
			guestIP = vm.network.GuestIP
		}
	}
	m.mu.RUnlock()

	if pid > 0 {
		actualMem = readProcRSSMB(pid)
	}

	// Capture secrets-proxy state BEFORE we start uploading drives. The
	// proxy session is keyed by guest IP; we look it up here so the
	// orchestrator can hand it to the target via PrepareMigrationIncoming.
	// Empty maps when no secrets are registered — orchestrator just passes
	// nil/empty through and target's ReregisterSession is a no-op.
	if m.secretsProxy != nil && guestIP != "" {
		secrets.SealedTokens = m.secretsProxy.GetSessionTokens(guestIP)
		secrets.EgressAllowlist = m.secretsProxy.GetSessionAllowlist(guestIP)
		secrets.TokenHosts = m.secretsProxy.GetSessionTokenHosts(guestIP)
	}

	mc := &MigrationCoordinator{
		manager:         m,
		checkpointStore: checkpointStore,
		migrations:      make(map[string]*MigrationState),
	}

	rootfsKey, workspaceKey, err = mc.MigrateToS3(ctx, sandboxID)
	return
}

// PrepareIncomingMigrationWithS3 downloads drives from S3 then prepares incoming migration.
// If overlayMode is true, the rootfs is a thin qcow2 overlay. When sourceGoldenVersion
// matches the target's current golden version, a fast metadata-only repoint is used.
// When versions differ, a full rebase downloads the old base from S3 and migrates
// the overlay to the current base — same logic as checkpoint fork rebase.
//
// secrets is the per-sandbox secrets-proxy state captured by PreCopyDrives on
// the source. We register it with this worker's secrets proxy AFTER the
// destination QEMU is staged (so vm.network.GuestIP is known) but BEFORE the
// source initiates the live migration. Once the migration completes and
// CompleteIncomingMigration unpauses the VM, the first outbound HTTPS
// request finds its substitution map already in place. Empty maps are a
// no-op (sandboxes without a secret store).
func (m *Manager) PrepareIncomingMigrationWithS3(ctx context.Context, sandboxID, rootfsS3Key, workspaceS3Key string, cpus, memMB, guestPort int, template string, checkpointStore *storage.CheckpointStore, overlayMode bool, sourceGoldenVersion string, secrets sandbox.MigrationSecrets) (incomingAddr string, hostPort int, err error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	// Clean any leftover state from a prior failed prepare attempt. Without this,
	// zstd refuses to overwrite an existing workspace.qcow2 and the retry fails
	// with "already exists; not overwritten". The incoming sandbox must not be
	// running on this worker yet, so removing is safe — drives come from S3.
	if err := os.RemoveAll(sandboxDir); err != nil {
		log.Printf("qemu: migration %s: cleanup stale sandbox dir: %v", sandboxID, err)
	}
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return "", 0, fmt.Errorf("mkdir: %w", err)
	}

	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")

	// Download rootfs + workspace in parallel. Workspace may be zstd-compressed.
	// Rebase runs as soon as rootfs is ready (overlaps with workspace download).
	type dlResult struct {
		err error
	}
	rootfsCh := make(chan dlResult, 1)
	workspaceCh := make(chan dlResult, 1)

	go func() {
		rootfsCh <- dlResult{downloadS3ToFile(ctx, checkpointStore, rootfsS3Key, rootfsPath)}
	}()
	go func() {
		isCompressed := strings.HasSuffix(workspaceS3Key, ".zst")
		dlPath := workspacePath
		if isCompressed {
			dlPath = workspacePath + ".zst"
		}
		if err := downloadS3ToFile(ctx, checkpointStore, workspaceS3Key, dlPath); err != nil {
			workspaceCh <- dlResult{fmt.Errorf("download workspace: %w", err)}
			return
		}
		if isCompressed {
			decompressCmd := exec.CommandContext(ctx, "zstd", "-d", "--rm", "-q", dlPath, "-o", workspacePath)
			if out, err := decompressCmd.CombinedOutput(); err != nil {
				workspaceCh <- dlResult{fmt.Errorf("decompress workspace: %w (%s)", err, strings.TrimSpace(string(out)))}
				return
			}
		}
		workspaceCh <- dlResult{nil}
	}()

	// Wait for rootfs download, then rebase (workspace downloads in parallel)
	if r := <-rootfsCh; r.err != nil {
		return "", 0, fmt.Errorf("download rootfs from S3: %w", r.err)
	}

	if overlayMode {
		if m.checkpointStore == nil && checkpointStore != nil {
			m.SetCheckpointStore(checkpointStore)
		}
		// Resolve the base file for the source's pinned goldenVersion:
		// current worker default if it matches, otherwise a retained or
		// on-demand-downloaded copy. Then metadata-repoint the overlay.
		resolveVer := sourceGoldenVersion
		if resolveVer == "" {
			resolveVer = m.GoldenVersion()
		}
		basePath, err := m.resolveBaseForVersion(ctx, resolveVer)
		if err != nil {
			return "", 0, fmt.Errorf("resolve base %s: %w", resolveVer, err)
		}
		absBase, _ := filepath.Abs(basePath)
		if err := rebaseMetadataOnly(ctx, rootfsPath, absBase); err != nil {
			return "", 0, fmt.Errorf("rebase rootfs to local base %s: %w", resolveVer, err)
		}
		log.Printf("qemu: migration %s: rootfs pinned to base %s", sandboxID, resolveVer)
	}

	// Wait for workspace download + decompress to finish
	if r := <-workspaceCh; r.err != nil {
		return "", 0, r.err
	}

	incomingAddr, hostPort, err = m.PrepareIncomingMigration(ctx, sandboxID, rootfsPath, workspacePath, cpus, memMB, guestPort, template)
	if err != nil {
		return "", 0, err
	}

	// Re-register the secrets-proxy session on this worker, keyed by the
	// guest IP we just allocated in PrepareIncomingMigration. Done before
	// returning so the source can issue the LiveMigrate RPC immediately
	// without a races window where the migrated VM is running but the
	// proxy has no substitution map.
	if m.secretsProxy != nil && len(secrets.SealedTokens) > 0 {
		if vm, getErr := m.getVM(sandboxID); getErr == nil && vm.network != nil && vm.network.GuestIP != "" {
			m.secretsProxy.ReregisterSession(sandboxID, vm.network.GuestIP, secrets.SealedTokens, secrets.EgressAllowlist, secrets.TokenHosts)
			log.Printf("qemu: migration %s: re-registered secrets proxy session (%d tokens, guestIP=%s)",
				sandboxID, len(secrets.SealedTokens), vm.network.GuestIP)
		}
	}

	return incomingAddr, hostPort, nil
}

// downloadS3ToFile downloads an S3 object to a local file.
func downloadS3ToFile(ctx context.Context, store *storage.CheckpointStore, key, localPath string) error {
	rc, err := store.Download(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, rc); err != nil {
		return err
	}
	return f.Sync()
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
