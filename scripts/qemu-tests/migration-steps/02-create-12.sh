#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 2: Create 12 sandboxes ==="
CREATED=0
for i in $(seq 1 12); do
    RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":0}')
    SB=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null)
    WK=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null)
    echo "  $i: $SB -> $WK"
    CREATED=$((CREATED+1))
    sleep 1
done
echo ""
echo "Created $CREATED sandboxes"
workers
echo ""
echo "DONE: Verify all 12 on one worker"
