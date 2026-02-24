#!/usr/bin/env bash
set -euo pipefail

# Heavy benchmark: sandbox with running processes + 2GB user data.
#
# Compares:
#   1. Fresh launch + full setup (install packages, start processes, write 2GB)
#   2. Wake from local checkpoint (everything already in place)
#   3. Wake from S3 checkpoint (simulates cross-worker migration)
#
# Usage: ./scripts/bench-heavy.sh <server-url> <api-key>

SERVER="${1:?Usage: $0 <server-url> <api-key>}"
API_KEY="${2:?Usage: $0 <server-url> <api-key>}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

header() { echo -e "\n${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"; echo -e "${BOLD}${CYAN}  $1${NC}"; echo -e "${BOLD}${CYAN}═══════════════════════════════════════════════════════════${NC}"; }
info()   { echo -e "${YELLOW}→${NC} $1"; }
ok()     { echo -e "${GREEN}✓${NC} $1"; }
fail()   { echo -e "${RED}✗${NC} $1"; }
timing() { echo -e "${BOLD}${GREEN}  ⏱  $1${NC}"; }

api() {
  local method="$1" path="$2"; shift 2
  curl -s -w "\n%{http_code}" -X "$method" "${SERVER}${path}" \
    -H "X-API-Key: ${API_KEY}" -H "Content-Type: application/json" "$@"
}

parse_response() {
  local body http_code
  body=$(echo "$1" | sed '$d')
  http_code=$(echo "$1" | tail -1)
  echo "$body"
  [[ "$http_code" -lt 400 ]] || { fail "HTTP $http_code: $body"; return 1; }
}

run_cmd() {
  # run_cmd <sandbox_id> <bash_command> [timeout_secs]
  local sid="$1" cmd="$2" tout="${3:-30}"
  local resp
  resp=$(curl -s --max-time "$tout" -X POST "${EXEC_URL}/sandboxes/${sid}/commands" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "$(python3 -c "import json; print(json.dumps({'cmd':'bash','args':['-c',$(python3 -c "import json; print(json.dumps('$cmd'))")]}))") ")
  echo "$resp"
}

run_cmd_raw() {
  # Passes a raw JSON body — use for complex commands with quotes
  local sid="$1" json_body="$2" tout="${3:-30}"
  curl -s --max-time "$tout" -X POST "${EXEC_URL}/sandboxes/${sid}/commands" \
    -H "${AUTH_HEADER}" -H "Content-Type: application/json" \
    -d "$json_body"
}

# ─────────────────────────────────────────────────────────
header "HEAVY BENCHMARK: Processes + 2GB Data"
echo -e "Server:  ${SERVER}"
echo -e "API Key: ${API_KEY:0:12}..."
echo ""

# ─────────────────────────────────────────────────────────
header "PHASE 1: Create sandbox + full setup"
info "Creating sandbox (python template, 4GB RAM, 4 CPUs)..."

T_TOTAL_START=$(python3 -c 'import time; print(time.time())')
T_START=$T_TOTAL_START

RESP=$(api POST /api/sandboxes -d '{
  "templateID": "python",
  "timeout": 1800,
  "memoryMB": 4096,
  "cpuCount": 4
}')
BODY=$(parse_response "$RESP") || exit 1

T_END=$(python3 -c 'import time; print(time.time())')
CREATE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

SANDBOX_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")
CONNECT_URL=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null || echo "")
TOKEN=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "")
WORKER_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('workerID',''))" 2>/dev/null || echo "")

if [[ -n "$CONNECT_URL" && -n "$TOKEN" ]]; then
  EXEC_URL="${CONNECT_URL}"
  AUTH_HEADER="Authorization: Bearer ${TOKEN}"
else
  EXEC_URL="${SERVER}"
  AUTH_HEADER="X-API-Key: ${API_KEY}"
fi

ok "Sandbox created: ${SANDBOX_ID} on ${WORKER_ID} (${CREATE_MS}ms)"

# --- Install packages ---
info "Installing packages (python packages, curl, etc.)..."
T_START=$(python3 -c 'import time; print(time.time())')

run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","apt-get update -qq && apt-get install -y -qq curl wget redis-tools net-tools > /dev/null 2>&1 && echo done"]}' 120 > /dev/null

T_END=$(python3 -c 'import time; print(time.time())')
APT_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
ok "APT packages installed (${APT_MS}ms)"

info "Installing Python packages..."
T_START=$(python3 -c 'import time; print(time.time())')

run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","pip install --quiet flask requests numpy pandas > /dev/null 2>&1 && echo done"]}' 120 > /dev/null

T_END=$(python3 -c 'import time; print(time.time())')
PIP_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
ok "Python packages installed (${PIP_MS}ms)"

# --- Start background processes ---
info "Starting background processes..."
T_START=$(python3 -c 'import time; print(time.time())')

# Start a Flask web server
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","cat > /tmp/app.py << PYEOF\nfrom flask import Flask\nimport os, time\napp = Flask(__name__)\nstart_time = time.time()\n@app.route(\"/\")\ndef index():\n    return {\"uptime\": time.time() - start_time, \"pid\": os.getpid()}\nif __name__ == \"__main__\":\n    app.run(host=\"0.0.0.0\", port=5000)\nPYEOF\nnohup python3 /tmp/app.py > /tmp/flask.log 2>&1 &\necho started"]}' 10 > /dev/null

# Start a CPU worker process
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","nohup python3 -c \"import time, hashlib; [hashlib.sha256(str(i).encode()).hexdigest() for i in iter(int, 1)]\" > /dev/null 2>&1 &\necho started"]}' 10 > /dev/null

# Start a periodic logger
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","nohup bash -c \"while true; do echo $(date) heartbeat >> /tmp/heartbeat.log; sleep 1; done\" > /dev/null 2>&1 &\necho started"]}' 10 > /dev/null

T_END=$(python3 -c 'import time; print(time.time())')
PROC_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
ok "3 background processes started (${PROC_MS}ms)"

# --- Write 2GB of data ---
info "Writing 2GB of user data (this will take a minute)..."
T_START=$(python3 -c 'import time; print(time.time())')

# Write 2GB in 256MB chunks to avoid command timeouts
for i in $(seq 1 8); do
  run_cmd_raw "$SANDBOX_ID" "{\"cmd\":\"bash\",\"args\":[\"-c\",\"dd if=/dev/urandom of=/tmp/data_${i}.bin bs=1M count=256 2>/dev/null && echo chunk_${i}_done\"]}" 120 > /dev/null
  echo -ne "  Writing chunk ${i}/8 ($(( i * 256 )) MB)...\r"
done
echo ""

T_END=$(python3 -c 'import time; print(time.time())')
DATA_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
DATA_SECS=$(python3 -c "print(f'{($T_END - $T_START):.1f}')")
ok "2GB data written (${DATA_SECS}s)"

# Verify data + processes
info "Verifying sandbox state..."
VERIFY=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo marker=benchmark-heavy-12345 && echo data_size=$(du -sh /tmp/data_*.bin 2>/dev/null | tail -1 | cut -f1) && echo total_files=$(ls /tmp/data_*.bin 2>/dev/null | wc -l) && echo procs=$(ps aux | grep -v grep | grep -c python) && echo flask_up=$(curl -s http://localhost:5000/ | head -c 50)"]}' 10)
echo "$VERIFY" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for line in d.get('stdout','').strip().split('\n'):
    print(f'  {line}')
" 2>/dev/null || echo "  (could not parse verify output)"

T_TOTAL_END=$(python3 -c 'import time; print(time.time())')
SETUP_TOTAL_MS=$(python3 -c "print(f'{($T_TOTAL_END - $T_TOTAL_START) * 1000:.0f}')")
SETUP_TOTAL_SECS=$(python3 -c "print(f'{($T_TOTAL_END - $T_TOTAL_START):.1f}')")

ok "Sandbox fully set up"
timing "Total fresh setup: ${SETUP_TOTAL_SECS}s (${SETUP_TOTAL_MS}ms)"
echo ""
echo -e "  Breakdown:"
echo -e "    Create container:  ${CREATE_MS}ms"
echo -e "    APT install:       ${APT_MS}ms"
echo -e "    pip install:       ${PIP_MS}ms"
echo -e "    Start processes:   ${PROC_MS}ms"
echo -e "    Write 2GB data:    ${DATA_SECS}s"

sleep 2

# ─────────────────────────────────────────────────────────
header "PHASE 2: Hibernate → Wake (local checkpoint)"
info "Hibernating sandbox with 2GB data + 3 running processes..."

T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
BODY=$(parse_response "$RESP") || { fail "Hibernate failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
HIB_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
HIB_SECS=$(python3 -c "print(f'{($T_END - $T_START):.1f}')")

CP_SIZE=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sizeBytes', 0))" 2>/dev/null || echo "0")
CP_SIZE_MB=$(python3 -c "print(f'{int($CP_SIZE) / 1024 / 1024:.1f}')")
ok "Hibernated"
timing "Hibernate: ${HIB_SECS}s (checkpoint: ${CP_SIZE_MB} MB)"

sleep 2

info "Waking from local checkpoint..."
T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/wake" -d '{"timeout": 1800}')
BODY=$(parse_response "$RESP") || { fail "Wake failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
LOCAL_WAKE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
LOCAL_WAKE_SECS=$(python3 -c "print(f'{($T_END - $T_START):.1f}')")

CONNECT_URL2=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null || echo "$CONNECT_URL")
TOKEN2=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "$TOKEN")
if [[ -n "$CONNECT_URL2" && -n "$TOKEN2" ]]; then
  EXEC_URL="${CONNECT_URL2}"
  AUTH_HEADER="Authorization: Bearer ${TOKEN2}"
fi

ok "Woke from local checkpoint"
timing "Local wake: ${LOCAL_WAKE_SECS}s (${LOCAL_WAKE_MS}ms)"

# Verify everything survived
info "Verifying state after local wake..."
VERIFY=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo data_size=$(du -sh /tmp/data_*.bin 2>/dev/null | tail -1 | cut -f1) && echo total_files=$(ls /tmp/data_*.bin 2>/dev/null | wc -l) && echo procs=$(ps aux | grep -v grep | grep -c python) && echo flask=$(curl -s http://localhost:5000/ | head -c 80) && echo heartbeat_lines=$(wc -l < /tmp/heartbeat.log 2>/dev/null || echo 0)"]}' 10)
echo "$VERIFY" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for line in d.get('stdout','').strip().split('\n'):
    print(f'  {line}')
" 2>/dev/null || echo "  (could not parse)"

sleep 2

# ─────────────────────────────────────────────────────────
header "PHASE 3: Hibernate → Purge Local → Wake (S3 migration)"
info "Hibernating again..."

T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
BODY=$(parse_response "$RESP") || { fail "Hibernate failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
HIB2_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
HIB2_SECS=$(python3 -c "print(f'{($T_END - $T_START):.1f}')")

CP2_SIZE=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sizeBytes', 0))" 2>/dev/null || echo "0")
CP2_SIZE_MB=$(python3 -c "print(f'{int($CP2_SIZE) / 1024 / 1024:.1f}')")
ok "Hibernated (checkpoint: ${CP2_SIZE_MB} MB)"
timing "Hibernate: ${HIB2_SECS}s"

info "Purging local checkpoint to force S3 pull..."
# Purge on both workers
ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 ubuntu@18.117.11.151 \
  "sudo find /data/sandboxes/checkpoints -name '*${SANDBOX_ID}*' -exec rm -rf {} + 2>/dev/null; echo purged" 2>/dev/null || true
ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 ubuntu@18.219.23.64 \
  "sudo find /data/sandboxes/checkpoints -name '*${SANDBOX_ID}*' -exec rm -rf {} + 2>/dev/null; echo purged" 2>/dev/null || true
ok "Local checkpoint purged"

sleep 1

info "Waking from S3 (must download checkpoint)..."
T_START=$(python3 -c 'import time; print(time.time())')
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/wake" -d '{"timeout": 1800}')
BODY=$(parse_response "$RESP") || { fail "S3 wake failed"; exit 1; }
T_END=$(python3 -c 'import time; print(time.time())')
S3_WAKE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
S3_WAKE_SECS=$(python3 -c "print(f'{($T_END - $T_START):.1f}')")

CONNECT_URL3=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null || echo "$CONNECT_URL")
TOKEN3=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || echo "$TOKEN")
if [[ -n "$CONNECT_URL3" && -n "$TOKEN3" ]]; then
  EXEC_URL="${CONNECT_URL3}"
  AUTH_HEADER="Authorization: Bearer ${TOKEN3}"
fi

ok "Woke from S3"
timing "S3 wake: ${S3_WAKE_SECS}s (${S3_WAKE_MS}ms)"

info "Verifying state after S3 restore..."
VERIFY=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo data_size=$(du -sh /tmp/data_*.bin 2>/dev/null | tail -1 | cut -f1) && echo total_files=$(ls /tmp/data_*.bin 2>/dev/null | wc -l) && echo procs=$(ps aux | grep -v grep | grep -c python) && echo flask=$(curl -s http://localhost:5000/ | head -c 80)"]}' 10)
echo "$VERIFY" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for line in d.get('stdout','').strip().split('\n'):
    print(f'  {line}')
" 2>/dev/null || echo "  (could not parse)"

# ─────────────────────────────────────────────────────────
# Cleanup
info "Cleaning up — destroying sandbox..."
api DELETE "/api/sandboxes/${SANDBOX_ID}" > /dev/null 2>&1 || true

# ─────────────────────────────────────────────────────────
header "RESULTS SUMMARY"
echo ""
echo -e "  ${BOLD}Sandbox config:${NC}  python template, 4 CPUs, 4GB RAM"
echo -e "  ${BOLD}Payload:${NC}         2GB random data, Flask server, CPU worker, heartbeat logger"
echo -e "  ${BOLD}Checkpoint:${NC}      ${CP_SIZE_MB} MB (1st), ${CP2_SIZE_MB} MB (2nd)"
echo ""
echo -e "  ┌───────────────────────────────────┬────────────┐"
echo -e "  │ Operation                         │ Time       │"
echo -e "  ├───────────────────────────────────┼────────────┤"
echo -e "  │ ${BOLD}Fresh create + full setup${NC}         │ ${BOLD}${RED}${SETUP_TOTAL_SECS}s${NC}     │"
echo -e "  │   └ Container create              │ ${CREATE_MS}ms     │"
echo -e "  │   └ APT packages                  │ ${APT_MS}ms   │"
echo -e "  │   └ pip packages                  │ ${PIP_MS}ms   │"
echo -e "  │   └ Start processes               │ ${PROC_MS}ms     │"
echo -e "  │   └ Write 2GB data                │ ${DATA_SECS}s      │"
echo -e "  ├───────────────────────────────────┼────────────┤"
echo -e "  │ Hibernate (1st)                   │ ${HIB_SECS}s       │"
echo -e "  │ ${BOLD}${GREEN}Wake (local checkpoint)${NC}           │ ${BOLD}${GREEN}${LOCAL_WAKE_SECS}s${NC}      │"
echo -e "  ├───────────────────────────────────┼────────────┤"
echo -e "  │ Hibernate (2nd)                   │ ${HIB2_SECS}s       │"
echo -e "  │ ${BOLD}${GREEN}Wake (S3 → restore)${NC}               │ ${BOLD}${GREEN}${S3_WAKE_SECS}s${NC}      │"
echo -e "  └───────────────────────────────────┴────────────┘"
echo ""
echo -e "  ${BOLD}${CYAN}Speedup (local wake vs fresh):${NC}  $(python3 -c "print(f'{int($SETUP_TOTAL_MS) / max(int($LOCAL_WAKE_MS), 1):.0f}')") x faster"
echo -e "  ${BOLD}${CYAN}Speedup (S3 wake vs fresh):${NC}    $(python3 -c "print(f'{int($SETUP_TOTAL_MS) / max(int($S3_WAKE_MS), 1):.0f}')") x faster"
echo ""
