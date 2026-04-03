#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 3: Scale all sandboxes to 4GB ==="
SANDBOXES=$(api "$API_URL/api/sandboxes?limit=100" | python3 -c "import sys,json; [print(s['sandboxID']) for s in json.load(sys.stdin)]" 2>/dev/null)
COUNT=0
for sb in $SANDBOXES; do
    COUNT=$((COUNT+1))
    RESULT=$(TIMEOUT=60 api -X PUT "$API_URL/api/sandboxes/$sb/limits" -d '{"memoryMB":4096,"cpuPercent":100}')
    OK=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok','?'))" 2>/dev/null)
    echo "  $COUNT: $sb -> ok=$OK"
    sleep 1
done
echo ""
echo "Scaled $COUNT sandboxes"
workers
echo ""
echo "DONE: Memory should be rising on the active worker"
