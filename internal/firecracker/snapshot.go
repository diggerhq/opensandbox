package firecracker

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
)

// SnapshotMeta holds metadata persisted alongside snapshot files.
// This is needed to restore the exact same VM configuration on wake.
type SnapshotMeta struct {
	SandboxID     string         `json:"sandboxId"`
	Network       *NetworkConfig `json:"network"`
	GuestCID      uint32         `json:"guestCID"`
	GuestMAC      string         `json:"guestMAC"`
	BootArgs      string         `json:"bootArgs"`
	RootfsPath    string         `json:"rootfsPath"`
	WorkspacePath string         `json:"workspacePath"`
	VsockPath     string         `json:"vsockPath"`
	CpuCount      int            `json:"cpuCount"`
	MemoryMB      int            `json:"memoryMB"`
	Template      string         `json:"template"`
	GuestPort     int            `json:"guestPort"`
}

// doHibernate pauses a running VM, creates a full memory snapshot, and kicks off
// an async S3 upload. Drives (rootfs + workspace) stay on local disk for fast
// same-machine wake. Only mem + vmstate + metadata are archived and uploaded.
//
// The API returns as soon as the local snapshot is written — the S3 upload
// happens in the background so hibernate latency is sub-second for the caller.
//
// Flow:
//  1. SyncFS via agent (flush disk buffers — agent stays alive)
//  2. Close gRPC connection (vsock must be inactive for snapshot)
//  3. Pause VM via Firecracker API socket
//  4. Create snapshot (mem_file + vmstate_file)
//  5. Kill the paused Firecracker process
//  6. Write snapshot-meta.json
//  7. Clean up network
//  8. (async) Archive snapshot files → tar.zst, upload to S3
func (m *Manager) doHibernate(ctx context.Context, vm *VMInstance, checkpointStore *storage.CheckpointStore) (*sandbox.HibernateResult, error) {
	t0 := time.Now()

	snapshotDir := filepath.Join(vm.sandboxDir, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir snapshot dir: %w", err)
	}

	memFile := filepath.Join(snapshotDir, "mem")
	vmstateFile := filepath.Join(snapshotDir, "vmstate")

	// Step 1: Sync filesystems inside the VM (agent stays alive for snapshot)
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := vm.agent.SyncFS(syncCtx)
		cancel()
		if err != nil {
			log.Printf("firecracker: SyncFS warning for %s: %v", vm.ID, err)
		}
	}
	log.Printf("firecracker: hibernate %s: syncfs done (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 2: Close gRPC connection — vsock must be inactive before snapshot
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}

	// Step 3: Pause the VM
	if vm.fcClient == nil {
		return nil, fmt.Errorf("no API client for VM %s (not in API mode)", vm.ID)
	}
	if err := vm.fcClient.PauseVM(); err != nil {
		return nil, fmt.Errorf("pause VM: %w", err)
	}
	log.Printf("firecracker: hibernate %s: paused (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 4: Create memory snapshot
	if err := vm.fcClient.CreateSnapshot(vmstateFile, memFile); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	log.Printf("firecracker: hibernate %s: snapshot created (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 5: Kill the paused Firecracker process
	if vm.cmd != nil && vm.cmd.Process != nil {
		vm.cmd.Process.Kill()
		vm.cmd.Wait()
	}

	// Step 6: Write snapshot metadata
	meta := &SnapshotMeta{
		SandboxID:     vm.ID,
		Network:       vm.network,
		GuestCID:      vm.guestCID,
		GuestMAC:      vm.guestMAC,
		BootArgs:      vm.bootArgs,
		RootfsPath:    filepath.Join(vm.sandboxDir, "rootfs.ext4"),
		WorkspacePath: filepath.Join(vm.sandboxDir, "workspace.ext4"),
		VsockPath:     vm.vsockPath,
		CpuCount:      vm.CpuCount,
		MemoryMB:      vm.MemoryMB,
		Template:      vm.Template,
		GuestPort:     vm.GuestPort,
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot meta: %w", err)
	}
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")
	if err := os.WriteFile(metaPath, metaJSON, 0644); err != nil {
		return nil, fmt.Errorf("write snapshot meta: %w", err)
	}

	// Step 7: Clean up network (keep snapshot files on disk for local wake)
	if vm.network != nil {
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	// Clean up sockets — the vsock UDS must be removed so snapshot restore
	// can re-bind to the same path without "Address in use" errors.
	if vm.apiSockPath != "" {
		os.Remove(vm.apiSockPath)
	}
	if vm.vsockPath != "" {
		os.Remove(vm.vsockPath)
	}

	checkpointKey := fmt.Sprintf("checkpoints/%s/%d.tar.zst", vm.ID, time.Now().Unix())
	localElapsed := time.Since(t0)
	log.Printf("firecracker: hibernate %s: local snapshot complete (%dms), starting async S3 upload",
		vm.ID, localElapsed.Milliseconds())

	// Step 8: Archive + upload to S3 in the background.
	// The archive includes mem + vmstate + metadata + workspace drive so that
	// the sandbox can be restored on ANY worker (cross-worker migration).
	// The rootfs is NOT archived — it's recreated from the template on wake.
	sandboxDir := vm.sandboxDir
	sandboxID := vm.ID
	m.uploadWg.Add(1)
	go func() {
		defer m.uploadWg.Done()
		t1 := time.Now()
		archivePath := filepath.Join(sandboxDir, "checkpoint.tar.zst")

		// Archive snapshot files + workspace drive.
		// Use sandboxDir as base so workspace.ext4 is at the right relative path.
		if err := createArchive(archivePath, sandboxDir, []string{
			"snapshot/mem",
			"snapshot/vmstate",
			"snapshot/snapshot-meta.json",
			"workspace.ext4",
		}); err != nil {
			log.Printf("firecracker: async archive failed for %s: %v", sandboxID, err)
			return
		}
		archiveInfo, err := os.Stat(archivePath)
		if err != nil {
			log.Printf("firecracker: async archive stat failed for %s: %v", sandboxID, err)
			return
		}
		log.Printf("firecracker: hibernate %s: archive created (%dms, %.1f MB)",
			sandboxID, time.Since(t1).Milliseconds(), float64(archiveInfo.Size())/(1024*1024))

		t2 := time.Now()
		uploadCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := checkpointStore.Upload(uploadCtx, checkpointKey, archivePath); err != nil {
			log.Printf("firecracker: async S3 upload failed for %s: %v", sandboxID, err)
			return
		}
		log.Printf("firecracker: hibernate %s: S3 upload complete (%dms, key=%s)",
			sandboxID, time.Since(t2).Milliseconds(), checkpointKey)

		os.Remove(archivePath)
	}()

	return &sandbox.HibernateResult{
		SandboxID:     sandboxID,
		CheckpointKey: checkpointKey,
		SizeBytes:     0, // not known yet — archive happens async
	}, nil
}

// doWake restores a VM from a memory snapshot. The VM resumes exactly where it
// was paused — all processes, memory, open files, and PIDs are intact.
//
// Flow:
//  1. Check for local snapshot files (fast path for same-machine wake)
//  2. If missing, download from S3 and extract
//  3. Read snapshot-meta.json for VM configuration
//  4. Verify drives exist at original paths
//  5. Recreate network with same TAP name (required by vmstate)
//  6. Start fresh Firecracker process with API socket
//  7. Load snapshot via API (restores full VM state)
//  8. Wait for agent to become available
//  9. Register VM
func (m *Manager) doWake(ctx context.Context, sandboxID, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	snapshotDir := filepath.Join(sandboxDir, "snapshot")

	memFile := filepath.Join(snapshotDir, "mem")
	vmstateFile := filepath.Join(snapshotDir, "vmstate")
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")

	// Step 1-2: Ensure snapshot files are local
	t0 := time.Now()
	memExists := fileExists(memFile)
	vmstateExists := fileExists(vmstateFile)
	log.Printf("firecracker: wake %s: checking local files: mem=%s (exists=%v) vmstate=%s (exists=%v)",
		sandboxID, memFile, memExists, vmstateFile, vmstateExists)

	// Local workspace-only recovery: no snapshot files, just workspace.ext4 on NVMe.
	// This happens after a hard kill where the worker didn't get to hibernate.
	isLocalWorkspace := strings.HasPrefix(checkpointKey, "local://")

	if !memExists || !vmstateExists {
		if isLocalWorkspace {
			// No snapshot to download — cold boot from local workspace.ext4.
			log.Printf("firecracker: wake %s: local workspace recovery (no snapshot)", sandboxID)
			return m.coldBootLocal(ctx, sandboxID, timeout)
		}
		log.Printf("firecracker: wake %s: local snapshot missing, downloading from S3 (key=%s)", sandboxID, checkpointKey)
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

		log.Printf("firecracker: wake %s: downloaded + wrote archive (%dms)", sandboxID, time.Since(t0).Milliseconds())
		// Extract to sandboxDir — archive contains snapshot/{mem,vmstate,snapshot-meta.json} + workspace.ext4
		if err := extractArchive(archivePath, sandboxDir); err != nil {
			return nil, fmt.Errorf("extract archive: %w", err)
		}
		os.Remove(archivePath)
		log.Printf("firecracker: wake %s: extracted archive (%dms total)", sandboxID, time.Since(t0).Milliseconds())
	} else {
		log.Printf("firecracker: wake %s: local snapshot found, skipping S3 download", sandboxID)
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

	// Step 4: Ensure drives exist — provision if missing (cross-worker wake)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")

	if !fileExists(rootfsPath) {
		// Cross-worker wake: recreate rootfs from template image
		log.Printf("firecracker: wake %s: rootfs missing, recreating from template %q", sandboxID, meta.Template)
		baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, meta.Template)
		if err != nil {
			return nil, fmt.Errorf("resolve base image for wake: %w", err)
		}
		if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
			return nil, fmt.Errorf("prepare rootfs for wake: %w", err)
		}
		log.Printf("firecracker: wake %s: rootfs recreated from %s", sandboxID, baseImage)
	}
	if !fileExists(workspacePath) {
		// workspace.ext4 should have been extracted from the S3 archive above.
		// If still missing, the archive was created before workspace archiving was added.
		return nil, fmt.Errorf("workspace not found at %s (not in S3 archive — sandbox was hibernated before workspace archiving was enabled)", workspacePath)
	}

	// Update metadata paths to this worker's sandboxDir (they may differ from the original worker).
	// Keep the vsock filename from the original — it's baked into the vmstate for snapshot restore.
	meta.RootfsPath = rootfsPath
	meta.WorkspacePath = workspacePath
	meta.VsockPath = filepath.Join(sandboxDir, filepath.Base(meta.VsockPath))

	// Step 5: Try snapshot restore (fast path) or cold boot (cross-worker fallback).
	//
	// Snapshot restore requires the exact same TAP name (baked into vmstate).
	// If the TAP name is already taken (cross-worker wake), fall back to cold boot:
	// boot a fresh VM with the restored rootfs + workspace. Processes won't survive
	// but filesystem state (user files, installed packages) is preserved.
	snapshotRestore := true
	netCfg, err := m.subnets.AllocateSpecific(meta.Network.TAPName)
	if err != nil {
		// TAP name collision — fall back to cold boot
		log.Printf("firecracker: wake %s: TAP %s unavailable (%v), falling back to cold boot",
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

	apiSockPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := meta.VsockPath
	os.Remove(apiSockPath)
	os.Remove(vsockPath)

	logPath := filepath.Join(sandboxDir, "firecracker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(m.cfg.FirecrackerBin, "--api-sock", apiSockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	logFile.Close()

	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("wait for API socket: %w", err)
	}

	if snapshotRestore {
		// Hot restore: load snapshot, VM resumes exactly where it was paused
		if err := fcClient.LoadSnapshot(vmstateFile, memFile, true); err != nil {
			cmd.Process.Kill()
			cmd.Wait()
			m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("load snapshot: %w", err)
		}
		log.Printf("firecracker: wake %s: snapshot restored (hot)", sandboxID)
	} else {
		// Cold boot: configure + boot fresh VM with restored drives.
		// Filesystem state is preserved but running processes are not.
		guestCID := m.allocateCID()
		guestMAC := generateMAC(sandboxID)
		bootArgs := fmt.Sprintf(
			"keep_bootcon console=ttyS0 reboot=k panic=1 pci=off "+
				"ip=%s::%s:%s::eth0:off "+
				"init=/sbin/init "+
				"osb.gateway=%s",
			netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
		)
		meta.GuestCID = guestCID
		meta.GuestMAC = guestMAC
		meta.BootArgs = bootArgs

		if err := fcClient.PutMachineConfig(meta.CpuCount, meta.MemoryMB); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: put machine config: %w", err)
		}
		if err := fcClient.PutBootSource(m.cfg.KernelPath, bootArgs); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: put boot source: %w", err)
		}
		if err := fcClient.PutDrive("rootfs", rootfsPath, true, false); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: put rootfs drive: %w", err)
		}
		if err := fcClient.PutDrive("workspace", workspacePath, false, false); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: put workspace drive: %w", err)
		}
		if err := fcClient.PutNetworkInterface("eth0", guestMAC, netCfg.TAPName); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: put network interface: %w", err)
		}
		if err := fcClient.PutVsock(guestCID, vsockPath); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: put vsock: %w", err)
		}
		if err := fcClient.StartInstance(); err != nil {
			cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
			return nil, fmt.Errorf("cold boot: start instance: %w", err)
		}
		log.Printf("firecracker: wake %s: cold boot complete (cross-worker migration, tap=%s)",
			sandboxID, netCfg.TAPName)
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
		CpuCount:    meta.CpuCount,
		MemoryMB:    meta.MemoryMB,
		HostPort:    hostPort,
		GuestPort:   netCfg.GuestPort,
		pid:         cmd.Process.Pid,
		cmd:         cmd,
		network:     netCfg,
		vsockPath:   vsockPath,
		sandboxDir:  sandboxDir,
		apiSockPath: apiSockPath,
		fcClient:    fcClient,
		guestMAC:    meta.GuestMAC,
		guestCID:    meta.GuestCID,
		bootArgs:    meta.BootArgs,
	}

	// Wait for agent
	agentClient, err := m.waitForAgent(context.Background(), vsockPath, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("agent not ready after wake: %w", err)
	}
	vm.agent = agentClient

	// Register VM
	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	mode := "snapshot restore"
	if !snapshotRestore {
		mode = "cold boot (cross-worker)"
	}
	log.Printf("firecracker: woke VM %s (%s, port=%d, tap=%s)",
		sandboxID, mode, hostPort, netCfg.TAPName)

	return vmToSandbox(vm), nil
}

// coldBootLocal boots a fresh VM using an existing workspace.ext4 on NVMe.
// Used for hard kill recovery — processes are lost but /workspace files are preserved.
func (m *Manager) coldBootLocal(ctx context.Context, sandboxID string, timeout int) (*types.Sandbox, error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")

	if !fileExists(workspacePath) {
		return nil, fmt.Errorf("workspace not found at %s", workspacePath)
	}

	// Read sandbox-meta.json for template + config
	sbMetaPath := filepath.Join(sandboxDir, "sandbox-meta.json")
	metaJSON, err := os.ReadFile(sbMetaPath)
	if err != nil {
		return nil, fmt.Errorf("read sandbox-meta.json: %w", err)
	}
	var meta SandboxMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, fmt.Errorf("parse sandbox-meta.json: %w", err)
	}

	// Recreate rootfs from template if missing
	if !fileExists(rootfsPath) {
		baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, meta.Template)
		if err != nil {
			return nil, fmt.Errorf("resolve base image: %w", err)
		}
		if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
			return nil, fmt.Errorf("prepare rootfs: %w", err)
		}
		log.Printf("firecracker: cold-boot-local %s: rootfs recreated from template %q", sandboxID, meta.Template)
	}

	// Allocate fresh network
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

	vsockPath := filepath.Join(sandboxDir, "vsock.sock")
	guestCID := m.allocateCID()
	guestMAC := generateMAC(sandboxID)
	bootArgs := fmt.Sprintf(
		"keep_bootcon console=ttyS0 reboot=k panic=1 pci=off "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	apiSockPath := filepath.Join(sandboxDir, "firecracker.sock")
	os.Remove(apiSockPath)
	os.Remove(vsockPath)

	logPath := filepath.Join(sandboxDir, "firecracker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(m.cfg.FirecrackerBin, "--api-sock", apiSockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	logFile.Close()

	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("wait for API socket: %w", err)
	}

	// Configure and start VM
	if err := fcClient.PutMachineConfig(cpus, memMB); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("put machine config: %w", err)
	}
	if err := fcClient.PutBootSource(m.cfg.KernelPath, bootArgs); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("put boot source: %w", err)
	}
	if err := fcClient.PutDrive("rootfs", rootfsPath, true, false); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("put rootfs drive: %w", err)
	}
	if err := fcClient.PutDrive("workspace", workspacePath, false, false); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("put workspace drive: %w", err)
	}
	if err := fcClient.PutNetworkInterface("eth0", guestMAC, netCfg.TAPName); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("put network interface: %w", err)
	}
	if err := fcClient.PutVsock(guestCID, vsockPath); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("put vsock: %w", err)
	}
	if err := fcClient.StartInstance(); err != nil {
		cmd.Process.Kill(); cmd.Wait(); m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("start instance: %w", err)
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
		vsockPath:   vsockPath,
		sandboxDir:  sandboxDir,
		apiSockPath: apiSockPath,
		fcClient:    fcClient,
		guestMAC:    guestMAC,
		guestCID:    guestCID,
		bootArgs:    bootArgs,
	}

	agentClient, err := m.waitForAgent(context.Background(), vsockPath, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("agent not ready after cold boot: %w", err)
	}
	vm.agent = agentClient

	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	log.Printf("firecracker: cold-boot-local %s (template=%s, port=%d, tap=%s)", sandboxID, meta.Template, hostPort, netCfg.TAPName)
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
