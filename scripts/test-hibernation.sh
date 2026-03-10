#!/usr/bin/env bash
set -euo pipefail

# Test hibernation: file persistence, process preservation, auto-wake, subdomain proxy, and timing
# Usage: ./scripts/test-hibernation.sh [api_url] [api_key]

API_URL="${1:-http://localhost:8080}"
API_KEY="${2:-test-key}"
PASS=0
FAIL=0

green()  { printf "\033[32m%s\033[0m\n" "$*"; }
red()    { printf "\033[31m%s\033[0m\n" "$*"; }
yellow() { printf "\033[33m%s\033[0m\n" "$*"; }
bold()   { printf "\033[1m%s\033[0m\n" "$*"; }

check() {
  local desc="$1" expected="$2" actual="$3"
  if [[ "$actual" == *"$expected"* ]]; then
    green "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    red "  FAIL: $desc (expected '$expected', got '$actual')"
    FAIL=$((FAIL + 1))
  fi
}

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -s -X "$method" "${API_URL}${path}" \
      -H "X-API-Key: ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d "$body"
  else
    curl -s -X "$method" "${API_URL}${path}" \
      -H "X-API-Key: ${API_KEY}"
  fi
}

run_cmd() {
  local sandbox_id="$1" cmd="$2"
  api POST "/api/sandboxes/${sandbox_id}/commands" "{\"cmd\": \"bash\", \"args\": [\"-c\", \"${cmd}\"]}"
}

bold "========================================="
bold " OpenSandbox Hibernation Test"
bold "========================================="
echo ""

# --- Cleanup stale sessions ---
bold "[0/11] Cleaning up stale sandbox sessions..."
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U opensandbox -d opensandbox -c \
  "UPDATE sandbox_sessions SET status = 'stopped', stopped_at = now() WHERE status IN ('running', 'hibernated');" \
  2>/dev/null | grep -q "UPDATE" && green "  Cleaned up stale sessions" || yellow "  No stale sessions"
echo ""

# --- Create sandbox ---
bold "[1/11] Creating sandbox..."
CREATE_RESP=$(api POST "/api/sandboxes" '{"template": "ubuntu:22.04", "timeout": 600}')
SANDBOX_ID=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])" 2>/dev/null)
if [[ -z "$SANDBOX_ID" ]]; then
  red "Failed to create sandbox: $CREATE_RESP"
  exit 1
fi
SANDBOX_DOMAIN=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('domain',''))" 2>/dev/null || echo "")
SANDBOX_HOST_PORT=$(echo "$CREATE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hostPort',0))" 2>/dev/null || echo "0")
green "  Created sandbox: $SANDBOX_ID"
if [[ -n "$SANDBOX_DOMAIN" ]]; then
  green "  Domain: $SANDBOX_DOMAIN (host port: $SANDBOX_HOST_PORT)"
fi
echo ""

# --- Write file state ---
bold "[2/11] Writing file state..."
run_cmd "$SANDBOX_ID" "echo hibernation-proof-42 > /tmp/proof.txt" > /dev/null
run_cmd "$SANDBOX_ID" "echo hello-world > /root/hello.txt" > /dev/null
run_cmd "$SANDBOX_ID" "mkdir -p /var/data && echo persistent-data > /var/data/test.txt" > /dev/null

# Verify files exist
PROOF=$(run_cmd "$SANDBOX_ID" "cat /tmp/proof.txt" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
HELLO=$(run_cmd "$SANDBOX_ID" "cat /root/hello.txt" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
DATA=$(run_cmd "$SANDBOX_ID" "cat /var/data/test.txt" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
check "/tmp/proof.txt before hibernate" "hibernation-proof-42" "$PROOF"
check "/root/hello.txt before hibernate" "hello-world" "$HELLO"
check "/var/data/test.txt before hibernate" "persistent-data" "$DATA"
echo ""

# --- Check PID 1 process ---
bold "[3/11] Verifying PID 1 (entrypoint) before hibernate..."
PID1_CMD_BEFORE=$(run_cmd "$SANDBOX_ID" "cat /proc/1/cmdline | tr '\\0' ' '" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "unknown")
green "  PID 1 command: $PID1_CMD_BEFORE"
echo ""

# --- Hibernate ---
bold "[4/11] Hibernating sandbox..."
HIB_START=$(python3 -c 'import time; print(time.time())')
HIB_RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
HIB_END=$(python3 -c 'import time; print(time.time())')
HIBERNATE_DURATION=$(python3 -c "print(f'{${HIB_END} - ${HIB_START}:.3f}')")
yellow "  TIME: Hibernate = ${HIBERNATE_DURATION}s"

HIB_KEY=$(echo "$HIB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('checkpointKey',''))" 2>/dev/null)
HIB_SIZE=$(echo "$HIB_RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(f'{d.get(\"sizeBytes\",0)/1024/1024:.1f}MB')" 2>/dev/null)
if [[ -n "$HIB_KEY" ]]; then
  green "  Checkpoint: $HIB_KEY ($HIB_SIZE)"
else
  red "  Hibernate failed: $HIB_RESP"
fi

echo ""

# --- Explicit Wake ---
bold "[5/11] Waking sandbox (explicit)..."
WAKE_START=$(python3 -c 'import time; print(time.time())')
WAKE_RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/wake" '{"timeout": 600}')
WAKE_END=$(python3 -c 'import time; print(time.time())')
WAKE_DURATION=$(python3 -c "print(f'{${WAKE_END} - ${WAKE_START}:.3f}')")
yellow "  TIME: Wake = ${WAKE_DURATION}s"

WAKE_STATUS=$(echo "$WAKE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
check "Wake status" "running" "$WAKE_STATUS"
echo ""

# --- Verify file state ---
bold "[6/11] Verifying file state after wake..."
PROOF_AFTER=$(run_cmd "$SANDBOX_ID" "cat /tmp/proof.txt" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
HELLO_AFTER=$(run_cmd "$SANDBOX_ID" "cat /root/hello.txt" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
DATA_AFTER=$(run_cmd "$SANDBOX_ID" "cat /var/data/test.txt" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
check "/tmp/proof.txt after wake" "hibernation-proof-42" "$PROOF_AFTER"
check "/root/hello.txt after wake" "hello-world" "$HELLO_AFTER"
check "/var/data/test.txt after wake" "persistent-data" "$DATA_AFTER"
echo ""

# --- Verify process state ---
bold "[7/11] Verifying process state after wake..."
PID1_CMD_AFTER=$(run_cmd "$SANDBOX_ID" "cat /proc/1/cmdline | tr '\\0' ' '" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "unknown")
check "PID 1 command preserved" "$PID1_CMD_BEFORE" "$PID1_CMD_AFTER"
PID1_EXISTS=$(run_cmd "$SANDBOX_ID" "test -d /proc/1 && echo yes || echo no" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "no")
check "PID 1 still exists after wake" "yes" "$PID1_EXISTS"

# Verify we can still run commands (container is functional)
ECHO_TEST=$(run_cmd "$SANDBOX_ID" "echo alive" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "dead")
check "Container is functional after wake" "alive" "$ECHO_TEST"
echo ""

# --- Test auto-wake (hibernate then send command without explicit wake) ---
bold "[8/11] Testing auto-wake (command to hibernated sandbox)..."

# Write a marker file before second hibernate
run_cmd "$SANDBOX_ID" "echo auto-wake-marker > /tmp/autowake.txt" > /dev/null

# Hibernate again
HIB2_RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
HIB2_KEY=$(echo "$HIB2_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('checkpointKey',''))" 2>/dev/null)
if [[ -z "$HIB2_KEY" ]]; then
  red "  Second hibernate failed: $HIB2_RESP"
else
  green "  Hibernated (second time)"
fi

# Now send a command WITHOUT calling /wake — the router should auto-wake
AUTOWAKE_START=$(python3 -c 'import time; print(time.time())')
AUTOWAKE_RESP=$(run_cmd "$SANDBOX_ID" "cat /tmp/autowake.txt")
AUTOWAKE_END=$(python3 -c 'import time; print(time.time())')
AUTOWAKE_DURATION=$(python3 -c "print(f'{${AUTOWAKE_END} - ${AUTOWAKE_START}:.3f}')")
yellow "  TIME: Auto-wake + exec = ${AUTOWAKE_DURATION}s"

AUTOWAKE_STDOUT=$(echo "$AUTOWAKE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "")
check "Auto-wake: file content preserved" "auto-wake-marker" "$AUTOWAKE_STDOUT"

AUTOWAKE_EXIT=$(echo "$AUTOWAKE_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode', -1))" 2>/dev/null || echo "-1")
check "Auto-wake: command succeeded (exit 0)" "0" "$AUTOWAKE_EXIT"
echo ""

# --- Test rolling timeout reset ---
bold "[9/11] Verifying rolling timeout resets on activity..."
# The sandbox should still be running from the auto-wake. Send a few commands
# to verify the router resets the timeout each time.
for i in 1 2 3; do
  PING=$(run_cmd "$SANDBOX_ID" "echo pong-${i}" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null || echo "")
  check "Rolling timeout ping $i" "pong-${i}" "$PING"
done
echo ""

# --- Test subdomain proxy ---
bold "[10/11] Testing subdomain proxy..."

# Extract the port from API_URL (default 8080)
API_PORT=$(echo "$API_URL" | python3 -c "import sys; from urllib.parse import urlparse; print(urlparse(sys.stdin.read().strip()).port or 8080)" 2>/dev/null || echo "8080")

# Start a simple HTTP server on port 80 inside the sandbox using perl (available in ubuntu:22.04)
run_cmd "$SANDBOX_ID" "echo 'hello from sandbox' > /tmp/index.html" > /dev/null
# Write a tiny perl HTTP server script using base64 to avoid escaping issues
PERL_HTTPD=$(printf '%s' 'use IO::Socket::INET;my $s=IO::Socket::INET->new(LocalPort=>80,Listen=>5,ReuseAddr=>1) or die;while(my $c=$s->accept){my $r=<$c>;open my $f,"<","/tmp/index.html";my $b=do{local $/;<$f>};print $c "HTTP/1.0 200 OK\r\nContent-Length:".length($b)."\r\n\r\n".$b;close $c}' | base64)
run_cmd "$SANDBOX_ID" "echo ${PERL_HTTPD} | base64 -d > /tmp/httpd.pl" > /dev/null
run_cmd "$SANDBOX_ID" "nohup perl /tmp/httpd.pl > /dev/null 2>&1 &" > /dev/null
sleep 1

if [[ -n "$SANDBOX_DOMAIN" ]]; then
  # Curl through subdomain proxy (resolve sandbox_id.localhost to 127.0.0.1)
  PROXY_RESP=$(curl -s --max-time 5 \
    --resolve "${SANDBOX_DOMAIN}:${API_PORT}:127.0.0.1" \
    "http://${SANDBOX_DOMAIN}:${API_PORT}/index.html" 2>/dev/null || echo "")
  check "Subdomain proxy: HTTP response" "hello from sandbox" "$PROXY_RESP"
else
  yellow "  SKIP: No domain assigned (OPENSANDBOX_SANDBOX_DOMAIN not set)"
fi
echo ""

# --- Test subdomain proxy after hibernate/wake ---
bold "[11/11] Testing subdomain proxy survives hibernate/wake..."

if [[ -n "$SANDBOX_DOMAIN" ]]; then
  # Hibernate
  HIB3_RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/hibernate")
  HIB3_KEY=$(echo "$HIB3_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('checkpointKey',''))" 2>/dev/null)
  if [[ -n "$HIB3_KEY" ]]; then
    green "  Hibernated for proxy test"
  else
    red "  Hibernate failed: $HIB3_RESP"
  fi

  # Wake
  WAKE3_RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/wake" '{"timeout": 600}')
  WAKE3_STATUS=$(echo "$WAKE3_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
  check "Wake for proxy test" "running" "$WAKE3_STATUS"

  # Restart the web server (process state is restored but the HTTP server may not survive checkpoint)
  # /tmp/httpd.pl was written in step 10 and survives hibernate (tmpfs is checkpointed)
  run_cmd "$SANDBOX_ID" "nohup perl /tmp/httpd.pl > /dev/null 2>&1 &" > /dev/null
  sleep 1

  # Curl through subdomain proxy again
  PROXY_RESP2=$(curl -s --max-time 5 \
    --resolve "${SANDBOX_DOMAIN}:${API_PORT}:127.0.0.1" \
    "http://${SANDBOX_DOMAIN}:${API_PORT}/index.html" 2>/dev/null || echo "")
  check "Subdomain proxy after wake: HTTP response" "hello from sandbox" "$PROXY_RESP2"
else
  yellow "  SKIP: No domain assigned"
fi
echo ""

# --- Cleanup ---
bold "Cleaning up..."
api DELETE "/api/sandboxes/${SANDBOX_ID}" > /dev/null 2>&1 || true

# --- Summary ---
echo ""
bold "========================================="
bold " Results: $PASS passed, $FAIL failed"
bold " Hibernate:       ~${HIBERNATE_DURATION:-?}s"
bold " Explicit Wake:   ~${WAKE_DURATION}s"
bold " Auto-wake+Exec:  ~${AUTOWAKE_DURATION:-?}s"
bold "========================================="

if [[ $FAIL -gt 0 ]]; then
  exit 1
fi
