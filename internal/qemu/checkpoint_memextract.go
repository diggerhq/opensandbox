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
func (m *Manager) extractCheckpointMemory(ctx context.Context, stagingDir, snapshotName string, memMB, cpuCount int) error {
	rootfs := filepath.Join(stagingDir, "rootfs.qcow2")
	workspace := filepath.Join(stagingDir, "workspace.qcow2")
	if !fileExists(rootfs) || !fileExists(workspace) {
		return fmt.Errorf("qcow2 drives missing in %s", stagingDir)
	}

	memFile := filepath.Join(stagingDir, "mem")
	qmpSock := filepath.Join(stagingDir, "extract-qmp.sock")
	logFile := filepath.Join(stagingDir, "extract-qemu.log")
	os.Remove(memFile)
	os.Remove(qmpSock)

	// Minimal QEMU args matching the original sandbox topology so loadvm's
	// device-state restoration finds the exact same device layout. A no-op
	// tap via "script=no,downscript=no,ifname=dummy" would be rejected by
	// the kernel, so we leave out -netdev/-device net entirely and rely on
	// the fact that loadvm's virtio-net state can be discarded (the extract
	// run never resumes the VM, only reads its memory).
	virtioMemPoolMB := ((16384 - memMB + 127) / 128) * 128
	if virtioMemPoolMB < 1024 {
		virtioMemPoolMB = 1024
	}
	maxMemMB := memMB + virtioMemPoolMB
	args := []string{
		"-machine", "q35,accel=kvm",
		"-cpu", "host",
		"-m", fmt.Sprintf("%dM,slots=1,maxmem=%dM", memMB, maxMemMB),
		"-object", fmt.Sprintf("memory-backend-ram,id=vmem0,size=%dM", virtioMemPoolMB),
		"-device", "virtio-mem-pci,memdev=vmem0,id=vm0,block-size=128M,requested-size=0",
		"-smp", fmt.Sprintf("%d", cpuCount),
		"-kernel", m.cfg.KernelPath,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio,cache=writethrough", rootfs),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio,cache=writethrough", workspace),
		// Match the live VM's network + virtio-serial devices so device state
		// IDs line up for loadvm. Netdev is "user" (no external reachability)
		// since the extract VM never sees packets — it only reads memory.
		"-netdev", "user,id=net0",
		"-device", "virtio-net-pci,netdev=net0",
		"-device", "virtio-serial-pci-non-transitional",
		"-chardev", fmt.Sprintf("socket,id=agent,path=%s-agent.sock,server=on,wait=off", qmpSock),
		"-device", "virtserialport,chardev=agent,name=agent",
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", qmpSock),
		"-nographic", "-nodefaults",
		"-S",
		"-loadvm", snapshotName,
	}

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
	os.Remove(qmpSock + "-agent.sock")

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
