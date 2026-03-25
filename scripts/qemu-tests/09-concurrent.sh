#!/usr/bin/env bash
# 09-concurrent.sh — Concurrent sandbox creation + exec under load
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb"; done; }
trap cleanup EXIT

h "Concurrent Load"

# 3 sandboxes at different tiers
SB1=$(create_sandbox); SANDBOXES+=("$SB1")
SB2=$(create_sandbox); SANDBOXES+=("$SB2")
SB3=$(create_sandbox); SANDBOXES+=("$SB3")
pass "Created 3 sandboxes"

api -X PUT "$API_URL/api/sandboxes/$SB2/limits" -d '{"maxMemoryMB":2048,"cpuPercent":200,"maxPids":256}' >/dev/null
api -X PUT "$API_URL/api/sandboxes/$SB3/limits" -d '{"maxMemoryMB":4096,"cpuPercent":400,"maxPids":256}' >/dev/null
sleep 1

MEM1=$(exec_stdout "$SB1" "free" "-m" | awk '/Mem:/{print $2}')
MEM2=$(exec_stdout "$SB2" "free" "-m" | awk '/Mem:/{print $2}')
MEM3=$(exec_stdout "$SB3" "free" "-m" | awk '/Mem:/{print $2}')

[ "$MEM1" -lt 1000 ] && pass "SB1 (1GB): ${MEM1}MB" || fail "SB1: ${MEM1}MB"
[ "$MEM2" -gt 1800 ] && pass "SB2 (2GB): ${MEM2}MB" || fail "SB2: ${MEM2}MB"
[ "$MEM3" -gt 3800 ] && pass "SB3 (4GB): ${MEM3}MB" || fail "SB3: ${MEM3}MB"

# Destroy tier sandboxes
for sb in "$SB1" "$SB2" "$SB3"; do destroy_sandbox "$sb"; done
SANDBOXES=()

# 10 concurrent creates
h "10 Concurrent Creates"
PIDS=()
TMPDIR=$(mktemp -d)
for i in $(seq 1 10); do
    (
        RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":300}')
        SB_ID=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID','FAIL'))" 2>/dev/null)
        echo "$SB_ID" > "$TMPDIR/sb-$i"
    ) &
    PIDS+=($!)
done

# Wait for all
CREATED=0
FAILED=0
for pid in "${PIDS[@]}"; do
    wait "$pid" 2>/dev/null || true
done

for i in $(seq 1 10); do
    SB_ID=$(cat "$TMPDIR/sb-$i" 2>/dev/null)
    if [ -n "$SB_ID" ] && [ "$SB_ID" != "FAIL" ]; then
        SANDBOXES+=("$SB_ID")
        CREATED=$((CREATED+1))
    else
        FAILED=$((FAILED+1))
    fi
done
rm -rf "$TMPDIR"

[ "$CREATED" -ge 8 ] && pass "Concurrent creates: $CREATED/10 succeeded" || fail "Concurrent creates: only $CREATED/10"

# Exec on all created sandboxes in parallel
h "Exec Under Load ($CREATED sandboxes)"
OK=0
for sb in "${SANDBOXES[@]}"; do
    OUT=$(exec_stdout "$sb" "echo" "load-test" 2>/dev/null)
    [ "$OUT" = "load-test" ] && OK=$((OK+1))
done
[ "$OK" -eq "${#SANDBOXES[@]}" ] && pass "All $OK sandboxes responsive" || fail "Only $OK/${#SANDBOXES[@]} responsive"

summary
