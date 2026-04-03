#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/00-env.sh"

echo "=== Step 1: Clean slate ==="
echo "Destroying all sandboxes from API..."
SANDBOXES=$(api "$API_URL/api/sandboxes?limit=100" | python3 -c "import sys,json; [print(s['sandboxID']) for s in json.load(sys.stdin)]" 2>/dev/null || echo "")
for sb in $SANDBOXES; do
    api -X DELETE "$API_URL/api/sandboxes/$sb" &
done
wait

echo "Killing orphaned VMs on all workers..."
for VM in $(az vm list --resource-group opensandbox-prod --query "[].name" -o tsv 2>/dev/null | grep osb-worker); do
    az vm run-command invoke --resource-group opensandbox-prod --name "$VM" --command-id RunShellScript \
      --scripts "systemctl stop opensandbox-worker; pkill -9 qemu-system 2>/dev/null; rm -rf /data/sandboxes/sandboxes/* /data/sandboxes/golden; systemctl start opensandbox-worker; sleep 5; systemctl is-active opensandbox-worker" 2>&1 | tail -1
done

sleep 10
echo "Workers:"
workers
echo ""
echo "DONE: Verify workers with 0 sandboxes above"
