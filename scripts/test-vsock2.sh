#!/usr/bin/env bash
set -euo pipefail

TEST_DIR="/data/sandboxes/sandboxes/test-conn2"
mkdir -p "$TEST_DIR"
cp --reflink=auto /data/sandboxes/firecracker/images/ubuntu.ext4 "$TEST_DIR/rootfs.ext4"
truncate -s 1G "$TEST_DIR/workspace.ext4"
mkfs.ext4 -q -F -L workspace "$TEST_DIR/workspace.ext4"

ip tuntap add dev tap-conn1 mode tap 2>/dev/null || true
ip addr flush dev tap-conn1 2>/dev/null || true
ip addr add 172.16.0.9/30 dev tap-conn1
ip link set tap-conn1 up

cat > "$TEST_DIR/vm-config.json" << 'CFGEOF'
{
  "boot-source": {
    "kernel_image_path": "/data/sandboxes/firecracker/vmlinux-arm64",
    "boot_args": "keep_bootcon console=ttyS0 reboot=k panic=1 pci=off ip=172.16.0.10::172.16.0.9:255.255.255.252::eth0:off init=/sbin/init osb.gateway=172.16.0.9"
  },
  "drives": [
    {"drive_id": "rootfs", "path_on_host": "/data/sandboxes/sandboxes/test-conn2/rootfs.ext4", "is_root_device": true, "is_read_only": false},
    {"drive_id": "workspace", "path_on_host": "/data/sandboxes/sandboxes/test-conn2/workspace.ext4", "is_root_device": false, "is_read_only": false}
  ],
  "network-interfaces": [
    {"iface_id": "eth0", "guest_mac": "AA:FC:00:00:01:03", "host_dev_name": "tap-conn1"}
  ],
  "vsock": {"guest_cid": 5, "uds_path": "/data/sandboxes/sandboxes/test-conn2/vsock.sock"},
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 512, "smt": false}
}
CFGEOF

echo "Starting Firecracker..."
/usr/local/bin/firecracker --config-file "$TEST_DIR/vm-config.json" --no-api > "$TEST_DIR/firecracker.log" 2>&1 &
FC_PID=$!
echo "PID=$FC_PID"

# Monitor for files appearing
for i in $(seq 1 20); do
    sleep 0.5
    echo "=== Check $i ($(date +%H:%M:%S.%N)) ==="
    ls "$TEST_DIR"/vsock.sock* 2>/dev/null || echo "nothing yet"
done

echo "=== Full directory listing ==="
ls -la "$TEST_DIR"/ 2>/dev/null

echo "=== FC log tail ==="
tail -15 "$TEST_DIR/firecracker.log"

echo "=== Try connecting to vsock.sock (not _1024) via python ==="
python3 << 'PYEOF'
import socket
import os

# Check what vsock.sock actually is
path = "/data/sandboxes/sandboxes/test-conn2/vsock.sock"
print(f"vsock.sock exists: {os.path.exists(path)}")
print(f"is socket: {os.path.exists(path) and not os.path.isfile(path)}")

# Try connecting directly to vsock.sock (it's the Firecracker vsock multiplexer)
try:
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(3)
    s.connect(path)
    print("Connected to vsock.sock directly!")
    # Send CONNECT command (Firecracker protocol)
    # In Firecracker, host-initiated connections go through the UDS
    # Protocol: connect to vsock.sock, then tell it which port
    s.sendall(b"CONNECT 1024\n")
    try:
        data = s.recv(1024)
        print(f"Response: {data}")
    except socket.timeout:
        print("No response (timeout)")
    s.close()
except Exception as e:
    print(f"Connect to vsock.sock failed: {e}")

# Try vsock.sock_1024
path2 = path + "_1024"
print(f"\nvsock.sock_1024 exists: {os.path.exists(path2)}")
try:
    s2 = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s2.settimeout(3)
    s2.connect(path2)
    print("Connected to vsock.sock_1024!")
    s2.close()
except Exception as e:
    print(f"Connect to vsock.sock_1024 failed: {e}")
PYEOF

# Cleanup
echo "=== Cleanup ==="
kill $FC_PID 2>/dev/null
wait $FC_PID 2>/dev/null || true
ip link del tap-conn1 2>/dev/null || true
rm -rf "$TEST_DIR"
echo "DONE"
