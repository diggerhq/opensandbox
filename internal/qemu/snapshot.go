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
	"time"

	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// SnapshotMeta holds metadata persisted alongside snapshot files.
type SnapshotMeta struct {
	SandboxID     string         `json:"sandboxId"`
	Network       *NetworkConfig `json:"network"`
	GuestCID      uint32         `json:"guestCID"`
	GuestMAC      string         `json:"guestMAC"`
	BootArgs      string         `json:"bootArgs"`
	RootfsPath    string         `json:"rootfsPath"`
	WorkspacePath string         `json:"workspacePath"`
	CpuCount      int            `json:"cpuCount"`
	MemoryMB      int            `json:"memoryMB"`
	Template      string         `json:"template"`
	GuestPort     int            `json:"guestPort"`
	SnapshotedAt  time.Time      `json:"snapshotedAt,omitempty"`
}

// doHibernate pauses a running VM, saves VM state via QMP migrate, and kicks off
// an async S3 upload. QEMU migration produces a single state file (memory + device
// state combined), unlike Firecracker's separate mem + vmstate files.
//
// Flow:
//  1. SyncFS via agent (flush disk buffers — agent stays alive)
//  2. Close gRPC connection (vsock must be inactive for migration)
//  3. QMP stop (pause VM)
//  4. QMP migrate "exec:cat > /path/snapshot/mem" (saves full VM state)
//  5. Poll query-migrate until completed
//  6. QMP quit (kill QEMU process)
//  7. Write snapshot-meta.json
//  8. Clean up network
//  9. (async) Archive snapshot files → tar.zst, upload to S3
func (m *Manager) doHibernate(ctx context.Context, vm *VMInstance, checkpointStore *storage.CheckpointStore) (*sandbox.HibernateResult, error) {
	t0 := time.Now()

	snapshotDir := filepath.Join(vm.sandboxDir, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir snapshot dir: %w", err)
	}

	memFile := filepath.Join(snapshotDir, "mem")

	// Step 1: Sync filesystems inside the VM
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, syncErr := vm.agent.Exec(syncCtx, &pb.ExecRequest{
			Command: "sync",
		})
		cancel()
		if syncErr != nil {
			log.Printf("qemu: hibernate %s: exec sync failed: %v", vm.ID, syncErr)
		}
	}
	log.Printf("qemu: hibernate %s: guest sync done (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 2: Close gRPC connection — vsock must be inactive before migration.
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
		time.Sleep(500 * time.Millisecond)
	}

	// Step 3: Stop (pause) the VM via QMP
	if vm.qmp == nil {
		return nil, fmt.Errorf("no QMP client for VM %s", vm.ID)
	}
	if err := vm.qmp.Stop(); err != nil {
		return nil, fmt.Errorf("QMP stop: %w", err)
	}
	log.Printf("qemu: hibernate %s: paused (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 4: Migrate — saves full VM state (memory + devices) to a single file
	migrateURI := fmt.Sprintf("exec:cat > %s", memFile)
	if err := vm.qmp.Migrate(migrateURI); err != nil {
		return nil, fmt.Errorf("QMP migrate: %w", err)
	}

	// Step 5: Wait for migration to complete
	if err := vm.qmp.WaitMigration(5 * time.Minute); err != nil {
		return nil, fmt.Errorf("migration wait: %w", err)
	}
	log.Printf("qemu: hibernate %s: migration complete (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 6: Quit QEMU process
	_ = vm.qmp.Quit()
	vm.qmp.Close()
	vm.qmp = nil

	// Also wait for the process to exit
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

	// Step 7: Write snapshot metadata
	meta := &SnapshotMeta{
		SandboxID:     vm.ID,
		Network:       vm.network,
		GuestCID:      vm.guestCID,
		GuestMAC:      vm.guestMAC,
		BootArgs:      vm.bootArgs,
		RootfsPath:    filepath.Join(vm.sandboxDir, "rootfs.ext4"),
		WorkspacePath: filepath.Join(vm.sandboxDir, "workspace.ext4"),
		CpuCount:      vm.CpuCount,
		MemoryMB:      vm.MemoryMB,
		Template:      vm.Template,
		GuestPort:     vm.GuestPort,
		SnapshotedAt:  time.Now(),
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot meta: %w", err)
	}
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")
	if err := os.WriteFile(metaPath, metaJSON, 0644); err != nil {
		return nil, fmt.Errorf("write snapshot meta: %w", err)
	}

	// Step 8: Clean up network
	if vm.network != nil {
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	// Clean up QMP socket
	if vm.qmpSockPath != "" {
		os.Remove(vm.qmpSockPath)
	}

	checkpointKey := fmt.Sprintf("checkpoints/%s/%d.tar.zst", vm.ID, time.Now().Unix())
	localElapsed := time.Since(t0)
	log.Printf("qemu: hibernate %s: local snapshot complete (%dms), starting async S3 upload",
		vm.ID, localElapsed.Milliseconds())

	// Step 9: Archive + upload to S3 in the background.
	sandboxDir := vm.sandboxDir
	sandboxID := vm.ID
	m.uploadWg.Add(1)
	go func() {
		defer m.uploadWg.Done()
		t1 := time.Now()
		archivePath := filepath.Join(sandboxDir, "checkpoint.tar.zst")

		if err := createArchive(archivePath, sandboxDir, []string{
			"snapshot/mem",
			"snapshot/snapshot-meta.json",
			"workspace.ext4",
		}); err != nil {
			log.Printf("qemu: async archive failed for %s: %v", sandboxID, err)
			return
		}
		archiveInfo, err := os.Stat(archivePath)
		if err != nil {
			log.Printf("qemu: async archive stat failed for %s: %v", sandboxID, err)
			return
		}
		log.Printf("qemu: hibernate %s: archive created (%dms, %.1f MB)",
			sandboxID, time.Since(t1).Milliseconds(), float64(archiveInfo.Size())/(1024*1024))

		t2 := time.Now()
		uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := checkpointStore.Upload(uploadCtx, checkpointKey, archivePath); err != nil {
			log.Printf("qemu: async S3 upload failed for %s: %v", sandboxID, err)
			return
		}
		log.Printf("qemu: hibernate %s: S3 upload complete (%dms, key=%s)",
			sandboxID, time.Since(t2).Milliseconds(), checkpointKey)

		os.Remove(archivePath)
	}()

	return &sandbox.HibernateResult{
		SandboxID:      sandboxID,
		HibernationKey: checkpointKey,
		SizeBytes:      0,
	}, nil
}

// doWake restores a VM from a migration snapshot. The VM resumes exactly where it
// was paused — all processes, memory, open files, and PIDs are intact.
//
// QEMU hot restore: start QEMU with -incoming "exec:cat /path/snapshot/mem" flag
// plus all normal CLI args. QEMU loads state from the incoming migration and resumes.
//
// Flow:
//  1. Check for local snapshot files (fast path for same-machine wake)
//  2. If missing, download from S3 and extract
//  3. Read snapshot-meta.json
//  4. Verify drives exist
//  5. Recreate network — try same TAP name for hot restore, fall back to cold boot
//  6. Start QEMU with -incoming flag for hot restore, or cold boot fresh VM
//  7. QMP cont to resume
//  8. Wait for agent, sync clock
//  9. Register VM
func (m *Manager) doWake(ctx context.Context, sandboxID, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	snapshotDir := filepath.Join(sandboxDir, "snapshot")

	memFile := filepath.Join(snapshotDir, "mem")
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")

	// Step 1-2: Ensure snapshot files are local
	t0 := time.Now()
	memExists := fileExists(memFile)
	log.Printf("qemu: wake %s: checking local files: mem=%s (exists=%v)", sandboxID, memFile, memExists)

	isLocalWorkspace := strings.HasPrefix(checkpointKey, "local://")

	if !memExists {
		if isLocalWorkspace {
			log.Printf("qemu: wake %s: local workspace recovery (no snapshot)", sandboxID)
			return m.coldBootLocal(ctx, sandboxID, timeout)
		}
		log.Printf("qemu: wake %s: local snapshot missing, downloading from S3 (key=%s)", sandboxID, checkpointKey)
		if err := os.MkdirAll(sandboxDir, 0755); err != nil {
			return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
		}
		if err := os.MkdirAll(snapshotDir, 0755); err != nil {
			return nil, fmt.Errorf("mkdir snapshot dir: %w", err)
		}

		archiveData, err := checkpointStore.Download(ctx, checkpointKey)
		if err != nil {
			return nil, fmt.Errorf("download checkpoint: %w", err)
		}

		archivePath := filepath.Join(sandboxDir, "checkpoint.tar.zst")
		archiveFile, err := os.Create(archivePath)
		if err != nil {
			archiveData.Close()
			return nil, fmt.Errorf("create archive file: %w", err)
		}
		if _, err := io.Copy(archiveFile, archiveData); err != nil {
			archiveFile.Close()
			archiveData.Close()
			return nil, fmt.Errorf("write archive: %w", err)
		}
		archiveFile.Close()
		archiveData.Close()

		log.Printf("qemu: wake %s: downloaded + wrote archive (%dms)", sandboxID, time.Since(t0).Milliseconds())
		if err := extractArchive(archivePath, sandboxDir); err != nil {
			return nil, fmt.Errorf("extract archive: %w", err)
		}
		os.Remove(archivePath)
		log.Printf("qemu: wake %s: extracted archive (%dms total)", sandboxID, time.Since(t0).Milliseconds())
	} else {
		log.Printf("qemu: wake %s: local snapshot found, skipping S3 download", sandboxID)
	}

	// Step 3: Read snapshot metadata
	metaJSON, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot meta: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, fmt.Errorf("parse snapshot meta: %w", err)
	}

	// Step 4: Ensure drives exist
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")

	if !fileExists(rootfsPath) {
		log.Printf("qemu: wake %s: rootfs missing, recreating from template %q", sandboxID, meta.Template)
		baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, meta.Template)
		if err != nil {
			return nil, fmt.Errorf("resolve base image for wake: %w", err)
		}
		if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
			return nil, fmt.Errorf("prepare rootfs for wake: %w", err)
		}
	}
	if !fileExists(workspacePath) {
		return nil, fmt.Errorf("workspace not found at %s", workspacePath)
	}

	// Step 5: Try snapshot restore (fast path) or cold boot fallback
	snapshotRestore := true
	netCfg, err := m.subnets.AllocateSpecific(meta.Network.TAPName)
	if err != nil {
		log.Printf("qemu: wake %s: TAP %s unavailable (%v), falling back to cold boot",
			sandboxID, meta.Network.TAPName, err)
		snapshotRestore = false
		netCfg, err = m.subnets.Allocate()
		if err != nil {
			return nil, fmt.Errorf("allocate subnet for cold boot: %w", err)
		}
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return nil, fmt.Errorf("create TAP: %w", err)
	}

	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = meta.GuestPort
	if netCfg.GuestPort == 0 {
		netCfg.GuestPort = 80
	}

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	guestCID := meta.GuestCID
	guestMAC := meta.GuestMAC
	bootArgs := meta.BootArgs

	if snapshotRestore {
		// Hot restore: start QEMU with -incoming to load migration state
		// Update boot args to use new network config IPs (same TAP name)
		qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
		os.Remove(qmpSockPath)

		logPath := filepath.Join(sandboxDir, "qemu.log")
		logFile, err := os.Create(logPath)
		if err != nil {
			m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("create log file: %w", err)
		}

		incomingURI := fmt.Sprintf("exec:cat %s", memFile)
		args := m.buildQEMUArgs(meta.CpuCount, meta.MemoryMB, rootfsPath, workspacePath,
			netCfg.TAPName, guestMAC, guestCID, qmpSockPath, bootArgs)
		args = append(args, "-incoming", incomingURI)

		cmd := exec.Command(m.cfg.QEMUBin, args...)
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		if err := cmd.Start(); err != nil {
			logFile.Close()
			m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("start qemu for restore: %w", err)
		}
		logFile.Close()

		// Connect QMP
		qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("QMP connect for restore: %w", err)
		}

		// Resume the VM after migration load
		if err := qmpClient.Cont(); err != nil {
			qmpClient.Close()
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("QMP cont after restore: %w", err)
		}
		log.Printf("qemu: wake %s: snapshot restored (hot)", sandboxID)

		now := time.Now()
		ttl := time.Duration(timeout) * time.Second
		if ttl <= 0 {
			ttl = 300 * time.Second
		}

		vm := &VMInstance{
			ID:          sandboxID,
			Template:    meta.Template,
			Status:      types.SandboxStatusRunning,
			StartedAt:   now,
			EndAt:       now.Add(ttl),
			CpuCount:    meta.CpuCount,
			MemoryMB:    meta.MemoryMB,
			HostPort:    hostPort,
			GuestPort:   netCfg.GuestPort,
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

		// Wait for agent — AF_VSOCK is available immediately when QEMU starts
		agentClient, err := m.waitForAgent(context.Background(), guestCID, 30*time.Second)
		if err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("agent not ready after wake: %w", err)
		}
		vm.agent = agentClient

		// Sync guest clock
		if err := syncGuestClock(context.Background(), agentClient); err != nil {
			log.Printf("qemu: wake %s: clock sync failed: %v", sandboxID, err)
		}

		m.mu.Lock()
		m.vms[sandboxID] = vm
		m.mu.Unlock()

		log.Printf("qemu: woke VM %s (snapshot restore, port=%d, tap=%s)",
			sandboxID, hostPort, netCfg.TAPName)
		return vmToSandbox(vm), nil
	}

	// Cold boot fallback: boot fresh VM with restored drives
	guestCID = m.allocateCID()
	guestMAC = generateMAC(sandboxID)
	bootArgs = fmt.Sprintf(
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
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := m.buildQEMUArgs(meta.CpuCount, meta.MemoryMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, guestCID, qmpSockPath, bootArgs)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("start qemu cold boot: %w", err)
	}
	logFile.Close()

	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("QMP connect for cold boot: %w", err)
	}

	log.Printf("qemu: wake %s: cold boot complete (cross-worker migration, tap=%s)",
		sandboxID, netCfg.TAPName)

	now := time.Now()
	ttl := time.Duration(timeout) * time.Second
	if ttl <= 0 {
		ttl = 300 * time.Second
	}

	vm := &VMInstance{
		ID:          sandboxID,
		Template:    meta.Template,
		Status:      types.SandboxStatusRunning,
		StartedAt:   now,
		EndAt:       now.Add(ttl),
		CpuCount:    meta.CpuCount,
		MemoryMB:    meta.MemoryMB,
		HostPort:    hostPort,
		GuestPort:   netCfg.GuestPort,
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

	agentClient, err := m.waitForAgent(context.Background(), guestCID, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("agent not ready after cold boot: %w", err)
	}
	vm.agent = agentClient

	if err := syncGuestClock(context.Background(), agentClient); err != nil {
		log.Printf("qemu: wake %s: clock sync failed: %v", sandboxID, err)
	}

	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	log.Printf("qemu: woke VM %s (cold boot, port=%d, tap=%s)",
		sandboxID, hostPort, netCfg.TAPName)
	return vmToSandbox(vm), nil
}

// coldBootLocal boots a fresh VM using an existing workspace.ext4 on disk.
func (m *Manager) coldBootLocal(ctx context.Context, sandboxID string, timeout int) (*types.Sandbox, error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")

	if !fileExists(workspacePath) {
		return nil, fmt.Errorf("workspace not found at %s", workspacePath)
	}

	sbMetaPath := filepath.Join(sandboxDir, "sandbox-meta.json")
	metaJSON, err := os.ReadFile(sbMetaPath)
	if err != nil {
		return nil, fmt.Errorf("read sandbox-meta.json: %w", err)
	}
	var meta SandboxMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, fmt.Errorf("parse sandbox-meta.json: %w", err)
	}

	if !fileExists(rootfsPath) {
		baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, meta.Template)
		if err != nil {
			return nil, fmt.Errorf("resolve base image: %w", err)
		}
		if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
			return nil, fmt.Errorf("prepare rootfs: %w", err)
		}
		log.Printf("qemu: cold-boot-local %s: rootfs recreated from template %q", sandboxID, meta.Template)
	}

	netCfg, err := m.subnets.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return nil, fmt.Errorf("create TAP: %w", err)
	}

	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	guestPort := meta.GuestPort
	if guestPort == 0 {
		guestPort = 80
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = guestPort

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	cpus := meta.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := meta.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}

	guestCID := m.allocateCID()
	guestMAC := generateMAC(sandboxID)
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
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, guestCID, qmpSockPath, bootArgs)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	logFile.Close()

	qmpClient, err := waitForQMP(qmpSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("QMP connect: %w", err)
	}

	now := time.Now()
	ttl := time.Duration(timeout) * time.Second
	if ttl <= 0 {
		ttl = 300 * time.Second
	}

	vm := &VMInstance{
		ID:          sandboxID,
		Template:    meta.Template,
		Status:      types.SandboxStatusRunning,
		StartedAt:   now,
		EndAt:       now.Add(ttl),
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

	agentClient, err := m.waitForAgent(context.Background(), guestCID, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("agent not ready after cold boot: %w", err)
	}
	vm.agent = agentClient

	if err := syncGuestClock(context.Background(), agentClient); err != nil {
		log.Printf("qemu: cold-boot-local %s: clock sync failed: %v", sandboxID, err)
	}

	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	log.Printf("qemu: cold-boot-local %s (template=%s, port=%d, tap=%s)", sandboxID, meta.Template, hostPort, netCfg.TAPName)
	return vmToSandbox(vm), nil
}

// createArchive creates a tar.zst archive of specific files from a directory.
func createArchive(archivePath, baseDir string, files []string) error {
	args := []string{
		"--zstd",
		"-cf", archivePath,
		"-C", baseDir,
	}
	args = append(args, files...)

	cmd := exec.Command("tar", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar create: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// extractArchive extracts a tar.zst archive to a directory.
func extractArchive(archivePath, destDir string) error {
	cmd := exec.Command("tar", "--zstd", "-xf", archivePath, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fileExists checks if a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// copyFileReflink copies a file using cp --reflink=auto.
func copyFileReflink(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s → %s: %w (%s)", src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// syncGuestClock sets the guest clock to the current host time via agent exec.
func syncGuestClock(ctx context.Context, agent *AgentClient) error {
	now := time.Now().Unix()
	cmd := fmt.Sprintf("date -s @%d > /dev/null 2>&1", now)
	resp, err := agent.Exec(ctx, &pb.ExecRequest{
		Command:        "/bin/sh",
		Args:           []string{"-c", cmd},
		TimeoutSeconds: 5,
	})
	if err != nil {
		return fmt.Errorf("exec clock sync: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("clock sync failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

// waitForQMP polls until the QMP socket appears and connects.
func waitForQMP(socketPath string, timeout time.Duration) (*QMPClient, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			qmp, err := ConnectQMP(socketPath, 5*time.Second)
			if err == nil {
				return qmp, nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("QMP socket %s not ready after %v", socketPath, timeout)
}
