#!/usr/bin/env bash
# 22-10k-stress.sh — 10,000 request stress test
# Fires requests in waves of 50 concurrent, 200 waves = 10,000 total.
# Tracks 502s, other errors, and latency percentiles.
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

TOTAL_REQUESTS=10000
CONCURRENCY=400
WAVES=$((TOTAL_REQUESTS / CONCURRENCY))

h "10,000 Request Stress Test ($CONCURRENCY concurrent x $WAVES waves)"

# Create sandbox
SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Warm up
exec_run "$SB" "echo" "warmup" >/dev/null
pass "Sandbox $SB ready"

# Results tracking
RESULTS_DIR=$(mktemp -d)
TOTAL_OK=0
TOTAL_502=0
TOTAL_OTHER=0
TOTAL_TIMEOUT=0
START_ALL=$(date +%s)

for wave in $(seq 1 "$WAVES"); do
    PIDS=()
    for j in $(seq 1 "$CONCURRENCY"); do
        REQ_NUM=$(( (wave - 1) * CONCURRENCY + j ))
        (
            START=$(date +%s%N)
            HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 \
                -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
                -H "Content-Type: application/json" \
                -H "X-API-Key: $API_KEY" \
                -d "{\"cmd\":\"echo\",\"args\":[\"r$REQ_NUM\"],\"timeout\":10}" 2>/dev/null || echo "000")
            END=$(date +%s%N)
            MS=$(( (END - START) / 1000000 ))
            echo "$HTTP_CODE $MS" > "$RESULTS_DIR/$REQ_NUM"
        ) &
        PIDS+=($!)
    done
    for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done

    # Progress every 10 waves (500 requests)
    if [ $((wave % 10)) -eq 0 ]; then
        DONE=$((wave * CONCURRENCY))
        printf '  [%d/%d] %d requests completed...\r' "$DONE" "$TOTAL_REQUESTS" "$DONE"
    fi
done

END_ALL=$(date +%s)
ELAPSED=$((END_ALL - START_ALL))
echo ""

# Aggregate results
ALL_LATENCIES=""
for i in $(seq 1 "$TOTAL_REQUESTS"); do
    LINE=$(cat "$RESULTS_DIR/$i" 2>/dev/null || echo "000 0")
    CODE=$(echo "$LINE" | awk '{print $1}')
    MS=$(echo "$LINE" | awk '{print $2}')
    ALL_LATENCIES="$ALL_LATENCIES $MS"

    case "$CODE" in
        200) TOTAL_OK=$((TOTAL_OK + 1)) ;;
        502) TOTAL_502=$((TOTAL_502 + 1)) ;;
        000) TOTAL_TIMEOUT=$((TOTAL_TIMEOUT + 1)) ;;
        *)   TOTAL_OTHER=$((TOTAL_OTHER + 1)) ;;
    esac
done
rm -rf "$RESULTS_DIR"

# Calculate percentiles (write to temp file to avoid pipe issues with large data)
SORTED_FILE=$(mktemp)
echo "$ALL_LATENCIES" | tr ' ' '\n' | sort -n | grep -v '^$' > "$SORTED_FILE"
COUNT=$(wc -l < "$SORTED_FILE" | tr -d ' ')
P50_IDX=$((COUNT * 50 / 100))
P95_IDX=$((COUNT * 95 / 100))
P99_IDX=$((COUNT * 99 / 100))
P50=$(sed -n "${P50_IDX}p" "$SORTED_FILE")
P95=$(sed -n "${P95_IDX}p" "$SORTED_FILE")
P99=$(sed -n "${P99_IDX}p" "$SORTED_FILE")
MIN=$(head -1 "$SORTED_FILE")
MAX=$(tail -1 "$SORTED_FILE")
rm -f "$SORTED_FILE"

RPS=$((TOTAL_REQUESTS / (ELAPSED > 0 ? ELAPSED : 1)))

echo ""
h "Results"
printf '  Total requests:  %d\n' "$TOTAL_REQUESTS"
printf '  Duration:        %ds\n' "$ELAPSED"
printf '  Throughput:      %d req/s\n' "$RPS"
echo ""
printf '  ✓ 200 OK:        %d\n' "$TOTAL_OK"
printf '  ✗ 502:           %d\n' "$TOTAL_502"
printf '  ✗ Timeout:       %d\n' "$TOTAL_TIMEOUT"
printf '  ✗ Other errors:  %d\n' "$TOTAL_OTHER"
echo ""
printf '  Latency p50:     %sms\n' "$P50"
printf '  Latency p95:     %sms\n' "$P95"
printf '  Latency p99:     %sms\n' "$P99"
printf '  Latency min:     %sms\n' "$MIN"
printf '  Latency max:     %sms\n' "$MAX"
echo ""

ERROR_RATE=$(( (TOTAL_502 + TOTAL_OTHER + TOTAL_TIMEOUT) * 100 / TOTAL_REQUESTS ))
[ "$TOTAL_502" -eq 0 ] && pass "Zero 502 errors" || fail "$TOTAL_502 x 502 errors"
[ "$ERROR_RATE" -le 1 ] && pass "Error rate: ${ERROR_RATE}% (threshold: 1%)" || fail "Error rate: ${ERROR_RATE}%"

summary
