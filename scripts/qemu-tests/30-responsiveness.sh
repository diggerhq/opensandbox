#!/usr/bin/env bash
# 30-responsiveness.sh — Scale up sandboxes and test every single one responds
#
# Creates sandboxes one at a time, tests each immediately after creation,
# then tests all of them again at the end before cleaning up.
#
# Required env:
#   OPENSANDBOX_API_URL
#   OPENSANDBOX_API_KEY
#
# Optional env:
#   SANDBOX_COUNT   — total sandboxes to create (default: 120)
#   BATCH_SIZE      — sandboxes per batch (default: 1)
#   BATCH_DELAY     — seconds between batches (default: 5)

source "$(dirname "$0")/common.sh"

SANDBOX_COUNT="${SANDBOX_COUNT:-120}"
BATCH_SIZE="${BATCH_SIZE:-1}"
BATCH_DELAY="${BATCH_DELAY:-5}"
declare -a SANDBOXES=()
declare -a WORKERS_SEEN=()

cleanup() {
    if [ ${#SANDBOXES[@]} -gt 0 ]; then
        h "Cleanup: destroying ${#SANDBOXES[@]} sandboxes"
        for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
            destroy_sandbox "$sb" &
        done
        wait 2>/dev/null || true
        echo "Cleanup complete"
    fi
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
" 2>/dev/null || echo "  (no worker data)"
}

# ============================================================
# Phase 1: Baseline
# ============================================================
h "Phase 1: Baseline"
worker_summary

# ============================================================
# Phase 2: Create sandboxes and test each one immediately
# ============================================================
h "Phase 2: Create $SANDBOX_COUNT sandboxes (batch=$BATCH_SIZE, delay=${BATCH_DELAY}s)"
echo "  Testing each sandbox immediately after creation"
echo ""

CREATE_OK=0
CREATE_FAIL=0
IMMEDIATE_OK=0
IMMEDIATE_FAIL=0
IMMEDIATE_FAILS=""

for batch_start in $(seq 1 "$BATCH_SIZE" "$SANDBOX_COUNT"); do
    batch_end=$((batch_start + BATCH_SIZE - 1))
    [ "$batch_end" -gt "$SANDBOX_COUNT" ] && batch_end=$SANDBOX_COUNT

    for i in $(seq "$batch_start" "$batch_end"); do
        # Create
        RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":0}' 2>/dev/null || echo '{}')
        SB_ID=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null || echo "FAIL")
        WORKER=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','?'))" 2>/dev/null || echo "?")

        if [ -n "$SB_ID" ] && [ "$SB_ID" != "FAIL" ]; then
            SANDBOXES+=("$SB_ID")
            CREATE_OK=$((CREATE_OK + 1))

            # Test immediately
            OUT=$(exec_stdout "$SB_ID" "echo" "alive" 2>/dev/null)
            if [ "$OUT" = "alive" ]; then
                IMMEDIATE_OK=$((IMMEDIATE_OK + 1))
            else
                IMMEDIATE_FAIL=$((IMMEDIATE_FAIL + 1))
                IMMEDIATE_FAILS="$IMMEDIATE_FAILS $SB_ID($WORKER)"
            fi
        else
            CREATE_FAIL=$((CREATE_FAIL + 1))
            # Show the error
            ERR=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error','unknown'))" 2>/dev/null || echo "unknown")
            echo "  FAIL #$i: $ERR"
        fi
    done

    # Progress every 10
    if [ $((CREATE_OK + CREATE_FAIL)) -ge 10 ] && [ $(( (CREATE_OK + CREATE_FAIL) % 10 )) -eq 0 ]; then
        echo "  progress: created=$CREATE_OK failed=$CREATE_FAIL responsive=$IMMEDIATE_OK unresponsive=$IMMEDIATE_FAIL"
    fi

    sleep "$BATCH_DELAY"
done

echo ""
echo "Creation: $CREATE_OK succeeded, $CREATE_FAIL failed"
echo "Immediate responsiveness: $IMMEDIATE_OK/$CREATE_OK OK"
if [ -n "$IMMEDIATE_FAILS" ]; then
    echo "  Failed sandboxes:$IMMEDIATE_FAILS"
fi

[ "$CREATE_OK" -ge $((SANDBOX_COUNT * 80 / 100)) ] && pass "Created >= 80% ($CREATE_OK/$SANDBOX_COUNT)" || fail "Only $CREATE_OK/$SANDBOX_COUNT created"
[ "$IMMEDIATE_OK" -ge $((CREATE_OK * 90 / 100)) ] && pass "Immediate responsiveness >= 90% ($IMMEDIATE_OK/$CREATE_OK)" || fail "Immediate responsiveness: $IMMEDIATE_OK/$CREATE_OK"

# ============================================================
# Phase 3: Worker state
# ============================================================
h "Phase 3: Worker state after ramp"
worker_summary

# ============================================================
# Phase 4: Test ALL sandboxes again
# ============================================================
h "Phase 4: Test all ${#SANDBOXES[@]} sandboxes"

RECHECK_OK=0
RECHECK_FAIL=0
RECHECK_FAILS=""

for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
    OUT=$(exec_stdout "$sb" "echo" "alive" 2>/dev/null)
    if [ "$OUT" = "alive" ]; then
        RECHECK_OK=$((RECHECK_OK + 1))
    else
        RECHECK_FAIL=$((RECHECK_FAIL + 1))
        RECHECK_FAILS="$RECHECK_FAILS $sb"
    fi
done

echo "Full recheck: $RECHECK_OK/${#SANDBOXES[@]} responsive"
if [ -n "$RECHECK_FAILS" ]; then
    echo "  Failed:$RECHECK_FAILS"
fi
[ "$RECHECK_OK" -ge $(( ${#SANDBOXES[@]} * 90 / 100 )) ] && pass "Full recheck >= 90% ($RECHECK_OK/${#SANDBOXES[@]})" || fail "Full recheck: $RECHECK_OK/${#SANDBOXES[@]}"

# ============================================================
# Phase 5: Scale down
# ============================================================
h "Phase 5: Scale down"
echo "Destroying all sandboxes..."
for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
    destroy_sandbox "$sb" &
done
wait 2>/dev/null || true
SANDBOXES=()
echo "All destroyed. Waiting 120s for autoscaler to drain..."
sleep 120
echo "Workers after drain:"
worker_summary

summary
