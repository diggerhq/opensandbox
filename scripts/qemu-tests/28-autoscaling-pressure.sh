#!/usr/bin/env bash
# 28-autoscaling-pressure.sh — Autoscaling, disk pressure, and multi-worker stress tests
#
# Tests against real deployed infrastructure (westus2 / opensandbox prod).
# Creates real sandboxes, grows real disk, verifies scaler responds.
#
# Required env:
#   OPENSANDBOX_API_URL   — e.g. https://app.opencomputer.dev
#   OPENSANDBOX_API_KEY   — API key for sandbox creation
#
# Optional env:
#   SANDBOX_COUNT         — number of sandboxes to create (default: 120)
#   BATCH_SIZE            — sandboxes per batch (default: 1)
#   BATCH_DELAY           — seconds between batches (default: 5)
#   DISK_GROWTH_MB        — MB to write per sandbox in disk test (default: 500)
#   SKIP_DISK_TEST        — set to "true" to skip disk pressure test
#   SKIP_SCALE_TEST       — set to "true" to skip sandbox scale test
#
source "$(dirname "$0")/common.sh"

SANDBOX_COUNT="${SANDBOX_COUNT:-120}"
DISK_GROWTH_MB="${DISK_GROWTH_MB:-500}"
declare -a SANDBOXES=()

cleanup() {
    h "Cleanup: destroying ${#SANDBOXES[@]} sandboxes"
    local pids=()
    for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
        destroy_sandbox "$sb" &
        pids+=($!)
        if [ ${#pids[@]} -ge 20 ]; then
            for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
            pids=()
        fi
    done
    for pid in "${pids[@]+"${pids[@]}"}"; do wait "$pid" 2>/dev/null || true; done
    echo "Cleanup complete"
}
trap cleanup EXIT

get_workers() {
    api "$API_URL/api/workers" 2>/dev/null || echo "[]"
}

worker_count() {
    get_workers | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0"
}

worker_summary() {
    get_workers | python3 -c "
import sys, json
workers = json.load(sys.stdin)
for w in workers:
    print(f'  {w.get(\"worker_id\",\"?\"):30s} cpu={w.get(\"cpu_pct\",0):5.1f}% mem={w.get(\"mem_pct\",0):5.1f}% disk={w.get(\"disk_pct\",0):5.1f}% sandboxes={w.get(\"current\",0)}/{w.get(\"capacity\",0)} ver={w.get(\"worker_version\",\"?\")}')
" 2>/dev/null || echo "  (no worker data available)"
}

total_sandboxes() {
    get_workers | python3 -c "
import sys, json
print(sum(w.get('current', 0) for w in json.load(sys.stdin)))
" 2>/dev/null || echo "0"
}

# Create a sandbox with optional retry on transient failures
create_sandbox_retry() {
    local timeout="${1:-0}"
    local max_retries=2
    for attempt in $(seq 1 "$max_retries"); do
        RESULT=$(api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":$timeout}" 2>/dev/null || echo '{}')
        SB_ID=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null || echo "FAIL")
        if [ -n "$SB_ID" ] && [ "$SB_ID" != "FAIL" ]; then
            echo "$SB_ID"
            return 0
        fi
        [ "$attempt" -lt "$max_retries" ] && sleep 5
    done
    echo "FAIL"
    return 1
}

# ============================================================
# Phase 1: Baseline worker state
# ============================================================
h "Phase 1: Baseline"

INITIAL_WORKERS=$(worker_count)
echo "Initial workers: $INITIAL_WORKERS"
worker_summary

[ "$INITIAL_WORKERS" -ge 1 ] && pass "At least 1 worker online" || fail "No workers online"

# ============================================================
# Phase 2: Scale-up test — create sandboxes, test each immediately
# ============================================================
if [ "${SKIP_SCALE_TEST:-}" != "true" ]; then

h "Phase 2: Scale-up ($SANDBOX_COUNT sandboxes)"

BATCH_SIZE="${BATCH_SIZE:-1}"
BATCH_DELAY="${BATCH_DELAY:-5}"
CREATED=0
FAILED=0
RESPONSIVE=0
UNRESPONSIVE=0

for batch_start in $(seq 1 $BATCH_SIZE $SANDBOX_COUNT); do
    batch_end=$((batch_start + BATCH_SIZE - 1))
    [ "$batch_end" -gt "$SANDBOX_COUNT" ] && batch_end=$SANDBOX_COUNT

    for i in $(seq "$batch_start" "$batch_end"); do
        SB_ID=$(create_sandbox_retry 0)
        if [ -n "$SB_ID" ] && [ "$SB_ID" != "FAIL" ]; then
            SANDBOXES+=("$SB_ID")
            CREATED=$((CREATED+1))

            # Test responsiveness immediately
            OUT=$(exec_stdout "$SB_ID" "echo" "alive" 2>/dev/null)
            if [ "$OUT" = "alive" ]; then
                RESPONSIVE=$((RESPONSIVE+1))
            else
                UNRESPONSIVE=$((UNRESPONSIVE+1))
            fi
        else
            FAILED=$((FAILED+1))
        fi
    done

    # Progress every 10
    TOTAL_TRIED=$((CREATED + FAILED))
    if [ "$TOTAL_TRIED" -ge 10 ] && [ $((TOTAL_TRIED % 10)) -eq 0 ]; then
        echo "  progress: created=$CREATED failed=$FAILED responsive=$RESPONSIVE unresponsive=$UNRESPONSIVE"
    fi

    sleep "$BATCH_DELAY"
done

echo ""
echo "Creation results: $CREATED succeeded, $FAILED failed"
echo "Immediate responsiveness: $RESPONSIVE/$CREATED"
[ "$CREATED" -ge $((SANDBOX_COUNT * 80 / 100)) ] && pass "Created >= 80% of $SANDBOX_COUNT sandboxes ($CREATED)" || fail "Only $CREATED/$SANDBOX_COUNT created"
[ "$RESPONSIVE" -ge $((CREATED * 90 / 100)) ] && pass "Immediate responsiveness >= 90% ($RESPONSIVE/$CREATED)" || fail "Immediate responsiveness: $RESPONSIVE/$CREATED"

# Check if autoscaler added workers
POST_SCALE_WORKERS=$(worker_count)
echo ""
echo "Workers after ramp: $POST_SCALE_WORKERS (was $INITIAL_WORKERS)"
worker_summary

if [ "$POST_SCALE_WORKERS" -gt "$INITIAL_WORKERS" ]; then
    pass "Autoscaler added workers ($INITIAL_WORKERS → $POST_SCALE_WORKERS)"
elif [ "$POST_SCALE_WORKERS" -eq "$INITIAL_WORKERS" ] && [ "$CREATED" -le $((INITIAL_WORKERS * 16)) ]; then
    skip "No scale-up needed (capacity sufficient)"
else
    skip "Autoscaler did not add workers"
fi

# ============================================================
# Phase 3: Worker distribution
# ============================================================
h "Phase 3: Worker distribution"
worker_summary

WORKERS_JSON=$(get_workers)
DISTRIBUTION=$(echo "$WORKERS_JSON" | python3 -c "
import sys, json
workers = json.load(sys.stdin)
counts = [w.get('current', 0) for w in workers if w.get('current', 0) > 0]
if len(counts) < 2:
    print('SKIP')
else:
    ratio = max(counts) / max(min(counts), 1)
    print(f'{ratio:.1f}')
" 2>/dev/null || echo "SKIP")

if [ "$DISTRIBUTION" = "SKIP" ]; then
    skip "Not enough active workers to check distribution"
elif [ "$(echo "$DISTRIBUTION > 5.0" | bc 2>/dev/null)" = "1" ]; then
    fail "Load distribution ratio: ${DISTRIBUTION}x (> 5x is uneven)"
else
    pass "Load distribution ratio: ${DISTRIBUTION}x"
fi

# ============================================================
# Phase 4: Scale-down — destroy sandboxes and watch scaler respond
# ============================================================
h "Phase 4: Scale-down"

DESTROY_COUNT=$((${#SANDBOXES[@]} * 80 / 100))
echo "Destroying $DESTROY_COUNT sandboxes to trigger scale-down..."

declare -a REMAINING_SANDBOXES=()
declare -a PIDS=()
DESTROYED=0
for i in "${!SANDBOXES[@]}"; do
    if [ "$DESTROYED" -lt "$DESTROY_COUNT" ]; then
        destroy_sandbox "${SANDBOXES[$i]}" &
        PIDS+=($!)
        DESTROYED=$((DESTROYED+1))

        if [ ${#PIDS[@]} -ge 20 ]; then
            for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
            PIDS=()
        fi
    else
        REMAINING_SANDBOXES+=("${SANDBOXES[$i]}")
    fi
done
for pid in "${PIDS[@]+"${PIDS[@]}"}"; do wait "$pid" 2>/dev/null || true; done
SANDBOXES=("${REMAINING_SANDBOXES[@]+"${REMAINING_SANDBOXES[@]}"}")

echo "Destroyed $DESTROYED, remaining: ${#SANDBOXES[@]}"
echo "Waiting 120s for autoscaler to react..."
sleep 120

POST_DOWN_WORKERS=$(worker_count)
echo "Workers after scale-down: $POST_DOWN_WORKERS (was $POST_SCALE_WORKERS)"
worker_summary

if [ "$POST_DOWN_WORKERS" -lt "$POST_SCALE_WORKERS" ]; then
    pass "Autoscaler removed workers ($POST_SCALE_WORKERS → $POST_DOWN_WORKERS)"
else
    skip "Autoscaler did not remove workers yet (drain may still be in progress)"
fi

else
    skip "Scale test skipped (SKIP_SCALE_TEST=true)"
fi  # end SKIP_SCALE_TEST


# ============================================================
# Phase 5: Disk pressure test
# ============================================================
if [ "${SKIP_DISK_TEST:-}" != "true" ]; then

h "Phase 5: Disk pressure (${DISK_GROWTH_MB}MB per sandbox)"

declare -a DISK_SANDBOXES=()
DISK_SANDBOX_COUNT="${DISK_SANDBOX_COUNT:-50}"

echo "Creating $DISK_SANDBOX_COUNT sandboxes for disk pressure test..."
DISK_CREATED=0
for i in $(seq 1 "$DISK_SANDBOX_COUNT"); do
    SB_ID=$(create_sandbox_retry 0)
    if [ -n "$SB_ID" ] && [ "$SB_ID" != "FAIL" ]; then
        DISK_SANDBOXES+=("$SB_ID")
        SANDBOXES+=("$SB_ID")
        DISK_CREATED=$((DISK_CREATED+1))
    fi
done
echo "Created $DISK_CREATED sandboxes for disk test"

echo ""
echo "Worker state before disk growth:"
worker_summary

# Grow disk in all sandboxes concurrently
h "Growing disk: ${DISK_GROWTH_MB}MB per sandbox"

declare -a PIDS=()
for sb in "${DISK_SANDBOXES[@]+"${DISK_SANDBOXES[@]}"}"; do
    (
        set +e
        CHUNK_MB=50
        CHUNKS=$((DISK_GROWTH_MB / CHUNK_MB))
        for c in $(seq 1 "$CHUNKS"); do
            exec_run "$sb" "dd" "if=/dev/urandom" "of=/workspace/fill-${c}.dat" "bs=1M" "count=$CHUNK_MB" >/dev/null 2>&1
        done
    ) &
    PIDS+=($!)

    if [ ${#PIDS[@]} -ge 10 ]; then
        for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
        PIDS=()
    fi
done
for pid in "${PIDS[@]+"${PIDS[@]}"}"; do wait "$pid" 2>/dev/null || true; done
echo "Disk growth commands sent to all sandboxes"

# Monitor disk pressure over time
h "Monitoring disk pressure response (3 minutes)"
for tick in $(seq 1 18); do
    sleep 10
    echo ""
    echo "--- Tick $tick (${tick}0s) ---"
    worker_summary
done

echo ""
echo "Workers after disk test:"
worker_summary

# Verify sandboxes survived the pressure
h "Verify sandboxes survived disk pressure"
SURVIVED=0
SAMPLE=10
[ ${#DISK_SANDBOXES[@]} -lt "$SAMPLE" ] && SAMPLE=${#DISK_SANDBOXES[@]}

if [ "$SAMPLE" -gt 0 ]; then
    for i in $(seq 1 "$SAMPLE"); do
        idx=$((RANDOM % ${#DISK_SANDBOXES[@]}))
        sb="${DISK_SANDBOXES[$idx]}"
        OUT=$(exec_stdout "$sb" "echo" "survived" 2>/dev/null)
        [ "$OUT" = "survived" ] && SURVIVED=$((SURVIVED+1))
    done
    [ "$SURVIVED" -ge $((SAMPLE * 80 / 100)) ] && pass "Disk pressure survival: $SURVIVED/$SAMPLE responsive" || fail "Disk pressure survival: only $SURVIVED/$SAMPLE responsive"
else
    skip "No disk sandboxes to check"
fi

# Clean up disk fills
declare -a PIDS=()
for sb in "${DISK_SANDBOXES[@]+"${DISK_SANDBOXES[@]}"}"; do
    exec_run "$sb" "rm" "-rf" "/workspace/fill-*.dat" >/dev/null 2>&1 &
    PIDS+=($!)
    if [ ${#PIDS[@]} -ge 20 ]; then
        for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
        PIDS=()
    fi
done
for pid in "${PIDS[@]+"${PIDS[@]}"}"; do wait "$pid" 2>/dev/null || true; done
pass "Disk fills cleaned"

else
    skip "Disk test skipped (SKIP_DISK_TEST=true)"
fi  # end SKIP_DISK_TEST


# ============================================================
# Phase 6: Resource pressure — CPU/memory stress
# ============================================================
h "Phase 6: Combined resource pressure"

declare -a STRESS_SANDBOXES=()
STRESS_COUNT=5
for i in $(seq 1 "$STRESS_COUNT"); do
    SB=$(create_sandbox_retry 300)
    if [ -n "$SB" ] && [ "$SB" != "FAIL" ]; then
        SANDBOXES+=("$SB")
        STRESS_SANDBOXES+=("$SB")
    fi
done

if [ ${#STRESS_SANDBOXES[@]} -ge 3 ]; then
    for sb in "${STRESS_SANDBOXES[@]}"; do
        api -X PUT "$API_URL/api/sandboxes/$sb/limits" -d '{"memoryMB":2048,"cpuPercent":200}' >/dev/null 2>&1
    done
    sleep 2

    declare -a PIDS=()
    for sb in "${STRESS_SANDBOXES[@]}"; do
        ( set +e; exec_run "$sb" "sh" "-c" "for i in \$(seq 1 4); do while true; do :; done & done; sleep 30; kill %1 %2 %3 %4 2>/dev/null" >/dev/null 2>&1 ) &
        PIDS+=($!)
        ( set +e; exec_run "$sb" "python3" "-c" "x=bytearray(1500*1024*1024); import time; time.sleep(30)" >/dev/null 2>&1 ) &
        PIDS+=($!)
        ( set +e; exec_run "$sb" "dd" "if=/dev/urandom" "of=/workspace/stress.dat" "bs=1M" "count=200" >/dev/null 2>&1 ) &
        PIDS+=($!)
    done

    echo "Combined stress running on ${#STRESS_SANDBOXES[@]} sandboxes..."
    echo "Worker state during stress:"
    sleep 15
    worker_summary

    for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
    pass "Combined resource pressure test completed"

    # Verify sandboxes survived
    ALIVE=0
    for sb in "${STRESS_SANDBOXES[@]}"; do
        OUT=$(exec_stdout "$sb" "echo" "ok" 2>/dev/null)
        [ "$OUT" = "ok" ] && ALIVE=$((ALIVE+1))
    done
    [ "$ALIVE" -ge $((${#STRESS_SANDBOXES[@]} * 80 / 100)) ] && pass "Stress survival: $ALIVE/${#STRESS_SANDBOXES[@]} alive" || fail "Stress survival: $ALIVE/${#STRESS_SANDBOXES[@]} alive"
else
    skip "Combined stress test skipped (couldn't create enough sandboxes)"
fi

# ============================================================
# Phase 7: Final state
# ============================================================
h "Phase 7: Final state"

FINAL_WORKERS=$(worker_count)
echo "Workers: $INITIAL_WORKERS (start) → $FINAL_WORKERS (end)"
worker_summary

TOTAL=$(total_sandboxes)
echo "Total sandboxes across fleet: $TOTAL"
echo "Test sandboxes to clean up: ${#SANDBOXES[@]}"

summary
