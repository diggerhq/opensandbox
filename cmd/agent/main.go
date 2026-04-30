// osb-agent is the sandbox agent that runs inside each VM.
// It listens for gRPC connections over virtio-serial (QEMU) or vsock (Firecracker)
// and handles exec, file ops, PTY sessions, and stats collection.
//
// Build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X main.Version=$(git rev-parse --short HEAD)" -o osb-agent ./cmd/agent
package main

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/opensandbox/opensandbox/internal/agent"
)

// Version is set at build time via -ldflags "-X main.Version=xxx".
var Version = "dev"

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Set a complete PATH — when running as PID 1 (init), there's no shell
	// profile to source, so PATH may be empty or minimal.
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

	log.Printf("osb-agent %s starting (build=pin-to-base/r3)", Version)

	// Listen on vsock port 1024 (inside Firecracker) or Unix socket (testing).
	lis, err := listenVsock()
	if err != nil {
		log.Fatalf("agent: failed to listen: %v", err)
	}

	srv := agent.NewServer(Version)
	// Only set ListenPort for vsock-based backends (Firecracker).
	// For virtio-serial (QEMU), PTY I/O flows over gRPC PTYAttach instead.
	if vsl, isVirtioSerial := lis.(*virtioSerialListener); isVirtioSerial {
		// Wire the PrepareHibernate RPC to reset the virtio-serial listener
		// synchronously — replaces the old SIGUSR1 + sleep dance.
		srv.OnPrepareHibernate = vsl.PrepareHibernate
	} else {
		srv.ListenPort = listenPortForPTY
	}

	// SIGCHLD reaper: when osb-agent runs as PID 1, every orphaned process
	// in the VM is re-parented to it. If we don't wait4() them they stay as
	// zombies, occupying a slot in the sandbox cgroup's pids.max budget.
	// Within hours of a build/test loop the customer hits "fork failed:
	// resource temporarily unavailable" even though they have nothing
	// running. Drain on each SIGCHLD; signals coalesce so we loop.
	startReaper()

	// Best-effort: start supervisord if installed and configured. Run as a
	// daemon (default mode, not -n) so it returns immediately and we don't
	// block agent startup. Supervisord becomes a regular child of PID 1; its
	// own children (configured programs) are reparented to it, not to us, so
	// our SIGCHLD reaper doesn't conflict with supervisord's own wait4 calls.
	// Without this, "supervisord baked into rootfs" only works while the user
	// manually starts it — programs don't auto-start at boot the way they do
	// on a systemd-managed VM.
	if _, err := os.Stat("/usr/bin/supervisord"); err == nil {
		if _, err := os.Stat("/etc/supervisor/supervisord.conf"); err == nil {
			if err := exec.Command("/usr/bin/supervisord", "-c", "/etc/supervisor/supervisord.conf").Start(); err != nil {
				log.Printf("agent: supervisord start failed: %v (continuing)", err)
			} else {
				log.Printf("agent: supervisord started (config=/etc/supervisor/supervisord.conf)")
			}
		}
	}

	// Signal handling:
	// SIGTERM/SIGINT: clean shutdown (exit)
	// SIGUSR1: hibernate prep — reset virtio-serial listener so Accept
	//          will poll for a new connection after migration restore
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGUSR1 {
				log.Printf("agent: SIGUSR1 — preparing for hibernate")
				if vsl, ok := lis.(*virtioSerialListener); ok {
					vsl.PrepareHibernate()
				}
			} else {
				log.Printf("agent: received %v, shutting down", sig)
				lis.Close()
				os.Exit(0)
			}
		}
	}()

	if err := srv.Serve(lis); err != nil {
		log.Fatalf("agent: serve failed: %v", err)
	}
}
