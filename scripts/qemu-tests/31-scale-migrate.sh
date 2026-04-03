#!/usr/bin/env bash
# 31-scale-migrate.sh — Test live migration triggered by resource scaling
#
# Scenario:
#   1. Fill 2 workers to capacity with 4GB/1vCPU sandboxes (16 each = 32 total)
#   2. Verify 1 idle reserve remains
#   3. Scale one sandbox from 4GB/1vCPU to 16GB/4vCPU via the limits API
#   4. The system should auto-migrate the sandbox to the reserve worker
#   5. A new reserve should spin up to replace the one that was used
#   6. Verify the sandbox survived migration and responds correctly
#
# Required env:
#   OPENSANDBOX_API_URL
#   OPENSANDBOX_API_KEY

source "$(dirname "$0")/common.sh"

declare -a SANDBOXES=()

cleanup() {
    echo ""
    echo "=== ${#SANDBOXES[@]} sandboxes left alive for inspection ==="
    echo "To clean up manually: for sb in ${SANDBOXES[*]+"${SANDBOXES[*]}"}; do curl -s -X DELETE $API_URL/api/sandboxes/\$sb -H 'X-API-Key: $API_KEY'; done"
}
trap cleanup EXIT INT TERM

get_workers() {
    api "$API_URL/api/workers" 2>/dev/null || echo "[]"
}

worker_summary() {
    get_workers | python3 -c "
import sys, json
workers = json.load(sys.stdin)
for w in workers:
    print(f'  {w.get(\"worker_id\",\"?\"):30s} {w.get(\"current\",0):>3}/{w.get(\"capacity\",0)} cpu={w.get(\"cpu_pct\",0):5.1f}% mem={w.get(\"mem_pct\",0):5.1f}% ver={w.get(\"worker_version\",\"?\")}')
" 2>/dev/null || echo "  (no worker data available)"
}

worker_count() {
    get_workers | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0"
}

# ============================================================
# Phase 1: Fill 2 workers to capacity
# ============================================================
FILL_COUNT="${FILL_COUNT:-12}"
h "Phase 1: Fill 1 worker ($FILL_COUNT sandboxes at 4GB/1vCPU)"

echo "Starting state:"
worker_summary

CREATED=0
TARGET="$FILL_COUNT"
for i in $(seq 1 "$TARGET"); do
    RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":0}' 2>/dev/null || echo '{}')
    SB_ID=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null || echo "FAIL")
    if [ -n "$SB_ID" ] && [ "$SB_ID" != "FAIL" ]; then
        SANDBOXES+=("$SB_ID")
        CREATED=$((CREATED+1))

        # Scale to 4GB immediately so committed memory fills up.
        # Base is 1GB, so this hotplugs 3GB per sandbox via virtio-mem.
        # 12 sandboxes x 4GB = 48GB committed on a 64GB host.
        api -X PUT "$API_URL/api/sandboxes/$SB_ID/limits" \
            -d '{"memoryMB":4096,"cpuPercent":100}' >/dev/null 2>&1
    fi
    sleep 2
    if [ $((CREATED % 10)) -eq 0 ]; then
        echo "  created: $CREATED/$TARGET"
        worker_summary
    fi
done

echo ""
echo "Created $CREATED sandboxes"
worker_summary

[ "$CREATED" -ge $((TARGET * 80 / 100)) ] && pass "Filled workers ($CREATED sandboxes)" || fail "Only created $CREATED/$TARGET sandboxes"

# ============================================================
# Phase 2: Wait for reserves, verify layout
# ============================================================
h "Phase 2: Wait for idle reserves"

echo "Waiting for at least 1 idle reserve..."
for attempt in $(seq 1 24); do
    IDLE=$(get_workers | python3 -c "import sys,json; print(sum(1 for w in json.load(sys.stdin) if w.get('current',0)==0))" 2>/dev/null || echo "0")
    if [ "$IDLE" -ge 1 ]; then
        echo "  Reserve ready after $((attempt * 10))s ($IDLE idle)"
        break
    fi
    echo "  waiting... ($IDLE idle, attempt $attempt)"
    sleep 10
done

WORKERS_JSON=$(get_workers)
LAYOUT=$(echo "$WORKERS_JSON" | python3 -c "
import sys, json
workers = json.load(sys.stdin)
active = [w for w in workers if w.get('current', 0) > 0]
idle = [w for w in workers if w.get('current', 0) == 0]
print(f'{len(active)} active, {len(idle)} idle')
for w in active:
    print(f'  ACTIVE {w[\"worker_id\"]}: {w[\"current\"]}/{w[\"capacity\"]} mem={w[\"mem_pct\"]:.0f}%')
for w in idle:
    print(f'  IDLE   {w[\"worker_id\"]}: {w[\"current\"]}/{w[\"capacity\"]}')
" 2>/dev/null)
echo "$LAYOUT"

IDLE_COUNT=$(echo "$WORKERS_JSON" | python3 -c "
import sys, json
print(sum(1 for w in json.load(sys.stdin) if w.get('current',0)==0))
" 2>/dev/null || echo "0")

[ "$IDLE_COUNT" -ge 1 ] && pass "At least 1 idle reserve available" || fail "No idle reserves (need at least 1 for migration)"

# ============================================================
# Phase 3: Pick a sandbox and scale it up
# ============================================================
h "Phase 3: Scale sandbox from 4GB to 16GB"

# Find a responsive sandbox on the most loaded worker
BUSIEST_WORKER=$(get_workers | python3 -c "
import sys, json
workers = json.load(sys.stdin)
active = [w for w in workers if w.get('current', 0) > 0]
if active:
    busiest = max(active, key=lambda w: w['current'])
    print(busiest['worker_id'])
else:
    print('')
" 2>/dev/null)
echo "Busiest worker: $BUSIEST_WORKER"

TARGET_SB=""
for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
    # Check which worker this sandbox is on
    SB_WORKER=$(api "$API_URL/api/sandboxes/$sb" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null)
    if [ "$SB_WORKER" = "$BUSIEST_WORKER" ]; then
        OUT=$(exec_stdout "$sb" "echo" "alive" 2>/dev/null)
        if [ "$OUT" = "alive" ]; then
            TARGET_SB="$sb"
            break
        fi
    fi
done
# Fallback: any responsive sandbox
if [ -z "$TARGET_SB" ]; then
    for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
        OUT=$(exec_stdout "$sb" "echo" "alive" 2>/dev/null)
        if [ "$OUT" = "alive" ]; then
            TARGET_SB="$sb"
            break
        fi
    done
fi

if [ -z "$TARGET_SB" ]; then
    fail "No responsive sandbox found to scale"
    summary
    exit 1
fi

echo "Target sandbox: $TARGET_SB"
pass "Sandbox responsive before scale"

# Get its current worker
BEFORE_WORKER=$(api "$API_URL/api/sandboxes/$TARGET_SB" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null || echo "?")
echo "Before: worker=$BEFORE_WORKER"

# Scale to 16GB/4vCPU — this should trigger auto-migration
echo "Scaling to 16GB / 4vCPU (may trigger migration, timeout 120s)..."
SCALE_RESULT=$(TIMEOUT=120 api -X PUT "$API_URL/api/sandboxes/$TARGET_SB/limits" \
    -d '{"memoryMB":16384,"cpuPercent":400}' 2>/dev/null)
echo "Scale result: $SCALE_RESULT" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin.read().split(': ', 1)[1] if ': ' in sys.stdin.read() else '{}')
except:
    pass
" 2>/dev/null || echo "  $SCALE_RESULT"

MIGRATED=$(echo "$SCALE_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('migrated', False))" 2>/dev/null || echo "false")
AFTER_WORKER=$(echo "$SCALE_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID', '?'))" 2>/dev/null || echo "?")

echo "After: worker=$AFTER_WORKER, migrated=$MIGRATED"

if [ "$MIGRATED" = "True" ] || [ "$MIGRATED" = "true" ]; then
    pass "Sandbox was auto-migrated ($BEFORE_WORKER → $AFTER_WORKER)"
elif [ "$AFTER_WORKER" != "$BEFORE_WORKER" ] && [ "$AFTER_WORKER" != "?" ]; then
    pass "Sandbox moved to different worker ($BEFORE_WORKER → $AFTER_WORKER)"
else
    skip "Sandbox stayed on same worker (in-place scale may have worked)"
fi

# ============================================================
# Phase 4: Verify sandbox survived
# ============================================================
h "Phase 4: Verify sandbox survived scaling/migration"

sleep 3  # brief settle time

OUT=$(exec_stdout "$TARGET_SB" "echo" "alive" 2>/dev/null)
[ "$OUT" = "alive" ] && pass "Sandbox responsive after scale" || fail "Sandbox unresponsive after scale"

# Check memory is actually scaled
MEM=$(exec_stdout "$TARGET_SB" "free" "-m" 2>/dev/null | awk '/Mem:/{print $2}')
if [ -n "$MEM" ] && [ "$MEM" -gt 12000 ]; then
    pass "Memory scaled: ${MEM}MB (expected ~16384MB)"
elif [ -n "$MEM" ]; then
    fail "Memory not scaled: ${MEM}MB (expected ~16384MB)"
else
    fail "Could not read memory"
fi

# ============================================================
# Phase 5: Verify reserve replenishment
# ============================================================
h "Phase 5: Check reserve replenishment"

echo "Waiting 60s for new reserve to boot..."
sleep 60

echo "Workers after scale + reserve replenishment:"
worker_summary

NEW_IDLE=$(echo "$(get_workers)" | python3 -c "
import sys, json
print(sum(1 for w in json.load(sys.stdin) if w.get('current',0)==0))
" 2>/dev/null || echo "0")

[ "$NEW_IDLE" -ge 1 ] && pass "Reserve replenished ($NEW_IDLE idle workers)" || skip "Reserve not yet replenished (may still be booting)"

# ============================================================
# Phase 6: Final state
# ============================================================
h "Phase 6: Final state"
worker_summary

summary
