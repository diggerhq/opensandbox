#!/usr/bin/env bash
# bench-qemu.sh — Benchmark all QEMU sandbox operations
#
# Measures client-perceived latency (API response time), not background work.
# Each operation runs N times, reports min/avg/median/max.
#
# Usage:
#   OPENSANDBOX_API_URL=http://host:8080 OPENSANDBOX_API_KEY=key ./scripts/bench-qemu.sh
#   RUNS=5 ./scripts/bench-qemu.sh   # default 3 runs per operation

set -euo pipefail

API="${OPENSANDBOX_API_URL:?Set OPENSANDBOX_API_URL}"
KEY="${OPENSANDBOX_API_KEY:?Set OPENSANDBOX_API_KEY}"
RUNS="${RUNS:-3}"

# ── Helpers ──

ms() { python3 -c "import time; print(int(time.time()*1000))"; }

bench() {
    local label="$1"; shift
    local times=()
    for i in $(seq 1 "$RUNS"); do
        local t0=$(ms)
        eval "$@" > /dev/null 2>&1
        local t1=$(ms)
        local dt=$((t1 - t0))
        times+=($dt)
        printf "    #%d: %dms\n" "$i" "$dt"
    done
    # Store for summary
    LABELS+=("$label")
    ALL_TIMES+=("${times[*]}")
}

stats() {
    local nums=($1)
    local sorted=($(printf '%s\n' "${nums[@]}" | sort -n))
    local n=${#sorted[@]}
    local sum=0
    for v in "${sorted[@]}"; do sum=$((sum + v)); done
    local avg=$((sum / n))
    local mid=$(( (n - 1) / 2 ))
    local median=${sorted[$mid]}
    echo "${sorted[0]} $avg $median ${sorted[$((n-1))]}"
}

h() { printf "\n\033[1;36m── %s ──\033[0m\n" "$1"; }

LABELS=()
ALL_TIMES=()

# ── Ensure clean state ──
curl -s "$API/api/sandboxes" -H "X-API-Key: $KEY" | python3 -c "
import sys,json,urllib.request
for s in json.load(sys.stdin):
    req = urllib.request.Request('$API/api/sandboxes/'+s['sandboxID'], method='DELETE', headers={'X-API-Key': '$KEY'})
    try: urllib.request.urlopen(req)
    except: pass
" 2>/dev/null

echo ""
echo "╔══════════════════════════════════════════════════════╗"
echo "║          QEMU Sandbox Benchmark ($RUNS runs each)          ║"
echo "╚══════════════════════════════════════════════════════╝"
echo ""

# ── Create sandbox ──
h "Create sandbox"
SANDBOX_IDS=()
bench "Create sandbox" '
    R=$(curl -s -X POST "$API/api/sandboxes" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"timeout\":300}")
    SID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)[\"sandboxID\"])" 2>/dev/null)
    SANDBOX_IDS+=("$SID")
'
# Keep one sandbox for subsequent tests
SB=$(curl -s -X POST "$API/api/sandboxes" -H 'Content-Type: application/json' -H "X-API-Key: $KEY" -d '{"timeout":600}' | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")

# ── Exec/run (simple command) ──
h "Exec/run (echo)"
bench "Exec (echo)" '
    curl -s -X POST "$API/api/sandboxes/$SB/exec/run" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"cmd\":\"echo\",\"args\":[\"hello\"],\"timeout\":5}"
'

# ── Exec/run (python) ──
h "Exec/run (python3 print)"
bench "Exec (python3)" '
    curl -s -X POST "$API/api/sandboxes/$SB/exec/run" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"cmd\":\"python3\",\"args\":[\"-c\",\"print(1+1)\"],\"timeout\":5}"
'

# ── Write file ──
h "Write file (1KB)"
bench "Write file" '
    curl -s -X PUT "$API/api/sandboxes/$SB/files?path=/workspace/bench.txt" -H "X-API-Key: $KEY" -H "Content-Type: application/octet-stream" --data-binary "benchmark-data-$(date +%s)"
'

# ── Read file ──
h "Read file"
bench "Read file" '
    curl -s "$API/api/sandboxes/$SB/files?path=/workspace/bench.txt" -H "X-API-Key: $KEY"
'

# ── List directory ──
h "List directory"
bench "List dir" '
    curl -s "$API/api/sandboxes/$SB/files/list?path=/workspace" -H "X-API-Key: $KEY"
'

# ── Scale up (memory hotplug) ──
h "Scale up (1GB → 2GB)"
bench "Scale up" '
    curl -s -X PUT "$API/api/sandboxes/$SB/limits" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"maxMemoryMB\":2048}"
'

# ── Metadata API (from inside VM) ──
h "Metadata /v1/status (from inside VM)"
bench "Metadata status" '
    curl -s -X POST "$API/api/sandboxes/$SB/exec/run" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"cmd\":\"curl\",\"args\":[\"-s\",\"http://169.254.169.254/v1/status\"],\"timeout\":5}"
'

# ── Create checkpoint ──
h "Create checkpoint"
CP_IDS=()
bench "Create checkpoint" '
    R=$(curl -s -X POST "$API/api/sandboxes/$SB/checkpoints" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"name\":\"bench-$(date +%s)\"}")
    CPID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)[\"id\"])" 2>/dev/null)
    CP_IDS+=("$CPID")
'

# Wait for last checkpoint to be ready
LAST_CP="${CP_IDS[-1]:-}"
if [ -n "$LAST_CP" ]; then
    for i in $(seq 1 20); do
        S=$(curl -s "$API/api/sandboxes/$SB/checkpoints/$LAST_CP" -H "X-API-Key: $KEY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','?'))" 2>/dev/null)
        [ "$S" = "ready" ] && break; sleep 1
    done
fi

# ── Restore checkpoint (in-place) ──
h "Restore checkpoint (in-place)"
if [ -n "$LAST_CP" ]; then
    bench "Restore checkpoint" '
        curl -s -X POST "$API/api/sandboxes/$SB/checkpoints/$LAST_CP/restore" -H "Content-Type: application/json" -H "X-API-Key: $KEY"
    '
fi

# ── First command after restore ──
h "First exec after restore"
bench "First exec after restore" '
    curl -s -X POST "$API/api/sandboxes/$SB/exec/run" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"cmd\":\"echo\",\"args\":[\"restored\"],\"timeout\":5}"
'

# ── Fork from checkpoint ──
h "Fork from checkpoint"
if [ -n "$LAST_CP" ]; then
    bench "Fork from checkpoint" '
        R=$(curl -s -X POST "$API/api/sandboxes/from-checkpoint/$LAST_CP" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"timeout\":300}")
        FID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get(\"sandboxID\",\"\"))" 2>/dev/null)
        # Wait for running
        for i in $(seq 1 30); do
            FS=$(curl -s "$API/api/sandboxes/$FID" -H "X-API-Key: $KEY" | python3 -c "import sys,json; print(json.load(sys.stdin).get(\"status\",\"?\"))" 2>/dev/null)
            [ "$FS" = "running" ] && break; sleep 0.5
        done
        curl -s -X DELETE "$API/api/sandboxes/$FID" -H "X-API-Key: $KEY"
    '
fi

# ── Hibernate ──
h "Hibernate"
bench "Hibernate" '
    curl -s -X POST "$API/api/sandboxes/$SB/hibernate" -H "X-API-Key: $KEY"
    sleep 2
'

# ── Wake ──
h "Wake"
bench "Wake" '
    curl -s -X POST "$API/api/sandboxes/$SB/wake" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"timeout\":300}"
'

# ── Get sandbox ──
h "Get sandbox"
bench "Get sandbox" '
    curl -s "$API/api/sandboxes/$SB" -H "X-API-Key: $KEY"
'

# ── List sandboxes ──
h "List sandboxes"
bench "List sandboxes" '
    curl -s "$API/api/sandboxes" -H "X-API-Key: $KEY"
'

# ── Delete sandbox ──
h "Delete sandbox"
bench "Delete sandbox" '
    R=$(curl -s -X POST "$API/api/sandboxes" -H "Content-Type: application/json" -H "X-API-Key: $KEY" -d "{\"timeout\":60}")
    DID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin)[\"sandboxID\"])" 2>/dev/null)
    curl -s -X DELETE "$API/api/sandboxes/$DID" -H "X-API-Key: $KEY"
'

# ── Cleanup ──
h "Cleanup"
curl -s -X DELETE "$API/api/sandboxes/$SB" -H "X-API-Key: $KEY" > /dev/null
for sid in "${SANDBOX_IDS[@]}"; do
    curl -s -X DELETE "$API/api/sandboxes/$sid" -H "X-API-Key: $KEY" > /dev/null 2>&1
done
echo "  Cleaned up"

# ── Results ──
echo ""
echo "── Results ──"
printf "%-28s %6s %8s %8s %8s %8s\n" "Operation" "Runs" "Min" "Avg" "Median" "Max"
printf "%-28s %6s %8s %8s %8s %8s\n" "────────────────────────────" "──────" "────────" "────────" "────────" "────────"

for i in "${!LABELS[@]}"; do
    read mn av md mx <<< "$(stats "${ALL_TIMES[$i]}")"
    printf "%-28s %6d %7dms %7dms %7dms %7dms\n" "${LABELS[$i]}" "$RUNS" "$mn" "$av" "$md" "$mx"
done
echo ""
