#!/usr/bin/env bash
set -euo pipefail

# Benchmark sandbox launch times via gRPC (runs on worker directly).
#
# Tests:
#   1. Fresh launch (cold start)
#   2. Hibernate → local wake (snapshot restore, same machine)
#   3. Hibernate → purge local → S3 wake (snapshot restore, cross-machine)
#   4. Process survival verification (the "money test")
#
# Usage: ./scripts/bench-grpc.sh <worker-ssh> <ssh-key>
# Example: ./scripts/bench-grpc.sh ubuntu@18.117.11.151 ~/.ssh/opensandbox-digger.pem

WORKER_SSH="${1:?Usage: $0 <user@host> <ssh-key>}"
SSH_KEY="${2:?Usage: $0 <user@host> <ssh-key>}"

SSH="ssh -i $SSH_KEY -o StrictHostKeyChecking=no $WORKER_SSH"

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

grpc() {
  $SSH "grpcurl -plaintext -import-path /tmp -proto worker.proto $* localhost:9090"
}

# Ensure grpcurl + proto are available on the worker
info "Checking worker prerequisites..."
$SSH "which grpcurl > /dev/null 2>&1" || { fail "grpcurl not found on worker"; exit 1; }
$SSH "test -f /tmp/worker.proto" || { fail "/tmp/worker.proto not found on worker — upload it first"; exit 1; }

# ─────────────────────────────────────────────────────
header "BENCHMARK: Firecracker Snapshot Launch Times"
echo -e "Worker: ${WORKER_SSH}"
echo ""

# ─────────────────────────────────────────────────────
header "TEST 1: Fresh Launch (cold start)"
info "Creating sandbox from ubuntu template..."

T_START=$(python3 -c 'import time; print(time.time())')

CREATE_RESP=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"template":"ubuntu","timeout":600,"cpu_count":1,"memory_mb":512}'"'"' \
  localhost:9090 worker.SandboxWorker/CreateSandbox' 2>&1)

T_END=$(python3 -c 'import time; print(time.time())')
FRESH_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

SANDBOX_ID=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxId'])")
STATUS=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")

ok "Sandbox created: ${SANDBOX_ID} (status: ${STATUS})"
timing "Fresh launch: ${FRESH_MS}ms"

# Write test data + start background process
info "Writing state and starting background process..."
$SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"sh","args":["-c","echo benchmark-marker-12345 > /workspace/bench.txt && date +%s > /workspace/bench-time.txt"]}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' > /dev/null 2>&1

$SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"sh","args":["-c","setsid sleep 9999 </dev/null >/dev/null 2>&1 & echo STARTED"]}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' > /dev/null 2>&1

# Get PID
PID_RESP=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"pgrep","args":["sleep"],"timeout":3}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' 2>&1)
SLEEP_PIDS=$(echo "$PID_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "none")

ok "Background sleep PIDs: $(echo $SLEEP_PIDS | tr '\n' ' ')"

# Verify state
MARKER=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"cat","args":["/workspace/bench.txt"],"timeout":3}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' 2>&1 | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "")

if [[ "$MARKER" == "benchmark-marker-12345" ]]; then
  ok "State verified"
else
  fail "State verification failed (got: $MARKER)"
fi

sleep 1

# ─────────────────────────────────────────────────────
header "TEST 2: Hibernate → Wake (local snapshot restore)"
info "Hibernating sandbox ${SANDBOX_ID}..."

T_START=$(python3 -c 'import time; print(time.time())')

HIB_RESP=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'"}'"'"' \
  localhost:9090 worker.SandboxWorker/HibernateSandbox' 2>&1)

T_END=$(python3 -c 'import time; print(time.time())')
HIB_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

CP_KEY=$(echo "$HIB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['checkpointKey'])")
CP_SIZE=$(echo "$HIB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sizeBytes','0'))")
CP_SIZE_MB=$(python3 -c "print(f'{int(\"$CP_SIZE\") / 1024 / 1024:.1f}')")

ok "Hibernated (checkpoint: ${CP_SIZE_MB} MB, key: ${CP_KEY})"
timing "Hibernate: ${HIB_MS}ms"

sleep 1

info "Waking sandbox (local snapshot restore)..."
T_START=$(python3 -c 'import time; print(time.time())')

WAKE_RESP=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","checkpoint_key":"'"$CP_KEY"'","timeout":600}'"'"' \
  localhost:9090 worker.SandboxWorker/WakeSandbox' 2>&1)

T_END=$(python3 -c 'import time; print(time.time())')
LOCAL_WAKE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

ok "Woke from local snapshot"
timing "Local wake: ${LOCAL_WAKE_MS}ms"

# Verify process survival
info "Verifying process survival after snapshot restore..."
PIDS_AFTER=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"pgrep","args":["sleep"],"timeout":3}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' 2>&1 | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "none")

if [[ "$PIDS_AFTER" == "$SLEEP_PIDS" ]]; then
  ok "Process survival verified — PIDs match: $(echo $PIDS_AFTER | tr '\n' ' ')"
else
  fail "PID mismatch — before: $(echo $SLEEP_PIDS | tr '\n' ' '), after: $(echo $PIDS_AFTER | tr '\n' ' ')"
fi

# Verify state
MARKER=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"cat","args":["/workspace/bench.txt"],"timeout":3}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' 2>&1 | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "")

if [[ "$MARKER" == "benchmark-marker-12345" ]]; then
  ok "Workspace state verified after wake"
else
  fail "Workspace state lost (got: $MARKER)"
fi

sleep 1

# ─────────────────────────────────────────────────────
header "TEST 3: Hibernate → Purge Local → Wake (S3 restore)"
info "Hibernating sandbox again..."

T_START=$(python3 -c 'import time; print(time.time())')

HIB2_RESP=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'"}'"'"' \
  localhost:9090 worker.SandboxWorker/HibernateSandbox' 2>&1)

T_END=$(python3 -c 'import time; print(time.time())')
HIB2_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

CP_KEY2=$(echo "$HIB2_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['checkpointKey'])")
ok "Hibernated"
timing "Hibernate: ${HIB2_MS}ms"

# Purge local snapshot files to force S3 download
info "Purging local snapshot files to force S3 restore..."
$SSH "sudo rm -rf /data/sandboxes/sandboxes/${SANDBOX_ID}/snapshot/" 2>/dev/null || true
ok "Local snapshot files purged"

sleep 1

info "Waking sandbox (must pull snapshot from S3)..."
T_START=$(python3 -c 'import time; print(time.time())')

WAKE2_RESP=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","checkpoint_key":"'"$CP_KEY2"'","timeout":600}'"'"' \
  localhost:9090 worker.SandboxWorker/WakeSandbox' 2>&1)

T_END=$(python3 -c 'import time; print(time.time())')
S3_WAKE_MS=$(python3 -c "print(f'{($T_END - $T_START) * 1000:.0f}')")

ok "Woke from S3 snapshot"
timing "S3 wake: ${S3_WAKE_MS}ms"

# Verify process survival through S3 round-trip
info "Verifying process survival after S3 restore..."
PIDS_S3=$($SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'","command":"pgrep","args":["sleep"],"timeout":3}'"'"' \
  localhost:9090 worker.SandboxWorker/ExecCommand' 2>&1 | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "none")

if [[ "$PIDS_S3" == "$SLEEP_PIDS" ]]; then
  ok "Process survival verified after S3 restore — PIDs: $(echo $PIDS_S3 | tr '\n' ' ')"
else
  fail "PID mismatch — original: $(echo $SLEEP_PIDS | tr '\n' ' '), S3 restore: $(echo $PIDS_S3 | tr '\n' ' ')"
fi

# ─────────────────────────────────────────────────────
# Cleanup
info "Cleaning up — destroying sandbox..."
$SSH 'grpcurl -plaintext -import-path /tmp -proto worker.proto \
  -d '"'"'{"sandbox_id":"'"$SANDBOX_ID"'"}'"'"' \
  localhost:9090 worker.SandboxWorker/DestroySandbox' > /dev/null 2>&1 || true

# ─────────────────────────────────────────────────────
header "RESULTS"
echo ""
echo -e "  ${BOLD}Fresh launch (cold start):${NC}     ${BOLD}${GREEN}${FRESH_MS}ms${NC}"
echo -e "  ${BOLD}Hibernate:${NC}                     ${HIB_MS}ms  (snapshot: ${CP_SIZE_MB} MB)"
echo -e "  ${BOLD}Wake (local snapshot):${NC}          ${BOLD}${GREEN}${LOCAL_WAKE_MS}ms${NC}  (process survival: ✓)"
echo -e "  ${BOLD}Hibernate (2nd):${NC}                ${HIB2_MS}ms"
echo -e "  ${BOLD}Wake (S3 snapshot):${NC}             ${BOLD}${GREEN}${S3_WAKE_MS}ms${NC}  (process survival: ✓)"
echo ""
echo -e "  ${CYAN}Speedup local vs fresh:${NC}        $(python3 -c "print(f'{int(\"$FRESH_MS\") / max(int(\"$LOCAL_WAKE_MS\"), 1):.1f}')") x"
echo -e "  ${CYAN}Speedup S3 vs fresh:${NC}           $(python3 -c "print(f'{int(\"$FRESH_MS\") / max(int(\"$S3_WAKE_MS\"), 1):.1f}')") x"
echo ""
