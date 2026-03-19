#!/usr/bin/env bash
# 15-snapshots-crud.sh — Snapshot list, get, delete, checkpoint delete
set +u
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "Snapshot CRUD"

# Create a snapshot
SNAP_NAME="crud-test-$$"
SNAP_RESULT=$(api -X POST "$API_URL/api/snapshots" -d "{
  \"name\":\"$SNAP_NAME\",
  \"image\":{\"base\":\"base\",\"steps\":[{\"type\":\"run\",\"args\":{\"commands\":[\"echo snap > /workspace/s.txt\"]}}]}
}")
SNAP_STATUS=$(echo "$SNAP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
[ "$SNAP_STATUS" = "ready" ] && pass "Create snapshot: $SNAP_NAME" || fail "Create: $SNAP_RESULT"

# List snapshots
LIST=$(api "$API_URL/api/snapshots")
echo "$LIST" | grep -q "$SNAP_NAME" && pass "List snapshots: found $SNAP_NAME" || fail "List: $LIST"

# Get snapshot by name
GET=$(api "$API_URL/api/snapshots/$SNAP_NAME")
echo "$GET" | grep -q "$SNAP_NAME" && pass "Get snapshot by name" || fail "Get: $GET"
CP_ID=$(echo "$GET" | python3 -c "import sys,json; print(json.load(sys.stdin).get('checkpointId',''))" 2>/dev/null)

# Use snapshot
SB=$(api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":300,\"snapshot\":\"$SNAP_NAME\"}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
[ -n "$SB" ] && SANDBOXES+=("$SB") && pass "Create from snapshot: $SB" || fail "Create from snapshot"
sleep 8
OUT=$(exec_stdout "$SB" "cat" "/workspace/s.txt")
[ "$OUT" = "snap" ] && pass "Snapshot content verified" || fail "Content: $OUT"

# Delete snapshot
api -X DELETE "$API_URL/api/snapshots/$SNAP_NAME" >/dev/null
LIST_AFTER=$(api "$API_URL/api/snapshots")
echo "$LIST_AFTER" | grep -q "$SNAP_NAME" && fail "Delete snapshot: still present" || pass "Delete snapshot: removed"

# Verify creating from deleted snapshot fails
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":300,\"snapshot\":\"$SNAP_NAME\"}")
echo "$RESULT" | grep -qi 'error\|not found' && pass "Deleted snapshot: create fails" || fail "Deleted snapshot still works: $RESULT"

h "Checkpoint Delete"

SB2=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":300}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -z "$SB2" ] || [ "$SB2" = "None" ]; then
    skip "Could not create sandbox for checkpoint test"
    summary
fi
SANDBOXES+=("$SB2")

# Create checkpoint
CP=$(api -X POST "$API_URL/api/sandboxes/$SB2/checkpoints" -d '{"name":"del-test"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP" ] && pass "Create checkpoint: ${CP:0:12}..." || { fail "Create checkpoint"; summary; }
sleep 8

# List checkpoints
LIST=$(api "$API_URL/api/sandboxes/$SB2/checkpoints")
echo "$LIST" | grep -q "$CP" && pass "List checkpoints: found" || fail "List: $LIST"

# Delete checkpoint
api -X DELETE "$API_URL/api/sandboxes/$SB2/checkpoints/$CP" >/dev/null
LIST_AFTER=$(api "$API_URL/api/sandboxes/$SB2/checkpoints")
echo "$LIST_AFTER" | grep -q "$CP" && fail "Checkpoint delete: still present" || pass "Checkpoint deleted"

summary
