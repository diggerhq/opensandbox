#!/usr/bin/env bash
# 21-latency-stress.sh — Latency and 502 stress test
# Simulates customer SDK latency testing: rapid concurrent requests to the same sandbox.
# Measures response times and counts 502/5xx errors.
set +u
source "$(dirname "$0")/common.sh"

TIMEOUT=30
SANDBOXES=()
cleanup() {
    set +u
    for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done
    set -u
}
trap cleanup EXIT

# ── Test 1: Sequential exec latency (baseline) ──────────────────────
h "Sequential Exec Latency (50 requests)"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Warm up
exec_run "$SB" "echo" "warmup" >/dev/null

TOTAL_MS=0
ERRORS=0
for i in $(seq 1 50); do
    START=$(date +%s%N)
    RESULT=$(exec_run "$SB" "echo" "ping-$i" 2>&1)
    END=$(date +%s%N)
    MS=$(( (END - START) / 1000000 ))
    TOTAL_MS=$((TOTAL_MS + MS))

    if echo "$RESULT" | grep -q '"exitCode":0'; then
        true
    else
        ERRORS=$((ERRORS + 1))
    fi
done
AVG=$((TOTAL_MS / 50))
pass "50 sequential execs: avg=${AVG}ms, errors=$ERRORS"
[ "$ERRORS" -eq 0 ] && pass "Zero errors" || fail "$ERRORS/50 errors"

# ── Test 2: Concurrent exec burst (10 at a time) ────────────────────
h "Concurrent Exec Burst (10x parallel, 5 rounds)"

BURST_OK=0
BURST_FAIL=0
BURST_502=0
for round in $(seq 1 5); do
    PIDS=()
    TMPDIR=$(mktemp -d)
    for j in $(seq 1 10); do
        (
            RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 \
                -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
                -H "Content-Type: application/json" \
                -H "X-API-Key: $API_KEY" \
                -d "{\"cmd\":\"echo\",\"args\":[\"burst-$round-$j\"],\"timeout\":10}")
            HTTP_CODE=$(echo "$RESULT" | tail -1)
            BODY=$(echo "$RESULT" | sed '$d')
            echo "$HTTP_CODE|$BODY" > "$TMPDIR/result-$j"
        ) &
        PIDS+=($!)
    done
    for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done

    for j in $(seq 1 10); do
        LINE=$(cat "$TMPDIR/result-$j" 2>/dev/null)
        CODE=$(echo "$LINE" | cut -d'|' -f1)
        if [ "$CODE" = "200" ]; then
            BURST_OK=$((BURST_OK + 1))
        elif [ "$CODE" = "502" ]; then
            BURST_502=$((BURST_502 + 1))
        else
            BURST_FAIL=$((BURST_FAIL + 1))
        fi
    done
    rm -rf "$TMPDIR"
done
TOTAL=$((BURST_OK + BURST_FAIL + BURST_502))
pass "Burst results: $BURST_OK/$TOTAL OK, $BURST_502 x 502, $BURST_FAIL other errors"
[ "$BURST_502" -eq 0 ] && pass "Zero 502s in burst test" || fail "$BURST_502 x 502 errors"

# ── Test 3: Concurrent file operations (mixed read/write) ───────────
h "Concurrent File I/O (10x parallel, 5 rounds)"

# Write a file to read
exec_run "$SB" "bash" "-c" "echo file-content > /workspace/test.txt" >/dev/null

FILE_OK=0
FILE_502=0
FILE_FAIL=0
for round in $(seq 1 5); do
    PIDS=()
    TMPDIR=$(mktemp -d)
    for j in $(seq 1 10); do
        (
            if [ $((j % 2)) -eq 0 ]; then
                # Read
                RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 \
                    "$API_URL/api/sandboxes/$SB/files?path=/workspace/test.txt" \
                    -H "X-API-Key: $API_KEY")
            else
                # Write
                RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 \
                    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/write-$j.txt" \
                    -H "X-API-Key: $API_KEY" \
                    -H "Content-Type: application/octet-stream" \
                    --data-binary "data-$round-$j")
            fi
            HTTP_CODE=$(echo "$RESULT" | tail -1)
            echo "$HTTP_CODE" > "$TMPDIR/result-$j"
        ) &
        PIDS+=($!)
    done
    for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done

    for j in $(seq 1 10); do
        CODE=$(cat "$TMPDIR/result-$j" 2>/dev/null)
        if [ "$CODE" = "200" ] || [ "$CODE" = "204" ]; then
            FILE_OK=$((FILE_OK + 1))
        elif [ "$CODE" = "502" ]; then
            FILE_502=$((FILE_502 + 1))
        else
            FILE_FAIL=$((FILE_FAIL + 1))
        fi
    done
    rm -rf "$TMPDIR"
done
TOTAL=$((FILE_OK + FILE_FAIL + FILE_502))
pass "File I/O results: $FILE_OK/$TOTAL OK, $FILE_502 x 502, $FILE_FAIL other errors"
[ "$FILE_502" -eq 0 ] && pass "Zero 502s in file I/O test" || fail "$FILE_502 x 502 errors"

# ── Test 4: Multi-sandbox concurrent (5 sandboxes, 10 req each) ─────
h "Multi-Sandbox Concurrent (5 sandboxes x 10 requests)"

MULTI_SBS=()
for i in $(seq 1 5); do
    SB_NEW=$(create_sandbox)
    SANDBOXES+=("$SB_NEW")
    MULTI_SBS+=("$SB_NEW")
done
pass "Created 5 sandboxes"

# Warm up all
for sb in "${MULTI_SBS[@]}"; do
    exec_run "$sb" "echo" "warmup" >/dev/null &
done
wait

MULTI_OK=0
MULTI_502=0
MULTI_FAIL=0
TMPDIR=$(mktemp -d)
PIDS=()
for sb in "${MULTI_SBS[@]}"; do
    for j in $(seq 1 10); do
        (
            RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 \
                -X POST "$API_URL/api/sandboxes/$sb/exec/run" \
                -H "Content-Type: application/json" \
                -H "X-API-Key: $API_KEY" \
                -d "{\"cmd\":\"echo\",\"args\":[\"multi-$j\"],\"timeout\":10}")
            HTTP_CODE=$(echo "$RESULT" | tail -1)
            echo "$HTTP_CODE" > "$TMPDIR/result-${sb}-${j}"
        ) &
        PIDS+=($!)
    done
done
for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done

for sb in "${MULTI_SBS[@]}"; do
    for j in $(seq 1 10); do
        CODE=$(cat "$TMPDIR/result-${sb}-${j}" 2>/dev/null)
        if [ "$CODE" = "200" ]; then
            MULTI_OK=$((MULTI_OK + 1))
        elif [ "$CODE" = "502" ]; then
            MULTI_502=$((MULTI_502 + 1))
        else
            MULTI_FAIL=$((MULTI_FAIL + 1))
        fi
    done
done
rm -rf "$TMPDIR"
TOTAL=$((MULTI_OK + MULTI_FAIL + MULTI_502))
pass "Multi-sandbox results: $MULTI_OK/$TOTAL OK, $MULTI_502 x 502, $MULTI_FAIL other errors"
[ "$MULTI_502" -eq 0 ] && pass "Zero 502s across 5 sandboxes" || fail "$MULTI_502 x 502 errors"

# ── Test 5: Rapid fire single sandbox (100 requests, max parallelism) ─
h "Rapid Fire (100 concurrent requests, single sandbox)"

RF_OK=0
RF_502=0
RF_FAIL=0
TMPDIR=$(mktemp -d)
PIDS=()
for i in $(seq 1 100); do
    (
        RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 \
            -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
            -H "Content-Type: application/json" \
            -H "X-API-Key: $API_KEY" \
            -d "{\"cmd\":\"echo\",\"args\":[\"rapid-$i\"],\"timeout\":10}")
        HTTP_CODE=$(echo "$RESULT" | tail -1)
        echo "$HTTP_CODE" > "$TMPDIR/result-$i"
    ) &
    PIDS+=($!)
done
for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done

for i in $(seq 1 100); do
    CODE=$(cat "$TMPDIR/result-$i" 2>/dev/null)
    if [ "$CODE" = "200" ]; then
        RF_OK=$((RF_OK + 1))
    elif [ "$CODE" = "502" ]; then
        RF_502=$((RF_502 + 1))
    else
        RF_FAIL=$((RF_FAIL + 1))
    fi
done
rm -rf "$TMPDIR"
TOTAL=$((RF_OK + RF_FAIL + RF_502))
pass "Rapid fire results: $RF_OK/$TOTAL OK, $RF_502 x 502, $RF_FAIL other errors"
[ "$RF_502" -eq 0 ] && pass "Zero 502s in 100-request rapid fire" || fail "$RF_502 x 502 errors"

summary
