#!/usr/bin/env bash
# 16-exec-sessions.sh — Exec sessions (create, list, WebSocket attach, kill)
set +u
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "Exec Sessions"

SB=$(create_sandbox)
SANDBOXES+=("$SB")

# Get sandbox token for worker direct access
TOKEN=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('token',''))" 2>/dev/null)
CONNECT=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('connectURL',''))" 2>/dev/null)

# Create exec session via server proxy
SESS_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" -d '{"cmd":"bash","args":["-c","for i in 1 2 3; do echo line-$i; sleep 0.5; done"]}')
SESS_ID=$(echo "$SESS_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sessionID',''))" 2>/dev/null)
[ -n "$SESS_ID" ] && pass "Create exec session: $SESS_ID" || fail "Create session: $SESS_RESULT"

# List exec sessions
sleep 1
LIST=$(api "$API_URL/api/sandboxes/$SB/exec")
echo "$LIST" | grep -q "$SESS_ID" && pass "List sessions: found $SESS_ID" || fail "List: $LIST"

# Wait for process to finish
sleep 3

# Create another session and kill it
SESS2=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" -d '{"cmd":"sleep","args":["300"]}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('sessionID',''))" 2>/dev/null)
[ -n "$SESS2" ] && pass "Create long-running session: $SESS2" || fail "Create session 2"

# Kill the session
KILL_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/$SESS2/kill" -d '{}')
pass "Kill session sent"

# Verify it's no longer running
sleep 1
LIST2=$(api "$API_URL/api/sandboxes/$SB/exec")
RUNNING=$(echo "$LIST2" | python3 -c "
import sys,json
sessions = json.load(sys.stdin)
running = [s for s in sessions if s.get('sessionID') == '$SESS2' and s.get('running')]
print(len(running))
" 2>/dev/null)
[ "$RUNNING" = "0" ] && pass "Killed session is not running" || fail "Session still running: $LIST2"

# Session with env vars
SESS3=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" -d '{"cmd":"bash","args":["-c","echo MY_VAR=$MY_VAR"],"envs":{"MY_VAR":"session-env"}}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('sessionID',''))" 2>/dev/null)
[ -n "$SESS3" ] && pass "Session with env vars: $SESS3" || fail "Session with envs"

# Session with timeout
SESS4=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" -d '{"cmd":"sleep","args":["300"],"timeout":2}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('sessionID',''))" 2>/dev/null)
[ -n "$SESS4" ] && pass "Session with timeout: $SESS4" || fail "Session with timeout"
sleep 4
LIST3=$(api "$API_URL/api/sandboxes/$SB/exec")
RUNNING2=$(echo "$LIST3" | python3 -c "
import sys,json
sessions = json.load(sys.stdin)
running = [s for s in sessions if s.get('sessionID') == '$SESS4' and s.get('running')]
print(len(running))
" 2>/dev/null)
[ "$RUNNING2" = "0" ] && pass "Timed-out session stopped" || fail "Timeout session still running"

summary
