// Package qemu implements sandbox.Manager using QEMU q35 VMs with KVM acceleration.
package qemu

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// QMPClient communicates with a QEMU instance via QMP (QEMU Machine Protocol)
// over a Unix domain socket. QMP uses line-delimited JSON messages.
type QMPClient struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// qmpResponse is the generic QMP response envelope.
type qmpResponse struct {
	Return json.RawMessage `json:"return,omitempty"`
	Error  *qmpError       `json:"error,omitempty"`
	Event  string          `json:"event,omitempty"`
	QMP    json.RawMessage `json:"QMP,omitempty"`
}

type qmpError struct {
	Class string `json:"class"`
	Desc  string `json:"desc"`
}

// qmpCommand is the generic QMP command envelope.
type qmpCommand struct {
	Execute   string      `json:"execute"`
	Arguments interface{} `json:"arguments,omitempty"`
}

// QMPStatus represents the response from query-status.
type QMPStatus struct {
	Running bool   `json:"running"`
	Status  string `json:"status"`
}

// QMPMigrateStatus represents the response from query-migrate.
type QMPMigrateStatus struct {
	Status string `json:"status"`
}

// ConnectQMP connects to the QMP socket and completes the handshake:
// 1. Read greeting {"QMP": {...}}
// 2. Send {"execute": "qmp_capabilities"}
// 3. Read {"return": {}}
func ConnectQMP(socketPath string, timeout time.Duration) (*QMPClient, error) {
	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial QMP socket %s: %w", socketPath, err)
	}

	_ = conn.SetDeadline(time.Now().Add(timeout))
	r := bufio.NewReader(conn)

	// Read greeting
	line, err := r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read QMP greeting: %w", err)
	}
	var greeting qmpResponse
	if err := json.Unmarshal(line, &greeting); err != nil || greeting.QMP == nil {
		conn.Close()
		return nil, fmt.Errorf("invalid QMP greeting: %s", string(line))
	}

	// Send qmp_capabilities
	capCmd, _ := json.Marshal(qmpCommand{Execute: "qmp_capabilities"})
	capCmd = append(capCmd, '\n')
	if _, err := conn.Write(capCmd); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send qmp_capabilities: %w", err)
	}

	// Read response
	line, err = r.ReadBytes('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read qmp_capabilities response: %w", err)
	}
	var capResp qmpResponse
	if err := json.Unmarshal(line, &capResp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse qmp_capabilities response: %w", err)
	}
	if capResp.Error != nil {
		conn.Close()
		return nil, fmt.Errorf("qmp_capabilities error: %s: %s", capResp.Error.Class, capResp.Error.Desc)
	}

	_ = conn.SetDeadline(time.Time{}) // clear deadline

	return &QMPClient{conn: conn, r: r}, nil
}

// execute sends a QMP command and returns the response, filtering out async events.
func (q *QMPClient) execute(cmd qmpCommand, timeout time.Duration) (*qmpResponse, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	data, _ := json.Marshal(cmd)
	data = append(data, '\n')

	_ = q.conn.SetDeadline(time.Now().Add(timeout))
	defer q.conn.SetDeadline(time.Time{})

	if _, err := q.conn.Write(data); err != nil {
		return nil, fmt.Errorf("write QMP command %s: %w", cmd.Execute, err)
	}

	// Read response, skipping async events
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("read QMP response for %s: %w", cmd.Execute, err)
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return nil, fmt.Errorf("parse QMP response for %s: %w (%s)", cmd.Execute, err, string(line))
		}
		// Skip async events
		if resp.Event != "" {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("QMP %s error: %s: %s", cmd.Execute, resp.Error.Class, resp.Error.Desc)
		}
		return &resp, nil
	}
}

// Stop pauses the VM (equivalent to pressing the pause button).
func (q *QMPClient) Stop() error {
	_, err := q.execute(qmpCommand{Execute: "stop"}, 10*time.Second)
	return err
}

// Cont resumes the VM after a stop.
func (q *QMPClient) Cont() error {
	_, err := q.execute(qmpCommand{Execute: "cont"}, 10*time.Second)
	return err
}

// Quit terminates the QEMU process.
func (q *QMPClient) Quit() error {
	_, err := q.execute(qmpCommand{Execute: "quit"}, 10*time.Second)
	return err
}

// QueryStatus returns the current VM status.
func (q *QMPClient) QueryStatus() (*QMPStatus, error) {
	resp, err := q.execute(qmpCommand{Execute: "query-status"}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var status QMPStatus
	if err := json.Unmarshal(resp.Return, &status); err != nil {
		return nil, fmt.Errorf("parse query-status: %w", err)
	}
	return &status, nil
}

// Migrate saves the VM state to the given URI.
// URI format: "exec:cat > /path/to/statefile"
func (q *QMPClient) Migrate(uri string) error {
	_, err := q.execute(qmpCommand{
		Execute:   "migrate",
		Arguments: map[string]string{"uri": uri},
	}, 5*time.Minute)
	return err
}

// QueryMigrate returns the current migration status.
func (q *QMPClient) QueryMigrate() (*QMPMigrateStatus, error) {
	resp, err := q.execute(qmpCommand{Execute: "query-migrate"}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var status QMPMigrateStatus
	if err := json.Unmarshal(resp.Return, &status); err != nil {
		return nil, fmt.Errorf("parse query-migrate: %w", err)
	}
	return &status, nil
}

// WaitMigration polls query-migrate until status is "completed" or an error occurs.
func (q *QMPClient) WaitMigration(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := q.QueryMigrate()
		if err != nil {
			return err
		}
		switch status.Status {
		case "completed":
			return nil
		case "failed":
			return fmt.Errorf("migration failed")
		case "cancelled":
			return fmt.Errorf("migration cancelled")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("migration timed out after %v", timeout)
}

// SaveVM creates an internal VM snapshot (memory + device state + disk deltas).
// Requires all drives to be qcow2 format. The snapshot is stored inside the qcow2 files.
func (q *QMPClient) SaveVM(name string) error {
	_, err := q.execute(qmpCommand{
		Execute:   "human-monitor-command",
		Arguments: map[string]string{"command-line": "savevm " + name},
	}, 5*time.Minute)
	return err
}

// LoadVM restores an internal VM snapshot. The VM reverts to the exact state
// when SaveVM was called — memory, devices, and disk contents all revert.
func (q *QMPClient) LoadVM(name string) error {
	_, err := q.execute(qmpCommand{
		Execute:   "human-monitor-command",
		Arguments: map[string]string{"command-line": "loadvm " + name},
	}, 5*time.Minute)
	return err
}

// DeleteVM deletes an internal VM snapshot.
func (q *QMPClient) DeleteVM(name string) error {
	_, err := q.execute(qmpCommand{
		Execute:   "human-monitor-command",
		Arguments: map[string]string{"command-line": "delvm " + name},
	}, 30*time.Second)
	return err
}

// Execute sends an arbitrary QMP command with the given arguments.
func (q *QMPClient) Execute(command string, args map[string]interface{}) error {
	cmd := qmpCommand{Execute: command}
	if len(args) > 0 {
		cmd.Arguments = args
	}
	_, err := q.execute(cmd, 10*time.Second)
	return err
}

// HotplugMemory adds a memory DIMM to the running VM.
// The guest kernel detects the new memory automatically.
// Each DIMM gets a unique ID based on the index (dimm0, dimm1, etc.).
func (q *QMPClient) HotplugMemory(dimmIndex int, sizeMB int) error {
	backendID := fmt.Sprintf("mem%d", dimmIndex)
	dimmID := fmt.Sprintf("dimm%d", dimmIndex)
	sizeBytes := int64(sizeMB) * 1024 * 1024

	// Step 1: Create memory backend
	if err := q.Execute("object-add", map[string]interface{}{
		"qom-type": "memory-backend-ram",
		"id":       backendID,
		"size":     sizeBytes,
	}); err != nil {
		return fmt.Errorf("object-add memory backend: %w", err)
	}

	// Step 2: Attach DIMM device
	if err := q.Execute("device_add", map[string]interface{}{
		"driver": "pc-dimm",
		"id":     dimmID,
		"memdev": backendID,
	}); err != nil {
		return fmt.Errorf("device_add dimm: %w", err)
	}

	return nil
}

// SendFd passes an open file descriptor to QEMU via the QMP getfd command.
// QEMU receives the fd via SCM_RIGHTS and registers it under the given name.
func (q *QMPClient) SendFd(name string, f *os.File) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	unixConn, ok := q.conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("QMP connection is not a Unix socket")
	}

	// Build the getfd command
	cmd := qmpCommand{
		Execute:   "getfd",
		Arguments: map[string]string{"fdname": name},
	}
	data, _ := json.Marshal(cmd)
	data = append(data, '\n')

	// Send the command with the fd attached via SCM_RIGHTS
	rights := syscall.UnixRights(int(f.Fd()))
	_, _, err := unixConn.WriteMsgUnix(data, rights, nil)
	if err != nil {
		return fmt.Errorf("send fd via SCM_RIGHTS: %w", err)
	}

	// Read response
	_ = q.conn.SetDeadline(time.Now().Add(10 * time.Second))
	defer q.conn.SetDeadline(time.Time{})
	for {
		line, err := q.r.ReadBytes('\n')
		if err != nil {
			return fmt.Errorf("read getfd response: %w", err)
		}
		var resp qmpResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			return fmt.Errorf("parse getfd response: %w", err)
		}
		if resp.Event != "" {
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("getfd error: %s: %s", resp.Error.Class, resp.Error.Desc)
		}
		return nil
	}
}

// Close closes the QMP connection.
func (q *QMPClient) Close() error {
	if q == nil || q.conn == nil {
		return nil
	}
	return q.conn.Close()
}
