#!/usr/bin/env bash
# test-secret-store-fork.sh — Test 3-layer secret store merging
#
# Store A (base):  GIT_TOKEN, SHARED_KEY          egress=[github.com]
# Store B (mid):   API_KEY, SHARED_KEY (override)  egress=[api.anthropic.com]
# Store C (child): DEPLOY_KEY, API_KEY (override)  egress=[deploy.example.com]
#
# Layer 1: clean snapshot + store A
# Layer 2: checkpoint of layer 1 + store B  → should have GIT_TOKEN(A), SHARED_KEY(B), API_KEY(B)
# Layer 3: checkpoint of layer 2 + store C  → should have GIT_TOKEN(A*), SHARED_KEY(B*), API_KEY(C), DEPLOY_KEY(C)
#   * = re-resolved from layer 2's SecretStore (B), which was the merged result
#
# Egress should aggregate across all layers.

set -eo pipefail

API="${OPENSANDBOX_API_URL:?}"
KEY="${OPENSANDBOX_API_KEY:?}"

api() { curl -s -H "X-API-Key: $KEY" -H "Content-Type: application/json" "$@"; }
exec_env() {
    api -X POST "$API/api/sandboxes/$1/exec/run" -d '{"cmd":"env"}' | \
        python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout',''))" 2>/dev/null
}
pass() { printf "  \033[32m✓ %s\033[0m\n" "$1"; }
fail() { printf "  \033[31m✗ %s\033[0m\n" "$1"; exit 1; }
h()    { printf "\n\033[1;34m=== %s ===\033[0m\n" "$1"; }

SANDBOX_IDS=()
STORE_IDS=()
SNAP_NAMES=()
cleanup() {
    echo ""
    echo "=== Cleanup ==="
    for sb in "${SANDBOX_IDS[@]}"; do
        api -X DELETE "$API/api/sandboxes/$sb" >/dev/null 2>&1 || true
    done
    for snap in "${SNAP_NAMES[@]}"; do
        api -X DELETE "$API/api/snapshots/$snap" >/dev/null 2>&1 || true
    done
    for sid in "${STORE_IDS[@]}"; do
        api -X DELETE "$API/api/secret-stores/$sid" >/dev/null 2>&1 || true
    done
    echo "  Done"
}
trap cleanup EXIT

# ─────────────────────────────────────────────────────────
h "Setup: Create 3 secret stores"

STORE_A=$(api -X POST "$API/api/secret-stores" -d '{"name":"test-store-a","egressAllowlist":["github.com"]}')
STORE_A_ID=$(echo "$STORE_A" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
STORE_IDS+=("$STORE_A_ID")
api -X PUT "$API/api/secret-stores/$STORE_A_ID/secrets/GIT_TOKEN" -d '{"value":"a-git-token"}' >/dev/null
api -X PUT "$API/api/secret-stores/$STORE_A_ID/secrets/SHARED_KEY" -d '{"value":"a-shared-key"}' >/dev/null
echo "  A: GIT_TOKEN, SHARED_KEY  egress=[github.com]"

STORE_B=$(api -X POST "$API/api/secret-stores" -d '{"name":"test-store-b","egressAllowlist":["api.anthropic.com"]}')
STORE_B_ID=$(echo "$STORE_B" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
STORE_IDS+=("$STORE_B_ID")
api -X PUT "$API/api/secret-stores/$STORE_B_ID/secrets/API_KEY" -d '{"value":"b-api-key"}' >/dev/null
api -X PUT "$API/api/secret-stores/$STORE_B_ID/secrets/SHARED_KEY" -d '{"value":"b-shared-override"}' >/dev/null
echo "  B: API_KEY, SHARED_KEY  egress=[api.anthropic.com]"

STORE_C=$(api -X POST "$API/api/secret-stores" -d '{"name":"test-store-c","egressAllowlist":["deploy.example.com"]}')
STORE_C_ID=$(echo "$STORE_C" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
STORE_IDS+=("$STORE_C_ID")
api -X PUT "$API/api/secret-stores/$STORE_C_ID/secrets/DEPLOY_KEY" -d '{"value":"c-deploy-key"}' >/dev/null
api -X PUT "$API/api/secret-stores/$STORE_C_ID/secrets/API_KEY" -d '{"value":"c-api-override"}' >/dev/null
echo "  C: DEPLOY_KEY, API_KEY  egress=[deploy.example.com]"
pass "3 stores created"

# ─────────────────────────────────────────────────────────
h "Setup: Create clean snapshot"

SNAP="test-snap-$$"
SNAP_NAMES+=("$SNAP")
api -X POST "$API/api/snapshots" -d "{\"name\":\"$SNAP\",\"image\":{\"template\":\"default\"}}" >/dev/null
for i in $(seq 1 30); do
    S=$(api "$API/api/snapshots/$SNAP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$S" = "ready" ] && break; sleep 2
done
[ "$S" = "ready" ] && pass "Snapshot ready" || fail "Snapshot not ready"

# ─────────────────────────────────────────────────────────
h "Layer 1: Clean snapshot + Store A"

L1=$(api -X POST "$API/api/sandboxes" -d "{\"snapshot\":\"$SNAP\",\"secretStore\":\"test-store-a\",\"timeout\":120}")
L1_ID=$(echo "$L1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
[ -n "$L1_ID" ] && [ "$L1_ID" != "null" ] || fail "Layer 1 failed: $L1"
SANDBOX_IDS+=("$L1_ID")
echo "  Sandbox: $L1_ID"
sleep 5

ENV_L1=$(exec_env "$L1_ID")
echo "$ENV_L1" | grep -q "GIT_TOKEN" || fail "L1: GIT_TOKEN missing"
echo "$ENV_L1" | grep -q "SHARED_KEY" || fail "L1: SHARED_KEY missing"
echo "$ENV_L1" | grep -q "API_KEY" && fail "L1: API_KEY should not exist" || true
pass "Layer 1: GIT_TOKEN ✓  SHARED_KEY ✓  no API_KEY ✓"

# Checkpoint layer 1
CP1=$(api -X POST "$API/api/sandboxes/$L1_ID/checkpoints" -d '{"name":"layer1-cp"}')
CP1_ID=$(echo "$CP1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP1_ID" ] && [ "$CP1_ID" != "null" ] || fail "Checkpoint 1 failed: $CP1"
for i in $(seq 1 30); do
    CS=$(api "$API/api/sandboxes/$L1_ID/checkpoints" | python3 -c "
import sys,json
for c in json.load(sys.stdin):
    if c.get('id','')=='$CP1_ID': print(c.get('status','')); break
" 2>/dev/null)
    [ "$CS" = "ready" ] && break; sleep 2
done
[ "$CS" = "ready" ] && pass "Checkpoint 1 ready ($CP1_ID)" || fail "Checkpoint 1 not ready"

# ─────────────────────────────────────────────────────────
h "Layer 2: Checkpoint(store A) + Store B"

L2=$(api -X POST "$API/api/sandboxes/from-checkpoint/$CP1_ID" -d '{"secretStore":"test-store-b"}')
L2_ID=$(echo "$L2" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
[ -n "$L2_ID" ] && [ "$L2_ID" != "null" ] || fail "Layer 2 failed: $L2"
SANDBOX_IDS+=("$L2_ID")
echo "  Sandbox: $L2_ID"
sleep 5

ENV_L2=$(exec_env "$L2_ID")
echo "  Checking secrets..."
echo "$ENV_L2" | grep -q "GIT_TOKEN" || fail "L2: GIT_TOKEN missing (should inherit from A)"
echo "$ENV_L2" | grep -q "API_KEY" || fail "L2: API_KEY missing (from B)"
echo "$ENV_L2" | grep -q "SHARED_KEY" || fail "L2: SHARED_KEY missing"
echo "$ENV_L2" | grep -q "DEPLOY_KEY" && fail "L2: DEPLOY_KEY should not exist yet" || true
pass "Layer 2: GIT_TOKEN(A) ✓  API_KEY(B) ✓  SHARED_KEY(B override) ✓  no DEPLOY_KEY ✓"

# Checkpoint layer 2
CP2=$(api -X POST "$API/api/sandboxes/$L2_ID/checkpoints" -d '{"name":"layer2-cp"}')
CP2_ID=$(echo "$CP2" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP2_ID" ] && [ "$CP2_ID" != "null" ] || fail "Checkpoint 2 failed: $CP2"
for i in $(seq 1 30); do
    CS=$(api "$API/api/sandboxes/$L2_ID/checkpoints" | python3 -c "
import sys,json
for c in json.load(sys.stdin):
    if c.get('id','')=='$CP2_ID': print(c.get('status','')); break
" 2>/dev/null)
    [ "$CS" = "ready" ] && break; sleep 2
done
[ "$CS" = "ready" ] && pass "Checkpoint 2 ready ($CP2_ID)" || fail "Checkpoint 2 not ready"

# ─────────────────────────────────────────────────────────
h "Layer 3: Checkpoint(store A+B merged) + Store C"

L3=$(api -X POST "$API/api/sandboxes/from-checkpoint/$CP2_ID" -d '{"secretStore":"test-store-c"}')
L3_ID=$(echo "$L3" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
[ -n "$L3_ID" ] && [ "$L3_ID" != "null" ] || fail "Layer 3 failed: $L3"
SANDBOX_IDS+=("$L3_ID")
echo "  Sandbox: $L3_ID"
sleep 5

ENV_L3=$(exec_env "$L3_ID")
echo "  Checking all secrets across 3 layers..."
echo "$ENV_L3" | grep -q "GIT_TOKEN" || fail "L3: GIT_TOKEN missing (from layer 1 base)"
echo "$ENV_L3" | grep -q "SHARED_KEY" || fail "L3: SHARED_KEY missing"
echo "$ENV_L3" | grep -q "API_KEY" || fail "L3: API_KEY missing (C should override B)"
echo "$ENV_L3" | grep -q "DEPLOY_KEY" || fail "L3: DEPLOY_KEY missing (from C)"
pass "Layer 3: GIT_TOKEN ✓  SHARED_KEY ✓  API_KEY(C override) ✓  DEPLOY_KEY(C) ✓"

# ─────────────────────────────────────────────────────────
h "Verify: Count total unique secret env vars"

SECRET_COUNT=$(echo "$ENV_L3" | grep -c "osb_sealed_" || true)
echo "  $SECRET_COUNT sealed tokens in layer 3"
[ "$SECRET_COUNT" -eq 4 ] && pass "Exactly 4 secrets (GIT_TOKEN, SHARED_KEY, API_KEY, DEPLOY_KEY)" || \
    fail "Expected 4 secrets, got $SECRET_COUNT"

# ─────────────────────────────────────────────────────────
h "Verify: Proxy is configured (egress should aggregate)"

echo "$ENV_L3" | grep -q "HTTP_PROXY" && pass "Secrets proxy active" || fail "Proxy not configured"

echo ""
echo "=== All tests passed ==="
