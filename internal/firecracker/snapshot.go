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
	"github.com/opensandbox/opensandbox/internal/sparse"
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
		HibernationKey: checkpointKey,
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

// --- Save as Template ---

// doSaveAsTemplate snapshots a running sandbox's drives for use as a reusable template.
// The VM is briefly paused while the drives are copied, then resumed. The user's sandbox
// continues running uninterrupted; the copied drives are compressed and uploaded to S3 async.
//
// Template drives are cached locally at {DataDir}/templates/{templateID}/ so that creating
// new sandboxes from this template is a fast reflink copy (no S3 download needed on the
// same worker). S3 is the durable backup for cross-worker use.
//
// Flow:
//  1. SyncFS via agent (flush disk buffers — agent stays alive)
//  2. Close gRPC connection (vsock must be inactive for pause)
//  3. Pause VM
//  4. Copy rootfs.ext4 and workspace.ext4 to template cache dir (reflink — instant on XFS)
//  5. Resume VM
//  6. Reconnect agent
//  7. (async) Compress cached drives → upload to S3 (cache stays on disk)
func (m *Manager) doSaveAsTemplate(ctx context.Context, vm *VMInstance, templateID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error) {
	t0 := time.Now()

	rootfsKey = fmt.Sprintf("templates/%s/rootfs.tar.zst", templateID)
	workspaceKey = fmt.Sprintf("templates/%s/workspace.sparse.zst", templateID)

	srcRootfs := filepath.Join(vm.sandboxDir, "rootfs.ext4")
	srcWorkspace := filepath.Join(vm.sandboxDir, "workspace.ext4")

	// Cache template drives locally for fast reflink-based creation
	cacheDir := m.templateCacheDir(templateID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", fmt.Errorf("create template cache dir: %w", err)
	}
	cachedRootfs := filepath.Join(cacheDir, "rootfs.ext4")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.ext4")

	// Step 1: SyncFS (agent stays alive — only the gRPC connection needs to be closed for pause)
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if syncErr := vm.agent.SyncFS(syncCtx); syncErr != nil {
			log.Printf("firecracker: SaveAsTemplate %s: SyncFS warning: %v", vm.ID, syncErr)
		}
		cancel()
	}

	// Step 2: Close gRPC connection (vsock must be inactive before Firecracker pause)
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}

	// Step 3: Pause the VM
	if err = vm.fcClient.PauseVM(); err != nil {
		// Try to reconnect agent even on error so the VM stays usable
		if agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 5*time.Second); reconnErr == nil {
			vm.agent = agent
		}
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("pause VM for template snapshot: %w", err)
	}
	log.Printf("firecracker: SaveAsTemplate %s: VM paused (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 4: Copy drive files while VM is paused (drives are stable)
	copyErr := copyFileReflink(srcRootfs, cachedRootfs)
	if copyErr == nil {
		copyErr = copyFileReflink(srcWorkspace, cachedWorkspace)
		if copyErr != nil {
			os.Remove(cachedRootfs)
		}
	}

	// Step 5: Resume the VM regardless of copy result
	resumeErr := vm.fcClient.ResumeVM()
	if resumeErr != nil {
		log.Printf("firecracker: SaveAsTemplate %s: CRITICAL: resume failed: %v", vm.ID, resumeErr)
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("resume VM after template snapshot: %w", resumeErr)
	}
	log.Printf("firecracker: SaveAsTemplate %s: VM resumed (%dms)", vm.ID, time.Since(t0).Milliseconds())

	if copyErr != nil {
		// Reconnect agent since VM is running again
		if agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 10*time.Second); reconnErr == nil {
			vm.agent = agent
		}
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("copy drives for template: %w", copyErr)
	}

	// Step 6: Reconnect agent (it was running inside the VM the whole time — just paused)
	agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 10*time.Second)
	if reconnErr != nil {
		log.Printf("firecracker: SaveAsTemplate %s: agent reconnect failed: %v (VM still running)", vm.ID, reconnErr)
	} else {
		vm.agent = agent
	}

	// Step 7: Async compress + upload (cached drives stay on disk for fast local creation)
	sandboxID := vm.ID
	m.uploadWg.Add(1)
	go func() {
		defer m.uploadWg.Done()

		uploadCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		t1 := time.Now()
		if err := compressAndUploadFile(uploadCtx, cachedRootfs, rootfsKey, checkpointStore); err != nil {
			log.Printf("firecracker: template %s: rootfs upload failed for sandbox %s: %v", templateID, sandboxID, err)
			return
		}
		log.Printf("firecracker: template %s: rootfs uploaded (%dms)", templateID, time.Since(t1).Milliseconds())

		t2 := time.Now()
		if err := sparseCompressAndUpload(uploadCtx, cachedWorkspace, workspaceKey, checkpointStore); err != nil {
			log.Printf("firecracker: template %s: workspace upload failed for sandbox %s: %v", templateID, sandboxID, err)
			return
		}
		log.Printf("firecracker: template %s: ready (%dms, total=%dms)", templateID, time.Since(t2).Milliseconds(), time.Since(t0).Milliseconds())
		if onReady != nil {
			onReady()
		}
	}()

	return rootfsKey, workspaceKey, nil
}

// templateCacheDir returns the local cache directory for a template's ext4 drives.
func (m *Manager) templateCacheDir(templateID string) string {
	return filepath.Join(m.cfg.DataDir, "templates", templateID)
}

// TemplateCachePath returns the local path to a cached template drive, or "" if not cached.
func (m *Manager) TemplateCachePath(templateID, filename string) string {
	p := filepath.Join(m.templateCacheDir(templateID), filename)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// checkpointCacheDir returns the local cache directory for a checkpoint's ext4 drives.
func (m *Manager) checkpointCacheDir(checkpointID string) string {
	return filepath.Join(m.cfg.DataDir, "checkpoints", checkpointID)
}

// CheckpointCachePath returns the local path to a cached checkpoint drive, or "" if not cached.
func (m *Manager) CheckpointCachePath(checkpointID, filename string) string {
	p := filepath.Join(m.checkpointCacheDir(checkpointID), filename)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// CreateCheckpoint snapshots a running sandbox's drives (rootfs + workspace) for later restore.
// The VM is briefly paused (~10-50ms) during file copy then resumed. Archive upload is async.
// Returns the pre-computed S3 keys immediately. onReady is called when the async upload finishes.
func (m *Manager) CreateCheckpoint(ctx context.Context, sandboxID, checkpointID string, checkpointStore *storage.CheckpointStore, onReady func()) (rootfsKey, workspaceKey string, err error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return "", "", err
	}

	t0 := time.Now()

	rootfsKey = fmt.Sprintf("checkpoints/%s/%s/rootfs.tar.zst", sandboxID, checkpointID)
	workspaceKey = fmt.Sprintf("checkpoints/%s/%s/workspace.sparse.zst", sandboxID, checkpointID)

	srcRootfs := filepath.Join(vm.sandboxDir, "rootfs.ext4")
	srcWorkspace := filepath.Join(vm.sandboxDir, "workspace.ext4")

	// Cache checkpoint drives locally for fast restore
	cacheDir := m.checkpointCacheDir(checkpointID)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", "", fmt.Errorf("create checkpoint cache dir: %w", err)
	}
	cachedRootfs := filepath.Join(cacheDir, "rootfs.ext4")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.ext4")

	// Step 1: SyncFS (agent stays alive)
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if syncErr := vm.agent.SyncFS(syncCtx); syncErr != nil {
			log.Printf("firecracker: CreateCheckpoint %s: SyncFS warning: %v", vm.ID, syncErr)
		}
		cancel()
	}

	// Step 2: Close gRPC connection (vsock must be inactive before pause)
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}

	// Step 3: Pause the VM
	if err = vm.fcClient.PauseVM(); err != nil {
		if agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 5*time.Second); reconnErr == nil {
			vm.agent = agent
		}
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("pause VM for checkpoint: %w", err)
	}
	log.Printf("firecracker: CreateCheckpoint %s/%s: VM paused (%dms)", vm.ID, checkpointID, time.Since(t0).Milliseconds())

	// Step 4: Copy drive files while VM is paused (drives are stable)
	copyErr := copyFileReflink(srcRootfs, cachedRootfs)
	if copyErr == nil {
		copyErr = copyFileReflink(srcWorkspace, cachedWorkspace)
		if copyErr != nil {
			os.Remove(cachedRootfs)
		}
	}

	// Step 5: Resume the VM regardless of copy result
	resumeErr := vm.fcClient.ResumeVM()
	if resumeErr != nil {
		log.Printf("firecracker: CreateCheckpoint %s: CRITICAL: resume failed: %v", vm.ID, resumeErr)
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("resume VM after checkpoint: %w", resumeErr)
	}
	log.Printf("firecracker: CreateCheckpoint %s/%s: VM resumed (%dms)", vm.ID, checkpointID, time.Since(t0).Milliseconds())

	if copyErr != nil {
		if agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 10*time.Second); reconnErr == nil {
			vm.agent = agent
		}
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("copy drives for checkpoint: %w", copyErr)
	}

	// Step 6: Reconnect agent
	agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 10*time.Second)
	if reconnErr != nil {
		log.Printf("firecracker: CreateCheckpoint %s: agent reconnect failed: %v (VM still running)", vm.ID, reconnErr)
	} else {
		vm.agent = agent
	}

	// Step 7: Async compress + upload to S3
	m.uploadWg.Add(1)
	go func() {
		defer m.uploadWg.Done()

		uploadCtx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		t1 := time.Now()
		if err := compressAndUploadFile(uploadCtx, cachedRootfs, rootfsKey, checkpointStore); err != nil {
			log.Printf("firecracker: checkpoint %s: rootfs upload failed: %v", checkpointID, err)
			return
		}
		log.Printf("firecracker: checkpoint %s: rootfs uploaded (%dms)", checkpointID, time.Since(t1).Milliseconds())

		t2 := time.Now()
		if err := sparseCompressAndUpload(uploadCtx, cachedWorkspace, workspaceKey, checkpointStore); err != nil {
			log.Printf("firecracker: checkpoint %s: workspace upload failed: %v", checkpointID, err)
			return
		}
		log.Printf("firecracker: checkpoint %s: ready (%dms, total=%dms)", checkpointID, time.Since(t2).Milliseconds(), time.Since(t0).Milliseconds())
		if onReady != nil {
			onReady()
		}
	}()

	return rootfsKey, workspaceKey, nil
}

// RestoreFromCheckpoint reverts a running sandbox to a checkpoint.
// The current VM is killed, drives are replaced with checkpoint copies, and a new VM is cold booted.
// The sandbox ID stays the same.
func (m *Manager) RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	cacheDir := m.checkpointCacheDir(checkpointID)
	cachedRootfs := filepath.Join(cacheDir, "rootfs.ext4")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.ext4")

	// Verify checkpoint files exist locally
	if _, err := os.Stat(cachedRootfs); err != nil {
		return fmt.Errorf("checkpoint rootfs not found locally: %w", err)
	}
	if _, err := os.Stat(cachedWorkspace); err != nil {
		return fmt.Errorf("checkpoint workspace not found locally: %w", err)
	}

	// Step 1: SyncFS (best effort — save current state in case needed)
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = vm.agent.SyncFS(syncCtx)
		cancel()
		vm.agent.Close()
		vm.agent = nil
	}

	// Step 2: Kill the current Firecracker process
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		_ = vm.cmd.Wait()
	}

	// Step 2b: Clean up socket files so the new VM can bind
	_ = os.Remove(vm.vsockPath)
	if vm.apiSockPath != "" {
		_ = os.Remove(vm.apiSockPath)
	}

	// Step 3: Save VM config before removing from map
	template := vm.Template
	cpuCount := vm.CpuCount
	memoryMB := vm.MemoryMB
	guestPort := vm.GuestPort

	// Step 4: Clean up network
	if vm.network != nil {
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	// Step 5: Remove old VM from tracking
	m.mu.Lock()
	delete(m.vms, sandboxID)
	m.mu.Unlock()

	// Step 6: Cold boot a fresh VM using checkpoint drives
	// Pass checkpoint cache paths as template keys so createWithID copies them
	// instead of creating fresh drives from the base image.
	cfg := types.SandboxConfig{
		Template:             template,
		CpuCount:             cpuCount,
		MemoryMB:             memoryMB,
		Port:                 guestPort,
		NetworkEnabled:       true,
		TemplateRootfsKey:    "local://" + cachedRootfs,
		TemplateWorkspaceKey: "local://" + cachedWorkspace,
	}

	sb, err := m.createWithID(ctx, sandboxID, cfg)
	if err != nil {
		return fmt.Errorf("cold boot after checkpoint restore: %w", err)
	}
	log.Printf("firecracker: RestoreFromCheckpoint %s/%s: VM cold booted (status=%s)", sandboxID, checkpointID, sb.Status)

	return nil
}

// copyFileReflink copies a file using cp --reflink=auto (instant on XFS reflink, fallback to full copy).
func copyFileReflink(src, dst string) error {
	cmd := exec.Command("cp", "--reflink=auto", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s → %s: %w (%s)", src, dst, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sparseCompressAndUpload creates a sparse archive of a file and uploads it to S3.
// Only non-zero 4KB blocks are stored, making archive size proportional to actual content.
func sparseCompressAndUpload(ctx context.Context, srcPath, s3Key string, store *storage.CheckpointStore) error {
	archivePath := srcPath + ".sparse.zst"
	blocks, err := sparse.Create(srcPath, archivePath)
	if err != nil {
		return fmt.Errorf("sparse compress: %w", err)
	}
	defer os.Remove(archivePath)

	info, _ := os.Stat(archivePath)
	log.Printf("firecracker: sparse archive %s: %d non-zero blocks, %d KB compressed",
		filepath.Base(srcPath), blocks, info.Size()/1024)

	if _, err := store.Upload(ctx, s3Key, archivePath); err != nil {
		return fmt.Errorf("upload to %s: %w", s3Key, err)
	}
	return nil
}

// compressAndUploadFile compresses a single file as a tar.zst archive and uploads it to S3.
func compressAndUploadFile(ctx context.Context, srcPath, s3Key string, store *storage.CheckpointStore) error {
	archivePath := srcPath + ".tar.zst"
	if err := createArchive(archivePath, filepath.Dir(srcPath), []string{filepath.Base(srcPath)}); err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	defer os.Remove(archivePath)

	if _, err := store.Upload(ctx, s3Key, archivePath); err != nil {
		return fmt.Errorf("upload to %s: %w", s3Key, err)
	}
	return nil
}
