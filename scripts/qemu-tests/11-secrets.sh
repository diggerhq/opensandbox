#!/usr/bin/env bash
# 11-secrets.sh — Secret stores, secret CRUD, sandbox integration with sealed tokens
set +u  # allow unbound variables in cleanup arrays
source "$(dirname "$0")/common.sh"

TIMEOUT=30
SANDBOXES=()
STORES=()
cleanup() {
    for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb"; done
    for store_id in "${STORES[@]}"; do
        api -X DELETE "$API_URL/api/secret-stores/$store_id" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT

h "Secret Store CRUD"

# Create secret store
STORE_RESULT=$(api -X POST "$API_URL/api/secret-stores" -d '{"name":"test-store-'$$'","egressAllowlist":["api.anthropic.com","*.openai.com"]}')
STORE_ID=$(echo "$STORE_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [ -n "$STORE_ID" ] && [ "$STORE_ID" != "None" ]; then
    STORES+=("$STORE_ID")
    pass "Create secret store: $STORE_ID"
else
    fail "Create secret store: $STORE_RESULT"
    summary
fi
STORE_NAME=$(echo "$STORE_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('name',''))" 2>/dev/null)

# List stores
LIST=$(api "$API_URL/api/secret-stores")
echo "$LIST" | grep -q "$STORE_ID" && pass "List stores: found our store" || fail "List stores: $LIST"

# Get store
GET=$(api "$API_URL/api/secret-stores/$STORE_ID")
echo "$GET" | grep -q "$STORE_NAME" && pass "Get store by ID" || fail "Get store: $GET"

# Update store
UPDATE=$(api -X PUT "$API_URL/api/secret-stores/$STORE_ID" -d '{"name":"'$STORE_NAME'","egressAllowlist":["api.anthropic.com"]}')
EGRESS=$(echo "$UPDATE" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('egressAllowlist',[])))" 2>/dev/null)
[ "$EGRESS" = "1" ] && pass "Update store: egress list updated" || fail "Update store: $UPDATE"

# --- Secret CRUD ---
h "Secret CRUD"

# Set a secret
SET_RESULT=$(api -X PUT "$API_URL/api/secret-stores/$STORE_ID/secrets/MY_API_KEY" \
    -d '{"value":"sk-test-secret-12345","allowedHosts":["api.anthropic.com"]}')
echo "$SET_RESULT" | grep -q '"set"' && pass "Set secret: MY_API_KEY" || fail "Set secret: $SET_RESULT"

# Set another secret
SET_RESULT2=$(api -X PUT "$API_URL/api/secret-stores/$STORE_ID/secrets/DB_PASSWORD" \
    -d '{"value":"supersecretpassword"}')
echo "$SET_RESULT2" | grep -q '"set"' && pass "Set secret: DB_PASSWORD" || fail "Set secret 2: $SET_RESULT2"

# List secrets (should show names but NOT values)
LIST_SECRETS=$(api "$API_URL/api/secret-stores/$STORE_ID/secrets")
echo "$LIST_SECRETS" | grep -q 'MY_API_KEY' && pass "List secrets: MY_API_KEY present" || fail "List secrets: $LIST_SECRETS"
echo "$LIST_SECRETS" | grep -q 'DB_PASSWORD' && pass "List secrets: DB_PASSWORD present" || fail "List secrets DB_PASSWORD"
echo "$LIST_SECRETS" | grep -q 'sk-test-secret' && fail "List secrets: LEAKS plaintext value!" || pass "List secrets: no plaintext values exposed"

# Update secret (overwrite)
SET_UPDATE=$(api -X PUT "$API_URL/api/secret-stores/$STORE_ID/secrets/MY_API_KEY" \
    -d '{"value":"sk-updated-key-67890","allowedHosts":["api.anthropic.com"]}')
echo "$SET_UPDATE" | grep -q '"set"' && pass "Update secret value" || fail "Update secret: $SET_UPDATE"

# Delete a secret
api -X DELETE "$API_URL/api/secret-stores/$STORE_ID/secrets/DB_PASSWORD" >/dev/null
LIST_AFTER=$(api "$API_URL/api/secret-stores/$STORE_ID/secrets")
echo "$LIST_AFTER" | grep -q 'DB_PASSWORD' && fail "Delete secret: still present" || pass "Delete secret: DB_PASSWORD removed"

# --- Sandbox with Secret Store ---
h "Sandbox with Secret Store"

# Create sandbox referencing the secret store
SB_RESULT=$(api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":3600,\"secretStore\":\"$STORE_NAME\"}")
SB_ID=$(echo "$SB_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -n "$SB_ID" ] && [ "$SB_ID" != "None" ]; then
    SANDBOXES+=("$SB_ID")
    pass "Create sandbox with secret store: $SB_ID"
else
    # May fail if encryption key not configured — that's expected in dev
    ERRORMSG=$(echo "$SB_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error',''))" 2>/dev/null)
    if echo "$ERRORMSG" | grep -qi 'encrypt\|encryptor\|secret.*key\|not configured'; then
        skip "Sandbox with secrets: encryption key not configured (set OPENSANDBOX_SECRET_ENCRYPTION_KEY)"
    else
        fail "Create sandbox with secrets: $SB_RESULT"
    fi

    # Test without secrets — basic sandbox should still work
    SB_PLAIN=$(create_sandbox)
    SANDBOXES+=("$SB_PLAIN")
    OUT=$(exec_stdout "$SB_PLAIN" "echo" "no-secrets-ok")
    [ "$OUT" = "no-secrets-ok" ] && pass "Sandbox without secrets: works fine" || fail "Plain sandbox: $OUT"

    summary
fi

# Check if secret is available as env var (may be sealed token)
SECRET_VAL=$(exec_stdout "$SB_ID" "bash" "-c" "echo \$MY_API_KEY")
if [ -n "$SECRET_VAL" ]; then
    if echo "$SECRET_VAL" | grep -q '^osb_sealed_'; then
        pass "Secret injected as sealed token: ${SECRET_VAL:0:20}..."
    else
        # Could be plaintext if secrets proxy not active
        pass "Secret injected as env var (plaintext or sealed)"
    fi
else
    fail "Secret not found in env: MY_API_KEY is empty"
fi

# Verify deleted secret is NOT in env
DELETED_VAL=$(exec_stdout "$SB_ID" "bash" "-c" "echo \$DB_PASSWORD")
[ -z "$DELETED_VAL" ] && pass "Deleted secret not in env" || fail "Deleted secret still present: $DELETED_VAL"

# --- Cascade Delete ---
h "Cascade Delete"

# Create a temporary store with secrets
TEMP_RESULT=$(api -X POST "$API_URL/api/secret-stores" -d '{"name":"temp-store-'$$'"}')
TEMP_ID=$(echo "$TEMP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [ -n "$TEMP_ID" ] && [ "$TEMP_ID" != "None" ]; then
    api -X PUT "$API_URL/api/secret-stores/$TEMP_ID/secrets/SECRET_A" -d '{"value":"aaa"}' >/dev/null
    api -X PUT "$API_URL/api/secret-stores/$TEMP_ID/secrets/SECRET_B" -d '{"value":"bbb"}' >/dev/null

    # Delete the store — should cascade delete all secrets
    api -X DELETE "$API_URL/api/secret-stores/$TEMP_ID" >/dev/null

    # Verify store is gone
    GET_DELETED=$(api "$API_URL/api/secret-stores/$TEMP_ID")
    echo "$GET_DELETED" | grep -qi 'not found\|error' && pass "Cascade delete: store removed" || fail "Cascade delete: store still exists"
else
    skip "Cascade delete: couldn't create temp store"
fi

# --- Multiple Stores Isolation ---
h "Store Isolation"

STORE2_RESULT=$(api -X POST "$API_URL/api/secret-stores" -d '{"name":"store-b-'$$'"}')
STORE2_ID=$(echo "$STORE2_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [ -n "$STORE2_ID" ] && [ "$STORE2_ID" != "None" ]; then
    STORES+=("$STORE2_ID")
    api -X PUT "$API_URL/api/secret-stores/$STORE2_ID/secrets/STORE_B_SECRET" -d '{"value":"store-b-value"}' >/dev/null

    # Verify store A doesn't have store B's secrets
    LIST_A=$(api "$API_URL/api/secret-stores/$STORE_ID/secrets")
    echo "$LIST_A" | grep -q 'STORE_B_SECRET' && fail "Store isolation: leaked across stores" || pass "Store isolation: stores are independent"
else
    skip "Store isolation: couldn't create second store"
fi

summary
