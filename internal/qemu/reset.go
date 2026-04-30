package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// RebootSandbox performs a soft, in-place guest reset on a running sandbox.
// The QEMU process, network (TAP/DNAT/host port), and disks all stay; only
// the guest CPU is reset and re-runs its boot path. This recovers the
// sandbox from in-guest wedges (zombie pile, OOM-killed agent, runaway
// processes) without touching anything externally observable.
//
// Implementation:
//  1. Best-effort `sync` via the agent so dirty pages reach the workspace
//     disk before we pull the rug. Best-effort because the agent may already
//     be wedged — that's the case we're trying to recover from.
//  2. QMP `system_reset`. Guest sees a hardware reset signal, kernel
//     re-boots from scratch.
//  3. The host-side gRPC connection to the agent dies as virtio-serial
//     resets. Close the old client, dial fresh after the new agent comes
//     up.
//  4. Re-sync clock (the guest reboot doesn't preserve wall time).
func (m *Manager) RebootSandbox(ctx context.Context, sandboxID string) error {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return err
	}

	if !vm.opMu.TryLock() {
		return fmt.Errorf("another operation is in progress on sandbox %s — try again shortly", sandboxID)
	}
	defer vm.opMu.Unlock()

	if vm.qmp == nil {
		return fmt.Errorf("sandbox %s: QMP not connected (try power-cycle instead)", sandboxID)
	}

	t0 := time.Now()

	// Best-effort sync. If the agent is wedged this fails fast and we
	// continue — the user is reaching for reboot precisely because state
	// is broken, so we don't gate the recovery on a graceful sync.
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = vm.agent.Exec(syncCtx, &pb.ExecRequest{Command: "sync", RunAsRoot: true})
		cancel()
	}

	// Close the host-side agent gRPC client. The underlying virtio-serial
	// socket will be torn down when the guest resets; we want a clean
	// shutdown of our end so it can be re-dialed.
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}

	if err := vm.qmp.SystemReset(); err != nil {
		return fmt.Errorf("system_reset: %w", err)
	}

	// Wait for the new agent to boot inside the guest and reconnect via
	// virtio-serial. waitForAgentSocket dials with backoff and verifies a
	// gRPC handshake before returning.
	agentClient, err := m.waitForAgentSocket(ctx, vm.agentSockPath, 60*time.Second)
	if err != nil {
		return fmt.Errorf("agent did not reconnect after reboot: %w", err)
	}
	vm.agent = agentClient

	if err := syncGuestClock(ctx, agentClient); err != nil {
		log.Printf("qemu: RebootSandbox %s: clock sync failed: %v", sandboxID, err)
	}

	log.Printf("qemu: RebootSandbox %s: complete (%dms)", sandboxID, time.Since(t0).Milliseconds())
	return nil
}

// PowerCycleSandbox performs a hard reset: the QEMU VMM is killed and a
// fresh QEMU process is started with the same on-disk drives. The sandbox
// keeps its identity (ID, project, secrets, env, persistent workspace
// data) but gets a new TAP, host port, and PID. Use this when the QEMU
// process itself is wedged (QMP unresponsive) or a soft reboot didn't
// recover.
//
// We deliberately keep the existing rootfs.qcow2 — it carries any user
// system-package installs and /etc edits the customer made. Resetting all
// the way back to the template is a separate, more drastic operation.
//
// Returns the sandbox's new external host port (TAP/DNAT changed). Caller
// is expected to update any stored routing record.
func (m *Manager) PowerCycleSandbox(ctx context.Context, sandboxID string) (hostPort int, err error) {
	vm, err := m.getVM(sandboxID)
	if err != nil {
		return 0, err
	}

	if !vm.opMu.TryLock() {
		return 0, fmt.Errorf("another operation is in progress on sandbox %s — try again shortly", sandboxID)
	}
	defer vm.opMu.Unlock()

	t0 := time.Now()

	// Best-effort sync before we yank the rug. The QEMU drive is opened
	// with cache=writethrough so the host always has a consistent view
	// once the guest issues a write — but the guest kernel's own page
	// cache may hold dirty data that hasn't been flushed yet. Skipping
	// this lost a workspace file in dev testing.
	if vm.agent != nil {
		syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, _ = vm.agent.Exec(syncCtx, &pb.ExecRequest{Command: "sync", RunAsRoot: true})
		cancel()
	}

	// Step 1: Kill the current VM. Mirrors RestoreFromCheckpoint's teardown.
	if vm.agent != nil {
		vm.agent.Close()
		vm.agent = nil
	}
	if vm.qmp != nil {
		_ = vm.qmp.Quit()
		vm.qmp.Close()
		vm.qmp = nil
	}
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

	// Step 2: Tear down the old network. New TAP/host port will be
	// allocated below. We can't reuse the old subnet entry because we've
	// already started releasing it.
	if vm.network != nil {
		RemoveMetadataDNAT(vm.network.TAPName, vm.network.HostIP)
		RemoveDNAT(vm.network)
		DeleteTAP(vm.network.TAPName)
		m.subnets.Release(vm.network.TAPName)
		vm.network = nil
	}

	// Step 3: Re-read sandbox metadata so we boot with the same template
	// and resource shape. The on-disk rootfs.qcow2 / workspace.qcow2 are
	// unchanged — this is "same box, freshly powered."
	sandboxDir := vm.sandboxDir
	rootfsPath := filepath.Join(sandboxDir, "rootfs.qcow2")
	workspacePath := filepath.Join(sandboxDir, "workspace.qcow2")
	if !fileExists(rootfsPath) || !fileExists(workspacePath) {
		return 0, fmt.Errorf("sandbox %s: drives missing (rootfs=%v, workspace=%v)",
			sandboxID, fileExists(rootfsPath), fileExists(workspacePath))
	}

	var meta SandboxMeta
	if data, err := os.ReadFile(filepath.Join(sandboxDir, "sandbox-meta.json")); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	cpus := vm.CpuCount
	if cpus <= 0 {
		cpus = m.cfg.DefaultCPUs
	}
	memMB := vm.MemoryMB
	if memMB <= 0 {
		memMB = m.cfg.DefaultMemoryMB
	}
	guestPort := vm.GuestPort
	if guestPort == 0 {
		guestPort = meta.GuestPort
	}
	if guestPort == 0 {
		guestPort = 80
	}

	// Step 4: Allocate fresh network plumbing.
	netCfg, err := m.subnets.Allocate()
	if err != nil {
		return 0, fmt.Errorf("allocate subnet: %w", err)
	}
	if err := CreateTAP(netCfg); err != nil {
		m.subnets.Release(netCfg.TAPName)
		return 0, fmt.Errorf("create TAP: %w", err)
	}
	freshPort, err := FindFreePort()
	if err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return 0, fmt.Errorf("find free port: %w", err)
	}
	netCfg.HostPort = freshPort
	netCfg.GuestPort = guestPort
	if err := AddDNAT(netCfg); err != nil {
		DeleteTAP(netCfg.TAPName)
		m.subnets.Release(netCfg.TAPName)
		return 0, fmt.Errorf("add DNAT: %w", err)
	}
	if err := AddMetadataDNAT(netCfg.TAPName, netCfg.HostIP); err != nil {
		log.Printf("qemu: PowerCycleSandbox %s: metadata DNAT failed: %v", sandboxID, err)
	}

	// Step 5: Start a fresh QEMU.
	guestMAC := generateMAC(sandboxID)
	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 root=/dev/vda rw ip=%s::%s:%s::eth0:off init=/sbin/init osb.gateway=%s",
		netCfg.GuestIP, netCfg.HostIP, netCfg.Mask, netCfg.HostIP,
	)
	qmpSockPath := filepath.Join(sandboxDir, "qmp.sock")
	agentSockPath := filepath.Join(sandboxDir, "agent.sock")
	os.Remove(qmpSockPath)
	os.Remove(agentSockPath)

	logFile, _ := os.Create(filepath.Join(sandboxDir, "qemu.log"))
	args := m.buildQEMUArgs(cpus, memMB, rootfsPath, workspacePath,
		netCfg.TAPName, guestMAC, agentSockPath, qmpSockPath, bootArgs)

	cmd := exec.Command(m.cfg.QEMUBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		m.cleanupVM(netCfg, "")
		return 0, fmt.Errorf("start QEMU: %w", err)
	}
	if logFile != nil {
		logFile.Close()
	}

	qmpClient, err := waitForQMP(qmpSockPath, 30*time.Second)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return 0, fmt.Errorf("QMP connect: %w", err)
	}

	agentClient, err := m.waitForAgentSocket(ctx, agentSockPath, 60*time.Second)
	if err != nil {
		qmpClient.Close()
		cmd.Process.Kill()
		cmd.Wait()
		m.cleanupVM(netCfg, "")
		return 0, fmt.Errorf("agent connect: %w", err)
	}

	if err := syncGuestClock(ctx, agentClient); err != nil {
		log.Printf("qemu: PowerCycleSandbox %s: clock sync failed: %v", sandboxID, err)
	}

	// Step 6: Swap fresh state into the existing VMInstance so callers
	// holding pointers to it continue to see a live sandbox.
	vm.cmd = cmd
	vm.qmp = qmpClient
	vm.agent = agentClient
	vm.network = netCfg
	vm.HostPort = freshPort
	vm.qmpSockPath = qmpSockPath
	vm.agentSockPath = agentSockPath
	vm.guestMAC = guestMAC
	vm.bootArgs = bootArgs
	vm.pid = cmd.Process.Pid

	log.Printf("qemu: PowerCycleSandbox %s: complete (%dms, port=%d, tap=%s)",
		sandboxID, time.Since(t0).Milliseconds(), freshPort, netCfg.TAPName)
	return freshPort, nil
}
