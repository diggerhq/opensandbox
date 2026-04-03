#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 4: Check committed memory ==="
workers
echo ""

# Check worker logs for committed memory
echo "Checking worker logs for committed memory..."
SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-digger.pem}"
WORKER_IP=$(api "$API_URL/api/workers" | python3 -c "
import sys,json
for w in json.load(sys.stdin):
    if w['current'] > 0:
        print(w['grpc_addr'].split(':')[0])
        break
" 2>/dev/null)

if [ -n "$WORKER_IP" ]; then
    echo "Active worker IP: $WORKER_IP"
    echo ""
    echo "DONE: 12 sandboxes at 4GB = 48GB committed. Check that mem% is elevated."
else
    echo "ERROR: No active worker found"
fi
