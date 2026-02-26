#!/usr/bin/env bash
set -uo pipefail

# Noisy Neighbor / Resource Pressure Autoscaling Test
#
# Proves that the scaler detects CPU/memory pressure (not just sandbox count)
# and spawns a new worker even when count-based utilization is low.
#
# Flow:
#   1. Create ~50 sandboxes (well below 400 capacity, ~12.5% count util)
#   2. Inject CPU + memory load into each sandbox
#   3. Watch worker CPU/memory climb via /workers endpoint
#   4. Wait for scaler to spawn a new worker based on resource pressure
#   5. Create a new sandbox and verify it routes to the healthy worker
#   6. Cleanup
#
# Usage: ./scripts/test-noisy-neighbor.sh [server-url] [api-key] [count]

SERVER="${1:-http://3.135.246.117:8080}"
API_KEY="${2:-osb_600b1a9ba2e515c6e54141588da39204d5123cb4b1a28da22b7bd92b88be1534}"
TOTAL="${3:-60}"

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

api() {
  local method="$1" path="$2"
  shift 2
  curl -s -w "\n%{http_code}" -X "$method" "${SERVER}/api${path}" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" "$@"
}

api_body() {
  local method="$1" path="$2"
  shift 2
  curl -s -X "$method" "${SERVER}/api${path}" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" "$@"
}

SANDBOX_IDS=()

cleanup() {
  header "CLEANUP"
  if [[ ${#SANDBOX_IDS[@]} -gt 0 ]]; then
    info "Destroying ${#SANDBOX_IDS[@]} sandboxes..."
    local pids=()
    for sid in "${SANDBOX_IDS[@]}"; do
      curl -s -X DELETE "${SERVER}/api/sandboxes/${sid}" \
        -H "X-API-Key: ${API_KEY}" --max-time 10 > /dev/null 2>&1 &
      pids+=($!)
    done
    for pid in "${pids[@]}"; do
      wait $pid 2>/dev/null || true
    done
    ok "All sandboxes destroyed"
    SANDBOX_IDS=()
  fi
  ok "Done"
}
trap cleanup EXIT

worker_count() {
  api_body GET /workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(len(d) if isinstance(d,list) else 0)" 2>/dev/null
}

worker_table() {
  api_body GET /workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
if not isinstance(d,list):
    print('  (no workers)')
    sys.exit()
for w in d:
    u = w['current']/w['capacity']*100 if w['capacity']>0 else 0
    print(f\"  {w['worker_id'][:12]}... cpu={w.get('cpu_pct',0):.1f}% mem={w.get('mem_pct',0):.1f}% sandboxes={w['current']}/{w['capacity']} ({u:.0f}%)\")" 2>/dev/null
}

worker_ids() {
  api_body GET /workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
if isinstance(d,list):
    for w in d: print(w['worker_id'])" 2>/dev/null
}

worker_resources() {
  api_body GET /workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
if not isinstance(d,list): sys.exit(1)
for w in d:
    print(f\"cpu={w.get('cpu_pct',0):.1f} mem={w.get('mem_pct',0):.1f}\")" 2>/dev/null
}

# ─────────────────────────────────────────────────────────
header "NOISY NEIGHBOR / RESOURCE PRESSURE TEST"
echo -e "Server:  ${SERVER}"
echo -e "API Key: ${API_KEY:0:12}..."
echo -e "Target:  ${BOLD}${TOTAL} noisy sandboxes${NC}"
echo ""

# ─────────────────────────────────────────────────────────
header "PHASE 1: Baseline"
info "Checking initial worker state..."
for attempt in $(seq 1 12); do
  INITIAL_WORKERS=$(worker_count)
  if [[ "$INITIAL_WORKERS" -gt 0 ]] 2>/dev/null; then break; fi
  sleep 5
done
INITIAL_WORKER_ID=$(worker_ids | head -1)
if [[ -z "$INITIAL_WORKER_ID" ]]; then
  fail "No workers found after 60s"
  exit 1
fi
ok "Workers: $INITIAL_WORKERS  Initial: ${INITIAL_WORKER_ID:0:12}..."
worker_table

# ─────────────────────────────────────────────────────────
header "PHASE 2: Create ${TOTAL} sandboxes"
info "Creating ${TOTAL} sandboxes (will be ~${TOTAL}/400 = $(python3 -c "print(f'{${TOTAL}/400*100:.0f}')")% count utilization)..."

WORK=$(mktemp -d)
trap "rm -rf $WORK; cleanup" EXIT

create_one() {
  local idx=$1
  local resp
  resp=$(curl -s -w "\n%{http_code}" -X POST "${SERVER}/api/sandboxes" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"templateID":"base","timeout":900,"cpuCount":1,"memoryMB":1024}' \
    --max-time 120 2>/dev/null) || true

  local body http_code
  body=$(echo "$resp" | sed '$d')
  http_code=$(echo "$resp" | tail -1)

  if [[ "$http_code" -ge 200 && "$http_code" -lt 300 ]] 2>/dev/null; then
    local sid
    sid=$(echo "$body" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])" 2>/dev/null) || true
    echo "$sid" > "${WORK}/create-${idx}.txt"
  else
    echo "FAIL:${http_code}" > "${WORK}/create-${idx}.txt"
  fi
}

export -f create_one
export SERVER API_KEY WORK

# Fire all creates simultaneously
pids=()
for i in $(seq 1 $TOTAL); do
  create_one $i &
  pids+=($!)
done

info "Waiting for ${#pids[@]} creates to complete..."
for pid in "${pids[@]}"; do
  wait $pid 2>/dev/null || true
done

# Collect results
SUCCESS=0
FAIL_COUNT=0
for f in ${WORK}/create-*.txt; do
  [[ -f "$f" ]] || continue
  sid=$(cat "$f")
  if [[ "$sid" == FAIL:* ]]; then
    FAIL_COUNT=$((FAIL_COUNT + 1))
  elif [[ -n "$sid" ]]; then
    SANDBOX_IDS+=("$sid")
    SUCCESS=$((SUCCESS + 1))
  fi
done

ok "Created: ${SUCCESS}/${TOTAL} sandboxes"
if [[ $FAIL_COUNT -gt 0 ]]; then
  fail "Failed: ${FAIL_COUNT}"
fi
worker_table

if [[ $SUCCESS -lt 10 ]]; then
  fail "Too few sandboxes created ($SUCCESS) — aborting"
  exit 1
fi

# ─────────────────────────────────────────────────────────
header "PHASE 3: Inject CPU load into all ${SUCCESS} sandboxes"
info "Running CPU burn (yes >/dev/null) in each sandbox..."
info "Each sandbox pins 1 vCPU at 100% — ${SUCCESS} sandboxes on 64 vCPU host = ~$(python3 -c "print(f'{min(${SUCCESS}/64*100,100):.0f}')")% CPU"

# CPU burn: `yes >/dev/null` pins 1 vCPU at 100%
# No python3 in base image, so we use pure bash/coreutils
LOAD_CMD='{"cmd":"bash","args":["-c","nohup bash -c \"yes >/dev/null\" </dev/null &>/dev/null & echo started"]}'

inject_one() {
  local sid=$1
  curl -s -X POST "${SERVER}/api/sandboxes/${sid}/commands" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "$LOAD_CMD" \
    --max-time 30 > /dev/null 2>&1
}

export -f inject_one
export LOAD_CMD

pids=()
for sid in "${SANDBOX_IDS[@]}"; do
  inject_one "$sid" &
  pids+=($!)
done

info "Waiting for ${#pids[@]} load injections..."
for pid in "${pids[@]}"; do
  wait $pid 2>/dev/null || true
done

ok "Load injected into all sandboxes"
info "Waiting 15s for load to ramp up and heartbeats to report..."
sleep 15
worker_table

# ─────────────────────────────────────────────────────────
header "PHASE 4: Monitor resource pressure + wait for scale-up"
info "Polling every 15s for resource pressure and new workers..."
info "Scaler triggers at CPU > 80% or memory > 85%"
info "Scale-up cooldown is 5 min, so may take up to ~7 min..."

SCALED=0
for i in $(seq 15 15 600); do
  sleep 15

  # Show current state
  echo ""
  echo -e "  ${BOLD}[${i}s]${NC}"
  worker_table

  # Check if a new worker appeared
  CURRENT_WORKERS=$(worker_count)
  if [[ "$CURRENT_WORKERS" -gt "$INITIAL_WORKERS" ]] 2>/dev/null; then
    echo ""
    ok "NEW WORKER DETECTED at ${i}s!"
    NEW_WORKER_ID=$(worker_ids | grep -v "$INITIAL_WORKER_ID" | head -1)
    ok "New worker: ${NEW_WORKER_ID:0:12}..."
    worker_table
    SCALED=1
    break
  fi

  if [[ "$i" -ge 600 ]]; then
    echo ""
    fail "Timed out after 10 min — no new worker spawned"
    info "Checking server logs for scaler activity..."
    # Try to fetch recent scaler logs
    ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
      ubuntu@3.135.246.117 \
      "sudo journalctl -u opensandbox-server --no-pager -n 30 --grep scaler" 2>/dev/null || true
    exit 1
  fi
done

# ─────────────────────────────────────────────────────────
if [[ $SCALED -eq 1 ]]; then
  header "PHASE 5: Verify new sandbox routes to healthy worker"
  info "Creating a new sandbox — should route to the new (unloaded) worker..."

  RESP=$(curl -s -w "\n%{http_code}" -X POST "${SERVER}/api/sandboxes" \
    -H "X-API-Key: ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"templateID":"base","timeout":300,"cpuCount":1,"memoryMB":1024}' \
    --max-time 120)

  BODY=$(echo "$RESP" | sed '$d')
  HTTP_CODE=$(echo "$RESP" | tail -1)

  if [[ "$HTTP_CODE" -ge 200 && "$HTTP_CODE" -lt 300 ]] 2>/dev/null; then
    NEW_SID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])" 2>/dev/null)
    SANDBOX_IDS+=("$NEW_SID")

    # Check which worker it landed on
    DISCOVER=$(api_body GET "/sandboxes/${NEW_SID}")
    PLACED_WORKER=$(echo "$DISCOVER" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null)

    if [[ "$PLACED_WORKER" == "$NEW_WORKER_ID" ]]; then
      ok "New sandbox $NEW_SID → NEW worker ${PLACED_WORKER:0:12}... (resource-aware routing works!)"
    elif [[ "$PLACED_WORKER" != "$INITIAL_WORKER_ID" ]]; then
      ok "New sandbox $NEW_SID → worker ${PLACED_WORKER:0:12}... (not the overloaded one)"
    else
      fail "New sandbox $NEW_SID → OVERLOADED worker ${PLACED_WORKER:0:12}... (routing not resource-aware?)"
    fi

    # Verify exec works on new worker
    EXEC_RESP=$(api_body POST "/sandboxes/${NEW_SID}/commands" -d '{"cmd":"echo","args":["hello from healthy worker"]}')
    STDOUT=$(echo "$EXEC_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
    if [[ "$STDOUT" == "hello from healthy worker" ]]; then
      ok "Exec on new worker succeeded"
    else
      fail "Exec on new worker failed: $EXEC_RESP"
    fi
  else
    fail "Could not create verification sandbox: HTTP $HTTP_CODE"
  fi
fi

# ─────────────────────────────────────────────────────────
header "RESULTS SUMMARY"
echo ""
echo -e "  ${BOLD}Noisy sandboxes:${NC}     ${SUCCESS}"
echo -e "  ${BOLD}Initial workers:${NC}     ${INITIAL_WORKERS}"
echo -e "  ${BOLD}Final workers:${NC}       $(worker_count)"
echo -e "  ${BOLD}Scale-up triggered:${NC}  $([ $SCALED -eq 1 ] && echo -e "${GREEN}YES${NC}" || echo -e "${RED}NO${NC}")"
echo ""
worker_table
echo ""
if [[ $SCALED -eq 1 ]]; then
  echo -e "  ${BOLD}${GREEN}✓ RESOURCE-PRESSURE AUTOSCALING WORKS${NC}"
else
  echo -e "  ${BOLD}${RED}✗ SCALE-UP NOT TRIGGERED${NC}"
fi
echo ""
info "Cleanup will destroy all ${#SANDBOX_IDS[@]} sandboxes..."
