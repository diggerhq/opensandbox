//go:build !linux

package main

import (
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
