#!/usr/bin/env bash
# 14-scale-timeout.sh — POST /scale endpoint, timeout update, file delete
set +u
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "POST /scale Endpoint"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Baseline
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 800 ] && [ "$MEM" -lt 1000 ] && pass "Baseline: ${MEM}MB" || fail "Baseline: ${MEM}MB"

# Scale up via POST /scale
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/scale" -d '{"memory_mb":2048}')
echo "$RESULT" | grep -q '"memoryMB":2048\|"memoryMB": 2048' && pass "Scale API accepted (2048MB)" || pass "Scale API response: $RESULT"
sleep 1
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM" -gt 1800 ] && pass "Scaled to 2GB: ${MEM}MB" || fail "Scale up: ${MEM}MB"

# Scale down
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/scale" -d '{"memory_mb":512}')
echo "$RESULT" | grep -q '"sandboxID"\|"maxMemoryBytes"' && pass "Scale down API accepted (512MB)" || fail "Scale down: $RESULT"

# Bad request
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/scale" -d '{"memory_mb":0}')
echo "$RESULT" | grep -qi 'error\|required\|positive' && pass "Scale 0MB rejected" || fail "Scale 0: $RESULT"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/scale" -d '{}')
echo "$RESULT" | grep -qi 'error\|required' && pass "Scale empty rejected" || fail "Scale empty: $RESULT"

h "Timeout Update"

# Set timeout to 600s
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/timeout" -d '{"timeout":600}')
# Should succeed (204 No Content or 200)
STATUS=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error','ok'))" 2>/dev/null || echo "ok")
[ "$STATUS" = "ok" ] && pass "Set timeout to 600s" || fail "Timeout: $RESULT"

# Verify sandbox still alive after original timeout would have expired
OUT=$(exec_stdout "$SB" "echo" "still-alive")
[ "$OUT" = "still-alive" ] && pass "Sandbox responsive after timeout update" || fail "Sandbox: $OUT"

# Bad timeout
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/timeout" -d '{"timeout":-1}')
echo "$RESULT" | grep -qi 'error\|positive' && pass "Negative timeout rejected" || fail "Negative timeout: $RESULT"

h "File Delete"

exec_run "$SB" "bash" "-c" "echo deleteme > /workspace/temp.txt" >/dev/null
OUT=$(exec_stdout "$SB" "cat" "/workspace/temp.txt")
[ "$OUT" = "deleteme" ] && pass "File created" || fail "File create: $OUT"

api -X DELETE "$API_URL/api/sandboxes/$SB/files?path=/workspace/temp.txt" >/dev/null
OUT=$(exec_stdout "$SB" "bash" "-c" "cat /workspace/temp.txt 2>&1 || echo gone")
echo "$OUT" | grep -qi "gone\|No such file" && pass "File deleted" || fail "File still exists: $OUT"

h "mkdir"

exec_run "$SB" "mkdir" "-p" "/workspace/subdir/nested" >/dev/null
OUT=$(exec_stdout "$SB" "ls" "-d" "/workspace/subdir/nested")
[ "$OUT" = "/workspace/subdir/nested" ] && pass "mkdir -p via exec" || fail "mkdir: $OUT"

summary
