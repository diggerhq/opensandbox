#!/usr/bin/env bash
# 13-preview-urls.sh — Preview URL creation, serving, listing, deletion
set +u
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "Preview URLs"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Start a simple HTTP server
exec_run "$SB" "bash" "-c" "mkdir -p /workspace && echo '<h1>Preview Test</h1>' > /workspace/index.html" >/dev/null
exec_run "$SB" "bash" "-c" "setsid python3 -m http.server 3000 --directory /workspace </dev/null >/dev/null 2>&1 &" >/dev/null
sleep 2

# Create preview URL
PREVIEW=$(api -X POST "$API_URL/api/sandboxes/$SB/preview" -d '{"port":3000}')
HOSTNAME=$(echo "$PREVIEW" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hostname',''))" 2>/dev/null)
[ -n "$HOSTNAME" ] && pass "Create preview: $HOSTNAME" || fail "Create preview: $PREVIEW"

# Verify content is served (HTTP only on dev — no TLS)
if [ -n "$HOSTNAME" ]; then
    CONTENT=$(curl -s --max-time 10 "http://$HOSTNAME" 2>/dev/null)
    echo "$CONTENT" | grep -q "Preview Test" && pass "Preview serves content" || fail "Preview content: ${CONTENT:0:50}"
fi

# List preview URLs
LIST=$(api "$API_URL/api/sandboxes/$SB/preview")
echo "$LIST" | grep -q "3000" && pass "List previews: port 3000 found" || fail "List: $LIST"

# Create second preview on different port
exec_run "$SB" "bash" "-c" "setsid python3 -m http.server 8080 --directory /workspace </dev/null >/dev/null 2>&1 &" >/dev/null
sleep 1
PREVIEW2=$(api -X POST "$API_URL/api/sandboxes/$SB/preview" -d '{"port":8080}')
HOSTNAME2=$(echo "$PREVIEW2" | python3 -c "import sys,json; print(json.load(sys.stdin).get('hostname',''))" 2>/dev/null)
[ -n "$HOSTNAME2" ] && pass "Second preview (port 8080): $HOSTNAME2" || fail "Second preview: $PREVIEW2"

# Delete preview
api -X DELETE "$API_URL/api/sandboxes/$SB/preview/3000" >/dev/null
LIST_AFTER=$(api "$API_URL/api/sandboxes/$SB/preview")
echo "$LIST_AFTER" | grep -q "3000" && fail "Delete preview: still present" || pass "Delete preview: port 3000 removed"

summary
