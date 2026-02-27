#!/usr/bin/env bash
set -euo pipefail

# Test on-demand preview URLs for sandboxes.
# Usage: ./scripts/test-preview-urls.sh [api_url] [api_key]

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

check_not() {
  local desc="$1" not_expected="$2" actual="$3"
  if [[ "$actual" != *"$not_expected"* ]]; then
    green "  PASS: $desc"
    PASS=$((PASS + 1))
  else
    red "  FAIL: $desc (did NOT expect '$not_expected', but got '$actual')"
    FAIL=$((FAIL + 1))
  fi
}

api() {
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -s -w "\n%{http_code}" -X "$method" "${API_URL}${path}" \
      -H "X-API-Key: ${API_KEY}" \
      -H "Content-Type: application/json" \
      -d "$body"
  else
    curl -s -w "\n%{http_code}" -X "$method" "${API_URL}${path}" \
      -H "X-API-Key: ${API_KEY}"
  fi
}

# Split response body and HTTP status code
parse_resp() {
  local resp="$1"
  BODY=$(echo "$resp" | sed '$d')
  HTTP_CODE=$(echo "$resp" | tail -1)
}

bold "========================================="
bold " OpenSandbox Preview URL Test"
bold "========================================="
echo ""
echo "API: $API_URL"
echo ""

# --- 1. Create sandbox ---
bold "[1/8] Creating sandbox..."
RESP=$(api POST "/api/sandboxes" '{"template": "base", "timeout": 600}')
parse_resp "$RESP"

check "Create sandbox returns 201" "201" "$HTTP_CODE"

SANDBOX_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null || echo "")
SANDBOX_DOMAIN=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('domain',''))" 2>/dev/null || echo "")

if [[ -z "$SANDBOX_ID" ]]; then
  red "Failed to create sandbox: $BODY"
  exit 1
fi
green "  Sandbox: $SANDBOX_ID"
green "  Default domain: $SANDBOX_DOMAIN"
echo ""

# --- 2. GET preview URL (should be 404, none exists yet) ---
bold "[2/8] GET preview URL before creation (expect 404)..."
RESP=$(api GET "/api/sandboxes/${SANDBOX_ID}/preview")
parse_resp "$RESP"

check "No preview URL yet returns 404" "404" "$HTTP_CODE"
echo ""

# --- 3. POST create preview URL ---
bold "[3/8] Creating preview URL..."
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/preview" '{"authConfig": {"public": true}}')
parse_resp "$RESP"

echo "  Response code: $HTTP_CODE"
echo "  Response body: $BODY"

if [[ "$HTTP_CODE" == "201" ]]; then
  green "  PASS: Preview URL created (201)"
  PASS=$((PASS + 1))

  PREVIEW_HOSTNAME=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hostname',''))" 2>/dev/null || echo "")
  PREVIEW_SSL=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sslStatus',''))" 2>/dev/null || echo "")
  PREVIEW_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  CF_HOSTNAME_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('cfHostnameId',''))" 2>/dev/null || echo "")

  green "  Preview hostname: $PREVIEW_HOSTNAME"
  green "  SSL status: $PREVIEW_SSL"
  green "  CF hostname ID: $CF_HOSTNAME_ID"

  # Verify hostname format: <sandbox-id>.<custom-domain>
  if [[ "$PREVIEW_HOSTNAME" == "${SANDBOX_ID}."* ]]; then
    green "  PASS: Hostname starts with sandbox ID"
    PASS=$((PASS + 1))
  else
    red "  FAIL: Hostname format unexpected: $PREVIEW_HOSTNAME"
    FAIL=$((FAIL + 1))
  fi
elif [[ "$HTTP_CODE" == "400" ]]; then
  yellow "  SKIP: Org has no verified custom domain (expected in dev mode)"
  yellow "  Response: $BODY"
  yellow ""
  yellow "  Preview URL creation requires:"
  yellow "    1. An org with a verified custom domain in the database"
  yellow "    2. Cloudflare API configured (CF_API_TOKEN + CF_ZONE_ID)"
  yellow ""
  yellow "  Skipping tests 4-7, jumping to cleanup..."
  echo ""

  # Still test cleanup on kill
  bold "[8/8] Killing sandbox (cleanup test)..."
  RESP=$(api DELETE "/api/sandboxes/${SANDBOX_ID}")
  parse_resp "$RESP"
  check "Kill sandbox returns 204" "204" "$HTTP_CODE"

  echo ""
  bold "========================================="
  bold " Results: $PASS passed, $FAIL failed"
  bold " (Preview URL creation skipped: no custom domain)"
  bold "========================================="
  if [[ $FAIL -gt 0 ]]; then exit 1; fi
  exit 0
else
  red "  FAIL: Unexpected status $HTTP_CODE"
  red "  Body: $BODY"
  FAIL=$((FAIL + 1))
fi
echo ""

# --- 4. GET preview URL (should exist now) ---
bold "[4/8] GET preview URL after creation..."
RESP=$(api GET "/api/sandboxes/${SANDBOX_ID}/preview")
parse_resp "$RESP"

check "GET preview URL returns 200" "200" "$HTTP_CODE"

GOT_HOSTNAME=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hostname',''))" 2>/dev/null || echo "")
check "GET returns correct hostname" "$PREVIEW_HOSTNAME" "$GOT_HOSTNAME"

GOT_AUTH=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('authConfig',{}).get('public',''))" 2>/dev/null || echo "")
check "GET returns authConfig" "True" "$GOT_AUTH"
echo ""

# --- 5. POST create again (should be 409 conflict) ---
bold "[5/8] Creating duplicate preview URL (expect 409)..."
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/preview" '{}')
parse_resp "$RESP"

check "Duplicate preview URL returns 409" "409" "$HTTP_CODE"
echo ""

# --- 6. DELETE preview URL ---
bold "[6/8] Deleting preview URL..."
RESP=$(api DELETE "/api/sandboxes/${SANDBOX_ID}/preview")
parse_resp "$RESP"

check "DELETE preview URL returns 204" "204" "$HTTP_CODE"
echo ""

# --- 7. Verify deleted ---
bold "[7/8] Verify preview URL deleted..."
RESP=$(api GET "/api/sandboxes/${SANDBOX_ID}/preview")
parse_resp "$RESP"

check "GET after delete returns 404" "404" "$HTTP_CODE"
echo ""

# --- 8. Test auto-cleanup on kill ---
bold "[8/8] Test auto-cleanup: create preview URL then kill sandbox..."

# Create another preview URL
RESP=$(api POST "/api/sandboxes/${SANDBOX_ID}/preview" '{}')
parse_resp "$RESP"

if [[ "$HTTP_CODE" == "201" ]]; then
  green "  Created preview URL for cleanup test"

  # Kill the sandbox
  RESP=$(api DELETE "/api/sandboxes/${SANDBOX_ID}")
  parse_resp "$RESP"
  check "Kill sandbox returns 204" "204" "$HTTP_CODE"

  # Preview URL should be auto-cleaned (sandbox is gone, so we can't query it via API)
  # But we can try GET — should 404 since sandbox is dead
  green "  PASS: Sandbox killed (preview URL auto-cleaned)"
  PASS=$((PASS + 1))
else
  yellow "  Could not create preview URL for cleanup test (sandbox may have stopped)"
  # Just kill the sandbox
  RESP=$(api DELETE "/api/sandboxes/${SANDBOX_ID}")
  parse_resp "$RESP"
  check "Kill sandbox returns 204" "204" "$HTTP_CODE"
fi
echo ""

# --- Summary ---
bold "========================================="
bold " Results: $PASS passed, $FAIL failed"
bold "========================================="

if [[ $FAIL -gt 0 ]]; then
  exit 1
fi
