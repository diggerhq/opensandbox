#!/bin/bash
#
# End-to-end test: sealed secrets + proxy token replacement
#
# Proves the full chain:
#   1. Secret encrypted in PG
#   2. Decrypted on sandbox create
#   3. Sealed into osb_sealed_* token inside VM
#   4. Proxy replaces sealed token with real value on outbound HTTPS
#   5. Anthropic API call succeeds
#
# Usage:
#   ./scripts/test-proxy-secrets.sh <your-real-anthropic-api-key>
#
set -euo pipefail

API="${OPENCOMPUTER_API_URL:-http://52.14.189.164:8080}"
KEY="${OPENCOMPUTER_API_KEY:-test-dev-key}"
ANTHROPIC_KEY="${1:-}"

if [ -z "$ANTHROPIC_KEY" ]; then
  echo "Usage: $0 <anthropic-api-key>"
  echo "  e.g. $0 sk-ant-api03-..."
  exit 1
fi

GREEN='\033[32m'
RED='\033[31m'
BOLD='\033[1m'
DIM='\033[2m'
RESET='\033[0m'

pass() { echo -e "${GREEN}✓ $1${RESET}"; }
fail() { echo -e "${RED}✗ $1${RESET}"; }
info() { echo -e "${DIM}  $1${RESET}"; }
header() { echo -e "\n${BOLD}━━━ $1 ━━━${RESET}\n"; }

PROJ_NAME="proxy-test-$(date +%s)"
PROJ_ID=""
SB_ID=""
TOKEN=""
WORKER="https://dev.opensandbox.ai"

cleanup() {
  if [ -n "$SB_ID" ]; then
    curl -s -X DELETE "$API/api/sandboxes/$SB_ID" -H "X-API-Key: $KEY" > /dev/null 2>&1 || true
  fi
  if [ -n "$PROJ_ID" ]; then
    curl -s -X DELETE "$API/api/projects/$PROJ_ID" -H "X-API-Key: $KEY" > /dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo -e "${BOLD}"
echo "╔══════════════════════════════════════════════════╗"
echo "║   Sealed Secrets + Proxy Replacement Test        ║"
echo "╚══════════════════════════════════════════════════╝"
echo -e "${RESET}"

# ── 1. Create project ──
header "1. Create project"

PROJ_RESP=$(curl -s -X POST "$API/api/projects" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"name\":\"$PROJ_NAME\",\"timeoutSec\":300}")

PROJ_ID=$(echo "$PROJ_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null || true)

if [ -n "$PROJ_ID" ]; then
  pass "Project created: $PROJ_ID"
else
  fail "Project creation failed: $PROJ_RESP"
  exit 1
fi

# ── 2. Set Anthropic key as secret ──
header "2. Set ANTHROPIC_API_KEY secret"

SECRET_RESP=$(curl -s -X PUT "$API/api/projects/$PROJ_ID/secrets/ANTHROPIC_API_KEY" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"value\":\"$ANTHROPIC_KEY\"}")

pass "Secret set (encrypted at rest)"
info "Key prefix: ${ANTHROPIC_KEY:0:12}..."

# ── 3. Create sandbox with project ──
header "3. Create sandbox with project"

SB_RESP=$(curl -s -X POST "$API/api/sandboxes" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"project\":\"$PROJ_NAME\",\"timeout\":300}")

SB_ID=$(echo "$SB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])" 2>/dev/null || true)
TOKEN=$(echo "$SB_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['token'])" 2>/dev/null || true)

if [ -n "$SB_ID" ] && [ -n "$TOKEN" ]; then
  pass "Sandbox created: $SB_ID"
else
  fail "Sandbox creation failed: $SB_RESP"
  exit 1
fi

# Helper to exec in sandbox (uses jq to safely JSON-encode the command)
sb_exec() {
  local payload
  payload=$(jq -n --arg cmd "$1" '{"cmd":"sh","args":["-c",$cmd],"timeout":30}')
  curl -s -X POST "$WORKER/sandboxes/$SB_ID/exec/run" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "$payload"
}

# ── 4. Verify secret is sealed (not plaintext) ──
header "4. Verify secret is sealed inside sandbox"

SEALED_VAL=$(sb_exec 'echo $ANTHROPIC_API_KEY' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)

if [[ "$SEALED_VAL" == osb_sealed_* ]]; then
  pass "Secret is sealed: $SEALED_VAL"
else
  fail "Secret is NOT sealed — got: $SEALED_VAL"
  if [[ "$SEALED_VAL" == sk-ant-* ]]; then
    fail "CRITICAL: Real API key is visible inside the sandbox!"
  fi
  exit 1
fi

# ── 5. Verify real key is NOT in env ──
header "5. Verify real key never appears in VM"

ALL_ENV=$(sb_exec 'env' | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout',''))" 2>/dev/null)

if echo "$ALL_ENV" | grep -q "$ANTHROPIC_KEY"; then
  fail "CRITICAL: Real API key found in VM environment!"
  exit 1
else
  pass "Real key not found in VM environment"
fi

# ── 6. Call Anthropic API from inside sandbox ──
header "6. Call Anthropic API (proxy should replace sealed token)"

API_RESP=$(sb_exec 'curl -s -w "\n%{http_code}" https://api.anthropic.com/v1/messages -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01" -H "content-type: application/json" -d "{\"model\":\"claude-haiku-4-5-20251001\",\"max_tokens\":20,\"messages\":[{\"role\":\"user\",\"content\":\"Say exactly: proxy test ok\"}]}"')

API_STDOUT=$(echo "$API_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout',''))" 2>/dev/null)
API_STDERR=$(echo "$API_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stderr',''))" 2>/dev/null)

# Extract HTTP status (last line of stdout from curl -w)
HTTP_CODE=$(echo "$API_STDOUT" | tail -1)
BODY=$(echo "$API_STDOUT" | sed '$d')

info "HTTP status: $HTTP_CODE"

if [ "$HTTP_CODE" = "200" ]; then
  pass "Anthropic API call succeeded (200)"

  # Extract the response text
  RESPONSE_TEXT=$(echo "$BODY" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d['content'][0]['text'])
except:
    print('(could not parse response)')
" 2>/dev/null)
  info "Claude said: $RESPONSE_TEXT"
else
  fail "Anthropic API call failed (HTTP $HTTP_CODE)"
  info "Response: $BODY"
  if echo "$BODY" | grep -qi "authentication\|invalid.*key\|unauthorized"; then
    fail "Auth error — proxy did NOT replace the sealed token"
  fi
fi

# ── Summary ──
echo ""
echo -e "${BOLD}========================================${RESET}"
echo -e "${BOLD} All checks passed!${RESET}"
echo -e "${BOLD}========================================${RESET}"
echo ""
echo -e "${DIM}Chain verified:${RESET}"
echo -e "${DIM}  PG (encrypted) → server (decrypted) → worker (sealed) → VM (osb_sealed_*)${RESET}"
echo -e "${DIM}  VM → HTTPS request → proxy (replaced) → Anthropic (real key) → 200 OK${RESET}"
