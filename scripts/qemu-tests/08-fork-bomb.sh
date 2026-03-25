#!/usr/bin/env bash
# 08-fork-bomb.sh — Fork bomb containment (sandbox wedges but host survives)
source "$(dirname "$0")/common.sh"

TIMEOUT=60
SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb"; done; }
trap cleanup EXIT

h "Fork Bomb Containment"

# Create two sandboxes — one for the bomb, one to verify host isn't affected
SB_BOMB=$(create_sandbox)
SB_CTRL=$(create_sandbox)
SANDBOXES+=("$SB_BOMB" "$SB_CTRL")

# Verify control sandbox works
OUT=$(exec_stdout "$SB_CTRL" "echo" "control-ok")
[ "$OUT" = "control-ok" ] && pass "Control sandbox: responsive" || fail "Control sandbox"

# Launch fork bomb (will timeout)
echo "  Launching fork bomb on $SB_BOMB (5s timeout)..."
RESULT=$(api --max-time 15 -X POST "$API_URL/api/sandboxes/$SB_BOMB/exec/run" \
    -d '{"cmd":"bash","args":["-c",":(){ :|:& };:"],"timeout":5}' 2>/dev/null || echo '{"error":"timeout"}')
pass "Fork bomb launched (returned or timed out)"

# Verify control sandbox still works
sleep 2
OUT=$(exec_stdout "$SB_CTRL" "echo" "still-alive")
[ "$OUT" = "still-alive" ] && pass "Control sandbox: still responsive after bomb" || fail "Control sandbox affected by bomb"

# Destroy the bombed sandbox
api -X DELETE "$API_URL/api/sandboxes/$SB_BOMB" >/dev/null 2>&1
pass "Bombed sandbox destroyed"

# Verify control sandbox still works after destroy
OUT=$(exec_stdout "$SB_CTRL" "echo" "final-check")
[ "$OUT" = "final-check" ] && pass "Control sandbox: works after bomb cleanup" || fail "Control sandbox post-cleanup"

summary
