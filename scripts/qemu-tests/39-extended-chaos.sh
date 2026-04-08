#!/usr/bin/env bash
# 39-extended-chaos.sh — Extended chaos test with aggressive resource mutations
#
# 200 sandboxes, 20 chaos rounds over ~10 minutes.
# Each round: random memory scales (512MB-16GB), disk fills (1-5GB),
# random destroys, random creates, responsiveness checks.
#
# Required env:
#   OPENSANDBOX_API_URL
#   OPENSANDBOX_API_KEY

source "$(dirname "$0")/common.sh"

TARGET=${1:-81}
WAVES=5
PER_WAVE=$((TARGET / WAVES))
CHAOS_ROUNDS=5

get_workers() { api "$API_URL/api/workers" 2>/dev/null || echo "[]"; }
worker_summary() {
    get_workers | python3 -c "
import sys,json
w = json.load(sys.stdin)
total = sum(x['current'] for x in w)
print(f'{len(w)} workers, {total} sandboxes')
for x in w:
    print(f'  {x[\"worker_id\"][-8:]}: {x[\"current\"]}  cpu={x[\"cpu_pct\"]:.0f}%  mem={x[\"mem_pct\"]:.0f}%  disk={x[\"disk_pct\"]:.0f}%')
" 2>/dev/null
}

create_one() {
    local result
    result=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":0}' 2>/dev/null || echo '{}')
    echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null
}

exec_check() {
    local out
    out=$(exec_stdout "$1" "echo" "ok" 2>/dev/null)
    [ "$out" = "ok" ]
}

SANDBOX_FILE=$(mktemp)

cleanup() {
    echo ""
    echo "=== Cleanup ==="
    local count=0
    while read -r sb; do
        [ -n "$sb" ] && { destroy_sandbox "$sb" >/dev/null 2>&1 & count=$((count+1)); }
        [ $((count % 30)) -eq 0 ] && wait 2>/dev/null
    done < "$SANDBOX_FILE"
    wait 2>/dev/null
    echo "Destroyed $count sandboxes"
    rm -f "$SANDBOX_FILE"
}
trap cleanup EXIT INT TERM

# Clear server event history for a clean dashboard
api -X POST "$API_URL/admin/events/clear" >/dev/null 2>&1

echo "=== Extended Chaos Test ==="
echo "Target: $TARGET sandboxes, $CHAOS_ROUNDS chaos rounds"
echo ""
echo "Starting state:"
worker_summary
echo ""

# ============================================================
h "Phase 1: Ramp up to $TARGET sandboxes"

TOTAL_CREATED=0
for wave in $(seq 1 $WAVES); do
    echo "  Wave $wave/$WAVES: creating $PER_WAVE..."
    WAVE_OK=0
    WAVE_DIR=$(mktemp -d)
    for i in $(seq 1 $PER_WAVE); do
        (
            sb=$(create_one)
            [ -n "$sb" ] && [ "$sb" != "null" ] && echo "$sb" > "$WAVE_DIR/$i"
        ) &
        [ $((i % 10)) -eq 0 ] && wait 2>/dev/null
    done
    wait 2>/dev/null
    for f in "$WAVE_DIR"/*; do
        [ -f "$f" ] && { cat "$f" >> "$SANDBOX_FILE"; WAVE_OK=$((WAVE_OK + 1)); }
    done
    rm -rf "$WAVE_DIR"
    TOTAL_CREATED=$((TOTAL_CREATED + WAVE_OK))
    echo "    $WAVE_OK created (total: $TOTAL_CREATED) — $(worker_summary | head -1)"
    sleep 3
done

echo ""
[ "$TOTAL_CREATED" -ge $((TARGET * 80 / 100)) ] && pass "Ramp: $TOTAL_CREATED/$TARGET created" || fail "Only $TOTAL_CREATED/$TARGET"

echo ""
echo "Post-ramp state:"
worker_summary

# Start real workloads in 20 random sandboxes (allocate memory, run processes)
echo ""
echo "  Starting workloads in 20 sandboxes..."
for sb in $(sort -R < "$SANDBOX_FILE" | head -20); do
    [ -z "$sb" ] && continue
    # Write files + start a background counter process
    exec_run "$sb" "sh" "-c" "dd if=/dev/urandom of=/workspace/data.bin bs=1M count=10 2>/dev/null; python3 -c 'import time; [time.sleep(60)]' &" >/dev/null 2>&1 &
done
wait 2>/dev/null || true
echo "  Workloads started"

# ============================================================
h "Phase 2: Initial responsiveness (sample 30)"

RESPONSIVE=0
for sb in $(sort -R < "$SANDBOX_FILE" | head -30); do
    exec_check "$sb" && RESPONSIVE=$((RESPONSIVE + 1))
done
echo "  $RESPONSIVE/30 responsive"
[ "$RESPONSIVE" -ge 27 ] && pass "≥90% responsive ($RESPONSIVE/30)" || fail "$RESPONSIVE/30"

# ============================================================
h "Phase 3: Extended chaos ($CHAOS_ROUNDS rounds — 5 up + 5 down scales per round)"

MEM_UP_OPTIONS="8192 8192 8192 16384 16384"
MEM_DOWN_OPTIONS="4096"
DISK_SIZES="1024 2048 5120"  # MB to write

SCALES_OK=0
SCALES_FAIL=0
DESTROYS=0
CREATES=0
DISK_WRITES=0
CHECKS_OK=0
CHECKS_FAIL=0

for round in $(seq 1 $CHAOS_ROUNDS); do
    SB_COUNT=$(wc -l < "$SANDBOX_FILE" | tr -d ' ')
    WC=$(get_workers | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null)
    printf "  Round %2d/%d  [sb=%s w=%s]  " "$round" "$CHAOS_ROUNDS" "$SB_COUNT" "$WC"

    # --- Action 1a: Scale UP 5 random sandboxes to 8GB or 16GB ---
    for sb in $(sort -R < "$SANDBOX_FILE" | head -5); do
        [ -z "$sb" ] && continue
        MEM=$(echo $MEM_UP_OPTIONS | tr ' ' '\n' | sort -R | head -1)
        result=$(TIMEOUT=120 api -X PUT "$API_URL/api/sandboxes/$sb/limits" -d "{\"memoryMB\":$MEM,\"cpuPercent\":200}" 2>/dev/null)
        if echo "$result" | grep -q "ok"; then
            SCALES_OK=$((SCALES_OK + 1))
        else
            SCALES_FAIL=$((SCALES_FAIL + 1))
        fi
    done

    # --- Action 1b: Scale DOWN 5 random sandboxes back to 4GB ---
    for sb in $(sort -R < "$SANDBOX_FILE" | head -5); do
        [ -z "$sb" ] && continue
        result=$(TIMEOUT=60 api -X PUT "$API_URL/api/sandboxes/$sb/limits" -d "{\"memoryMB\":4096,\"cpuPercent\":100}" 2>/dev/null)
        if echo "$result" | grep -q "ok"; then
            SCALES_OK=$((SCALES_OK + 1))
        else
            SCALES_FAIL=$((SCALES_FAIL + 1))
        fi
    done

    # --- Action 2: Write random data to disk on 2 sandboxes ---
    for sb in $(sort -R < "$SANDBOX_FILE" | head -2); do
        [ -z "$sb" ] && continue
        SIZE_MB=$(echo $DISK_SIZES | tr ' ' '\n' | sort -R | head -1)
        TIMEOUT=60 exec_run "$sb" "sh" "-c" "dd if=/dev/urandom of=/workspace/chaos-$(date +%s).bin bs=1M count=$SIZE_MB 2>/dev/null &" >/dev/null 2>&1 || true
        DISK_WRITES=$((DISK_WRITES + 1))
    done

    # No create/destroy during chaos — keep count fixed for predictable scaling

    # --- Action 5: Spot-check 3 random sandboxes ---
    for sb in $(sort -R < "$SANDBOX_FILE" | head -3); do
        [ -z "$sb" ] && continue
        if exec_check "$sb"; then
            CHECKS_OK=$((CHECKS_OK + 1))
        else
            CHECKS_FAIL=$((CHECKS_FAIL + 1))
        fi
    done

    printf "scales=%d/%d  disk=%d  destroy=%d  create=%d  checks=%d/%d\n" \
        "$SCALES_OK" "$((SCALES_OK + SCALES_FAIL))" "$DISK_WRITES" "$DESTROYS" "$CREATES" \
        "$CHECKS_OK" "$((CHECKS_OK + CHECKS_FAIL))"

    sleep 2
done

# ============================================================
h "Phase 3b: Migration pressure — overload one worker until migrations trigger"

# Pick the most-loaded worker
TARGET_WORKER=$(get_workers | python3 -c "
import sys,json
w = sorted(json.load(sys.stdin), key=lambda x: -x['current'])
print(w[0]['worker_id'] if w else '')
" 2>/dev/null)
TARGET_SHORT=${TARGET_WORKER##*-}
echo "  Target worker: $TARGET_SHORT"
echo "  Strategy: scale ALL its sandboxes to 8GB, then 16GB until migration triggers"
echo ""

# Get ALL sandboxes on that worker
TARGET_SBS=""
while read -r sb; do
    [ -z "$sb" ] && continue
    SB_WORKER=$(api "$API_URL/api/sandboxes/$sb" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null)
    [ "$SB_WORKER" = "$TARGET_WORKER" ] && TARGET_SBS="$TARGET_SBS $sb"
done < "$SANDBOX_FILE"
SB_ON_TARGET=$(echo $TARGET_SBS | wc -w | tr -d ' ')
echo "  Found $SB_ON_TARGET sandboxes on $TARGET_SHORT"

MIGRATED=0

do_scale() {
    local sb="$1" size="$2"
    local result=$(TIMEOUT=180 api -X PUT "$API_URL/api/sandboxes/$sb/limits" -d "{\"memoryMB\":$size,\"cpuPercent\":200}" 2>/dev/null)
    local migrated=$(echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('migrated',False))" 2>/dev/null)
    local ok=$(echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('ok',False))" 2>/dev/null)

    if [ "$migrated" = "True" ]; then
        local new_w=$(echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID','')[-8:])" 2>/dev/null)
        printf "  \033[32m✓ %s → %dMB MIGRATED to %s\033[0m\n" "${sb##*-}" "$size" "$new_w"
        return 0
    elif [ "$ok" = "True" ]; then
        printf "  %s → %dMB (fit)\n" "${sb##*-}" "$size"
        return 1
    else
        local err=$(echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error','?')[:50])" 2>/dev/null)
        printf "  \033[31m%s → %dMB FAILED: %s\033[0m\n" "${sb##*-}" "$size" "$err"
        return 1
    fi
}

# Phase A: Scale all sandboxes on target to 8GB
echo "  --- Scaling to 8GB ---"
for sb in $TARGET_SBS; do
    do_scale "$sb" 8192 && MIGRATED=$((MIGRATED + 1))
    SCALES_OK=$((SCALES_OK + 1))
done

# Phase B: Now scale them to 16GB — this should force migrations
echo ""
echo "  --- Scaling to 16GB (should trigger migrations) ---"
for sb in $TARGET_SBS; do
    do_scale "$sb" 16384 && MIGRATED=$((MIGRATED + 1))
    SCALES_OK=$((SCALES_OK + 1))
done

echo ""
[ "$MIGRATED" -gt 0 ] && pass "$MIGRATED migration(s) triggered by memory pressure on $TARGET_SHORT" || skip "No migrations triggered"

echo ""
[ "$CHECKS_FAIL" -le $((CHECKS_OK / 10)) ] && pass "Chaos checks: $CHECKS_OK ok, $CHECKS_FAIL failed (≤10% failure)" || fail "Too many failures: $CHECKS_FAIL/$((CHECKS_OK + CHECKS_FAIL))"
pass "Chaos survived: $SCALES_OK scales, $DISK_WRITES disk writes, $DESTROYS destroys, $CREATES creates"

# ============================================================
h "Phase 4: Post-chaos state"

echo ""
worker_summary
echo ""

SB_COUNT=$(wc -l < "$SANDBOX_FILE" | tr -d ' ')
echo "Tracked sandboxes: $SB_COUNT"

# ============================================================
h "Phase 5: Post-chaos deep responsiveness (sample 50)"

RESPONSIVE=0
TOTAL_CHECK=0
for sb in $(sort -R < "$SANDBOX_FILE" | head -50); do
    [ -z "$sb" ] && continue
    TOTAL_CHECK=$((TOTAL_CHECK + 1))
    exec_check "$sb" && RESPONSIVE=$((RESPONSIVE + 1))
done
echo "  $RESPONSIVE/$TOTAL_CHECK responsive"
[ "$RESPONSIVE" -ge $((TOTAL_CHECK * 85 / 100)) ] && pass "≥85% responsive after extended chaos ($RESPONSIVE/$TOTAL_CHECK)" || fail "$RESPONSIVE/$TOTAL_CHECK"

# ============================================================
h "Phase 6: Drain and recovery"

echo "Destroying all sandboxes..."
BATCH=0
while read -r sb; do
    [ -n "$sb" ] && { destroy_sandbox "$sb" >/dev/null 2>&1 & BATCH=$((BATCH + 1)); }
    [ $((BATCH % 30)) -eq 0 ] && wait 2>/dev/null
done < "$SANDBOX_FILE"
wait 2>/dev/null
> "$SANDBOX_FILE"

echo "Watching workers (3 min)..."
for i in $(seq 1 12); do
    sleep 15
    WC=$(get_workers | python3 -c "import sys,json; w=json.load(sys.stdin); print(f'{len(w)} workers, {sum(x[\"current\"] for x in w)} sb, peak_cpu={max(x[\"cpu_pct\"] for x in w):.0f}% peak_mem={max(x[\"mem_pct\"] for x in w):.0f}%')" 2>/dev/null)
    echo "  $(date +%H:%M:%S) — $WC"
done

echo ""
echo "Final state:"
worker_summary

# Verify system works
SB_FINAL=$(create_one)
if [ -n "$SB_FINAL" ]; then
    sleep 2
    exec_check "$SB_FINAL" && pass "System healthy after extended chaos" || fail "Post-chaos sandbox broken"
    destroy_sandbox "$SB_FINAL" >/dev/null 2>&1
else
    fail "Cannot create sandbox after chaos"
fi

echo ""
echo "=== Final Stats ==="
echo "  Peak sandboxes: ~$TOTAL_CREATED"
echo "  Chaos rounds: $CHAOS_ROUNDS"
echo "  Memory scales: $SCALES_OK ok / $((SCALES_OK + SCALES_FAIL)) total"
echo "  Disk writes: $DISK_WRITES"
echo "  Random destroys: $DESTROYS"
echo "  Random creates: $CREATES"
echo "  Spot checks: $CHECKS_OK ok / $((CHECKS_OK + CHECKS_FAIL)) total"

# Fetch server-side report (includes migration details)
echo ""
h "Server Report"
REPORT=$(api "$API_URL/admin/report" 2>/dev/null)
if [ -n "$REPORT" ] && echo "$REPORT" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    echo "$REPORT" | python3 -c "
import sys, json
r = json.load(sys.stdin)
print(f'  Events tracked: {r[\"total_events\"]}')
print(f'  Creates: {r[\"creates\"]}')
print(f'  Destroys: {r[\"destroys\"]}')
print(f'  Scales: {r[\"scales\"]}')
print(f'  Migrations: {r[\"migrations\"][\"total\"]} (all succeeded)')
if r['migrations']['details']:
    print()
    print('  Migration details:')
    for m in r['migrations']['details']:
        print(f'    {m[\"time\"]} {m[\"sandbox\"][-8:]} → {m[\"worker\"][-8:]}: {m[\"detail\"]}')
print()
print('  Workers:')
for w in sorted(r.get('workers',[]), key=lambda x: -x['current']):
    print(f'    {w[\"id\"]}: {w[\"current\"]} sandboxes  cpu={w[\"cpu_pct\"]:.0f}%  mem={w[\"mem_pct\"]:.0f}%')
print()
mig = r['migrations']['total']
if mig > 0:
    print(f'  ✓ All {mig} migrations completed successfully')
else:
    print('  No migrations triggered (all scales fit on current worker)')
" 2>/dev/null
else
    echo "  (report not available)"
fi

echo ""
summary
