#!/usr/bin/env bash
set -euo pipefail

TEST_DIR="/data/sandboxes/sandboxes/test-conn"
mkdir -p "$TEST_DIR"
cp --reflink=auto /data/sandboxes/firecracker/images/ubuntu.ext4 "$TEST_DIR/rootfs.ext4"
truncate -s 1G "$TEST_DIR/workspace.ext4"
mkfs.ext4 -q -F -L workspace "$TEST_DIR/workspace.ext4"

ip tuntap add dev tap-conn0 mode tap 2>/dev/null || true
ip addr flush dev tap-conn0 2>/dev/null || true
ip addr add 172.16.0.1/30 dev tap-conn0
ip link set tap-conn0 up

cat > "$TEST_DIR/vm-config.json" << 'CFGEOF'
{
  "boot-source": {
    "kernel_image_path": "/data/sandboxes/firecracker/vmlinux-arm64",
    "boot_args": "keep_bootcon console=ttyS0 reboot=k panic=1 pci=off ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off init=/sbin/init osb.gateway=172.16.0.1"
  },
  "drives": [
    {"drive_id": "rootfs", "path_on_host": "/data/sandboxes/sandboxes/test-conn/rootfs.ext4", "is_root_device": true, "is_read_only": false},
    {"drive_id": "workspace", "path_on_host": "/data/sandboxes/sandboxes/test-conn/workspace.ext4", "is_root_device": false, "is_read_only": false}
  ],
  "network-interfaces": [
    {"iface_id": "eth0", "guest_mac": "AA:FC:00:00:01:02", "host_dev_name": "tap-conn0"}
  ],
  "vsock": {"guest_cid": 4, "uds_path": "/data/sandboxes/sandboxes/test-conn/vsock.sock"},
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 512, "smt": false}
}
CFGEOF

echo "Starting Firecracker..."
/usr/local/bin/firecracker --config-file "$TEST_DIR/vm-config.json" --no-api > "$TEST_DIR/firecracker.log" 2>&1 &
FC_PID=$!
echo "PID=$FC_PID"

sleep 3

echo "=== vsock.sock check ==="
ls -la "$TEST_DIR"/vsock.sock* 2>/dev/null || echo "NO vsock files"

echo "=== Agent output (tail of FC log) ==="
tail -10 "$TEST_DIR/firecracker.log"

echo "=== Try connecting to vsock_1024 via python ==="
python3 << 'PYEOF'
import socket
import sys
try:
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(3)
    s.connect("/data/sandboxes/sandboxes/test-conn/vsock.sock_1024")
    print("Connected via python!")
    # Try to send HTTP/2 preface (gRPC uses HTTP/2)
    s.sendall(b"PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
    try:
        data = s.recv(1024)
        print(f"Received {len(data)} bytes: {data[:50]}")
    except socket.timeout:
        print("Recv timed out (normal if gRPC)")
    s.close()
except Exception as e:
    print(f"Connect failed: {e}")
    sys.exit(1)
PYEOF

echo "=== Try grpcurl if available ==="
which grpcurl 2>/dev/null && grpcurl -plaintext -unix "$TEST_DIR/vsock.sock_1024" list 2>&1 || echo "grpcurl not available"

# Cleanup
echo "=== Cleanup ==="
kill $FC_PID 2>/dev/null
wait $FC_PID 2>/dev/null || true
ip link del tap-conn0 2>/dev/null || true
rm -rf "$TEST_DIR"
echo "DONE"
