#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 6: Check server + worker logs ==="
SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-digger.pem}"

echo "--- Server logs ---"
az vm run-command invoke --resource-group opensandbox-prod --name osb-controlplane --command-id RunShellScript \
  --scripts "journalctl -u opensandbox-server --no-pager --since '5 min ago' | grep -E 'scale-limits|scale-migrate|migration-prepare|health check' | tail -15" 2>&1 | python3 -c "import sys,json; [print(v.get('message','')) for v in json.load(sys.stdin).get('value',[])]" 2>&1

echo ""
echo "--- Target worker logs ---"
# Find the target worker from the server log
TARGET=$(az vm run-command invoke --resource-group opensandbox-prod --name osb-controlplane --command-id RunShellScript \
  --scripts "journalctl -u opensandbox-server --no-pager --since '5 min ago' | grep 'migrating to' | tail -1" 2>&1 | python3 -c "import sys,json; print([v.get('message','') for v in json.load(sys.stdin).get('value',[])][0])" 2>&1)
echo "  $TARGET"

echo ""
workers
echo ""
echo "DONE: Review logs above for the migration path"
