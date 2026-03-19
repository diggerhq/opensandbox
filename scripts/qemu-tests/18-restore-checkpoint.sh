#!/usr/bin/env bash
# 18-restore-checkpoint.sh — In-place checkpoint restore (loadvm), verify state revert
set +u
source "$(dirname "$0")/common.sh"

TIMEOUT=60
SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "Restore Checkpoint (In-Place Revert)"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Write state before checkpoint
exec_run "$SB" "bash" "-c" "echo before-checkpoint > /workspace/state.txt" >/dev/null
exec_run "$SB" "bash" "-c" "echo v1 > /workspace/version.txt" >/dev/null
pass "Pre-checkpoint state written"

# Create checkpoint
CP=$(api -X POST "$API_URL/api/sandboxes/$SB/checkpoints" -d '{"name":"restore-test"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP" ] && pass "Checkpoint: ${CP:0:12}..." || { fail "Create checkpoint"; summary; }
sleep 8

# Modify state AFTER checkpoint
exec_run "$SB" "bash" "-c" "echo after-checkpoint > /workspace/state.txt" >/dev/null
exec_run "$SB" "bash" "-c" "echo v2 > /workspace/version.txt" >/dev/null
exec_run "$SB" "bash" "-c" "echo new-file > /workspace/new.txt" >/dev/null
OUT=$(exec_stdout "$SB" "cat" "/workspace/state.txt")
[ "$OUT" = "after-checkpoint" ] && pass "State modified: after-checkpoint" || fail "Modify: $OUT"

# Restore
api -X POST "$API_URL/api/sandboxes/$SB/checkpoints/$CP/restore" >/dev/null
pass "Restore initiated"

# Wait for loadvm + agent reconnect
sleep 5

# Verify state reverted
OUT=$(exec_stdout "$SB" "cat" "/workspace/state.txt")
[ "$OUT" = "before-checkpoint" ] && pass "State reverted: before-checkpoint" || fail "State: '$OUT'"

OUT=$(exec_stdout "$SB" "cat" "/workspace/version.txt")
[ "$OUT" = "v1" ] && pass "Version reverted: v1" || fail "Version: '$OUT'"

OUT=$(exec_stdout "$SB" "bash" "-c" "cat /workspace/new.txt 2>&1 || echo gone")
echo "$OUT" | grep -qi "gone\|No such file" && pass "Post-checkpoint file gone" || fail "New file: '$OUT'"

# Sandbox still functional after restore
OUT=$(exec_stdout "$SB" "echo" "alive-after-restore")
[ "$OUT" = "alive-after-restore" ] && pass "Sandbox functional after restore" || fail "Post-restore: $OUT"

# Restore again (multiple restores)
h "Multiple Restores"
exec_run "$SB" "bash" "-c" "echo modified-again > /workspace/state.txt" >/dev/null
api -X POST "$API_URL/api/sandboxes/$SB/checkpoints/$CP/restore" >/dev/null
sleep 5
OUT=$(exec_stdout "$SB" "cat" "/workspace/state.txt")
[ "$OUT" = "before-checkpoint" ] && pass "Second restore: reverted again" || fail "Second restore: '$OUT'"

summary
