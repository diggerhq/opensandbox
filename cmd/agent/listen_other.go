//go:build !linux

package main

import (
	"fmt"
	"log"
	"net"
	"os"
)

// listenVsock on non-Linux falls back to a Unix domain socket.
// vsock is Linux-only, but we want the agent to build on macOS for testing.
func listenVsock() (net.Listener, error) {
	sockPath := "/tmp/osb-agent.sock"
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	log.Printf("agent: listening on %s (non-Linux, vsock not available)", sockPath)
	return lis, nil
}

// listenPortForPTY returns a ListenPortFunc for PTY data ports.
// On non-Linux, it uses Unix domain sockets since vsock is not available.
func listenPortForPTY(port uint32) (net.Listener, error) {
	sockPath := fmt.Sprintf("/tmp/pty-%d.sock", port)
	os.Remove(sockPath)
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("pty unix listen port %d: %w", port, err)
	}
	log.Printf("agent: PTY data listening on %s (vsock not available)", sockPath)
	return lis, nil
}
