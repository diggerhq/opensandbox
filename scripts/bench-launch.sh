#!/usr/bin/env bash
set -euo pipefail

# Benchmark sandbox launch times:
#   1. Fresh launch (cold start — no checkpoint)
#   2. Wake from local checkpoint (hibernated, data still on disk)
#   3. Wake from S3 checkpoint (simulate migration — purge local, pull from S3)
#
# Usage: ./scripts/bench-launch.sh <server-url> <api-key>
# Example: ./scripts/bench-launch.sh http://3.135.246.117:8080 osb_600b1a...

SERVER="${1:?Usage: $0 <server-url> <api-key>}"
API_KEY="${2:?Usage: $0 <server-url> <api-key>}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

header() { echo -e "\n${BOLD}${CYAN}═══════════════════════════════════════════════════${NC}"; echo -e "${BOLD}${CYAN}  $1${NC}"; echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════${NC}"; }
info()   { echo -e "${YELLOW}→${NC} $1"; }
ok()     { echo -e "${GREEN}✓${NC} $1"; }
fail()   { echo -e "${RED}✗${NC} $1"; }
timing() { echo -e "${BOLD}${GREEN}  ⏱  $1${NC}"; }

api() {
  local method="$1" path="$2"
  shift 2
  curl -s -w "\n%{http_code}" -X "$method" "${SERVER}${path}" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" "$@"
}

parse_response() {
  local body http_code
  body=$(echo "$1" | sed '$d')
  http_code=$(echo "$1" | tail -1)
  echo "$body"
  if [[ "$http_code" -ge 400 ]]; then
    fail "HTTP $http_code: $body"
    return 1
  fi
}

# ─────────────────────────────────────────────────────
header "BENCHMARK: Sandbox Launch Times"
echo -e "Server:  ${SERVER}"
echo -e "API Key: ${API_KEY:0:12}..."
echo ""

# ─────────────────────────────────────────────────────
header "TEST 1: Fresh Launch (cold start)"
info "Creating sandbox from base template..."

T_START=$(python3 -c 'import time; print(time.time())')

RESP=$(api POST /api/sandboxes -d '{
  "templateID": "base",
  "timeout": 600,
  "memoryMB": 512,
  "cpuCount": 2
}')
BODY=$(parse_response "$RESP") || exit 1

T_END=$(python3 -c 'import time; print(time.time())')
FRESH_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

SANDBOX_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")
CONNECT_URL=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null || echo "")
TOKEN=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "")
WORKER_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null || echo "")

ok "Sandbox created: ${SANDBOX_ID}"
ok "Worker: ${WORKER_ID}"
timing "Fresh launch: ${FRESH_MS}ms"

# Write some state so we can verify persistence after wake
info "Writing state into sandbox..."
if [[ -n "$CONNECT_URL" && -n "$TOKEN" ]]; then
  EXEC_URL="${CONNECT_URL}"
  AUTH_HEADER="Authorization: Bearer ${TOKEN}"
else
  EXEC_URL="${SERVER}"
  AUTH_HEADER="X-API-Key: ${API_KEY}"
fi

curl -s -X POST "${EXEC_URL}/sandboxes/${SANDBOX_ID}/commands" \
  -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"bash","args":["-c","echo benchmark-marker-12345 > /tmp/bench.txt && date +%s > /tmp/bench-time.txt"]}' > /dev/null 2>&1

# Verify state
CMD_RESP=$(curl -s -X POST "${EXEC_URL}/sandboxes/${SANDBOX_ID}/commands" \
  -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"bash","args":["-c","cat /tmp/bench.txt"]}')
MARKER=$(echo "$CMD_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','').strip())" 2>/dev/null || echo "")
if [[ "$MARKER" == "benchmark-marker-12345" ]]; then
  ok "State written and verified"
else
  fail "Could not verify state (got: $MARKER)"
fi

sleep 1

# ─────────────────────────────────────────────────────
header "TEST 2: Hibernate → Wake (local checkpoint)"
info "Hibernating sandbox ${SANDBOX_ID}..."

T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
BODY=$(parse_response "$RESP") || { fail "Hibernate failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
HIB_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

CP_SIZE=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sizeBytes', 0))" 2>/dev/null || echo "0")
CP_SIZE_MB=$(python3 -c "print(f'{$CP_SIZE / 1024 / 1024:.1f}')")
ok "Hibernated (checkpoint: ${CP_SIZE_MB} MB)"
timing "Hibernate: ${HIB_MS}ms"

sleep 1

info "Waking sandbox (local checkpoint on same worker)..."
T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/wake" -d '{"timeout": 600}')
BODY=$(parse_response "$RESP") || { fail "Wake failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
LOCAL_WAKE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

# Update connect info in case worker changed
CONNECT_URL2=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null || echo "$CONNECT_URL")
TOKEN2=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "$TOKEN")
if [[ -n "$CONNECT_URL2" && -n "$TOKEN2" ]]; then
  EXEC_URL="${CONNECT_URL2}"
  AUTH_HEADER="Authorization: Bearer ${TOKEN2}"
fi

ok "Woke from local checkpoint"
timing "Local wake: ${LOCAL_WAKE_MS}ms"

# Verify state persisted
info "Verifying state persistence..."
CMD_RESP=$(curl -s -X POST "${EXEC_URL}/sandboxes/${SANDBOX_ID}/commands" \
  -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"bash","args":["-c","cat /tmp/bench.txt"]}')
MARKER=$(echo "$CMD_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','').strip())" 2>/dev/null || echo "")
if [[ "$MARKER" == "benchmark-marker-12345" ]]; then
  ok "State verified after local wake ✓"
else
  fail "State lost after wake (got: $MARKER)"
fi

sleep 1

# ─────────────────────────────────────────────────────
header "TEST 3: Hibernate → Purge Local → Wake (S3 migration)"
info "Hibernating sandbox again..."

T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
BODY=$(parse_response "$RESP") || { fail "Hibernate failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
HIB2_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
ok "Hibernated again"
timing "Hibernate: ${HIB2_MS}ms"

# Find which worker had this sandbox and purge the local checkpoint
info "Purging local checkpoint to simulate S3 migration..."
WAKE_WORKER=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null || echo "$WORKER_ID")

# We need to remove the local checkpoint files from the worker
# The checkpoint lives under DATA_DIR/checkpoints/<sandboxID>/
# We'll SSH to the worker and remove it

# Figure out which worker IP to SSH to
if [[ "$WAKE_WORKER" == *"099088f8"* ]]; then
  WORKER_IP="18.117.11.151"
elif [[ "$WAKE_WORKER" == *"0e250197"* ]]; then
  WORKER_IP="18.219.23.64"
else
  # Try both
  WORKER_IP="18.117.11.151"
fi

# Remove local checkpoint data
ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 "ubuntu@${WORKER_IP}" \
  "sudo find /data/sandboxes/checkpoints -name '*${SANDBOX_ID}*' -exec rm -rf {} + 2>/dev/null; echo 'purged'" 2>/dev/null || info "Could not purge on ${WORKER_IP}, trying other worker..."

# Also try the other worker just in case
if [[ "$WORKER_IP" == "18.117.11.151" ]]; then
  ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 ubuntu@18.219.23.64 \
    "sudo find /data/sandboxes/checkpoints -name '*${SANDBOX_ID}*' -exec rm -rf {} + 2>/dev/null; echo 'purged'" 2>/dev/null || true
else
  ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 ubuntu@18.117.11.151 \
    "sudo find /data/sandboxes/checkpoints -name '*${SANDBOX_ID}*' -exec rm -rf {} + 2>/dev/null; echo 'purged'" 2>/dev/null || true
fi

ok "Local checkpoint purged — will restore from S3"

sleep 1

info "Waking sandbox (must pull from S3)..."
T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/wake" -d '{"timeout": 600}')
BODY=$(parse_response "$RESP") || { fail "S3 wake failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
S3_WAKE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

CONNECT_URL3=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null || echo "$CONNECT_URL")
TOKEN3=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "$TOKEN")
if [[ -n "$CONNECT_URL3" && -n "$TOKEN3" ]]; then
  EXEC_URL="${CONNECT_URL3}"
  AUTH_HEADER="Authorization: Bearer ${TOKEN3}"
fi

ok "Woke from S3 checkpoint"
timing "S3 wake: ${S3_WAKE_MS}ms"

# Verify state persisted through S3 round-trip
info "Verifying state persistence after S3 restore..."
CMD_RESP=$(curl -s -X POST "${EXEC_URL}/sandboxes/${SANDBOX_ID}/commands" \
  -H "${AUTH_HEADER}" \
  -H "Content-Type: application/json" \
  -d '{"cmd":"bash","args":["-c","cat /tmp/bench.txt"]}')
MARKER=$(echo "$CMD_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('stdout','').strip())" 2>/dev/null || echo "")
if [[ "$MARKER" == "benchmark-marker-12345" ]]; then
  ok "State verified after S3 wake ✓"
else
  fail "State lost after S3 wake (got: $MARKER)"
fi

# ─────────────────────────────────────────────────────
# Cleanup
info "Cleaning up — destroying sandbox..."
api DELETE "/api/sandboxes/${SANDBOX_ID}" > /dev/null 2>&1 || true

# ─────────────────────────────────────────────────────
header "RESULTS"
echo ""
echo -e "  ${BOLD}Fresh launch (cold start):${NC}     ${BOLD}${GREEN}${FRESH_MS}ms${NC}"
echo -e "  ${BOLD}Hibernate:${NC}                     ${HIB_MS}ms  (checkpoint: ${CP_SIZE_MB} MB)"
echo -e "  ${BOLD}Wake (local checkpoint):${NC}        ${BOLD}${GREEN}${LOCAL_WAKE_MS}ms${NC}"
echo -e "  ${BOLD}Hibernate (2nd):${NC}                ${HIB2_MS}ms"
echo -e "  ${BOLD}Wake (S3 → restore):${NC}            ${BOLD}${GREEN}${S3_WAKE_MS}ms${NC}"
echo ""
echo -e "  ${CYAN}Speedup local vs fresh:${NC}        $(python3 -c "print(f'{$FRESH_MS / max($LOCAL_WAKE_MS, 1):.1f}')") x"
echo -e "  ${CYAN}Speedup S3 vs fresh:${NC}           $(python3 -c "print(f'{$FRESH_MS / max($S3_WAKE_MS, 1):.1f}')") x"
echo ""
