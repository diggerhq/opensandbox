#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 7: Test QEMU startup on target worker ==="

# Find the idle (target) worker
TARGET_VM=$(az vm list --resource-group opensandbox-prod --query "[].name" -o tsv 2>&1 | grep "osb-worker-" | grep -v "osb-worker-1" | head -1)
echo "Target VM: $TARGET_VM"

if [ -z "$TARGET_VM" ]; then
    echo "ERROR: No autoscaled worker found"
    exit 1
fi

echo ""
echo "Testing QEMU with 4GB base + 15GB virtio-mem pool..."
az vm run-command invoke --resource-group opensandbox-prod --name "$TARGET_VM" --command-id RunShellScript \
  --scripts "
rm -rf /tmp/qemu-test; mkdir -p /tmp/qemu-test

# Create qcow2 overlay
qemu-img create -f qcow2 -b /data/firecracker/images/default.ext4 -F raw /tmp/qemu-test/rootfs.qcow2 2>&1

# Start QEMU same as migration would
qemu-system-x86_64 \
  -machine q35,accel=kvm -cpu host \
  -m 4096M,slots=1,maxmem=19456M \
  -object memory-backend-ram,id=vmem0,size=15360M \
  -device virtio-mem-pci,memdev=vmem0,id=vm0,block-size=128M,requested-size=0 \
  -smp 1 \
  -kernel /opt/opensandbox/vmlinux \
  -append 'console=ttyS0 root=/dev/vda rw' \
  -drive file=/tmp/qemu-test/rootfs.qcow2,format=qcow2,if=virtio \
  -qmp unix:/tmp/qemu-test/qmp.sock,server,nowait \
  -nographic -nodefaults \
  -incoming tcp:0:44444 \
  2>/tmp/qemu-test/qemu.log &
PID=\$!
echo \"QEMU pid: \$PID\"

for i in \$(seq 1 30); do
    if python3 -c \"
import socket
s=socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.settimeout(2)
s.connect('/tmp/qemu-test/qmp.sock')
data=s.recv(4096)
print('QMP connected after \${i}s:', data.decode()[:100])
s.close()
\" 2>/dev/null; then
        kill \$PID 2>/dev/null
        rm -rf /tmp/qemu-test
        exit 0
    fi
    sleep 1
done

echo 'QMP FAILED after 30s'
echo 'QEMU log:'
cat /tmp/qemu-test/qemu.log
echo 'Process state:'
ps -p \$PID -o stat= 2>/dev/null || echo 'dead'
kill \$PID 2>/dev/null
rm -rf /tmp/qemu-test
" 2>&1 | python3 -c "import sys,json; [print(v.get('message','')) for v in json.load(sys.stdin).get('value',[])]" 2>&1

echo ""
echo "DONE: If QMP connected, the migration target QEMU works. If failed, check the log above."
