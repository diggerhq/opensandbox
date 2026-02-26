#!/usr/bin/env bash
set -uo pipefail

# 400-Concurrent Sandbox Load Test
#
# Fires ALL sandbox creates simultaneously — no batching, no mercy.
# Tests real-world worst case: 400 customers show up at once.
#
# Usage: ./scripts/test-400-concurrent.sh [server-url] [api-key] [count]

SERVER="${1:-http://3.135.246.117:8080}"
API_KEY="${2:-osb_600b1a9ba2e515c6e54141588da39204d5123cb4b1a28da22b7bd92b88be1534}"
TOTAL="${3:-400}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

header() { echo -e "\n${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"; echo -e "${BOLD}${CYAN}  $1${NC}"; echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"; }
info()   { echo -e "${YELLOW}→${NC} $1"; }
ok()     { echo -e "${GREEN}✓${NC} $1"; }
fail()   { echo -e "${RED}✗${NC} $1"; }

# Temp dir for results
WORK=$(mktemp -d)
trap "rm -rf $WORK" EXIT

header "CONCURRENT SANDBOX LOAD TEST"
echo -e "Server:  ${SERVER}"
echo -e "API Key: ${API_KEY:0:12}..."
echo -e "Target:  ${BOLD}${TOTAL} sandboxes — all at once${NC}"
echo -e "Config:  1 vCPU, 1024 MB RAM each"
echo ""

# ─────────────────────────────────────────────────────────
# Phase 1: Fire all creates simultaneously
# ─────────────────────────────────────────────────────────
header "PHASE 1: Launching ${TOTAL} sandboxes simultaneously"

create_one() {
  local idx=$1
  local out="${WORK}/create-${idx}.txt"
  local t0=$(python3 -c 'import time; print(int(time.time()*1000))')

  local resp
  resp=$(curl -s -w "\n%{http_code}" -X POST "${SERVER}/api/sandboxes" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"templateID":"base","timeout":600,"cpuCount":1,"memoryMB":1024}' \
    --max-time 180 2>/dev/null) || true

  local t1=$(python3 -c 'import time; print(int(time.time()*1000))')
  local ms=$((t1 - t0))

  local body http_code
  body=$(echo "$resp" | sed '$d')
  http_code=$(echo "$resp" | tail -1)

  if [[ "$http_code" -ge 200 && "$http_code" -lt 300 ]] 2>/dev/null; then
    local sid
    sid=$(echo "$body" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])" 2>/dev/null) || true
    echo "${sid:-},${ms},ok" > "$out"
  else
    echo ",${ms},fail:${http_code}" > "$out"
  fi
}

export -f create_one
export SERVER API_KEY WORK

info "Firing ${TOTAL} create requests..."
T_START=$(python3 -c 'import time; print(int(time.time()*1000))')

# Launch ALL at once
pids=()
for i in $(seq 1 $TOTAL); do
  create_one $i &
  pids+=($!)
done

info "${#pids[@]} background creates launched, waiting for all to complete..."

# Progress monitor in background
(
  while true; do
    sleep 10
    done_count=$(ls ${WORK}/create-*.txt 2>/dev/null | wc -l | tr -d ' ')
    ok_count=$(grep -l ',ok$' ${WORK}/create-*.txt 2>/dev/null | wc -l | tr -d ' ') || true
    fail_count=$(grep -l ',fail' ${WORK}/create-*.txt 2>/dev/null | wc -l | tr -d ' ') || true
    echo -e "  ... ${done_count}/${TOTAL} done (${ok_count} ok, ${fail_count} failed)"
    if [[ "$done_count" -ge "$TOTAL" ]] 2>/dev/null; then break; fi
  done
) &
MONITOR_PID=$!

# Wait for all creates
for pid in "${pids[@]}"; do
  wait $pid 2>/dev/null || true
done

kill $MONITOR_PID 2>/dev/null || true
wait $MONITOR_PID 2>/dev/null || true

T_END=$(python3 -c 'import time; print(int(time.time()*1000))')
WALL_MS=$((T_END - T_START))
WALL_SECS=$(python3 -c "print(f'{$WALL_MS/1000:.1f}')")

# Collect results
SANDBOX_IDS=()
CREATE_TIMES=()
SUCCESS=0
FAIL=0

for f in ${WORK}/create-*.txt; do
  [[ -f "$f" ]] || continue
  line=$(cat "$f")
  id=$(echo "$line" | cut -d, -f1)
  ms=$(echo "$line" | cut -d, -f2)
  status=$(echo "$line" | cut -d, -f3)
  CREATE_TIMES+=("$ms")
  if [[ "$status" == "ok" && -n "$id" ]]; then
    SANDBOX_IDS+=("$id")
    SUCCESS=$((SUCCESS + 1))
  else
    FAIL=$((FAIL + 1))
  fi
done

echo ""
ok "Created: ${SUCCESS}/${TOTAL} sandboxes in ${WALL_SECS}s wall time"
if [[ $FAIL -gt 0 ]]; then
  fail "Failed: ${FAIL}"
  # Show a few failure reasons
  grep ',fail' ${WORK}/create-*.txt 2>/dev/null | head -5 | while read line; do
    echo -e "    ${RED}${line}${NC}"
  done
fi

# Timing stats
if [[ ${#CREATE_TIMES[@]} -gt 0 ]]; then
  SORTED=($(printf '%s\n' "${CREATE_TIMES[@]}" | sort -n))
  NUM=${#SORTED[@]}
  MIN_C=${SORTED[0]}
  MAX_C=${SORTED[$((NUM - 1))]}
  MED_C=${SORTED[$((NUM / 2))]}
  P95_C=${SORTED[$((NUM * 95 / 100))]}
  SUM=0; for t in "${SORTED[@]}"; do SUM=$((SUM + t)); done
  AVG_C=$((SUM / NUM))

  echo ""
  echo -e "  Create latency (per-request):"
  echo -e "    Min:    ${MIN_C}ms"
  echo -e "    Median: ${MED_C}ms"
  echo -e "    Avg:    ${AVG_C}ms"
  echo -e "    P95:    ${P95_C}ms"
  echo -e "    Max:    ${MAX_C}ms"
  echo -e "    Wall:   ${WALL_MS}ms"
fi

if [[ $SUCCESS -eq 0 ]]; then
  fail "No sandboxes created — aborting"
  exit 1
fi

# ─────────────────────────────────────────────────────────
# Phase 2: Execute a command on ALL sandboxes
# ─────────────────────────────────────────────────────────
header "PHASE 2: Execute command on all ${SUCCESS} sandboxes simultaneously"

exec_one() {
  local sid=$1
  local out="${WORK}/exec-${sid}.txt"
  local t0=$(python3 -c 'import time; print(int(time.time()*1000))')

  local resp
  resp=$(curl -s -w "\n%{http_code}" -X POST "${SERVER}/api/sandboxes/${sid}/commands" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"cmd":"echo","args":["alive"]}' \
    --max-time 30 2>/dev/null) || true

  local t1=$(python3 -c 'import time; print(int(time.time()*1000))')
  local ms=$((t1 - t0))
  local http_code=$(echo "$resp" | tail -1)

  if [[ "$http_code" == "200" ]] 2>/dev/null; then
    echo "${ms},ok" > "$out"
  else
    echo "${ms},fail:${http_code}" > "$out"
  fi
}

export -f exec_one

info "Firing ${SUCCESS} exec requests..."
T_EXEC_START=$(python3 -c 'import time; print(int(time.time()*1000))')

pids=()
for sid in "${SANDBOX_IDS[@]}"; do
  exec_one "$sid" &
  pids+=($!)
done

for pid in "${pids[@]}"; do
  wait $pid 2>/dev/null || true
done

T_EXEC_END=$(python3 -c 'import time; print(int(time.time()*1000))')
EXEC_WALL=$((T_EXEC_END - T_EXEC_START))

EXEC_OK=0
EXEC_FAIL=0
EXEC_TIMES=()
for sid in "${SANDBOX_IDS[@]}"; do
  line=$(cat "${WORK}/exec-${sid}.txt" 2>/dev/null) || line="0,missing"
  ms=$(echo "$line" | cut -d, -f1)
  st=$(echo "$line" | cut -d, -f2)
  EXEC_TIMES+=("$ms")
  if [[ "$st" == "ok" ]]; then EXEC_OK=$((EXEC_OK + 1)); else EXEC_FAIL=$((EXEC_FAIL + 1)); fi
done

ok "Exec: ${EXEC_OK}/${SUCCESS} succeeded (wall: ${EXEC_WALL}ms)"
if [[ $EXEC_FAIL -gt 0 ]]; then fail "Exec failed: ${EXEC_FAIL}"; fi

if [[ ${#EXEC_TIMES[@]} -gt 0 ]]; then
  SORTED_E=($(printf '%s\n' "${EXEC_TIMES[@]}" | sort -n))
  NUM_E=${#SORTED_E[@]}
  echo -e "  Exec latency:"
  echo -e "    Min:    ${SORTED_E[0]}ms"
  echo -e "    Median: ${SORTED_E[$((NUM_E / 2))]}ms"
  echo -e "    P95:    ${SORTED_E[$((NUM_E * 95 / 100))]}ms"
  echo -e "    Max:    ${SORTED_E[$((NUM_E - 1))]}ms"
fi

# ─────────────────────────────────────────────────────────
# Phase 3: Worker resource usage snapshot
# ─────────────────────────────────────────────────────────
header "PHASE 3: Worker resource usage with ${SUCCESS} active sandboxes"

ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=10 ubuntu@18.117.11.151 \
  "echo '=== Memory ===' && free -g && echo '' && \
   echo '=== Firecracker VMs ===' && ps aux | grep 'firecracker --api-sock' | grep -v grep | wc -l && echo '' && \
   echo '=== TAP devices ===' && ip link show | grep fc-tap | wc -l && echo '' && \
   echo '=== CPU load ===' && uptime && echo '' && \
   echo '=== Disk ===' && df -h /data" 2>/dev/null || fail "Could not SSH to worker"

# ─────────────────────────────────────────────────────────
# Phase 4: Destroy all sandboxes simultaneously
# ─────────────────────────────────────────────────────────
header "PHASE 4: Destroying ${SUCCESS} sandboxes simultaneously"

T_DEL_START=$(python3 -c 'import time; print(int(time.time()*1000))')

pids=()
for sid in "${SANDBOX_IDS[@]}"; do
  curl -s -X DELETE "${SERVER}/api/sandboxes/${sid}" \
    -H "X-API-Key: ${API_KEY}" --max-time 30 > /dev/null 2>&1 &
  pids+=($!)
done

for pid in "${pids[@]}"; do
  wait $pid 2>/dev/null || true
done

T_DEL_END=$(python3 -c 'import time; print(int(time.time()*1000))')
DEL_MS=$((T_DEL_END - T_DEL_START))

ok "All ${SUCCESS} sandboxes deleted in $((DEL_MS/1000))s"

sleep 5

info "Post-cleanup worker state:"
ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=10 ubuntu@18.117.11.151 \
  "echo 'Firecracker VMs:' && ps aux | grep 'firecracker --api-sock' | grep -v grep | wc -l && \
   echo 'Memory:' && free -g | head -2" 2>/dev/null || true

# ─────────────────────────────────────────────────────────
header "RESULTS SUMMARY"
echo ""
echo -e "  ${BOLD}Target:${NC}              ${TOTAL} sandboxes"
echo -e "  ${BOLD}Created:${NC}             ${GREEN}${SUCCESS}${NC} (${FAIL} failed)"
echo -e "  ${BOLD}Exec success:${NC}        ${GREEN}${EXEC_OK}${NC}/${SUCCESS}"
echo -e "  ${BOLD}Create wall time:${NC}    ${WALL_SECS}s"
echo -e "  ${BOLD}Delete wall time:${NC}    $((DEL_MS/1000))s"
if [[ ${#CREATE_TIMES[@]} -gt 0 ]]; then
  echo -e "  ${BOLD}Create latency:${NC}      min=${MIN_C}ms med=${MED_C}ms avg=${AVG_C}ms p95=${P95_C}ms max=${MAX_C}ms"
fi
echo ""
if [[ $SUCCESS -ge $TOTAL ]]; then
  echo -e "  ${BOLD}${GREEN}✓ ALL ${TOTAL} SANDBOXES CREATED AND EXECUTED SUCCESSFULLY${NC}"
else
  echo -e "  ${BOLD}${RED}✗ Only ${SUCCESS}/${TOTAL} sandboxes created${NC}"
fi
echo ""
