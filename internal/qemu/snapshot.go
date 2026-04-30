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
	"syscall"
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
	BaseMemoryMB  int            `json:"baseMemoryMB,omitempty"`
	Template      string         `json:"template"`
	GuestPort        int                 `json:"guestPort"`
	GoldenVersion    string              `json:"goldenVersion,omitempty"`
	SnapshotedAt     time.Time           `json:"snapshotedAt,omitempty"`
	SealedTokens     map[string]string   `json:"sealedTokens,omitempty"`
	EgressAllowlist  []string            `json:"egressAllowlist,omitempty"`
	TokenHosts       map[string][]string `json:"tokenHosts,omitempty"`
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

	// Step 1: Sync filesystems and quiesce agent.
	// Don't unmount /workspace — open FDs prevent clean unmount and cause ext4 corruption.
	// PrepareHibernate syncs dirty pages and resets the virtio-serial listener
	// synchronously, so no post-close sleep is needed.
	if vm.agent != nil {
		prepareAgentForHibernate(ctx, vm.agent)
		vm.agent.Close()
		vm.agent = nil
	}
	log.Printf("qemu: hibernate %s: guest sync + unmount done (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 2: savevm — saves memory + device state INTO the qcow2 files.
	// Same mechanism as CreateCheckpoint. On wake, loadvm restores everything
	// including running processes, open files, and memory contents.
	if vm.qmp == nil {
		return nil, fmt.Errorf("no QMP client for VM %s", vm.ID)
	}
	snapshotName := "hibernate"
	if err := vm.qmp.SaveVM(snapshotName); err != nil {
		// Try to resume VM so it's not left in a broken paused state
		if contErr := vm.qmp.Cont(); contErr != nil {
			log.Printf("qemu: hibernate %s: failed to resume after savevm failure: %v", vm.ID, contErr)
		}
		// Try to reconnect agent
		if agent, reconnErr := m.waitForAgentSocket(context.Background(), vm.agentSockPath, 5*time.Second); reconnErr == nil {
			vm.agent = agent
		}
		return nil, fmt.Errorf("savevm failed: %w", err)
	}
	log.Printf("qemu: hibernate %s: savevm complete (%dms)", vm.ID, time.Since(t0).Milliseconds())

	// Step 3: Quit QEMU process (snapshot is inside the qcow2 files now)
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
	// Brief pause to ensure qcow2 file locks are released after QEMU exit
	time.Sleep(200 * time.Millisecond)

	// Step 4: Write snapshot metadata
	meta := &SnapshotMeta{
		SandboxID:     vm.ID,
		Network:       vm.network,
		GuestCID:      vm.guestCID,
		GuestMAC:      vm.guestMAC,
		BootArgs:      vm.bootArgs,
		RootfsPath:    detectDrivePath(vm.sandboxDir, "rootfs"),
		WorkspacePath: detectDrivePath(vm.sandboxDir, "workspace"),
		CpuCount:      vm.CpuCount,
		MemoryMB:      vm.MemoryMB,
		BaseMemoryMB:  vm.baseMemoryMB,
		Template:      vm.Template,
		GuestPort:     vm.GuestPort,
		GoldenVersion: vm.goldenVersion,
		SnapshotedAt:  time.Now(),
	}

	// Persist secrets proxy state so wake can re-register the session.
	if m.secretsProxy != nil && vm.network != nil {
		meta.SealedTokens = m.secretsProxy.GetSessionTokens(vm.network.GuestIP)
		meta.EgressAllowlist = m.secretsProxy.GetSessionAllowlist(vm.network.GuestIP)
		meta.TokenHosts = m.secretsProxy.GetSessionTokenHosts(vm.network.GuestIP)
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot meta: %w", err)
	}
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")
	// Atomic write: write to temp file then rename to avoid partial JSON on crash
	tmpMetaPath := metaPath + ".tmp"
	if err := os.WriteFile(tmpMetaPath, metaJSON, 0644); err != nil {
		return nil, fmt.Errorf("write snapshot meta: %w", err)
	}
	if err := os.Rename(tmpMetaPath, metaPath); err != nil {
		return nil, fmt.Errorf("rename snapshot meta: %w", err)
	}

	// Step 5: Clean up network
	if vm.network != nil {
		RemoveMetadataDNAT(vm.network.TAPName, vm.network.HostIP)
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
	}

	if vm.qmpSockPath != "" {
		os.Remove(vm.qmpSockPath)
	}

	checkpointKey := fmt.Sprintf("checkpoints/%s/%d.tar.zst", vm.ID, time.Now().Unix())
	localElapsed := time.Since(t0)
	log.Printf("qemu: hibernate %s: local snapshot complete (%dms), starting async S3 upload",
		vm.ID, localElapsed.Milliseconds())

	// Step 9: Archive + upload to S3 in the background.
	// Reflink-copy the qcow2 drives so the archive reads from stable copies
	// while wake can freely open the originals. Without this, wake starts QEMU
	// which modifies the qcow2 files while tar is still reading them →
	// "file changed as we read it" → corrupted archive → data loss on next wake.
	sandboxDir := vm.sandboxDir
	sandboxID := vm.ID
	workspaceFile := filepath.Base(detectDrivePath(sandboxDir, "workspace"))
	rootfsFile := filepath.Base(detectDrivePath(sandboxDir, "rootfs"))

	archiveDir := filepath.Join(sandboxDir, "archive-staging")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir archive-staging: %w", err)
	}
	// Copy metadata
	snapshotStaging := filepath.Join(archiveDir, "snapshot")
	os.MkdirAll(snapshotStaging, 0755)
	copyFileReflink(metaPath, filepath.Join(snapshotStaging, "snapshot-meta.json"))

	// Reflink-copy drives (COW — fast, no extra disk until divergence).
	// cp --reflink=auto falls back to regular copy if reflink not supported.
	for _, driveFile := range []string{rootfsFile, workspaceFile} {
		src := filepath.Join(sandboxDir, driveFile)
		dst := filepath.Join(archiveDir, driveFile)
		if err := copyFileReflink(src, dst); err != nil {
			os.RemoveAll(archiveDir)
			return nil, fmt.Errorf("copy %s for archive staging: %w", driveFile, err)
		}
	}
	// Flatten staged rootfs for S3 portability — the qcow2 overlay references a
	// local backing file (base ext4 image) that won't exist on other workers.
	// `qemu-img rebase -b ""` merges backing file data into the overlay, making it
	// self-contained while preserving internal savevm snapshots (unlike qemu-img convert).
	stagedRootfs := filepath.Join(archiveDir, rootfsFile)
	rebaseCmd := exec.Command("qemu-img", "rebase", "-b", "", stagedRootfs)
	if out, err := rebaseCmd.CombinedOutput(); err != nil {
		log.Printf("qemu: hibernate %s: rootfs rebase failed (archive may not be portable): %v (%s)",
			sandboxID, err, strings.TrimSpace(string(out)))
	}
	log.Printf("qemu: hibernate %s: archive staging ready", sandboxID)

	// Signal channel so destroyVM can wait for archive completion before deleting files.
	archiveDone := make(chan struct{})
	vm.archiveDone = archiveDone

	m.uploadWg.Add(1)
	go func() {
		defer m.uploadWg.Done()
		defer close(archiveDone)
		defer os.RemoveAll(archiveDir) // clean up staging copies when done
		t1 := time.Now()
		archivePath := filepath.Join(sandboxDir, "checkpoint.tar.zst")

		// Archive from the staging copies — originals are free for wake/QEMU.
		if err := createArchive(archivePath, archiveDir, []string{
			"snapshot/snapshot-meta.json",
			rootfsFile,
			workspaceFile,
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
			os.Remove(archivePath) // clean up — local qcow2 files are the source of truth for same-worker wake
			return
		}
		log.Printf("qemu: hibernate %s: S3 upload complete (%dms, key=%s)",
			sandboxID, time.Since(t2).Milliseconds(), checkpointKey)

		os.Remove(archivePath) // only delete after successful upload
	}()

	return &sandbox.HibernateResult{
		SandboxID:      sandboxID,
		HibernationKey: checkpointKey,
		SizeBytes:      0,
	}, nil
}

// doWake restores a VM from a savevm snapshot. Uses the same mechanism as
// ForkFromCheckpoint: start QEMU paused, loadvm from the qcow2 files, cont.
// All processes, memory, open files, and PIDs are restored exactly.
//
// Flow:
//  1. Ensure qcow2 files are local (download from S3 if needed)
//  2. Read snapshot-meta.json
//  3. Set up network (TAP, DNAT)
//  4. Start QEMU paused (-S) with the user's qcow2 drives
//  5. loadvm "hibernate" → restores full VM state from qcow2
//  6. cont → VM resumes with all processes alive
//  7. Agent reconnects via virtio-serial
//  8. Mount /workspace, patch network, sync clock
func (m *Manager) doWake(ctx context.Context, sandboxID, checkpointKey string, checkpointStore *storage.CheckpointStore, timeout int) (*types.Sandbox, error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	snapshotDir := filepath.Join(sandboxDir, "snapshot")
	metaPath := filepath.Join(snapshotDir, "snapshot-meta.json")

	// Wait for any in-flight hibernate archive to finish before proceeding.
	// Same-worker wake: archive reads from staging copies (safe), but we need
	// archive-staging/ cleaned up before starting QEMU to avoid disk waste.
	// Cross-worker wake: S3 upload must complete before download can succeed.
	archiveStagingDir := filepath.Join(sandboxDir, "archive-staging")
	if fileExists(archiveStagingDir) {
		log.Printf("qemu: wake %s: waiting for in-flight archive to complete", sandboxID)
		for i := 0; i < 60; i++ { // up to 30 seconds
			if !fileExists(archiveStagingDir) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if fileExists(archiveStagingDir) {
			// Don't force-remove — the archive goroutine's defer will clean up.
			// Removing while tar is mid-read corrupts the S3 archive.
			log.Printf("qemu: wake %s: archive staging still present after 30s, proceeding (goroutine will clean up)", sandboxID)
		}
	}

	// Step 1: Ensure qcow2 files are local
	t0 := time.Now()
	rootfsPath := detectDrivePath(sandboxDir, "rootfs")
	rootfsExists := fileExists(rootfsPath)

	isLocalWorkspace := strings.HasPrefix(checkpointKey, "local://")

	if !rootfsExists {
		if isLocalWorkspace {
			log.Printf("qemu: wake %s: local workspace recovery (no snapshot)", sandboxID)
			return m.coldBootLocal(ctx, sandboxID, timeout)
		}
		log.Printf("qemu: wake %s: local files missing, downloading from S3 (key=%s)", sandboxID, checkpointKey)
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

		log.Printf("qemu: wake %s: downloaded archive (%dms)", sandboxID, time.Since(t0).Milliseconds())
		if err := extractArchive(archivePath, sandboxDir); err != nil {
			return nil, fmt.Errorf("extract archive: %w", err)
		}
		os.Remove(archivePath)
		log.Printf("qemu: wake %s: extracted (%dms total)", sandboxID, time.Since(t0).Milliseconds())
		rootfsPath = detectDrivePath(sandboxDir, "rootfs")
	} else {
		log.Printf("qemu: wake %s: local files found", sandboxID)
	}

	// Step 2: Read snapshot metadata
	metaJSON, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot meta: %w", err)
	}
	var meta SnapshotMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return nil, fmt.Errorf("parse snapshot meta: %w", err)
	}

	workspacePath := detectDrivePath(sandboxDir, "workspace")
	if !fileExists(workspacePath) {
		return nil, fmt.Errorf("workspace not found at %s", workspacePath)
	}

	// Step 3: Set up network
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
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: warning: metadata DNAT failed: %v", err)
	}

	// Step 4: Start QEMU paused with the user's qcow2 drives
	guestCID := m.allocateCID()
	guestMAC := generateMAC(sandboxID)
	baseMem := meta.BaseMemoryMB
	if baseMem <= 0 {
		baseMem = m.cfg.DefaultMemoryMB
	}
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
	os.Remove(qmpSockPath)
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("create log: %w", err)
	}

	args := m.buildQEMUArgs(meta.CpuCount, baseMem, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)
	args = append(args, "-S") // start paused for loadvm

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	logFile.Close()

	// Step 5: Connect QMP, loadvm, cont — same as RestoreFromCheckpoint
	qmpClient, err := waitForQMP(qmpSockPath, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("QMP connect: %w", err)
	}

	if err := qmpClient.LoadVM("hibernate"); err != nil {
		// loadvm failed — fall back to cold boot
		log.Printf("qemu: wake %s: loadvm failed (%v), falling back to cold boot", sandboxID, err)
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return m.coldBootLocal(ctx, sandboxID, timeout)
	}

	// Re-plug virtio-mem to match the pre-hibernate total BEFORE Cont. The VM
	// is paused, so the kernel sees the full memory map immediately on resume
	// — without this, restored processes that were using more than baseMem
	// OOM before any post-resume scale could land. Mirrors the
	// RestoreFromCheckpoint path (manager.go:2536). Also keeps host-side
	// accounting honest: vm.MemoryMB stays equal to what's actually plugged,
	// not the ceiling, so TotalCommittedMemoryMB reflects reality.
	pluggedMB := 0
	if meta.MemoryMB > baseMem {
		additionalMB := alignVirtioMemBlock(meta.MemoryMB - baseMem)
		if err := qmpClient.SetVirtioMemSize(additionalMB); err != nil {
			log.Printf("qemu: wake %s: pre-resume virtio-mem plug to %dMB failed: %v (continuing with base %dMB)",
				sandboxID, additionalMB, err, baseMem)
		} else {
			pluggedMB = additionalMB
			log.Printf("qemu: wake %s: pre-resume virtio-mem plug %dMB (base=%d, total=%d)",
				sandboxID, additionalMB, baseMem, baseMem+additionalMB)
		}
	}

	if err := qmpClient.Cont(); err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("QMP cont: %w", err)
	}
	log.Printf("qemu: wake %s: loadvm + cont done (%dms)", sandboxID, time.Since(t0).Milliseconds())

	// Step 6: Reconnect agent + mount workspace + patch network
	agentClient, err := m.waitForAgentSocket(context.Background(), agentSockPath, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("agent not ready: %w", err)
	}

	// Hibernate captured the guest with its mount state intact (we never
	// unmount before savevm), so loadvm restores the correct mount layout
	// automatically. Do NOT blindly mount /dev/vdb on /home/sandbox here:
	// cold-booted VMs have their workspace disk mounted at /workspace per the
	// guest's fstab, with /home/sandbox being a regular directory on the
	// rootfs. If we force-mount /dev/vdb over /home/sandbox post-wake, we
	// shadow any files the user wrote to /home/sandbox (which live on the
	// rootfs qcow2) with the empty workspace qcow2 view, silently losing
	// their data.

	if err := patchGuestNetwork(context.Background(), agentClient, netCfg); err != nil {
		log.Printf("qemu: wake %s: network patch failed: %v", sandboxID, err)
	}

	if err := syncGuestClock(context.Background(), agentClient); err != nil {
		log.Printf("qemu: wake %s: clock sync failed: %v", sandboxID, err)
	}

	// Re-register secrets proxy session from persisted tokens.
	if m.secretsProxy != nil && len(meta.SealedTokens) > 0 {
		m.secretsProxy.ReregisterSession(sandboxID, netCfg.GuestIP, meta.SealedTokens, meta.EgressAllowlist, meta.TokenHosts)
		log.Printf("qemu: wake %s: re-registered secrets proxy session (%d tokens)", sandboxID, len(meta.SealedTokens))
	}

	log.Printf("qemu: wake %s: golden restore complete (port=%d, tap=%s)",
		sandboxID, hostPort, netCfg.TAPName)

	now := time.Now()
	ttl := time.Duration(timeout) * time.Second
	if ttl <= 0 {
		ttl = 300 * time.Second
	}

	vm := &VMInstance{
		ID:                   sandboxID,
		Template:             meta.Template,
		Status:               types.SandboxStatusRunning,
		StartedAt:            now,
		EndAt:                now.Add(ttl),
		CpuCount:             meta.CpuCount,
		MemoryMB:             baseMem + pluggedMB, // actually-plugged total, not the ceiling — keeps committed accounting honest
		baseMemoryMB:         baseMem,
		virtioMemRequestedMB: pluggedMB,
		HostPort:             hostPort,
		GuestPort:             netCfg.GuestPort,
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
		goldenVersion: m.goldenVersion, // set on wake — VM runs on the current worker's base
	}
	vm.agent = agentClient

	// Agent binary updates happen via qemu-img rebase of the rootfs, not via
	// runtime re-exec. See the "Runtime agent upgrade" comment in manager.go
	// for the rationale.

	m.mu.Lock()
	m.vms[sandboxID] = vm
	m.mu.Unlock()

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(sandboxID, netCfg.GuestIP, vm.Template, vm.StartedAt)
	}

	log.Printf("qemu: woke VM %s (port=%d, tap=%s)",
		sandboxID, hostPort, netCfg.TAPName)
	return vmToSandbox(vm), nil
}

// coldBootLocal boots a fresh VM using an existing workspace.ext4 on disk.
func (m *Manager) coldBootLocal(ctx context.Context, sandboxID string, timeout int) (*types.Sandbox, error) {
	sandboxDir := filepath.Join(m.cfg.DataDir, "sandboxes", sandboxID)
	workspacePath := detectDrivePath(sandboxDir, "workspace")
	rootfsPath := detectDrivePath(sandboxDir, "rootfs")

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

	// Add metadata service DNAT (169.254.169.254:80 → host:8888)
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: warning: metadata DNAT failed for %s: %v", netCfg.TAPName, err)
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
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(agentSockPath)

	logPath := filepath.Join(sandboxDir, "qemu.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		m.cleanupVM(netCfg, "")
		return nil, fmt.Errorf("create log file: %w", err)
	}

	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)

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
		ID:            sandboxID,
		Template:      meta.Template,
		Status:        types.SandboxStatusRunning,
		StartedAt:     now,
		EndAt:         now.Add(ttl),
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
		goldenVersion: m.goldenVersion, // cold boot: VM runs on the current worker's base
	}

	agentClient, err := m.waitForAgentSocket(context.Background(), agentSockPath, 30*time.Second)
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

	// Notify metadata server
	if m.onSandboxReady != nil {
		m.onSandboxReady(sandboxID, netCfg.GuestIP, meta.Template, vm.StartedAt)
	}

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

// checkReflinkSupport verifies the filesystem supports reflink by creating a test file
// and reflink-copying it. Returns nil if reflink works, error otherwise.
func checkReflinkSupport(dir string) error {
	testFile := filepath.Join(dir, ".reflink-test")
	testCopy := filepath.Join(dir, ".reflink-test-copy")
	defer os.Remove(testFile)
	defer os.Remove(testCopy)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(testFile, []byte("reflink-test"), 0644); err != nil {
		return err
	}
	cmd := exec.Command("cp", "--reflink=always", testFile, testCopy)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reflink test failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// diskUsagePercent returns the disk usage percentage for the given path.
func diskUsagePercent(path string) (int, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return 0, nil
	}
	used := total - free
	return int(used * 100 / total), nil
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
		RunAsRoot:      true,
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
