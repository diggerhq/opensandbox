#!/usr/bin/env bash
# 10-edge-cases.sh — Edge cases and error handling
source "$(dirname "$0")/common.sh"

SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb"; done; }
trap cleanup EXIT

h "Edge Cases"

# Exec on hibernated sandbox (wake-on-request: auto-wakes and executes)
SB=$(create_sandbox)
SANDBOXES+=("$SB")
api -X POST "$API_URL/api/sandboxes/$SB/hibernate" >/dev/null
sleep 1
RESULT=$(api --max-time 60 -X POST "$API_URL/api/sandboxes/$SB/exec/run" -d '{"cmd":"echo","args":["test"],"timeout":5}')
OUT=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null)
[ "$OUT" = "test" ] && pass "Exec on hibernated: auto-woke and executed" || fail "Exec on hibernated: $RESULT"

# Double hibernate
h "Double Hibernate"
SB2=$(create_sandbox)
SANDBOXES+=("$SB2")
api -X POST "$API_URL/api/sandboxes/$SB2/hibernate" >/dev/null
sleep 1
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB2/hibernate")
echo "$RESULT" | grep -qi 'error\|already\|not running' && pass "Double hibernate: returns error" || fail "Double hibernate: $RESULT"

# Destroy hibernated sandbox
api -X DELETE "$API_URL/api/sandboxes/$SB2" >/dev/null
sleep 1
STATUS=$(api "$API_URL/api/sandboxes/$SB2" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','gone'))" 2>/dev/null)
[ "$STATUS" != "running" ] && pass "Destroy hibernated sandbox (status=$STATUS)" || fail "Destroy hibernated: still running"

# Create with zero timeout (should default)
h "Default Timeout"
SB3=$(create_sandbox 0)
SANDBOXES+=("$SB3")
OUT=$(exec_stdout "$SB3" "echo" "default-timeout-ok")
[ "$OUT" = "default-timeout-ok" ] && pass "Create with timeout=0 works" || fail "Default timeout: '$OUT'"

# Binary file round-trip
h "Binary File Round-Trip"
SB4=$(create_sandbox)
SANDBOXES+=("$SB4")

# Generate random binary data, write via API, read back, compare
dd if=/dev/urandom bs=4096 count=1 2>/dev/null > /tmp/qemu-test-binary.bin
HASH_LOCAL=$(shasum -a 256 /tmp/qemu-test-binary.bin | cut -d' ' -f1)

api -X PUT "$API_URL/api/sandboxes/$SB4/files?path=/workspace/binary.bin" \
    -H "Content-Type: application/octet-stream" \
    --data-binary @/tmp/qemu-test-binary.bin >/dev/null

HASH_REMOTE=$(exec_stdout "$SB4" "sha256sum" "/workspace/binary.bin" | awk '{print $1}')
[ "$HASH_LOCAL" = "$HASH_REMOTE" ] && pass "Binary file round-trip (hash match)" || fail "Binary mismatch: $HASH_LOCAL vs $HASH_REMOTE"
rm -f /tmp/qemu-test-binary.bin

# Auth tests
h "Authentication"
RESULT=$(curl -s --max-time 5 -X POST "$API_URL/api/sandboxes" \
    -H "Content-Type: application/json" -d '{"timeout":300}')
echo "$RESULT" | grep -qi 'unauthorized\|missing.*key\|api key' && pass "No API key: 401" || fail "No key: $RESULT"

RESULT=$(curl -s --max-time 5 -X POST "$API_URL/api/sandboxes" \
    -H "Content-Type: application/json" -H "X-API-Key: wrong-key" -d '{"timeout":300}')
echo "$RESULT" | grep -qi 'invalid\|forbidden\|unauthorized' && pass "Wrong API key: rejected" || fail "Wrong key: $RESULT"

RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":300}')
echo "$RESULT" | grep -q 'sandboxID' && pass "Correct API key: 200" || fail "Correct key: $RESULT"
NEW_SB=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
[ -n "$NEW_SB" ] && SANDBOXES+=("$NEW_SB")

summary
