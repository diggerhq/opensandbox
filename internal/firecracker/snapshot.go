package firecracker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/opensandbox/opensandbox/internal/sandbox"
	"github.com/opensandbox/opensandbox/internal/sparse"
	"github.com/opensandbox/opensandbox/internal/storage"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/agent"
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

	// Step 1: Sync filesystems inside the VM (agent stays alive for snapshot).
	// Use exec-based sync because syscall.Sync() from PID 1 doesn't reliably flush
	// dirty pages to virtio-blk after snapshot restore.
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, syncErr := vm.agent.Exec(syncCtx, &pb.ExecRequest{
			Command: "sync",
		})
		cancel()
		if syncErr != nil {
			log.Printf("firecracker: hibernate %s: exec sync failed: %v", vm.ID, syncErr)
		}
	}
	log.Printf("firecracker: hibernate %s: guest sync done (%dms)", vm.ID, time.Since(t0).Milliseconds())

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

	// Step 1: Flush guest filesystem data to disk.
	// Use exec-based sync because syscall.Sync() from PID 1 doesn't reliably flush
	// dirty pages to virtio-blk after snapshot restore.
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, syncErr := vm.agent.Exec(syncCtx, &pb.ExecRequest{
			Command: "sync",
		})
		cancel()
		if syncErr != nil {
			log.Printf("firecracker: SaveAsTemplate %s: exec sync warning: %v", vm.ID, syncErr)
		}
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

	// Step 4: Host-side fsync as insurance — guest sync should have already flushed.
	for _, drivePath := range []string{srcRootfs, srcWorkspace} {
		if f, err := os.OpenFile(drivePath, os.O_RDWR, 0); err == nil {
			f.Sync()
			f.Close()
		}
	}

	// Step 4b: Copy drive files while VM is paused (drives are stable)
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

// CreateCheckpoint snapshots a running sandbox's state for later restore.
// The VM is briefly paused during memory snapshot + drive reflink then resumed. Archive upload is async.
// Returns the pre-computed S3 keys immediately. onReady is called when the async upload finishes.
//
// SyncFS is intentionally skipped: the memory snapshot captures the guest kernel's entire page
// cache, including all dirty (unflushed) data. On warm restore, the guest resumes with its full
// page cache intact — no data is lost regardless of how much dirty data existed at snapshot time.
// For cold boot fallback (no memory snapshot), unflushed data would be lost, equivalent to crash
// recovery (ext4 journaling handles consistency). Skipping SyncFS eliminates the biggest latency
// contributor: flushing 10GB+ of dirty data to disk could take 20-30s on EBS gp3.
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

	// Step 1: Flush guest filesystem data to disk (required for cold boot forks that don't use memory snapshot).
	// We run `sync` as a subprocess because syscall.Sync() from PID 1 (the agent) doesn't reliably
	// flush dirty pages to the virtio-blk device after snapshot restore. Running sync as a child
	// process works correctly and ensures all dirty pages are written to the host file.
	if vm.agent != nil {
		tSync1 := time.Now()
		syncCtx, syncCancel := context.WithTimeout(ctx, 10*time.Second)
		resp, syncErr := vm.agent.Exec(syncCtx, &pb.ExecRequest{
			Command: "sync",
		})
		syncCancel()
		if syncErr != nil {
			log.Printf("firecracker: CreateCheckpoint %s/%s: exec sync failed: %v", vm.ID, checkpointID, syncErr)
		} else if resp.ExitCode != 0 {
			log.Printf("firecracker: CreateCheckpoint %s/%s: exec sync exit code %d: %s", vm.ID, checkpointID, resp.ExitCode, resp.Stderr)
		} else {
			log.Printf("firecracker: CreateCheckpoint %s/%s: guest sync completed (%dms)", vm.ID, checkpointID, time.Since(tSync1).Milliseconds())
		}
	} else {
		log.Printf("firecracker: CreateCheckpoint %s/%s: WARNING: vm.agent is nil, guest sync SKIPPED!", vm.ID, checkpointID)
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

	// Step 4: Create memory snapshot to tmpfs (/dev/shm) for fast write.
	// Writing to tmpfs avoids NVMe fsync overhead (the main bottleneck: 15-100s on disk).
	// Tmpfs writes are memory-bandwidth limited (~30-100ms for 1GB) since they're just memcpy.
	// The memory file is moved from tmpfs to the checkpoint cache dir in the background after resume.
	snapshotDir := filepath.Join(cacheDir, "snapshot")
	if mkdirErr := os.MkdirAll(snapshotDir, 0755); mkdirErr != nil {
		log.Printf("firecracker: CreateCheckpoint %s: mkdir snapshot dir failed: %v", vm.ID, mkdirErr)
	}
	vmstateFile := filepath.Join(snapshotDir, "vmstate")
	tmpfsMemFile := fmt.Sprintf("/dev/shm/fc-snap-%s-%s.mem", sandboxID, checkpointID)
	finalMemFile := filepath.Join(snapshotDir, "mem")

	var snapshotErr error
	tSnap := time.Now()
	if err := vm.fcClient.CreateSnapshot(vmstateFile, tmpfsMemFile); err != nil {
		log.Printf("firecracker: CreateCheckpoint %s/%s: memory snapshot failed: %v (will fall back to cold boot restore)", vm.ID, checkpointID, err)
		snapshotErr = err
		// Clean up partial snapshot files
		os.Remove(tmpfsMemFile)
		os.Remove(vmstateFile)
		os.RemoveAll(snapshotDir)
	} else {
		log.Printf("firecracker: CreateCheckpoint %s/%s: memory snapshot to tmpfs (%dms)", vm.ID, checkpointID, time.Since(tSnap).Milliseconds())
	}

	// Step 5: Host-side fsync as insurance — guest sync should have already flushed
	// all dirty pages via Firecracker pwrite, but this ensures XFS CoW block allocation.
	for _, drivePath := range []string{srcRootfs, srcWorkspace} {
		if f, err := os.OpenFile(drivePath, os.O_RDWR, 0); err == nil {
			f.Sync()
			f.Close()
		}
	}

	// Step 5b: Copy drive files while VM is paused (drives are stable via reflink, sub-ms)
	copyErr := copyFileReflink(srcRootfs, cachedRootfs)
	if copyErr == nil {
		copyErr = copyFileReflink(srcWorkspace, cachedWorkspace)
		if copyErr != nil {
			os.Remove(cachedRootfs)
		}
	}

	// Step 6: Resume the VM regardless of copy/snapshot result
	resumeErr := vm.fcClient.ResumeVM()
	if resumeErr != nil {
		log.Printf("firecracker: CreateCheckpoint %s: CRITICAL: resume failed: %v", vm.ID, resumeErr)
		os.Remove(tmpfsMemFile)
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("resume VM after checkpoint: %w", resumeErr)
	}
	pauseDuration := time.Since(t0)
	log.Printf("firecracker: CreateCheckpoint %s/%s: VM resumed, total pause=%dms", vm.ID, checkpointID, pauseDuration.Milliseconds())

	if copyErr != nil {
		os.Remove(tmpfsMemFile)
		if agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 10*time.Second); reconnErr == nil {
			vm.agent = agent
		}
		os.RemoveAll(cacheDir)
		return "", "", fmt.Errorf("copy drives for checkpoint: %w", copyErr)
	}

	// Step 7: Reconnect agent
	agent, reconnErr := m.waitForAgent(ctx, vm.vsockPath, 10*time.Second)
	if reconnErr != nil {
		log.Printf("firecracker: CreateCheckpoint %s: agent reconnect failed: %v (VM still running)", vm.ID, reconnErr)
	} else {
		vm.agent = agent
	}

	// Step 8: Write snapshot metadata + move mem from tmpfs (background)
	// If memory snapshot succeeded, write metadata and move the mem file
	// from /dev/shm to the checkpoint cache dir. This happens after resume
	// so it doesn't contribute to pause duration.
	hasSnapshot := snapshotErr == nil

	if hasSnapshot {
		// Write snapshot metadata (needed for warm restore)
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
		metaJSON, _ := json.MarshalIndent(meta, "", "  ")
		if writeErr := os.WriteFile(filepath.Join(snapshotDir, "snapshot-meta.json"), metaJSON, 0644); writeErr != nil {
			log.Printf("firecracker: CreateCheckpoint %s: write snapshot meta failed: %v", vm.ID, writeErr)
			hasSnapshot = false
			os.Remove(tmpfsMemFile)
		}
	}

	// Step 9: Async move mem from tmpfs + compress + upload to S3
	m.uploadWg.Add(1)
	go func() {
		defer m.uploadWg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("firecracker: checkpoint %s: upload goroutine panic: %v", checkpointID, r)
			}
		}()

		// Move memory file from tmpfs to checkpoint cache (frees tmpfs RAM)
		if hasSnapshot {
			tMove := time.Now()
			if moveErr := os.Rename(tmpfsMemFile, finalMemFile); moveErr != nil {
				// Rename across filesystems (tmpfs→xfs) won't work, fall back to copy+delete
				if cpErr := copyFile(tmpfsMemFile, finalMemFile); cpErr != nil {
					log.Printf("firecracker: checkpoint %s: move mem from tmpfs failed: %v", checkpointID, cpErr)
					os.Remove(tmpfsMemFile)
				} else {
					os.Remove(tmpfsMemFile)
					log.Printf("firecracker: checkpoint %s: mem copied from tmpfs (%dms)", checkpointID, time.Since(tMove).Milliseconds())
				}
			} else {
				log.Printf("firecracker: checkpoint %s: mem moved from tmpfs (%dms)", checkpointID, time.Since(tMove).Milliseconds())
			}
		}

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
// Attempts warm restore (LoadSnapshot) if a memory snapshot exists in the checkpoint cache.
// Falls back to cold boot if warm restore fails or no snapshot is available.
func (m *Manager) RestoreFromCheckpoint(ctx context.Context, sandboxID, checkpointID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	// Signal that this VM is restoring — Exec and other operations will wait on this channel
	restoreCh := make(chan struct{})
	vm.restoring = restoreCh
	defer close(restoreCh)

	cacheDir := m.checkpointCacheDir(checkpointID)
	cachedRootfs := filepath.Join(cacheDir, "rootfs.ext4")
	cachedWorkspace := filepath.Join(cacheDir, "workspace.ext4")

	// Verify checkpoint drives exist locally
	if _, err := os.Stat(cachedRootfs); err != nil {
		return fmt.Errorf("checkpoint rootfs not found locally: %w", err)
	}
	if _, err := os.Stat(cachedWorkspace); err != nil {
		return fmt.Errorf("checkpoint workspace not found locally: %w", err)
	}

	// Check if warm restore is possible (memory snapshot exists in checkpoint cache)
	snapshotDir := filepath.Join(cacheDir, "snapshot")
	memFile := filepath.Join(snapshotDir, "mem")
	vmstateFile := filepath.Join(snapshotDir, "vmstate")
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")
	canWarmRestore := fileExists(memFile) && fileExists(vmstateFile) && fileExists(metaPath)

	if canWarmRestore {
		if err := m.warmRestoreFromCheckpoint(ctx, vm, sandboxID, checkpointID, cacheDir); err != nil {
			log.Printf("firecracker: RestoreFromCheckpoint %s/%s: warm restore failed: %v, falling back to cold boot", sandboxID, checkpointID, err)
			// Warm restore may have killed the VM — re-fetch to check
			vm, _ = m.getVM(sandboxID)
		} else {
			return nil // Warm restore succeeded
		}
	}

	// Cold boot fallback
	return m.coldBootRestoreFromCheckpoint(ctx, vm, sandboxID, checkpointID, cachedRootfs, cachedWorkspace)
}

// warmRestoreFromCheckpoint restores a sandbox using Firecracker's snapshot load.
// The VM resumes from the exact state when the checkpoint was created — all processes
// are preserved, and restore completes in ~200-500ms instead of ~3.5s cold boot.
func (m *Manager) warmRestoreFromCheckpoint(ctx context.Context, vm *VMInstance, sandboxID, checkpointID string, cacheDir string) error {
	t0 := time.Now()

	snapshotDir := filepath.Join(cacheDir, "snapshot")
	memFile := filepath.Join(snapshotDir, "mem")
	vmstateFile := filepath.Join(snapshotDir, "vmstate")
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")

	// Read snapshot metadata
	metaJSON, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read snapshot meta: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return fmt.Errorf("parse snapshot meta: %w", err)
	}

	// Save VM state we need after kill
	sandboxDir := vm.sandboxDir
	endAt := vm.EndAt

	// Step 1: Close agent on current VM
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}

	// Step 2: Kill the current Firecracker process
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		_ = vm.cmd.Wait()
	}

	// Step 3: Clean up sockets
	_ = os.Remove(vm.vsockPath)
	if vm.apiSockPath != "" {
		_ = os.Remove(vm.apiSockPath)
	}

	// Step 4: Reflink copy checkpoint drives to sandbox dir (same absolute paths baked into vmstate)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
	if err := copyFileReflink(filepath.Join(cacheDir, "rootfs.ext4"), rootfsPath); err != nil {
		return fmt.Errorf("copy checkpoint rootfs: %w", err)
	}
	if err := copyFileReflink(filepath.Join(cacheDir, "workspace.ext4"), workspacePath); err != nil {
		return fmt.Errorf("copy checkpoint workspace: %w", err)
	}

	// Step 5: Release old network and reclaim the same TAP name (baked into vmstate)
	if vm.network != nil {
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	netCfg, err := m.subnets.AllocateSpecific(meta.Network.TAPName)
	if err != nil {
		return fmt.Errorf("reclaim TAP %s for warm restore: %w", meta.Network.TAPName, err)
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
	netCfg.GuestPort = meta.GuestPort
	if netCfg.GuestPort == 0 {
		netCfg.GuestPort = m.cfg.DefaultPort
	}

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return fmt.Errorf("add DNAT: %w", err)
	}

	// Step 6: Start fresh Firecracker process (snapshot mode — no config needed)
	apiSockPath := filepath.Join(sandboxDir, "firecracker.sock")
	vsockPath := meta.VsockPath
	_ = os.Remove(apiSockPath)
	_ = os.Remove(vsockPath)

	logPath := filepath.Join(sandboxDir, "firecracker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(m.cfg.FirecrackerBin, "--api-sock", apiSockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("start firecracker: %w", err)
	}
	logFile.Close()

	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("wait for API socket: %w", err)
	}

	// Step 7: Load snapshot — VM resumes exactly where it was paused during checkpoint
	if err := fcClient.LoadSnapshot(vmstateFile, memFile, true); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("load snapshot: %w", err)
	}

	log.Printf("firecracker: RestoreFromCheckpoint %s/%s: snapshot loaded (%dms)", sandboxID, checkpointID, time.Since(t0).Milliseconds())

	// Step 8: Wait for agent (should be near-instant — agent was running when snapshot was taken)
	agentClient, err := m.waitForAgent(context.Background(), vsockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return fmt.Errorf("agent not ready after warm restore: %w", err)
	}

	// Step 9: Register restored VM in tracking map
	newVM := &VMInstance{
		ID:          sandboxID,
		Template:    meta.Template,
		Status:      types.SandboxStatusRunning,
		StartedAt:   time.Now(),
		EndAt:       endAt,
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
		agent:       agentClient,
	}

	m.mu.Lock()
	delete(m.vms, sandboxID) // remove old entry if still present
	m.vms[sandboxID] = newVM
	m.mu.Unlock()

	log.Printf("firecracker: RestoreFromCheckpoint %s/%s: warm restore complete (%dms, port=%d, tap=%s)",
		sandboxID, checkpointID, time.Since(t0).Milliseconds(), hostPort, netCfg.TAPName)

	return nil
}

// coldBootRestoreFromCheckpoint is the fallback restore path.
// Kills the VM, replaces drives, and cold boots a new VM from checkpoint drives.
func (m *Manager) coldBootRestoreFromCheckpoint(ctx context.Context, vm *VMInstance, sandboxID, checkpointID, cachedRootfs, cachedWorkspace string) error {
	if vm != nil {
		// Close agent and kill process if not already done by failed warm restore
		if vm.agent != nil {
			vm.agent.Close()
			vm.agent = nil
		}
		if vm.cmd != nil && vm.cmd.Process != nil {
			_ = vm.cmd.Process.Kill()
			_ = vm.cmd.Wait()
		}

		// Clean up sockets
		_ = os.Remove(vm.vsockPath)
		if vm.apiSockPath != "" {
			_ = os.Remove(vm.apiSockPath)
		}

		// Clean up network
		if vm.network != nil {
			RemoveDNAT(vm.network)
			DeleteTAP(vm.network.TAPName)
			m.subnets.Release(vm.network.TAPName)
		}
	}

	// NOTE: We do NOT delete the old VM from the tracking map here.
	// The old VM entry (with restoring channel) must remain so that
	// concurrent Exec calls can find it and wait on the restoring channel.
	// createWithID below will overwrite the entry with the new VM.

	// Cold boot a fresh VM using checkpoint drives
	template := "default"
	cpuCount := 0
	memoryMB := 0
	guestPort := 0
	if vm != nil {
		template = vm.Template
		cpuCount = vm.CpuCount
		memoryMB = vm.MemoryMB
		guestPort = vm.GuestPort
	}

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

// copyFile copies a file using standard cp (no reflink). Used for cross-filesystem
// copies like tmpfs → xfs where os.Rename doesn't work.
func copyFile(src, dst string) error {
	cmd := exec.Command("cp", src, dst)
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

	info, statErr := os.Stat(archivePath)
	if statErr != nil {
		return fmt.Errorf("stat sparse archive %s: %w", archivePath, statErr)
	}
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

// --- Warm Fork from Checkpoint ---

// Firecracker CRC64: Jones polynomial (bit-reversed form) with init=0, no final XOR.
// Polynomial 0x95AC9329AC4BC9B5 is the bit-reverse of 0xad93d23594c935a9.
// Go's hash/crc64 package uses a different polynomial (ECMA or ISO), so we build
// our own table. The lookup algorithm is standard LSB-first (reflected).
var firecrackerCRC64Table = func() [256]uint64 {
	const poly = 0x95AC9329AC4BC9B5
	var table [256]uint64
	for i := 0; i < 256; i++ {
		crc := uint64(i)
		for j := 0; j < 8; j++ {
			if crc&1 == 1 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}
		table[i] = crc
	}
	return table
}()

func firecrackerCRC64(data []byte) uint64 {
	crc := uint64(0)
	for _, v := range data {
		crc = firecrackerCRC64Table[byte(crc)^v] ^ (crc >> 8)
	}
	return crc
}

// patchVMStateBinary performs binary find-and-replace on a Firecracker vmstate file.
// It patches sandbox ID (fixing drive paths + vsock path), TAP name, and CID,
// then recalculates the CRC64 checksum that Firecracker validates on load.
//
// The vmstate format: [serialized data][8-byte CRC64 Jones/Redis checksum]
// TAP names must be the same length (use fixed-width fc-tap%07d format).
// Returns an error if the old sandbox ID is not found (safety check).
func patchVMStateBinary(vmstatePath, oldSandboxID, newSandboxID, oldTAP, newTAP string, oldCID, newCID uint32) error {
	raw, err := os.ReadFile(vmstatePath)
	if err != nil {
		return fmt.Errorf("read vmstate: %w", err)
	}

	if len(raw) < 8 {
		return fmt.Errorf("vmstate too small (%d bytes)", len(raw))
	}

	// Split into data and trailing CRC64 checksum.
	// Firecracker uses Jones/Redis CRC64 (bit-reversed polynomial 0x95AC9329AC4BC9B5,
	// reflected/LSB-first lookup, init=0, no final XOR).
	data := raw[:len(raw)-8]
	storedCRC := binary.LittleEndian.Uint64(raw[len(raw)-8:])

	computedCRC := firecrackerCRC64(data)
	if computedCRC != storedCRC {
		return fmt.Errorf("vmstate CRC mismatch before patching: stored=%d computed=%d (unexpected format)", storedCRC, computedCRC)
	}

	// Patch sandbox ID — fixes drive paths (/data/sandboxes/{id}/rootfs.ext4, workspace.ext4)
	// and vsock path (/data/sandboxes/{id}/vsock.sock)
	oldID := []byte(oldSandboxID)
	newID := []byte(newSandboxID)
	if len(oldID) != len(newID) {
		return fmt.Errorf("sandbox ID length mismatch: %q (%d) vs %q (%d)", oldSandboxID, len(oldID), newSandboxID, len(newID))
	}
	if !bytes.Contains(data, oldID) {
		return fmt.Errorf("old sandbox ID %q not found in vmstate — cannot patch", oldSandboxID)
	}
	data = bytes.ReplaceAll(data, oldID, newID)

	// Patch TAP name — must be same length (fixed-width format)
	if oldTAP != "" && newTAP != "" && oldTAP != newTAP {
		oldTAPBytes := []byte(oldTAP)
		newTAPBytes := []byte(newTAP)
		if len(oldTAPBytes) != len(newTAPBytes) {
			return fmt.Errorf("TAP name length mismatch: %q (%d) vs %q (%d) — use fixed-width format", oldTAP, len(oldTAPBytes), newTAP, len(newTAPBytes))
		}
		data = bytes.ReplaceAll(data, oldTAPBytes, newTAPBytes)
	}

	// Patch CID — search for the old CID (uint32 LE) near the vsock.sock string.
	// The CID appears in the vsock device state in the vmstate binary.
	if oldCID != newCID {
		oldCIDBytes := make([]byte, 4)
		newCIDBytes := make([]byte, 4)
		binary.LittleEndian.PutUint32(oldCIDBytes, oldCID)
		binary.LittleEndian.PutUint32(newCIDBytes, newCID)

		// Find vsock.sock marker and search within 1KB around it for the CID
		marker := []byte("vsock.sock")
		idx := bytes.Index(data, marker)
		if idx >= 0 {
			searchStart := idx - 512
			if searchStart < 0 {
				searchStart = 0
			}
			searchEnd := idx + 512
			if searchEnd > len(data) {
				searchEnd = len(data)
			}
			region := data[searchStart:searchEnd]
			cidIdx := bytes.Index(region, oldCIDBytes)
			if cidIdx >= 0 {
				absIdx := searchStart + cidIdx
				copy(data[absIdx:absIdx+4], newCIDBytes)
				log.Printf("firecracker: patchVMState: patched CID %d→%d at offset %d", oldCID, newCID, absIdx)
			} else {
				log.Printf("firecracker: patchVMState: CID %d not found near vsock marker (best-effort, continuing)", oldCID)
			}
		} else {
			log.Printf("firecracker: patchVMState: vsock.sock marker not found (best-effort, continuing)")
		}
	}

	// Recompute CRC64 over patched data and append
	newCRC := firecrackerCRC64(data)
	crcBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(crcBytes, newCRC)
	patched := append(data, crcBytes...)

	if err := os.WriteFile(vmstatePath, patched, 0644); err != nil {
		return fmt.Errorf("write patched vmstate: %w", err)
	}
	log.Printf("firecracker: patchVMState: CRC64 updated (%d→%d)", storedCRC, newCRC)
	return nil
}

// reconfigureGuestNetwork updates the guest's network configuration via agent Exec.
// After warm fork/golden snapshot, the guest has the old network from the source snapshot.
// This brings eth0 up (golden snapshot takes it down to drain virtio queues),
// flushes the old config, and applies the new IPs and gateway.
func reconfigureGuestNetwork(ctx context.Context, agent *AgentClient, guestIP, hostIP string, cidr int) error {
	cmd := fmt.Sprintf("ip link set eth0 up && ip addr flush dev eth0 && ip addr add %s/%d dev eth0 && ip route add default via %s", guestIP, cidr, hostIP)
	resp, err := agent.Exec(ctx, &pb.ExecRequest{
		Command:        "/bin/sh",
		Args:           []string{"-c", cmd},
		TimeoutSeconds: 5,
	})
	if err != nil {
		return fmt.Errorf("exec network reconfig: %w", err)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("network reconfig failed (exit %d): %s", resp.ExitCode, resp.Stderr)
	}
	return nil
}

// ForkFromCheckpoint creates a new sandbox from an existing checkpoint.
// Tries warm fork first (LoadSnapshot with binary-patched vmstate, ~300ms),
// falls back to cold boot (~1.8s) if warm fork fails or snapshot files are missing.
// The sandbox ID can be pre-determined via cfg.SandboxID for async creation flows.
func (m *Manager) ForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig) (*types.Sandbox, error) {
	cacheDir := m.checkpointCacheDir(checkpointID)

	newID := cfg.SandboxID
	if newID == "" {
		newID = "sb-" + uuid.New().String()[:8]
	}

	// Try warm fork if snapshot files exist locally
	snapshotDir := filepath.Join(cacheDir, "snapshot")
	canWarm := fileExists(filepath.Join(snapshotDir, "mem")) &&
		fileExists(filepath.Join(snapshotDir, "vmstate")) &&
		fileExists(filepath.Join(snapshotDir, "snapshot-meta.json"))

	if canWarm {
		sb, err := m.warmForkFromCheckpoint(ctx, checkpointID, cfg, cacheDir)
		if err == nil {
			return sb, nil
		}
		log.Printf("firecracker: ForkFromCheckpoint %s: warm fork failed: %v, falling back to cold boot", checkpointID, err)
	}

	// Cold boot fallback: use checkpoint drives from local cache
	rootfsPath := filepath.Join(cacheDir, "rootfs.ext4")
	workspacePath := filepath.Join(cacheDir, "workspace.ext4")
	if fileExists(rootfsPath) && fileExists(workspacePath) {
		cfg.TemplateRootfsKey = "local://" + rootfsPath
		cfg.TemplateWorkspaceKey = "local://" + workspacePath
		log.Printf("firecracker: ForkFromCheckpoint %s: cold boot from local cached drives for %s", checkpointID, newID)
	} else {
		log.Printf("firecracker: ForkFromCheckpoint %s: no local drives, will download from S3 for %s", checkpointID, newID)
	}

	return m.createWithID(ctx, newID, cfg)
}

// warmForkFromCheckpoint creates a new sandbox by loading the checkpoint's memory snapshot
// into a fresh Firecracker process. The vmstate is binary-patched to update the sandbox ID,
// TAP name, and CID. Guest networking is reconfigured after resume via Exec RPC.
func (m *Manager) warmForkFromCheckpoint(ctx context.Context, checkpointID string, cfg types.SandboxConfig, cacheDir string) (*types.Sandbox, error) {
	t0 := time.Now()

	// Step 1: Generate new sandbox ID and create sandbox dir
	newID := cfg.SandboxID
	if newID == "" {
		newID = "sb-" + uuid.New().String()[:8]
	}
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", newID)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// Step 2: Read source snapshot metadata
	snapshotDir := filepath.Join(cacheDir, "snapshot")
	metaJSON, err := os.ReadFile(filepath.Join(snapshotDir, "snapshot-meta.json"))
	if err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("read snapshot meta: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("parse snapshot meta: %w", err)
	}

	oldSandboxID := meta.SandboxID
	log.Printf("firecracker: warmFork %s→%s from checkpoint %s", oldSandboxID, newID, checkpointID)

	// Step 3: Copy checkpoint drives to new sandbox dir (reflink)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
	if err := copyFileReflink(filepath.Join(cacheDir, "rootfs.ext4"), rootfsPath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy checkpoint rootfs: %w", err)
	}
	if err := copyFileReflink(filepath.Join(cacheDir, "workspace.ext4"), workspacePath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy checkpoint workspace: %w", err)
	}

	// Step 4: Copy snapshot files (mem + vmstate) to new sandbox dir
	newSnapshotDir := filepath.Join(sandboxDir, "snapshot")
	if err := os.MkdirAll(newSnapshotDir, 0755); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	memFile := filepath.Join(newSnapshotDir, "mem")
	vmstateFile := filepath.Join(newSnapshotDir, "vmstate")
	if err := copyFileReflink(filepath.Join(snapshotDir, "mem"), memFile); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy mem: %w", err)
	}
	if err := copyFileReflink(filepath.Join(snapshotDir, "vmstate"), vmstateFile); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy vmstate: %w", err)
	}

	log.Printf("firecracker: warmFork %s: files copied (%dms)", newID, time.Since(t0).Milliseconds())

	// Step 5: Allocate new network (TAP, subnet, DNAT)
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

	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = meta.GuestPort
	if netCfg.GuestPort == 0 {
		netCfg.GuestPort = m.cfg.DefaultPort
	}

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	// Step 6: Binary-patch vmstate (sandbox ID, TAP, CID)
	newCID := m.allocateCID()
	oldTAP := ""
	if meta.Network != nil {
		oldTAP = meta.Network.TAPName
	}
	if err := patchVMStateBinary(vmstateFile, oldSandboxID, newID, oldTAP, netCfg.TAPName, meta.GuestCID, newCID); err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("patch vmstate: %w", err)
	}
	log.Printf("firecracker: warmFork %s: vmstate patched (%dms)", newID, time.Since(t0).Milliseconds())

	// Step 7: Start fresh Firecracker process
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")
	apiSockPath := filepath.Join(sandboxDir, "firecracker.sock")
	_ = os.Remove(apiSockPath)
	_ = os.Remove(vsockPath)

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

	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("wait for API socket: %w", err)
	}

	// Step 8: Load snapshot — VM resumes from checkpoint state
	if err := fcClient.LoadSnapshot(vmstateFile, memFile, true); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	log.Printf("firecracker: warmFork %s: snapshot loaded (%dms)", newID, time.Since(t0).Milliseconds())

	// Step 9: Wait for agent (should be near-instant — agent was running when snapshot was taken)
	agentClient, err := m.waitForAgent(context.Background(), vsockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("agent not ready after warm fork: %w", err)
	}

	// Step 10: Reconfigure guest network (guest has old IPs from checkpoint)
	if err := reconfigureGuestNetwork(ctx, agentClient, netCfg.GuestIP, netCfg.HostIP, netCfg.CIDR); err != nil {
		log.Printf("firecracker: warmFork %s: network reconfig failed: %v (VM still running, network may not work)", newID, err)
		// Don't fail — the VM is running and accessible via vsock, just external network may be broken
	}

	// Step 11: Write sandbox-meta.json for crash recovery
	sbMeta := SandboxMeta{
		SandboxID: newID,
		Template:  meta.Template,
		CpuCount:  meta.CpuCount,
		MemoryMB:  meta.MemoryMB,
		GuestPort: netCfg.GuestPort,
	}
	sbMetaJSON, _ := json.Marshal(sbMeta)
	_ = os.WriteFile(filepath.Join(sandboxDir, "sandbox-meta.json"), sbMetaJSON, 0644)

	// Step 12: Register VM
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 300
	}

	now := time.Now()
	vm := &VMInstance{
		ID:          newID,
		Template:    meta.Template,
		Status:      types.SandboxStatusRunning,
		StartedAt:   now,
		EndAt:       now.Add(time.Duration(timeout) * time.Second),
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
		guestCID:    newCID,
		bootArgs:    meta.BootArgs,
		agent:       agentClient,
	}

	m.mu.Lock()
	m.vms[newID] = vm
	m.mu.Unlock()

	log.Printf("firecracker: warmFork %s from checkpoint %s: complete (%dms, port=%d, tap=%s)",
		newID, checkpointID, time.Since(t0).Milliseconds(), hostPort, netCfg.TAPName)

	return vmToSandbox(vm), nil
}

// --- Golden Snapshot ---

// errNoGoldenSnapshot is returned when no golden snapshot is available.
var errNoGoldenSnapshot = fmt.Errorf("no golden snapshot available")

// PrepareGoldenSnapshot boots a default VM, creates a memory snapshot, and stores it
// for fast VM creation. All subsequent Sandbox.create() calls for the default template
// will use LoadSnapshot (~500ms) instead of cold boot (~2s).
func (m *Manager) PrepareGoldenSnapshot() error {
	t0 := time.Now()
	goldenDir := filepath.Join(m.cfg.DataDir, "golden-snapshot", "default")

	// Clean up any existing golden snapshot
	os.RemoveAll(goldenDir)
	if err := os.MkdirAll(goldenDir, 0755); err != nil {
		return fmt.Errorf("mkdir golden dir: %w", err)
	}

	// Step 1: Boot a temporary VM with default config
	// Use same-length ID as real sandboxes (sb-XXXXXXXX = 11 chars) for vmstate binary patching
	tempID := "sb-" + uuid.New().String()[:8]
	tempDir := filepath.Join(m.cfg.DataDir, "sandboxes", tempID)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return fmt.Errorf("mkdir temp sandbox dir: %w", err)
	}

	// Prepare drives
	rootfsPath := filepath.Join(tempDir, "rootfs.ext4")
	workspacePath := filepath.Join(tempDir, "workspace.ext4")

	baseImage, err := ResolveBaseImage(m.cfg.ImagesDir, "default")
	if err != nil {
		os.RemoveAll(tempDir)
		return fmt.Errorf("resolve base image: %w", err)
	}
	if err := PrepareRootfs(baseImage, rootfsPath); err != nil {
		os.RemoveAll(tempDir)
		return fmt.Errorf("prepare rootfs: %w", err)
	}
	if err := CreateWorkspace(workspacePath, m.cfg.DefaultDiskMB); err != nil {
		os.RemoveAll(tempDir)
		return fmt.Errorf("create workspace: %w", err)
	}

	// Allocate network
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		os.RemoveAll(tempDir)
		return fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(tempDir)
		return fmt.Errorf("create TAP: %w", err)
	}

	cpus := m.cfg.DefaultCPUs
	memMB := m.cfg.DefaultMemoryMB
	guestCID := m.allocateCID()
	vsockPath := filepath.Join(tempDir, "vsock.sock")
	guestMAC := generateMAC(tempID)

	bootArgs := fmt.Sprintf(
		"keep_bootcon console=ttyS0 reboot=k panic=1 pci=off "+
			"ip=%s::%s:%s::eth0:off "+
			"init=/sbin/init "+
			"osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)

	// Start Firecracker
	apiSockPath := filepath.Join(tempDir, "firecracker.sock")
	logPath := filepath.Join(tempDir, "firecracker.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(m.cfg.FirecrackerBin, "--api-sock", apiSockPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("start firecracker: %w", err)
	}
	logFile.Close()

	// Configure VM
	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("wait for API socket: %w", err)
	}
	if err := fcClient.PutMachineConfig(cpus, memMB); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("put machine config: %w", err)
	}
	if err := fcClient.PutBootSource(m.cfg.KernelPath, bootArgs); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("put boot source: %w", err)
	}
	if err := fcClient.PutDrive("rootfs", rootfsPath, true, false); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("put rootfs drive: %w", err)
	}
	if err := fcClient.PutDrive("workspace", workspacePath, false, false); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("put workspace drive: %w", err)
	}
	if err := fcClient.PutNetworkInterface("eth0", guestMAC, netCfg.TAPName); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("put network interface: %w", err)
	}
	if err := fcClient.PutVsock(guestCID, vsockPath); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("put vsock: %w", err)
	}
	if err := fcClient.StartInstance(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("start instance: %w", err)
	}

	log.Printf("firecracker: golden snapshot: VM booted (%dms)", time.Since(t0).Milliseconds())

	// Step 2: Wait for agent
	agent, err := m.waitForAgent(context.Background(), vsockPath, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("agent not ready: %w", err)
	}

	log.Printf("firecracker: golden snapshot: agent ready (%dms)", time.Since(t0).Milliseconds())

	// Step 3: Flush filesystem, quiesce network, close agent, pause VM, snapshot
	_ = agent.SyncFS(context.Background())

	// Bring eth0 down to drain virtio-net queues cleanly. Without this,
	// snapshot restore with a different TAP backend causes virtqueue corruption
	// ("output.0:id 0 is not a head!") and a soft lockup in the guest kernel.
	_, _ = agent.Exec(context.Background(), &pb.ExecRequest{
		Command:        "/bin/sh",
		Args:           []string{"-c", "ip link set eth0 down"},
		TimeoutSeconds: 5,
	})

	agent.Close()

	if err := fcClient.PauseVM(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("pause VM: %w", err)
	}

	snapshotDir := filepath.Join(goldenDir, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("mkdir snapshot dir: %w", err)
	}

	memFile := filepath.Join(snapshotDir, "mem")
	vmstateFile := filepath.Join(snapshotDir, "vmstate")
	if err := fcClient.CreateSnapshot(vmstateFile, memFile); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		return fmt.Errorf("create snapshot: %w", err)
	}

	log.Printf("firecracker: golden snapshot: memory snapshot captured (%dms)", time.Since(t0).Milliseconds())

	// Step 4: Copy drives to golden dir (reflink for speed)
	if err := copyFileReflink(rootfsPath, filepath.Join(goldenDir, "rootfs.ext4")); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		os.RemoveAll(goldenDir)
		return fmt.Errorf("copy rootfs: %w", err)
	}
	if err := copyFileReflink(workspacePath, filepath.Join(goldenDir, "workspace.ext4")); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, tempDir)
		os.RemoveAll(goldenDir)
		return fmt.Errorf("copy workspace: %w", err)
	}

	// Step 5: Write metadata
	meta := SnapshotMeta{
		SandboxID: tempID,
		Network:   netCfg,
		GuestCID:  guestCID,
		GuestMAC:  guestMAC,
		BootArgs:  bootArgs,
		CpuCount:  cpus,
		MemoryMB:  memMB,
		Template:  "default",
		GuestPort: m.cfg.DefaultPort,
	}
	metaJSON, _ := json.Marshal(meta)
	_ = os.WriteFile(filepath.Join(snapshotDir, "snapshot-meta.json"), metaJSON, 0644)

	// Step 6: Kill the temporary VM + clean up
	cmd.Process.Kill()
	cmd.Wait()
	DeleteTAP(netCfg.TAPName)
	m.subnets.Release(netCfg.TAPName)
	RemoveDNAT(netCfg)
	os.RemoveAll(tempDir)

	// Step 7: Register golden snapshot
	gs := &GoldenSnapshot{
		Dir:   goldenDir,
		Meta:  meta,
		Ready: true,
	}
	m.goldenMu.Lock()
	m.golden = gs
	m.goldenMu.Unlock()

	log.Printf("firecracker: golden snapshot ready at %s (%dms)", goldenDir, time.Since(t0).Milliseconds())
	return nil
}

// createFromGoldenSnapshot creates a new sandbox using the pre-booted golden snapshot.
// Returns errNoGoldenSnapshot if no golden snapshot is available.
func (m *Manager) createFromGoldenSnapshot(ctx context.Context, id string, cfg types.SandboxConfig) (*types.Sandbox, error) {
	m.goldenMu.RLock()
	gs := m.golden
	m.goldenMu.RUnlock()

	if gs == nil || !gs.Ready {
		return nil, errNoGoldenSnapshot
	}

	t0 := time.Now()
	meta := gs.Meta
	goldenDir := gs.Dir

	// Verify golden snapshot files still exist
	if !fileExists(filepath.Join(goldenDir, "snapshot", "mem")) ||
		!fileExists(filepath.Join(goldenDir, "snapshot", "vmstate")) {
		return nil, errNoGoldenSnapshot
	}

	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", id)
	if err := os.MkdirAll(sandboxDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir sandbox dir: %w", err)
	}

	// Step 1: Copy drives from golden snapshot (reflink = instant CoW)
	rootfsPath := filepath.Join(sandboxDir, "rootfs.ext4")
	workspacePath := filepath.Join(sandboxDir, "workspace.ext4")
	if err := copyFileReflink(filepath.Join(goldenDir, "rootfs.ext4"), rootfsPath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy golden rootfs: %w", err)
	}
	if err := copyFileReflink(filepath.Join(goldenDir, "workspace.ext4"), workspacePath); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy golden workspace: %w", err)
	}

	// Step 2: Copy snapshot files (mem + vmstate)
	snapshotDir := filepath.Join(sandboxDir, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0755); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	memFile := filepath.Join(snapshotDir, "mem")
	vmstateFile := filepath.Join(snapshotDir, "vmstate")
	if err := copyFileReflink(filepath.Join(goldenDir, "snapshot", "mem"), memFile); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy golden mem: %w", err)
	}
	if err := copyFileReflink(filepath.Join(goldenDir, "snapshot", "vmstate"), vmstateFile); err != nil {
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("copy golden vmstate: %w", err)
	}

	log.Printf("firecracker: goldenCreate %s: files copied (%dms)", id, time.Since(t0).Milliseconds())

	// Step 3: Allocate new network
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

	hostPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("find free port: %w", err)
	}
	guestPort := cfg.Port
	if guestPort == 0 {
		guestPort = m.cfg.DefaultPort
	}
	netCfg.HostPort = hostPort
	netCfg.GuestPort = guestPort

	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		os.RemoveAll(sandboxDir)
		return nil, fmt.Errorf("add DNAT: %w", err)
	}

	// Step 4: Binary-patch vmstate
	newCID := m.allocateCID()
	oldTAP := ""
	if meta.Network != nil {
		oldTAP = meta.Network.TAPName
	}
	if err := patchVMStateBinary(vmstateFile, meta.SandboxID, id, oldTAP, netCfg.TAPName, meta.GuestCID, newCID); err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("patch vmstate: %w", err)
	}
	log.Printf("firecracker: goldenCreate %s: vmstate patched (%dms)", id, time.Since(t0).Milliseconds())

	// Step 5: Start fresh Firecracker process
	vsockPath := filepath.Join(sandboxDir, "vsock.sock")
	apiSockPath := filepath.Join(sandboxDir, "firecracker.sock")
	_ = os.Remove(apiSockPath)
	_ = os.Remove(vsockPath)

	logPath := filepath.Join(sandboxDir, "firecracker.log")
	logFileH, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("create log file: %w", err)
	}

	cmd := exec.Command(m.cfg.FirecrackerBin, "--api-sock", apiSockPath)
	cmd.Stdout = logFileH
	cmd.Stderr = logFileH
	if err := cmd.Start(); err != nil {
		logFileH.Close()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("start firecracker: %w", err)
	}
	logFileH.Close()

	fcClient := NewFirecrackerClient(apiSockPath)
	if err := fcClient.WaitForSocket(5 * time.Second); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("wait for API socket: %w", err)
	}

	// Step 6: Load snapshot
	if err := fcClient.LoadSnapshot(vmstateFile, memFile, true); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("load snapshot: %w", err)
	}
	log.Printf("firecracker: goldenCreate %s: snapshot loaded (%dms)", id, time.Since(t0).Milliseconds())

	// Step 7: Wait for agent
	agentClient, err := m.waitForAgent(context.Background(), vsockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, sandboxDir)
		return nil, fmt.Errorf("agent not ready after golden create: %w", err)
	}

	// Step 8: Reconfigure guest network
	if err := reconfigureGuestNetwork(ctx, agentClient, netCfg.GuestIP, netCfg.HostIP, netCfg.CIDR); err != nil {
		log.Printf("firecracker: goldenCreate %s: network reconfig failed: %v (VM still running)", id, err)
	}

	// Step 9: Write sandbox-meta.json
	cpus := cfg.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}

	sbMeta := SandboxMeta{
		SandboxID: id,
		Template:  "default",
		CpuCount:  cpus,
		MemoryMB:  memMB,
		GuestPort: guestPort,
	}
	sbMetaJSON, _ := json.Marshal(sbMeta)
	_ = os.WriteFile(filepath.Join(sandboxDir, "sandbox-meta.json"), sbMetaJSON, 0644)

	// Step 10: Register VM
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}

	now := time.Now()
	vm := &VMInstance{
		ID:          id,
		Template:    "default",
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
		guestMAC:    generateMAC(id),
		guestCID:    newCID,
		bootArgs:    meta.BootArgs,
		agent:       agentClient,
	}

	m.mu.Lock()
	m.vms[id] = vm
	m.mu.Unlock()

	log.Printf("firecracker: goldenCreate %s: complete (%dms, port=%d, tap=%s)",
		id, time.Since(t0).Milliseconds(), hostPort, netCfg.TAPName)

	return vmToSandbox(vm), nil
}
