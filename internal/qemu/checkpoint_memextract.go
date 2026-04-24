package qemu

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// extractCheckpointMemory converts a savevm-based checkpoint (memory embedded
// in qcow2 as an internal snapshot) into an external mem.zst + plain qcow2
// drives without internal snapshots. Fork restore paths already prefer the
// external-memory format (see ForkFromCheckpoint restore-mode logic) and
// qemu-img rebase composes cleanly with plain overlays — so cross-golden
// forks work correctly after this runs.
//
// The function is destructive on stagingDir: it may delete the internal
// snapshot from the qcow2 files and create mem.zst. Callers should invoke it
// on a staging directory that has been reflinked from the live VM, not on
// live drives.
//
// Runs out-of-band in the async upload goroutine so user-visible save
// latency is unchanged. On failure, the staging directory is left in its
// savevm-only state — fork restore falls back to loadvm, which still works
// for same-golden forks.
func (m *Manager) extractCheckpointMemory(ctx context.Context, stagingDir, snapshotName, guestMAC string, memMB, cpuCount int) error {
	rootfs := filepath.Join(stagingDir, "rootfs.qcow2")
	workspace := filepath.Join(stagingDir, "workspace.qcow2")
	if !fileExists(rootfs) || !fileExists(workspace) {
		return fmt.Errorf("qcow2 drives missing in %s", stagingDir)
	}

	memFile := filepath.Join(stagingDir, "mem")
	// Linux UNIX socket paths max 108 bytes. Staging dir
	// (/data/sandboxes/checkpoint-snapshots/<uuid>.staging/...) is already
	// over the limit with a reasonable filename, so put the throwaway
	// sockets in /tmp.
	shortID := fmt.Sprintf("osb-ex-%d", time.Now().UnixNano())
	qmpSock := filepath.Join("/tmp", shortID+".qmp")
	agentSock := filepath.Join("/tmp", shortID+".ag")
	logFile := filepath.Join(stagingDir, "extract-qemu.log")
	os.Remove(memFile)
	os.Remove(qmpSock)
	os.Remove(agentSock)

	// loadvm requires the runtime device layout to match what savevm recorded
	// byte-for-byte (device IDs, order, -serial stdio, even virtio-mem pool
	// size). Reuse buildQEMUArgs — same function the live VM was started with
	// — plus -S and -loadvm to hold the restored state paused so we can
	// migrate the memory out of it.
	//
	// A temporary TAP is allocated because the virtio-net device in the
	// savevm stream expects a netdev backend, and "-netdev user" is a
	// different backend type that QEMU rejects during loadvm.
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		return fmt.Errorf("allocate tap: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return fmt.Errorf("create tap: %w", err)
	}
	defer func() {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
	}()

	// bootArgs don't matter for loadvm — the guest doesn't actually boot,
	// we only load the snapshot's memory+device state and migrate out.
	args := m.buildQEMUArgs(cpuCount, memMB, rootfs, workspace, netCfg.TAPName, guestMAC, agentSock, qmpSock, "")
	args = append(args, "-S", "-loadvm", snapshotName)

	logf, err := os.Create(logFile)
	if err != nil {
		return fmt.Errorf("create extract log: %w", err)
	}
	defer logf.Close()

	cmd := exec.CommandContext(ctx, m.cfg.QEMUBin, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start extract qemu: %w", err)
	}

	killQEMU := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			cmd.Wait()
		}
	}

	qmp, err := waitForQMP(qmpSock, 30*time.Second)
	if err != nil {
		killQEMU()
		return fmt.Errorf("extract qmp connect: %w", err)
	}

	// Migrate memory+device state to an external file. VM stays paused (-S)
	// the whole time; migrate tolerates that.
	if err := qmp.Migrate(fmt.Sprintf("exec:cat > %s", memFile)); err != nil {
		qmp.Close()
		killQEMU()
		return fmt.Errorf("extract migrate: %w", err)
	}
	if err := qmp.WaitMigration(5 * time.Minute); err != nil {
		qmp.Close()
		killQEMU()
		return fmt.Errorf("extract migrate wait: %w", err)
	}

	_ = qmp.Quit()
	qmp.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		killQEMU()
	}
	os.Remove(qmpSock)
	os.Remove(agentSock)

	// Compress memory file with zstd -3 (same level golden uses). -3 on an
	// uncompressed VM memory dump typically achieves 3-5x reduction at
	// ~500MB/s on a single core.
	memZst := memFile + ".zst"
	zstdOut, zstdErr := exec.Command("zstd", "-3", "--rm", memFile, "-o", memZst).CombinedOutput()
	if zstdErr != nil {
		os.Remove(memFile)
		os.Remove(memZst)
		return fmt.Errorf("zstd compress mem: %w (%s)", zstdErr, string(zstdOut))
	}

	// Strip the internal savevm snapshot from the qcow2 drives. After this,
	// the overlays are plain qcow2 files with only their backing-file
	// reference — qemu-img rebase operates on them correctly, and fork
	// restore uses the external mem.zst instead of loadvm.
	for _, f := range []string{rootfs, workspace} {
		if out, derr := exec.Command("qemu-img", "snapshot", "-d", snapshotName, f).CombinedOutput(); derr != nil {
			// Workspace may not actually carry the snapshot entry (savevm stores
			// VMSTATE in only one drive); a "snapshot not found" error here is
			// benign. Log and continue.
			log.Printf("qemu: extractCheckpointMemory: snapshot -d %s on %s: %v (%s)", snapshotName, f, derr, string(out))
		}
	}

	// Remove the snapshot-name file — restore logic only uses it when an
	// external mem file is absent, and we've now guaranteed one is present.
	// Leave it in place for backward compat: old worker binaries that predate
	// this code only understand the loadvm path and would break without it.
	return nil
}
