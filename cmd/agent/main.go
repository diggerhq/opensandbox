// osb-agent is the sandbox agent that runs inside each VM.
// It listens for gRPC connections over virtio-serial (QEMU) or vsock (Firecracker)
// and handles exec, file ops, PTY sessions, and stats collection.
//
// Build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X main.Version=$(git rev-parse --short HEAD)" -o osb-agent ./cmd/agent
package main

import (
	"log"
	"os"
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

	log.Printf("osb-agent %s starting (build=pin-to-base)", Version)

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
