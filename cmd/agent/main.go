// osb-agent is the sandbox agent that runs inside each Firecracker microVM.
// It listens for gRPC connections over vsock and handles exec, file ops,
// PTY sessions, and stats collection.
//
// Build: CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o osb-agent ./cmd/agent
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/opensandbox/opensandbox/internal/agent"
)

const version = "0.1.0"

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("osb-agent %s starting", version)

	// Listen on vsock port 1024 (inside Firecracker) or Unix socket (testing).
	lis, err := listenVsock()
	if err != nil {
		log.Fatalf("agent: failed to listen: %v", err)
	}

	srv := agent.NewServer(version)
	srv.ListenPort = listenPortForPTY

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Printf("agent: received %v, shutting down", sig)
		lis.Close()
		os.Exit(0)
	}()

	if err := srv.Serve(lis); err != nil {
		log.Fatalf("agent: serve failed: %v", err)
	}
}
