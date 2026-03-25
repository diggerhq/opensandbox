#!/usr/bin/env bash
# 01-lifecycle.sh — Sandbox create, list, get, destroy, auto-timeout
source "$(dirname "$0")/common.sh"

h "Sandbox Lifecycle"

# Create
SB=$(create_sandbox 3600)
[ -n "$SB" ] && pass "Create sandbox: $SB" || fail "Create sandbox"

# List
LIST=$(api "$API_URL/api/sandboxes")
echo "$LIST" | grep -q "$SB" && pass "List shows sandbox" || fail "List shows sandbox"

# Get
STATUS=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])")
[ "$STATUS" = "running" ] && pass "Get status: running" || fail "Get status: $STATUS"

# Destroy
api -X DELETE "$API_URL/api/sandboxes/$SB" >/dev/null
sleep 1
STATUS=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','gone'))" 2>/dev/null)
[ "$STATUS" != "running" ] && pass "Destroy: sandbox gone" || fail "Destroy: still running"

# Timeout auto-destroy
h "Auto-timeout (10s)"
SB2=$(create_sandbox 10)
[ -n "$SB2" ] && pass "Create with timeout=10: $SB2" || fail "Create with timeout=10"
echo "  Waiting 15s for auto-destroy..."
sleep 15
STATUS=$(api "$API_URL/api/sandboxes/$SB2" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','gone'))" 2>/dev/null)
if [ "$STATUS" != "running" ]; then
    pass "Auto-timeout: sandbox destroyed after 10s"
else
    fail "Auto-timeout: sandbox still running"
    destroy_sandbox "$SB2"
fi

summary
