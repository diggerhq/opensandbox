#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 5: Scale one sandbox to 16GB ==="

# Pick a sandbox from the busiest worker (most committed memory)
BUSIEST=$(api "$API_URL/api/workers" | python3 -c "
import sys,json
ws=json.load(sys.stdin)
best=max(ws, key=lambda w: w['current'])
print(best['worker_id'])
" 2>/dev/null)
echo "Busiest worker: $BUSIEST"

SB=$(api "$API_URL/api/sandboxes?limit=100" | python3 -c "
import sys,json
for s in json.load(sys.stdin):
    if s.get('workerID','') == '$BUSIEST':
        print(s['sandboxID'])
        break
" 2>/dev/null)
WORKER=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null)

echo "Sandbox: $SB"
echo "Current worker: $WORKER"
echo ""

# Test responsive first
OUT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["alive"],"timeout":5}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
echo "Responsive: $OUT"
echo ""

echo "Scaling to 16GB / 4vCPU (timeout 180s)..."
RESULT=$(TIMEOUT=180 api -X PUT "$API_URL/api/sandboxes/$SB/limits" -d '{"memoryMB":16384,"cpuPercent":400}')
echo "Response: $RESULT"
echo ""

# Parse result
MIGRATED=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('migrated','?'))" 2>/dev/null || echo "?")
NEW_WORKER=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null || echo "?")
ERROR=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error','none'))" 2>/dev/null || echo "?")

echo "Migrated: $MIGRATED"
echo "New worker: $NEW_WORKER"
echo "Error: $ERROR"
echo ""

# Check memory
MEM=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"free","args":["-m"],"timeout":5}' | python3 -c "import sys,json; [print(l) for l in json.load(sys.stdin).get('stdout','').split('\n') if 'Mem' in l]" 2>/dev/null)
echo "Memory: $MEM"
echo ""

workers
echo ""
echo "DONE: Check if migration happened or error above"
