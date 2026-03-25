#!/usr/bin/env bash
# 06-checkpoint-fork.sh — Checkpoint creation, forking, state isolation
source "$(dirname "$0")/common.sh"

TIMEOUT=60
SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb"; done; }
trap cleanup EXIT

h "Checkpoint / Fork"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Install numpy + write workspace file before checkpoint
exec_run "$SB" "bash" "-c" "pip3 install --quiet numpy 2>/dev/null" >/dev/null
exec_run "$SB" "bash" "-c" "echo checkpoint-data > /workspace/check.txt" >/dev/null
pass "Setup: numpy + workspace file"

# Create checkpoint
CP_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/checkpoints" -d '{"name":"test-cp"}')
CP_ID=$(echo "$CP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
[ -n "$CP_ID" ] && pass "Create checkpoint: $CP_ID" || { fail "Create checkpoint"; summary; }

sleep 5  # wait for savevm

# Fork from checkpoint
FORK_RESULT=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$CP_ID" -d '{"timeout":3600}')
FORK_ID=$(echo "$FORK_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")
SANDBOXES+=("$FORK_ID")
[ -n "$FORK_ID" ] && pass "Fork: $FORK_ID" || { fail "Fork failed"; summary; }

# Wait for fork to be ready
sleep 8
wait_for_sandbox "$FORK_ID" 15 && pass "Fork running" || { fail "Fork not ready"; summary; }

# Verify numpy on fork
OUT=$(exec_stdout "$FORK_ID" "python3" "-c" "import numpy; print(numpy.__version__)")
[ -n "$OUT" ] && pass "Fork has numpy: $OUT" || fail "Fork numpy missing"

# Verify workspace file on fork
OUT=$(exec_stdout "$FORK_ID" "cat" "/workspace/check.txt")
[ "$OUT" = "checkpoint-data" ] && pass "Fork has workspace file" || fail "Fork workspace: '$OUT'"

# Pre-scale checkpoint test
h "Pre-Scale Checkpoint"
api -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -d '{"maxMemoryMB":4096,"cpuPercent":400,"maxPids":256}' >/dev/null
sleep 1
MEM_ORIG=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
pass "Original scaled to 4GB: ${MEM_ORIG}MB"

# Fork from the PRE-SCALE checkpoint
FORK2_RESULT=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$CP_ID" -d '{"timeout":3600}')
FORK2_ID=$(echo "$FORK2_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")
SANDBOXES+=("$FORK2_ID")
sleep 8
wait_for_sandbox "$FORK2_ID" 15

MEM_FORK=$(exec_stdout "$FORK2_ID" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM_FORK" -lt 1100 ] && pass "Fork has original 1GB: ${MEM_FORK}MB (not 4GB)" || fail "Fork memory: ${MEM_FORK}MB (expected ~896)"

# Isolation: write to fork, verify original unaffected
h "Fork Isolation"
exec_run "$FORK_ID" "bash" "-c" "echo fork-only > /workspace/fork-file.txt" >/dev/null
OUT=$(exec_stdout "$SB" "bash" "-c" "cat /workspace/fork-file.txt 2>/dev/null || echo not-found")
[ "$OUT" = "not-found" ] && pass "Fork isolated from original" || fail "Fork leak: '$OUT'"

# Multiple forks
h "Multiple Forks"
for i in 1 2 3; do
    F_RESULT=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$CP_ID" -d '{"timeout":3600}')
    F_ID=$(echo "$F_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")
    SANDBOXES+=("$F_ID")
    sleep 5
    if wait_for_sandbox "$F_ID" 15; then
        pass "Fork $i: $F_ID"
    else
        fail "Fork $i failed"
    fi
done

summary
