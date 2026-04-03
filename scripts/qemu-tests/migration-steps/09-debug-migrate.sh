#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Debug Migration Test ==="

echo "Step 1: Create sandbox"
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":0}')
SB=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null)
SRC=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null)
echo "  $SB on $SRC"

echo ""
echo "Step 2: Exec before migration"
EXEC_BEFORE=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["before-migrate"],"timeout":5}')
echo "  Raw response: $EXEC_BEFORE"

echo ""
echo "Step 3: Write marker"
api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"sh","args":["-c","echo test123 > /workspace/marker.txt"],"timeout":5}' >/dev/null

echo ""
echo "Step 4: Find target"
TARGET=$(api "$API_URL/api/workers" | python3 -c "
import sys,json
for w in json.load(sys.stdin):
    if w['worker_id'] != '$SRC':
        print(w['worker_id'])
        break
" 2>/dev/null)
echo "  Source: $SRC"
echo "  Target: $TARGET"

echo ""
echo "Step 5: Check DB status BEFORE migrate"
DB_BEFORE=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'status={d.get(\"status\",\"?\")} worker={d.get(\"workerID\",\"?\")}')" 2>/dev/null)
echo "  $DB_BEFORE"

echo ""
echo "Step 6: Migrate"
MIGRATE_RESULT=$(TIMEOUT=180 api -X POST "$API_URL/api/sandboxes/$SB/migrate" -d "{\"targetWorker\":\"$TARGET\"}")
echo "  Result: $MIGRATE_RESULT"

echo ""
echo "Step 7: Check DB status AFTER migrate"
DB_AFTER=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'status={d.get(\"status\",\"?\")} worker={d.get(\"workerID\",\"?\")}')" 2>/dev/null)
echo "  $DB_AFTER"

echo ""
echo "Step 8: Exec IMMEDIATELY after migration (via control plane)"
EXEC_AFTER=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["after-migrate"],"timeout":5}')
echo "  Raw response: $EXEC_AFTER"

echo ""
echo "Step 9: Wait 3s then exec again"
sleep 3
EXEC_WAIT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["after-wait"],"timeout":5}')
echo "  Raw response: $EXEC_WAIT"

echo ""
echo "Step 10: Try exec directly on TARGET worker HTTP (bypass CP)"
TARGET_HTTP=$(api "$API_URL/api/workers" | python3 -c "
import sys,json
for w in json.load(sys.stdin):
    if w['worker_id'] == '$TARGET':
        print(w['http_addr'])
        break
" 2>/dev/null)
echo "  Target HTTP: $TARGET_HTTP"
if [ -n "$TARGET_HTTP" ]; then
    # Get a token for the sandbox
    TOKEN=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
    DIRECT_EXEC=$(curl -s --max-time 10 -X POST "$TARGET_HTTP/sandboxes/$SB/exec/run" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -d '{"cmd":"echo","args":["direct-exec"],"timeout":5}')
    echo "  Direct response: $DIRECT_EXEC"
fi

echo ""
echo "Step 11: Check marker file"
MARKER=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"cat","args":["/workspace/marker.txt"],"timeout":5}')
echo "  Marker response: $MARKER"

echo ""
echo "Step 12: Workers state"
workers

echo ""
echo "=== NOT cleaning up — sandbox $SB left alive for inspection ==="
