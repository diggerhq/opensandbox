#!/usr/bin/env bash
set -o pipefail

SERVER="https://app.opensandbox.ai"
API_KEY="osb_600b1a9ba2e515c6e54141588da39204d5123cb4b1a28da22b7bd92b88be1534"
AUTH_HEADER="X-API-Key: $API_KEY"
SSH="ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5"
CP=ubuntu@3.135.246.117

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

SANDBOX_IDS=()

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}   $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
header(){ echo -e "\n${BOLD}${CYAN}━━━ $* ━━━${NC}"; }

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -s -X "$method" "$SERVER/api$path" -H "$AUTH_HEADER" -H "Content-Type: application/json" -d "$body"
  else
    curl -s -X "$method" "$SERVER/api$path" -H "$AUTH_HEADER"
  fi
}

create_sandbox() {
  local resp sid
  for retry in 1 2 3; do
    resp=$(api POST /sandboxes '{"template":"base","timeout":600}')
    sid=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
    if [[ -n "$sid" && "$sid" != "None" && "$sid" != "" ]]; then
      echo "$sid"
      return 0
    fi
    sleep 3
  done
  echo "FAILED: $resp" >&2
  return 1
}

worker_of()  { api GET "/sandboxes/$1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null; }
workers()    { api GET /workers; }
worker_count(){ workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
if isinstance(d,list): print(len(d))
else: print(0)
" 2>/dev/null; }
worker_ids() { workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
if isinstance(d,list):
    for w in d: print(w['worker_id'])
" 2>/dev/null; }
worker_table(){ workers | python3 -c "
import sys,json
d=json.load(sys.stdin)
if not isinstance(d,list): print('  (no workers)'); sys.exit()
for w in d:
    u = w['current']/w['capacity']*100 if w['capacity']>0 else 0
    print(f\"  {w['worker_id']}: {w['current']}/{w['capacity']} ({u:.0f}%)\")
" 2>/dev/null; }

cleanup() {
  header "CLEANUP"
  if [[ ${#SANDBOX_IDS[@]} -gt 0 ]]; then
    info "Destroying ${#SANDBOX_IDS[@]} sandboxes..."
    for sid in "${SANDBOX_IDS[@]}"; do
      api DELETE "/sandboxes/$sid" > /dev/null 2>&1
    done
    SANDBOX_IDS=()
  fi
  ok "Done"
}
trap cleanup EXIT

# ═══════════════════════════════════════════════════════════
header "PHASE 1: Baseline"
# ═══════════════════════════════════════════════════════════
info "Waiting for worker registry to populate..."
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
info "Workers: $INITIAL_WORKERS  Initial: $INITIAL_WORKER_ID"
worker_table

# ═══════════════════════════════════════════════════════════
header "PHASE 2: Fill to 80% (4/5 sandboxes)"
# ═══════════════════════════════════════════════════════════
for i in 1 2 3 4; do
  sid=$(create_sandbox)
  SANDBOX_IDS+=("$sid")
  ok "$i/4: $sid → $(worker_of "$sid")"
done
sleep 5
worker_table

# ═══════════════════════════════════════════════════════════
header "PHASE 3: Wait for new worker"
# ═══════════════════════════════════════════════════════════
info "Polling every 10s (up to 15 min)..."
for i in $(seq 10 10 900); do
  sleep 10
  n=$(worker_count)
  echo -e "  ${i}s  workers=$n"
  if [[ "$n" -gt "$INITIAL_WORKERS" ]]; then
    ok "New worker appeared at ${i}s!"
    worker_table
    NEW_WORKER_ID=$(worker_ids | grep -v "$INITIAL_WORKER_ID" | head -1)
    info "New worker: $NEW_WORKER_ID"
    break
  fi
  if [[ "$i" -ge 900 ]]; then
    fail "Timed out after 15 min"
    $SSH $CP "sudo journalctl -u opensandbox-server --no-pager -n 20 --grep scaler" 2>/dev/null
    exit 1
  fi
done

# ═══════════════════════════════════════════════════════════
header "PHASE 4: Place sandbox on new worker"
# ═══════════════════════════════════════════════════════════
sid=$(create_sandbox)
SANDBOX_IDS+=("$sid")
w=$(worker_of "$sid")
if [[ "$w" == "$NEW_WORKER_ID" ]]; then
  ok "$sid → NEW worker $w"
else
  warn "$sid → $w (expected $NEW_WORKER_ID)"
fi

# ═══════════════════════════════════════════════════════════
header "PHASE 5: Cross-worker migration"
# ═══════════════════════════════════════════════════════════
MIGRATE_SID=""
info "Looking for sandbox on initial worker ($INITIAL_WORKER_ID) from ${#SANDBOX_IDS[@]} sandboxes..."
for sid in "${SANDBOX_IDS[@]}"; do
  w=$(worker_of "$sid")
  info "  $sid → worker=$w"
  if [[ "$w" == "$INITIAL_WORKER_ID" ]]; then MIGRATE_SID="$sid"; break; fi
done

if [[ -z "$MIGRATE_SID" ]]; then
  warn "No sandbox on initial worker to migrate"
else
  info "Hibernate $MIGRATE_SID (on $INITIAL_WORKER_ID)..."
  hib=$(api POST "/sandboxes/$MIGRATE_SID/hibernate")
  echo "$hib" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'  status={d.get(\"status\")} key={d.get(\"checkpointKey\",\"\")[:40]}')" 2>/dev/null

  info "Waiting 60s for S3 upload (includes workspace.ext4)..."
  sleep 60

  info "Waking $MIGRATE_SID..."
  wake=$(api POST "/sandboxes/$MIGRATE_SID/wake" '{"timeout":300}')
  wake_worker=$(echo "$wake" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null)

  if [[ "$wake_worker" != "$INITIAL_WORKER_ID" && -n "$wake_worker" && "$wake_worker" != "None" ]]; then
    ok "MIGRATED: $INITIAL_WORKER_ID → $wake_worker"
  elif [[ "$wake_worker" == "$INITIAL_WORKER_ID" ]]; then
    warn "Woke on same worker (it was least-loaded)"
  else
    fail "Wake failed: $wake"
  fi
fi

# ═══════════════════════════════════════════════════════════
header "PHASE 6: Exec on both workers"
# ═══════════════════════════════════════════════════════════
info "Testing exec on ${#SANDBOX_IDS[@]} sandboxes..."
for sid in "${SANDBOX_IDS[@]}"; do
  w=$(worker_of "$sid")
  resp=$(api POST "/sandboxes/$sid/commands" '{"cmd":"echo ok"}')
  ec=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode','?'))" 2>/dev/null)
  [[ "$ec" == "0" ]] && ok "$sid ($w)" || fail "$sid ($w) exit=$ec resp=$resp"
done

# ═══════════════════════════════════════════════════════════
header "PHASE 7: Scale down"
# ═══════════════════════════════════════════════════════════
info "Destroying ${#SANDBOX_IDS[@]} sandboxes..."
for sid in "${SANDBOX_IDS[@]}"; do
  resp=$(api DELETE "/sandboxes/$sid")
  info "  destroyed $sid"
done
SANDBOX_IDS=()
info "Waiting 30s for worker counts to update..."
sleep 30
worker_table

info "Waiting for scale-down (up to 5 min)..."
for i in $(seq 10 10 300); do
  sleep 10
  n=$(worker_count)
  echo -e "  ${i}s  workers=$n"
  if [[ "$n" -le "$INITIAL_WORKERS" ]]; then
    ok "Scaled down at ${i}s"
    break
  fi
done

header "DONE"
worker_table
