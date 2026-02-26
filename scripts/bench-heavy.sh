#!/usr/bin/env bash
set -euo pipefail

# Heavy benchmark: sandbox with running processes + 2GB user data on workspace.
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

run_cmd_raw() {
  # Passes a raw JSON body — no client-side timeout, let the command finish
  local sid="$1" json_body="$2"
  curl -s -X POST "${EXEC_URL}/sandboxes/${sid}/commands" \
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
info "Installing system packages (curl, wget, net-tools)..."
T_START=$(python3 -c 'import time; print(time.time())')

RESP=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","apt-get update -qq && apt-get install -y -qq curl wget net-tools > /dev/null 2>&1 && echo done"]}' 180)
EXIT_CODE=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode','?'))" 2>/dev/null || echo "?")

T_END=$(python3 -c 'import time; print(time.time())')
APT_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

if [[ "$EXIT_CODE" == "0" ]]; then
  ok "APT packages installed (${APT_MS}ms)"
else
  fail "APT install failed (exit=$EXIT_CODE, ${APT_MS}ms)"
  echo "  Response: $(echo "$RESP" | head -c 200)"
fi

info "Installing Python packages (flask, requests, numpy, pandas)..."
T_START=$(python3 -c 'import time; print(time.time())')

RESP=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","pip install --quiet flask requests numpy pandas 2>&1 | tail -3 && echo done"]}' 180)
EXIT_CODE=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode','?'))" 2>/dev/null || echo "?")

T_END=$(python3 -c 'import time; print(time.time())')
PIP_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

if [[ "$EXIT_CODE" == "0" ]]; then
  ok "Python packages installed (${PIP_MS}ms)"
else
  fail "pip install failed (exit=$EXIT_CODE, ${PIP_MS}ms)"
  echo "  Response: $(echo "$RESP" | head -c 300)"
fi

# --- Start background processes ---
info "Starting background processes..."
T_START=$(python3 -c 'import time; print(time.time())')

# Start a Flask web server
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","cat > /workspace/app.py << PYEOF\nfrom flask import Flask\nimport os, time\napp = Flask(__name__)\nstart_time = time.time()\n@app.route(\"/\")\ndef index():\n    return {\"uptime\": time.time() - start_time, \"pid\": os.getpid()}\nif __name__ == \"__main__\":\n    app.run(host=\"0.0.0.0\", port=5000)\nPYEOF\nnohup python3 /workspace/app.py > /workspace/flask.log 2>&1 &\nsleep 0.5 && echo started"]}' 10 > /dev/null

# Start a CPU worker process
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","nohup python3 -c \"import time, hashlib\nwhile True:\n    hashlib.sha256(str(time.time()).encode()).hexdigest()\n    time.sleep(0.001)\" > /dev/null 2>&1 &\necho started"]}' 10 > /dev/null

# Start a periodic logger
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","nohup bash -c \"while true; do date >> /workspace/heartbeat.log; sleep 1; done\" > /dev/null 2>&1 &\necho started"]}' 10 > /dev/null

T_END=$(python3 -c 'import time; print(time.time())')
PROC_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
ok "3 background processes started (${PROC_MS}ms)"

# --- Write 2GB of data to /workspace (persists through hibernate) ---
info "Writing 2GB of user data to /workspace..."
T_START=$(python3 -c 'import time; print(time.time())')

# Write 2GB in 256MB chunks
for i in $(seq 1 8); do
  run_cmd_raw "$SANDBOX_ID" "{\"cmd\":\"bash\",\"args\":[\"-c\",\"dd if=/dev/urandom of=/workspace/data_${i}.bin bs=1M count=256 2>/dev/null && echo chunk_${i}_done\"]}" 120 > /dev/null
  echo -ne "  Writing chunk ${i}/8 ($(( i * 256 )) MB)...\r"
done
echo ""

T_END=$(python3 -c 'import time; print(time.time())')
DATA_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")
DATA_SECS=$(python3 -c "print(f'{($T_END - $T_START):.1f}')")
ok "2GB data written to /workspace (${DATA_SECS}s)"

# Write a marker for verification
run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo heavy-benchmark-marker > /workspace/marker.txt"]}' 5 > /dev/null

# Verify data + processes
info "Verifying sandbox state..."
VERIFY=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo marker=$(cat /workspace/marker.txt) && echo data_size=$(du -sh /workspace/data_*.bin 2>/dev/null | tail -1) && echo total_files=$(ls /workspace/data_*.bin 2>/dev/null | wc -l) && echo procs=$(ps aux | grep -v grep | grep -c python || echo 0) && echo flask_up=$(curl -s http://localhost:5000/ | head -c 80)"]}' 10)
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
echo -e "    Create VM:         ${CREATE_MS}ms"
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
VERIFY=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo marker=$(cat /workspace/marker.txt) && echo data_files=$(ls /workspace/data_*.bin 2>/dev/null | wc -l) && echo data_size=$(du -sh /workspace/data_*.bin 2>/dev/null | tail -1)"]}' 10)
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

info "Waiting for async S3 upload to finish..."
sleep 30

info "Purging snapshot files + all NVMe cache (keeping drives for restore)..."
ssh -i ~/.ssh/opensandbox-digger.pem -o StrictHostKeyChecking=no -o ConnectTimeout=5 ubuntu@18.117.11.151 \
  "sudo rm -rf /data/sandboxes/sandboxes/${SANDBOX_ID}/snapshot /data/sandboxes/sandboxes/${SANDBOX_ID}/checkpoint.tar.zst && sudo rm -rf /data/sandboxes/checkpoints && sudo mkdir -p /data/sandboxes/checkpoints && echo purged" 2>/dev/null || true
ok "Snapshot + cache purged (drives kept)"

sleep 2

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
VERIFY=$(run_cmd_raw "$SANDBOX_ID" '{"cmd":"bash","args":["-c","echo marker=$(cat /workspace/marker.txt) && echo data_files=$(ls /workspace/data_*.bin 2>/dev/null | wc -l) && echo data_size=$(du -sh /workspace/data_*.bin 2>/dev/null | tail -1)"]}' 10)
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
echo -e "  ${BOLD}Payload:${NC}         2GB random data on /workspace, Flask server, CPU worker, logger"
echo -e "  ${BOLD}Checkpoint:${NC}      ${CP_SIZE_MB} MB (1st), ${CP2_SIZE_MB} MB (2nd)"
echo ""
echo -e "  ┌───────────────────────────────────┬──────────────┐"
echo -e "  │ Operation                         │ Time         │"
echo -e "  ├───────────────────────────────────┼──────────────┤"
printf  "  │ ${BOLD}Fresh create + full setup${NC}         │ ${BOLD}${RED}%10ss${NC}  │\n" "$SETUP_TOTAL_SECS"
printf  "  │   └ VM create                     │ %10sms │\n" "$CREATE_MS"
printf  "  │   └ APT packages                  │ %10sms │\n" "$APT_MS"
printf  "  │   └ pip packages                  │ %10sms │\n" "$PIP_MS"
printf  "  │   └ Start processes               │ %10sms │\n" "$PROC_MS"
printf  "  │   └ Write 2GB data                │ %10ss  │\n" "$DATA_SECS"
echo -e "  ├───────────────────────────────────┼──────────────┤"
printf  "  │ Hibernate (1st, %5s MB)         │ %10ss  │\n" "$CP_SIZE_MB" "$HIB_SECS"
printf  "  │ ${BOLD}${GREEN}Wake (local checkpoint)${NC}           │ ${BOLD}${GREEN}%10ss${NC}  │\n" "$LOCAL_WAKE_SECS"
echo -e "  ├───────────────────────────────────┼──────────────┤"
printf  "  │ Hibernate (2nd, %5s MB)         │ %10ss  │\n" "$CP2_SIZE_MB" "$HIB2_SECS"
printf  "  │ ${BOLD}${GREEN}Wake (S3 → restore)${NC}               │ ${BOLD}${GREEN}%10ss${NC}  │\n" "$S3_WAKE_SECS"
echo -e "  └───────────────────────────────────┴──────────────┘"
echo ""
echo -e "  ${BOLD}${CYAN}Speedup (local wake vs fresh):${NC}  $(python3 -c "print(f'{int($SETUP_TOTAL_MS) / max(int($LOCAL_WAKE_MS), 1):.0f}')") x faster"
echo -e "  ${BOLD}${CYAN}Speedup (S3 wake vs fresh):${NC}     $(python3 -c "print(f'{int($SETUP_TOTAL_MS) / max(int($S3_WAKE_MS), 1):.0f}')") x faster"
echo ""
