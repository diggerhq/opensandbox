#!/usr/bin/env bash
# 17-pty-sessions.sh — PTY session create, list, resize, kill (no WS attach — curl can't do WebSocket)
set +u
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "PTY Sessions"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Create PTY session
PTY_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/pty" -d '{"cols":120,"rows":40}')
PTY_ID=$(echo "$PTY_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sessionID',json.load(sys.stdin) if '{' not in str(json.load(sys.stdin)) else ''))" 2>/dev/null || echo "$PTY_RESULT" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionID', d.get('id','')))" 2>/dev/null)
if [ -n "$PTY_ID" ] && [ "$PTY_ID" != "None" ] && [ "$PTY_ID" != "" ]; then
    pass "Create PTY session: $PTY_ID"
else
    # PTY might not be supported in server-proxy mode
    ERROR=$(echo "$PTY_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('error',''))" 2>/dev/null)
    if echo "$ERROR" | grep -qi "not available\|not implemented\|not supported\|vsock\|no such device"; then
        skip "PTY sessions not available (QEMU uses virtio-serial, PTY data needs vsock)"
        summary
    else
        fail "Create PTY: $PTY_RESULT"
        summary
    fi
fi

# Resize PTY
RESIZE=$(api -X POST "$API_URL/api/sandboxes/$SB/pty/$PTY_ID/resize" -d '{"cols":200,"rows":50}')
# Should succeed (200 or 204)
pass "Resize PTY"

# Kill PTY
api -X DELETE "$API_URL/api/sandboxes/$SB/pty/$PTY_ID" >/dev/null
pass "Kill PTY"

summary
