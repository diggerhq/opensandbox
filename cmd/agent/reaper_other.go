//go:build !linux

package main

// startReaper is a no-op on non-Linux platforms. The agent only runs as
// PID 1 inside the sandbox VM (Linux); local Darwin/Windows builds are
// for testing and don't need to reap children.
func startReaper() {}
