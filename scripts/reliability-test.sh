#!/usr/bin/env bash
set -uo pipefail

# Reliability test: runs benchmarks + TypeScript SDK tests in a loop.
# Usage: ./scripts/reliability-test.sh [iterations]

ITERATIONS="${1:-10}"
SERVER="https://app.opensandbox.ai"
API_KEY="osb_600b1a9ba2e515c6e54141588da39204d5123cb4b1a28da22b7bd92b88be1534"
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"

# Export env vars for SDK tests
export OPENSANDBOX_API_KEY="$API_KEY"
export OPENSANDBOX_API_URL="$SERVER"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

RESULTS_FILE="/tmp/reliability-results-$(date +%s).txt"

pass=0
fail=0

cleanup_sandboxes() {
  for id in $(curl -s "$SERVER/api/sandboxes" -H "X-API-Key: $API_KEY" 2>/dev/null | python3 -c "import sys,json; [print(s['sandboxID']) for s in json.load(sys.stdin)]" 2>/dev/null); do
    curl -s -X DELETE "$SERVER/api/sandboxes/$id" -H "X-API-Key: $API_KEY" > /dev/null 2>&1
  done
}

run_test() {
  local iter="$1" name="$2" cmd="$3"
  echo -e "\n${BOLD}${CYAN}━━━ Run $iter/$ITERATIONS: $name ━━━${NC}"
  local t_start=$(python3 -c 'import time; print(time.time())')

  eval "$cmd" > "/tmp/reliability-run-${iter}-${name}.log" 2>&1
  local exit_code=$?

  local t_end=$(python3 -c 'import time; print(time.time())')
  local elapsed=$(python3 -c "print(f'{($t_end - $t_start):.1f}')")

  if [[ $exit_code -eq 0 ]]; then
    echo -e "  ${GREEN}✓${NC} ${name} passed (${elapsed}s)"
    echo "PASS  iter=$iter  test=$name  time=${elapsed}s" >> "$RESULTS_FILE"
    ((pass++))
  else
    echo -e "  ${RED}✗${NC} ${name} FAILED (exit=$exit_code, ${elapsed}s)"
    echo "FAIL  iter=$iter  test=$name  time=${elapsed}s  exit=$exit_code" >> "$RESULTS_FILE"
    echo "  Log: /tmp/reliability-run-${iter}-${name}.log"
    ((fail++))
  fi
}

echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${CYAN}  RELIABILITY TEST: $ITERATIONS iterations${NC}"
echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo ""
echo "Results log: $RESULTS_FILE"

T_TOTAL_START=$(python3 -c 'import time; print(time.time())')

for i in $(seq 1 "$ITERATIONS"); do
  echo -e "\n${BOLD}${YELLOW}╔══════════════════════════════════════════╗${NC}"
  echo -e "${BOLD}${YELLOW}║  ITERATION $i / $ITERATIONS                          ║${NC}"
  echo -e "${BOLD}${YELLOW}╚══════════════════════════════════════════╝${NC}"

  # Clean slate
  cleanup_sandboxes

  # Light benchmark (gRPC, runs on worker)
  run_test "$i" "bench-grpc" "bash ${REPO_DIR}/scripts/bench-grpc.sh ubuntu@18.117.11.151 ~/.ssh/opensandbox-digger.pem"

  # Clean slate
  cleanup_sandboxes

  # TypeScript SDK tests (subshell to avoid cwd contamination)
  run_test "$i" "sdk-tests" "(cd ${REPO_DIR}/sdks/typescript && npx tsx examples/run-all-tests.ts)"

  # Clean slate
  cleanup_sandboxes
done

T_TOTAL_END=$(python3 -c 'import time; print(time.time())')
TOTAL_SECS=$(python3 -c "print(f'{($T_TOTAL_END - $T_TOTAL_START):.0f}')")
TOTAL_MINS=$(python3 -c "print(f'{($T_TOTAL_END - $T_TOTAL_START) / 60:.1f}')")

echo -e "\n${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${CYAN}  RESULTS SUMMARY${NC}"
echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "  Iterations: $ITERATIONS"
echo -e "  Total time: ${TOTAL_MINS} min (${TOTAL_SECS}s)"
echo -e "  ${GREEN}Passed: $pass${NC}"
echo -e "  ${RED}Failed: $fail${NC}"
echo ""

if [[ $fail -gt 0 ]]; then
  echo -e "  ${RED}${BOLD}FAILURES:${NC}"
  grep "^FAIL" "$RESULTS_FILE" | while read -r line; do
    echo -e "    ${RED}$line${NC}"
  done
  echo ""
fi

echo -e "  Full results: $RESULTS_FILE"
echo ""
