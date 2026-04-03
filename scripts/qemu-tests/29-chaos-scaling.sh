#!/usr/bin/env bash
# 29-chaos-scaling.sh — Chaos scaling stress test
#
# Ramps up sandboxes to a target count, then continuously scales random
# sandboxes to different resource tiers, creates new ones, and destroys
# others — all while the autoscaler is reacting.
#
# Required env:
#   OPENSANDBOX_API_URL   — e.g. https://app.opencomputer.dev
#   OPENSANDBOX_API_KEY   — API key for sandbox creation
#
# Optional env:
#   TARGET_COUNT          — total sandboxes to ramp to (default: 200)
#   BATCH_SIZE            — sandboxes to create per batch during ramp (default: 20)
#   CHAOS_ROUNDS          — number of chaos rounds after ramp (default: 60)
#   CHAOS_INTERVAL        — seconds between chaos rounds (default: 5)
#   SCALE_BATCH           — sandboxes to scale per round (default: 10)
#   CHURN_COUNT           — sandboxes to destroy+recreate per round (default: 5)

source "$(dirname "$0")/common.sh"

TARGET="${TARGET_COUNT:-200}"
BATCH="${BATCH_SIZE:-20}"
ROUNDS="${CHAOS_ROUNDS:-60}"
INTERVAL="${CHAOS_INTERVAL:-5}"
SCALE_BATCH="${SCALE_BATCH:-10}"
CHURN="${CHURN_COUNT:-5}"

# Valid memory tiers (from pkg/types/sandbox.go)
TIERS=(4096 8192 16384 32768)
# Corresponding CPU percents
CPUS=(100 200 400 800)

# Track all sandbox IDs
declare -a SANDBOXES=()

cleanup() {
    echo ""
    h "Cleanup: destroying ${#SANDBOXES[@]} sandboxes"
    local pids=()
    for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
        destroy_sandbox "$sb" &
        pids+=($!)
        # Throttle: max 20 concurrent destroys
        if [ ${#pids[@]} -ge 20 ]; then
            wait "${pids[0]}" 2>/dev/null || true
            pids=("${pids[@]:1}")
        fi
    done
    wait 2>/dev/null || true
    echo "Cleanup complete"
}
trap cleanup EXIT INT TERM

# --- Helpers ---

random_element() {
    local arr=("$@")
    echo "${arr[$((RANDOM % ${#arr[@]}))]}"
}

random_range() {
    local min=$1 max=$2
    echo $(( RANDOM % (max - min + 1) + min ))
}

get_workers() {
    api "$API_URL/api/workers" 2>/dev/null | python3 -c "
import sys, json
try:
    workers = json.load(sys.stdin)
    for w in workers:
        wid = w.get('worker_id','?')
        cur = w.get('current',0)
        cap = w.get('capacity',0)
        cpu = w.get('cpu_pct',0)
        mem = w.get('mem_pct',0)
        ver = w.get('worker_version','?')
        print(f'  {wid}: {cur}/{cap} sandboxes, cpu={cpu:.0f}% mem={mem:.0f}% ver={ver}')
except: pass
" 2>/dev/null
}

# ============================================================
# Phase 1: Ramp up
# ============================================================
h "Phase 1: Ramp up to $TARGET sandboxes (batches of $BATCH)"

created=0
while [ $created -lt "$TARGET" ]; do
    batch_target=$((created + BATCH))
    [ $batch_target -gt "$TARGET" ] && batch_target=$TARGET

    # Create batch in parallel
    pids=()
    tmpdir=$(mktemp -d)
    for i in $(seq $((created + 1)) "$batch_target"); do
        (
            sb=$(create_sandbox 7200)
            echo "$sb" > "$tmpdir/$i"
        ) &
        pids+=($!)
    done
    wait "${pids[@]}" 2>/dev/null || true

    # Collect IDs
    for i in $(seq $((created + 1)) "$batch_target"); do
        if [ -f "$tmpdir/$i" ]; then
            sb=$(cat "$tmpdir/$i")
            if [ -n "$sb" ] && [ "$sb" != "null" ]; then
                SANDBOXES+=("$sb")
            fi
        fi
    done
    rm -rf "$tmpdir"

    created=${#SANDBOXES[@]}
    echo "  Created: $created / $TARGET"
done

pass "Ramped to ${#SANDBOXES[@]} sandboxes"
echo ""
echo "Workers after ramp:"
get_workers
echo ""

# ============================================================
# Phase 2: Chaos rounds
# ============================================================
h "Phase 2: Chaos scaling ($ROUNDS rounds, every ${INTERVAL}s)"
echo "  Each round: scale $SCALE_BATCH random sandboxes + churn $CHURN"

scales_ok=0
scales_fail=0
creates_ok=0
destroys=0

for round in $(seq 1 "$ROUNDS"); do
    round_start=$(date +%s)

    # --- Scale random sandboxes to random tiers ---
    scale_pids=()
    for _ in $(seq 1 "$SCALE_BATCH"); do
        if [ ${#SANDBOXES[@]} -eq 0 ]; then break; fi
        idx=$((RANDOM % ${#SANDBOXES[@]}))
        sb="${SANDBOXES[$idx]}"
        tier_idx=$((RANDOM % ${#TIERS[@]}))
        mem="${TIERS[$tier_idx]}"
        cpu="${CPUS[$tier_idx]}"

        (
            result=$(api -X PUT "$API_URL/api/sandboxes/$sb/limits" \
                -d "{\"memoryMB\":$mem,\"cpuPercent\":$cpu}" 2>/dev/null)
            if echo "$result" | grep -q "sandboxID"; then
                echo "ok"
            else
                echo "fail"
            fi
        ) &
        scale_pids+=($!)
    done

    # --- Churn: destroy some, create replacements ---
    churn_pids=()
    destroyed_indices=()
    for _ in $(seq 1 "$CHURN"); do
        if [ ${#SANDBOXES[@]} -le $((CHURN + 1)) ]; then break; fi
        idx=$((RANDOM % ${#SANDBOXES[@]}))
        sb="${SANDBOXES[$idx]}"
        destroy_sandbox "$sb" &
        churn_pids+=($!)
        destroyed_indices+=("$idx")
        destroys=$((destroys + 1))
    done

    # Wait for scales
    for pid in "${scale_pids[@]}"; do
        result=$(wait "$pid" 2>/dev/null && echo "ok" || echo "fail")
        if [ "$result" = "ok" ]; then
            scales_ok=$((scales_ok + 1))
        else
            scales_fail=$((scales_fail + 1))
        fi
    done

    # Wait for destroys, then remove from array and create replacements
    wait "${churn_pids[@]}" 2>/dev/null || true

    # Remove destroyed sandboxes (in reverse order to preserve indices)
    IFS=$'\n' sorted=($(printf '%s\n' "${destroyed_indices[@]}" | sort -rn)); unset IFS
    for idx in "${sorted[@]}"; do
        if [ "$idx" -lt ${#SANDBOXES[@]} ]; then
            unset 'SANDBOXES[idx]'
        fi
    done
    # Compact array
    SANDBOXES=("${SANDBOXES[@]}")

    # Create replacements
    tmpdir=$(mktemp -d)
    replace_pids=()
    for i in $(seq 1 "$CHURN"); do
        (
            sb=$(create_sandbox 7200 2>/dev/null)
            echo "$sb" > "$tmpdir/$i"
        ) &
        replace_pids+=($!)
    done
    wait "${replace_pids[@]}" 2>/dev/null || true
    for i in $(seq 1 "$CHURN"); do
        if [ -f "$tmpdir/$i" ]; then
            sb=$(cat "$tmpdir/$i")
            if [ -n "$sb" ] && [ "$sb" != "null" ]; then
                SANDBOXES+=("$sb")
                creates_ok=$((creates_ok + 1))
            fi
        fi
    done
    rm -rf "$tmpdir"

    # Status every 10 rounds
    if [ $((round % 10)) -eq 0 ]; then
        elapsed=$(( $(date +%s) - round_start ))
        echo "  Round $round/$ROUNDS: ${#SANDBOXES[@]} sandboxes, scales=$scales_ok/$((scales_ok+scales_fail)), churn=$destroys destroyed / $creates_ok created (${elapsed}s)"
        get_workers
    fi

    # Wait for interval
    elapsed=$(( $(date +%s) - round_start ))
    remaining=$((INTERVAL - elapsed))
    [ $remaining -gt 0 ] && sleep "$remaining"
done

echo ""
echo "Chaos results:"
echo "  Scales attempted: $((scales_ok + scales_fail))"
echo "  Scales succeeded: $scales_ok"
echo "  Scales failed:    $scales_fail"
echo "  Sandboxes destroyed: $destroys"
echo "  Sandboxes created:   $creates_ok"
echo "  Final count: ${#SANDBOXES[@]}"

[ $scales_fail -lt $((scales_ok / 10)) ] && pass "Scale success rate > 90%" || fail "Too many scale failures: $scales_fail / $((scales_ok + scales_fail))"

# ============================================================
# Phase 3: Verify survivors
# ============================================================
h "Phase 3: Verify survivors (sample of 20)"

sample_size=20
[ ${#SANDBOXES[@]} -lt $sample_size ] && sample_size=${#SANDBOXES[@]}
alive=0
dead=0

for _ in $(seq 1 "$sample_size"); do
    idx=$((RANDOM % ${#SANDBOXES[@]}))
    sb="${SANDBOXES[$idx]}"
    result=$(exec_stdout "$sb" "echo" "alive" 2>/dev/null)
    if [ "$result" = "alive" ]; then
        alive=$((alive + 1))
    else
        dead=$((dead + 1))
    fi
done

echo "  Alive: $alive / $sample_size"
[ $dead -le $((sample_size / 10)) ] && pass "Survival rate > 90%" || fail "Too many dead: $dead / $sample_size"

# ============================================================
# Phase 4: Final worker state
# ============================================================
h "Phase 4: Final worker state"
get_workers

summary
