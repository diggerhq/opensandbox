package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// FirecrackerClient is a minimal HTTP client for the Firecracker API socket.
type FirecrackerClient struct {
	socketPath string
	httpClient *http.Client
}

// NewFirecrackerClient creates a client that talks to a Firecracker instance
// via its Unix domain API socket.
func NewFirecrackerClient(socketPath string) *FirecrackerClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &FirecrackerClient{
		socketPath: socketPath,
		httpClient: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

// WaitForSocket polls until the API socket file exists on disk.
func (c *FirecrackerClient) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(c.socketPath); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("firecracker API socket %s not ready after %v", c.socketPath, timeout)
}

// PutBootSource configures the kernel boot source.
func (c *FirecrackerClient) PutBootSource(kernelPath, bootArgs string) error {
	body := map[string]string{
		"kernel_image_path": kernelPath,
		"boot_args":         bootArgs,
	}
	return c.put("/boot-source", body)
}

// PutDrive attaches a block device (drive) to the VM.
func (c *FirecrackerClient) PutDrive(driveID, pathOnHost string, isRootDevice, isReadOnly bool) error {
	body := map[string]interface{}{
		"drive_id":       driveID,
		"path_on_host":   pathOnHost,
		"is_root_device": isRootDevice,
		"is_read_only":   isReadOnly,
	}
	return c.putWithID("/drives", driveID, body)
}

// PutNetworkInterface attaches a network interface.
func (c *FirecrackerClient) PutNetworkInterface(ifaceID, guestMAC, hostDevName string) error {
	body := map[string]interface{}{
		"iface_id":      ifaceID,
		"guest_mac":     guestMAC,
		"host_dev_name": hostDevName,
	}
	return c.putWithID("/network-interfaces", ifaceID, body)
}

// PutVsock configures the vsock device.
func (c *FirecrackerClient) PutVsock(guestCID uint32, udsPath string) error {
	body := map[string]interface{}{
		"guest_cid": guestCID,
		"uds_path":  udsPath,
	}
	return c.put("/vsock", body)
}

// PutMachineConfig sets vCPU count and memory size.
func (c *FirecrackerClient) PutMachineConfig(vcpuCount, memSizeMib int) error {
	body := map[string]interface{}{
		"vcpu_count":   vcpuCount,
		"mem_size_mib": memSizeMib,
	}
	return c.put("/machine-config", body)
}

// StartInstance boots the configured VM.
func (c *FirecrackerClient) StartInstance() error {
	body := map[string]string{
		"action_type": "InstanceStart",
	}
	return c.put("/actions", body)
}

// PauseVM pauses a running VM.
func (c *FirecrackerClient) PauseVM() error {
	body := map[string]string{
		"state": "Paused",
	}
	return c.patch("/vm", body)
}

// ResumeVM resumes a paused VM.
func (c *FirecrackerClient) ResumeVM() error {
	body := map[string]string{
		"state": "Resumed",
	}
	return c.patch("/vm", body)
}

// CreateSnapshot creates a full VM snapshot (memory + device state).
// The VM must be paused before calling this.
func (c *FirecrackerClient) CreateSnapshot(snapshotPath, memFilePath string) error {
	body := map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": snapshotPath,
		"mem_file_path": memFilePath,
	}
	return c.put("/snapshot/create", body)
}

// LoadSnapshot restores a VM from a snapshot.
// If resumeVM is true, the VM starts running immediately after load.
func (c *FirecrackerClient) LoadSnapshot(snapshotPath, memFilePath string, resumeVM bool) error {
	body := map[string]interface{}{
		"snapshot_path":        snapshotPath,
		"mem_backend": map[string]string{
			"backend_path":   memFilePath,
			"backend_type":   "File",
		},
		"enable_diff_snapshots": false,
		"resume_vm":             resumeVM,
	}
	return c.put("/snapshot/load", body)
}

func (c *FirecrackerClient) put(path string, body interface{}) error {
	return c.doRequest(http.MethodPut, path, body)
}

func (c *FirecrackerClient) putWithID(basePath, id string, body interface{}) error {
	return c.doRequest(http.MethodPut, basePath+"/"+id, body)
}

func (c *FirecrackerClient) patch(path string, body interface{}) error {
	return c.doRequest(http.MethodPatch, path, body)
}

func (c *FirecrackerClient) doRequest(method, path string, body interface{}) error {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(jsonBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("firecracker API %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	return nil
}
