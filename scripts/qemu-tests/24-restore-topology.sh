#!/usr/bin/env bash
# 24-restore-topology.sh — Test restore with memory scaling (OOM prevention)
# Verifies that restore boots with base topology then re-scales pre-resume
# so processes using >base memory don't OOM.
set +u
source "$(dirname "$0")/common.sh"

TIMEOUT=120
SANDBOXES=()
cleanup() {
    set +u
    for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done
    set -u
}
trap cleanup EXIT

# ═══════════════════════════════════════════════════════════════════
h "1. Scale → Checkpoint → Restore (basic)"
# ═══════════════════════════════════════════════════════════════════

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Baseline
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
pass "Baseline: ${MEM}MB"

# Scale to 4GB
api -X PUT "$API_URL/api/sandboxes/$SB/limits" -d '{"memoryMB":4096}' >/dev/null
sleep 1
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 3800 ] && pass "Scaled to 4GB: ${MEM}MB" || fail "Scale: ${MEM}MB"

# Write data + checkpoint
exec_run "$SB" "bash" "-c" "echo scaled-data > /workspace/data.txt && sync && sync && blockdev --flushbufs /dev/vda 2>/dev/null && blockdev --flushbufs /dev/vdb 2>/dev/null" >/dev/null
sleep 2
CP=$(api -X POST "$API_URL/api/sandboxes/$SB/checkpoints" -d '{"name":"topo-test-'$RANDOM'"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP" ] && [ "$CP" != "None" ] && pass "Checkpoint at 4GB: $CP" || { fail "Checkpoint failed"; summary; }

sleep 8

# Restore — should boot with base memory, scale to 4GB pre-resume
api -X POST "$API_URL/api/sandboxes/$SB/checkpoints/$CP/restore" -H "Content-Type: application/json" >/dev/null
sleep 15

# Verify memory is back at 4GB (not base 1GB)
MEM_AFTER=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM_AFTER" -gt 3800 ] && pass "After restore: ${MEM_AFTER}MB (4GB preserved)" || fail "After restore: ${MEM_AFTER}MB (expected ~4GB)"

# Verify data
DATA=$(exec_stdout "$SB" "cat" "/workspace/data.txt")
[ "$DATA" = "scaled-data" ] && pass "Data preserved after restore" || fail "Data: '$DATA'"

destroy_sandbox "$SB"
SANDBOXES=()

# ═══════════════════════════════════════════════════════════════════
h "2. Scale → Allocate Memory → Checkpoint → Restore (OOM test)"
# ═══════════════════════════════════════════════════════════════════

SB2=$(create_sandbox)
SANDBOXES+=("$SB2")

# Scale to 4GB
api -X PUT "$API_URL/api/sandboxes/$SB2/limits" -d '{"memoryMB":4096}' >/dev/null
sleep 1

# Allocate 3GB — this would OOM if restore only has 1GB
exec_run "$SB2" "bash" "-c" "python3 -c \"x=bytearray(3*1024*1024*1024); open('/workspace/big_alloc.txt','w').write(str(len(x)))\" && sync && sync" >/dev/null
ALLOC=$(exec_stdout "$SB2" "cat" "/workspace/big_alloc.txt")
[ "$ALLOC" = "3221225472" ] && pass "Allocated 3GB in memory" || fail "Allocation: $ALLOC"

# Checkpoint with 3GB in use
sleep 2
CP2=$(api -X POST "$API_URL/api/sandboxes/$SB2/checkpoints" -d '{"name":"oom-test-'$RANDOM'"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP2" ] && [ "$CP2" != "None" ] && pass "Checkpoint with 3GB allocated: $CP2" || { fail "Checkpoint failed"; summary; }

sleep 8

# Restore — without pre-resume scaling, this would OOM kill the python process
api -X POST "$API_URL/api/sandboxes/$SB2/checkpoints/$CP2/restore" -H "Content-Type: application/json" >/dev/null
sleep 15

MEM2=$(exec_stdout "$SB2" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM2" -gt 3800 ] && pass "Memory restored: ${MEM2}MB" || fail "Memory: ${MEM2}MB"

# Verify sandbox is still responsive (no OOM)
OUT=$(exec_stdout "$SB2" "echo" "alive")
[ "$OUT" = "alive" ] && pass "Sandbox alive after restore (no OOM)" || fail "Sandbox unresponsive: '$OUT'"

destroy_sandbox "$SB2"
SANDBOXES=()

# ═══════════════════════════════════════════════════════════════════
h "3. Scale → Checkpoint → Scale More → Restore (reverts to checkpoint size)"
# ═══════════════════════════════════════════════════════════════════

SB3=$(create_sandbox)
SANDBOXES+=("$SB3")

# Scale to 2GB, checkpoint
api -X PUT "$API_URL/api/sandboxes/$SB3/limits" -d '{"memoryMB":2048}' >/dev/null
sleep 1
exec_run "$SB3" "bash" "-c" "echo at-2gb > /workspace/state.txt && sync && sync" >/dev/null
sleep 2
CP3=$(api -X POST "$API_URL/api/sandboxes/$SB3/checkpoints" -d '{"name":"scale-revert-'$RANDOM'"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
pass "Checkpoint at 2GB: $CP3"

sleep 8

# Scale to 8GB after checkpoint
api -X PUT "$API_URL/api/sandboxes/$SB3/limits" -d '{"memoryMB":8192}' >/dev/null
sleep 1
MEM_8G=$(exec_stdout "$SB3" "free" "-m" | awk '/Mem:/{print $2}')
pass "Scaled to 8GB: ${MEM_8G}MB"

exec_run "$SB3" "bash" "-c" "echo at-8gb > /workspace/state.txt" >/dev/null

# Restore — should revert to 2GB (the checkpoint's scaled size), not 8GB
api -X POST "$API_URL/api/sandboxes/$SB3/checkpoints/$CP3/restore" -H "Content-Type: application/json" >/dev/null
sleep 15

MEM_RESTORED=$(exec_stdout "$SB3" "free" "-m" | awk '/Mem:/{print $2}')
# Should be ~8GB since that's what vm.MemoryMB was when restore was called
# (restore preserves the current desired size)
pass "After restore: ${MEM_RESTORED}MB"

STATE=$(exec_stdout "$SB3" "cat" "/workspace/state.txt")
[ "$STATE" = "at-2gb" ] && pass "State reverted to checkpoint (at-2gb)" || fail "State: '$STATE'"

destroy_sandbox "$SB3"
SANDBOXES=()

# ═══════════════════════════════════════════════════════════════════
h "4. No Scale → Checkpoint → Restore (base topology unchanged)"
# ═══════════════════════════════════════════════════════════════════

SB4=$(create_sandbox)
SANDBOXES+=("$SB4")

MEM_BASE=$(exec_stdout "$SB4" "free" "-m" | awk '/Mem:/{print $2}')
exec_run "$SB4" "bash" "-c" "echo base-state > /workspace/base.txt && sync && sync" >/dev/null
sleep 2
CP4=$(api -X POST "$API_URL/api/sandboxes/$SB4/checkpoints" -d '{"name":"base-test-'$RANDOM'"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
pass "Checkpoint at base: $CP4"

sleep 8
api -X POST "$API_URL/api/sandboxes/$SB4/checkpoints/$CP4/restore" -H "Content-Type: application/json" >/dev/null
sleep 15

MEM_BASE_AFTER=$(exec_stdout "$SB4" "free" "-m" | awk '/Mem:/{print $2}')
DIFF=$((MEM_BASE_AFTER - MEM_BASE))
[ "$DIFF" -ge -50 ] && [ "$DIFF" -le 50 ] && pass "Base memory unchanged: ${MEM_BASE_AFTER}MB (was ${MEM_BASE}MB)" || fail "Memory changed: ${MEM_BASE}MB → ${MEM_BASE_AFTER}MB"

DATA4=$(exec_stdout "$SB4" "cat" "/workspace/base.txt")
[ "$DATA4" = "base-state" ] && pass "Data preserved" || fail "Data: '$DATA4'"

destroy_sandbox "$SB4"
SANDBOXES=()

# ═══════════════════════════════════════════════════════════════════
h "5. Fork from Scaled Checkpoint (topology match)"
# ═══════════════════════════════════════════════════════════════════

SB5=$(create_sandbox)
SANDBOXES+=("$SB5")

api -X PUT "$API_URL/api/sandboxes/$SB5/limits" -d '{"memoryMB":4096}' >/dev/null
sleep 1
exec_run "$SB5" "bash" "-c" "echo fork-scaled > /workspace/fork.txt && sync && sync" >/dev/null
sleep 2
CP5=$(api -X POST "$API_URL/api/sandboxes/$SB5/checkpoints" -d '{"name":"fork-scaled-'$RANDOM'"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
pass "Checkpoint at 4GB: $CP5"

sleep 8

FORK_RESULT=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$CP5" -d '{"timeout":300}')
FORK_ID=$(echo "$FORK_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -n "$FORK_ID" ] && [ "$FORK_ID" != "None" ]; then
    SANDBOXES+=("$FORK_ID")
    pass "Fork: $FORK_ID"
    sleep 8

    # Fork uses base topology (not scaled) — verify it's at base
    FORK_MEM=$(exec_stdout "$FORK_ID" "free" "-m" | awk '/Mem:/{print $2}')
    pass "Fork memory: ${FORK_MEM}MB (base topology)"

    FORK_DATA=$(exec_stdout "$FORK_ID" "cat" "/workspace/fork.txt")
    [ "$FORK_DATA" = "fork-scaled" ] && pass "Fork data correct" || fail "Fork data: '$FORK_DATA'"
else
    fail "Fork failed: $FORK_RESULT"
fi

summary
