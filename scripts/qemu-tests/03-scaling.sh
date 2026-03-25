#!/usr/bin/env bash
# 03-scaling.sh — Memory scaling via virtio-mem (external + internal API)
source "$(dirname "$0")/common.sh"

h "Memory Scaling (virtio-mem)"

SB=$(create_sandbox)
trap "destroy_sandbox $SB" EXIT

# Baseline
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 800 ] && [ "$MEM" -lt 1000 ] && pass "Baseline: ${MEM}MB (~1GB)" || fail "Baseline: ${MEM}MB"

# Scale to 2GB via external API
api -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -d '{"maxMemoryMB":2048,"cpuPercent":200,"maxPids":256}' >/dev/null
sleep 1
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 1800 ] && [ "$MEM" -lt 2100 ] && pass "Scale 2GB (external): ${MEM}MB" || fail "Scale 2GB: ${MEM}MB"

# Scale to 4GB via external API
api -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -d '{"maxMemoryMB":4096,"cpuPercent":400,"maxPids":256}' >/dev/null
sleep 1
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 3800 ] && [ "$MEM" -lt 4200 ] && pass "Scale 4GB: ${MEM}MB" || fail "Scale 4GB: ${MEM}MB"

# Scale to 8GB and allocate 7GB
api -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -d '{"maxMemoryMB":8192,"cpuPercent":800,"maxPids":256}' >/dev/null
sleep 1
OUT=$(exec_stdout "$SB" "python3" "-c" "x=bytearray(7*1024*1024*1024); print(len(x))")
[ "$OUT" = "7516192768" ] && pass "8GB: 7GB allocation OK" || fail "8GB allocation: '$OUT'"

# Scale down to 1GB (cgroup enforced, physical stays)
api -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -d '{"maxMemoryMB":1024,"cpuPercent":100,"maxPids":128}' >/dev/null
sleep 1
CGROUP_MAX=$(exec_stdout "$SB" "cat" "/sys/fs/cgroup/sandbox/memory.max")
# Should be ~1GB in bytes
CGROUP_MB=$((CGROUP_MAX / 1024 / 1024))
[ "$CGROUP_MB" -gt 900 ] && [ "$CGROUP_MB" -lt 1200 ] && pass "Scale down cgroup: ${CGROUP_MB}MB" || fail "Scale down cgroup: ${CGROUP_MB}MB"

# Internal scale API
h "Internal Scale API (169.254.169.254)"
RESULT=$(exec_stdout "$SB" "curl" "-s" "-X" "POST" "http://169.254.169.254/v1/scale" "-d" '{"memoryMB":2048}')
echo "$RESULT" | grep -q '"ok":true' && pass "Internal scale API" || fail "Internal scale: $RESULT"
sleep 1
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 1800 ] && pass "Internal scale verified: ${MEM}MB" || fail "Internal scale verify: ${MEM}MB"

summary
