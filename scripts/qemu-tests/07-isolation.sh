#!/usr/bin/env bash
# 07-isolation.sh — Resource isolation, cgroups, process list, cross-sandbox isolation
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb"; done; }
trap cleanup EXIT

h "Resource Isolation & Cgroups"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# PID 1 is agent
PIDS=$(exec_stdout "$SB" "ps" "aux")
echo "$PIDS" | grep -q 'osb-agent' && pass "PID 1 is osb-agent" || fail "PID 1 not osb-agent"

# Count user processes (exclude kernel threads [bracketed], ps itself, header)
USER_PROCS=$(echo "$PIDS" | grep -v '\[.*\]' | grep -v 'ps aux' | grep -v 'USER' | grep -cv '^\s*$' || true)
[ "$USER_PROCS" -le 2 ] && pass "Minimal user processes: $USER_PROCS" || fail "Too many user processes: $USER_PROCS"

# Cgroup pids.max
PIDS_MAX=$(exec_stdout "$SB" "cat" "/sys/fs/cgroup/sandbox/pids.max")
[ "$PIDS_MAX" = "128" ] && pass "pids.max = 128" || fail "pids.max = $PIDS_MAX"

# Cgroup memory.max (~90% of 1GB)
MEM_MAX=$(exec_stdout "$SB" "cat" "/sys/fs/cgroup/sandbox/memory.max")
MEM_MAX_MB=$((MEM_MAX / 1024 / 1024))
[ "$MEM_MAX_MB" -gt 700 ] && [ "$MEM_MAX_MB" -lt 950 ] && pass "memory.max = ${MEM_MAX_MB}MB (90% of 1GB)" || fail "memory.max = ${MEM_MAX_MB}MB"

# Cgroup cpu.max
CPU_MAX=$(exec_stdout "$SB" "cat" "/sys/fs/cgroup/sandbox/cpu.max")
echo "$CPU_MAX" | grep -qE '^[0-9]+ 100000$' && pass "cpu.max = $CPU_MAX" || fail "cpu.max = $CPU_MAX"

# Cross-sandbox isolation
h "Cross-Sandbox Isolation"
SB_A=$(create_sandbox)
SB_B=$(create_sandbox)
SANDBOXES+=("$SB_A" "$SB_B")

exec_run "$SB_A" "bash" "-c" "echo secret-A > /workspace/secret.txt" >/dev/null
OUT=$(exec_stdout "$SB_B" "bash" "-c" "cat /workspace/secret.txt 2>/dev/null || echo not-found")
[ "$OUT" = "not-found" ] && pass "Cross-sandbox isolated (A's file not in B)" || fail "Cross-sandbox leak: '$OUT'"

# Network works
h "Network"
OUT=$(exec_stdout "$SB" "bash" "-c" "ping -c1 -W3 8.8.8.8 2>&1 | grep -c 'bytes from' || echo 0")
[ "$OUT" = "1" ] && pass "Network: ping 8.8.8.8" || fail "Ping failed"

OUT=$(exec_stdout "$SB" "bash" "-c" "nslookup google.com 2>&1 | grep -c 'Address' || echo 0")
[ "$OUT" -ge 1 ] && pass "DNS resolution" || fail "DNS failed"

summary
