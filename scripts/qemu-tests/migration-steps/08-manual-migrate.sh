#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Manual Migration Test ==="

echo "Step 1: Create a sandbox"
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":0}')
SB=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null)
SRC=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null)
echo "  Sandbox: $SB on $SRC"

echo ""
echo "Step 2: Verify responsive"
OUT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["hello"],"timeout":5}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','FAIL').strip())" 2>/dev/null)
echo "  Response: $OUT"

echo ""
echo "Step 3: Write a marker file"
api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"sh","args":["-c","echo migration-test > /workspace/marker.txt"],"timeout":5}' >/dev/null
MARKER=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"cat","args":["/workspace/marker.txt"],"timeout":5}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','FAIL').strip())" 2>/dev/null)
echo "  Marker: $MARKER"

echo ""
echo "Step 4: Find target worker"
TARGET=$(api "$API_URL/api/workers" | python3 -c "
import sys,json
for w in json.load(sys.stdin):
    if w['worker_id'] != '$SRC':
        print(w['worker_id'])
        break
" 2>/dev/null)
echo "  Source: $SRC"
echo "  Target: $TARGET"

if [ -z "$TARGET" ] || [ "$TARGET" = "" ]; then
    echo "ERROR: No target worker available"
    exit 1
fi

echo ""
echo "Step 5: Migrate (timeout 180s)"
MIGRATE_RESULT=$(TIMEOUT=180 api -X POST "$API_URL/api/sandboxes/$SB/migrate" -d "{\"targetWorker\":\"$TARGET\"}")
echo "  Result: $MIGRATE_RESULT"

echo ""
echo "Step 6: Verify sandbox survived (waiting 5s for agent)"
sleep 5
OUT2=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["alive"],"timeout":5}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','FAIL').strip())" 2>/dev/null)
echo "  Response: $OUT2"

echo ""
echo "Step 7: Verify marker file survived"
MARKER2=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"cat","args":["/workspace/marker.txt"],"timeout":5}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','FAIL').strip())" 2>/dev/null)
echo "  Marker: $MARKER2"

echo ""
echo "Step 8: Check which worker it's on now"
NEW_WORKER=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null)
echo "  Before: $SRC"
echo "  After:  $NEW_WORKER"

echo ""
if [ "$OUT2" = "alive" ] && [ "$MARKER2" = "migration-test" ] && [ "$NEW_WORKER" = "$TARGET" ]; then
    echo "SUCCESS: Sandbox migrated and survived with data intact"
else
    echo "FAILED: something went wrong"
    echo "  alive=$OUT2 marker=$MARKER2 worker=$NEW_WORKER"
fi

echo ""
echo "Step 9: Cleanup"
api -X DELETE "$API_URL/api/sandboxes/$SB" >/dev/null 2>&1
echo "  Destroyed $SB"
