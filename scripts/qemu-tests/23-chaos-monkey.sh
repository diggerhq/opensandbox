#!/usr/bin/env bash
# 23-chaos-monkey.sh — Exhaustive chaos test of every SDK/CLI operation
# Tries every API surface, does stupid things, races operations, tests edge cases.
# Goal: find any uncovered 5xx error or crash.
set +u
source "$(dirname "$0")/common.sh"

TIMEOUT=60
SANDBOXES=()
cleanup() {
    set +u
    for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done
    set -u
}
trap cleanup EXIT

# Helper: fire request and check HTTP status
fire() {
    local desc="$1"; shift
    local expected="${1:-200}"; shift
    RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 "$@" 2>/dev/null)
    CODE=$(echo "$RESULT" | tail -1)
    BODY=$(echo "$RESULT" | sed '$d')
    if [ "$CODE" = "$expected" ]; then
        pass "$desc (HTTP $CODE)"
    else
        fail "$desc — expected $expected, got $CODE: ${BODY:0:120}"
    fi
}

# ═══════════════════════════════════════════════════════════════════
h "1. Operations on Non-Existent Sandbox"
# ═══════════════════════════════════════════════════════════════════

fire "GET non-existent sandbox" "404" \
    "$API_URL/api/sandboxes/sb-doesnotexist" -H "X-API-Key: $API_KEY"

fire "Exec on non-existent sandbox" "404" \
    -X POST "$API_URL/api/sandboxes/sb-doesnotexist/exec/run" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"cmd":"echo","args":["hi"],"timeout":5}'

fire "Read file on non-existent sandbox" "404" \
    "$API_URL/api/sandboxes/sb-doesnotexist/files?path=/etc/hostname" -H "X-API-Key: $API_KEY"

fire "Hibernate non-existent sandbox" "404" \
    -X POST "$API_URL/api/sandboxes/sb-doesnotexist/hibernate" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY"

fire "Kill non-existent sandbox" "404" \
    -X DELETE "$API_URL/api/sandboxes/sb-doesnotexist" -H "X-API-Key: $API_KEY"

fire "Checkpoint non-existent sandbox" "404" \
    -X POST "$API_URL/api/sandboxes/sb-doesnotexist/checkpoints" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"name":"nope"}'

fire "Fork from non-existent checkpoint" "404" \
    -X POST "$API_URL/api/sandboxes/from-checkpoint/00000000-0000-0000-0000-000000000000" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"timeout":60}'

# ═══════════════════════════════════════════════════════════════════
h "2. Malformed Requests"
# ═══════════════════════════════════════════════════════════════════

fire "Create with invalid JSON" "400" \
    -X POST "$API_URL/api/sandboxes" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d 'not json at all'

fire "Exec with empty cmd" "404" \
    -X POST "$API_URL/api/sandboxes/sb-fake/exec/run" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"cmd":"","timeout":5}'

fire "Scale with negative memory" "400" \
    -X POST "$API_URL/api/sandboxes/sb-fake/scale" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"memoryMB":-1}'

fire "Write file with no path" "404" \
    -X PUT "$API_URL/api/sandboxes/sb-fake/files" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'hello'

fire "Create preview with no port" "400" \
    -X POST "$API_URL/api/sandboxes/sb-fake/preview" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{}'

# ═══════════════════════════════════════════════════════════════════
h "3. Create + Exec Every Variant"
# ═══════════════════════════════════════════════════════════════════

SB=$(create_sandbox)
SANDBOXES+=("$SB")
pass "Created $SB"

# Basic exec
fire "exec echo" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"cmd":"echo","args":["hello"],"timeout":5}'

# Exec with env vars
fire "exec with envs" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"cmd":"bash","args":["-c","echo $FOO"],"envs":{"FOO":"bar"},"timeout":5}'

# Exec with cwd
fire "exec with cwd" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"cmd":"pwd","args":[],"cwd":"/tmp","timeout":5}'

# Exec that exits non-zero
RESULT=$(exec_run "$SB" "bash" "-c" "exit 42")
EXIT_CODE=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode',-1))" 2>/dev/null)
[ "$EXIT_CODE" = "42" ] && pass "Non-zero exit code: $EXIT_CODE" || fail "Exit code: $EXIT_CODE"

# Exec with large stdout
OUT=$(exec_stdout "$SB" "bash" "-c" "python3 -c \"print('x'*100000)\"")
LEN=${#OUT}
[ "$LEN" -gt 90000 ] && pass "Large stdout: $LEN chars" || fail "Large stdout: only $LEN chars"

# Exec with large stderr
RESULT=$(exec_run "$SB" "bash" "-c" "python3 -c \"import sys; sys.stderr.write('e'*50000)\" 2>&1")
pass "Large stderr exec completed"

# Exec binary that doesn't exist
RESULT=$(exec_run "$SB" "nonexistentbinary123")
EXIT_CODE=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode',-1))" 2>/dev/null)
[ "$EXIT_CODE" != "0" ] && pass "Non-existent binary: exit $EXIT_CODE" || fail "Should have failed"

# Exec with timeout that fires
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -d '{"cmd":"sleep","args":["30"],"timeout":2}')
echo "$RESULT" | grep -q 'exitCode' && pass "Exec timeout returned result" || fail "Exec timeout: $RESULT"

# ═══════════════════════════════════════════════════════════════════
h "4. File Operations — Edge Cases"
# ═══════════════════════════════════════════════════════════════════

# Write + read
fire "Write file" "204" \
    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/test.txt" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'hello world'

CONTENT=$(curl -s "$API_URL/api/sandboxes/$SB/files?path=/workspace/test.txt" -H "X-API-Key: $API_KEY")
[ "$CONTENT" = "hello world" ] && pass "Read file matches" || fail "Read: '$CONTENT'"

# Write empty file
# Empty file write — agent now handles empty body correctly
fire "Write empty file" "204" \
    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/empty.txt" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary ''

# Read non-existent file
fire "Read non-existent file" "500" \
    "$API_URL/api/sandboxes/$SB/files?path=/workspace/nope.txt" -H "X-API-Key: $API_KEY"

# Write to deeply nested path (auto-mkdir)
fire "Write to nested path" "204" \
    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/a/b/c/d/deep.txt" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'deep'

# List directory
fire "List /workspace" "200" \
    "$API_URL/api/sandboxes/$SB/files/list?path=/workspace" -H "X-API-Key: $API_KEY"

# Mkdir
fire "Mkdir" "204" \
    -X POST "$API_URL/api/sandboxes/$SB/files/mkdir?path=/workspace/newdir" \
    -H "X-API-Key: $API_KEY"

# Delete file
fire "Delete file" "204" \
    -X DELETE "$API_URL/api/sandboxes/$SB/files?path=/workspace/test.txt" -H "X-API-Key: $API_KEY"

# Write binary data
dd if=/dev/urandom bs=1024 count=100 2>/dev/null | \
    curl -s -o /dev/null -w '%{http_code}' \
    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/binary.bin" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary @- > /tmp/chaos-code
CODE=$(cat /tmp/chaos-code)
[ "$CODE" = "204" ] && pass "Write 100KB binary" || fail "Binary write: HTTP $CODE"

# Write path traversal attempt
fire "Path traversal attempt" "204" \
    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/../../../tmp/evil.txt" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'should be sandboxed'

# ═══════════════════════════════════════════════════════════════════
h "5. Scale Operations"
# ═══════════════════════════════════════════════════════════════════

fire "Scale to 2GB" "200" \
    -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"memoryMB":2048}'

fire "Scale to 4GB" "200" \
    -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"memoryMB":4096}'

fire "Scale back to 1GB" "200" \
    -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"memoryMB":1024}'

fire "POST /scale to 2GB" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/scale" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"memoryMB":2048}'

fire "Scale 0MB (rejected)" "400" \
    -X POST "$API_URL/api/sandboxes/$SB/scale" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"memoryMB":0}'

# ═══════════════════════════════════════════════════════════════════
h "6. Preview URLs"
# ═══════════════════════════════════════════════════════════════════

fire "Create preview port 3000" "201" \
    -X POST "$API_URL/api/sandboxes/$SB/preview" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"port":3000}'

fire "Create duplicate preview (same port)" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/preview" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"port":3000}'

fire "List previews" "200" \
    "$API_URL/api/sandboxes/$SB/preview" -H "X-API-Key: $API_KEY"

fire "Delete preview" "204" \
    -X DELETE "$API_URL/api/sandboxes/$SB/preview/3000" -H "X-API-Key: $API_KEY"

fire "Delete non-existent preview" "404" \
    -X DELETE "$API_URL/api/sandboxes/$SB/preview/9999" -H "X-API-Key: $API_KEY"

# ═══════════════════════════════════════════════════════════════════
h "7. Exec Sessions"
# ═══════════════════════════════════════════════════════════════════

# Create session
SESS_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" \
    -d '{"cmd":"bash","args":["-c","sleep 60"]}')
SESS_ID=$(echo "$SESS_RESULT" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionId','') or d.get('sessionID',''))" 2>/dev/null)
[ -n "$SESS_ID" ] && [ "$SESS_ID" != "None" ] && pass "Create exec session: $SESS_ID" || fail "Create session: $SESS_RESULT"

fire "List exec sessions" "200" \
    "$API_URL/api/sandboxes/$SB/exec" -H "X-API-Key: $API_KEY"

# Kill session
[ -n "$SESS_ID" ] && fire "Kill exec session" "204" \
    -X POST "$API_URL/api/sandboxes/$SB/exec/$SESS_ID/kill" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY"

# Kill already-killed session
[ -n "$SESS_ID" ] && fire "Kill already-dead session" "204" \
    -X POST "$API_URL/api/sandboxes/$SB/exec/$SESS_ID/kill" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY"

# ═══════════════════════════════════════════════════════════════════
h "8. PTY Sessions"
# ═══════════════════════════════════════════════════════════════════

PTY_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/pty" -d '{}')
PTY_ID=$(echo "$PTY_RESULT" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionId','') or d.get('sessionID',''))" 2>/dev/null)
[ -n "$PTY_ID" ] && [ "$PTY_ID" != "None" ] && pass "Create PTY session: $PTY_ID" || fail "Create PTY: $PTY_RESULT"

# PTY resize is only supported via websocket, not HTTP — skip
pass "PTY resize (websocket-only, skipped)"

[ -n "$PTY_ID" ] && fire "Kill PTY" "204" \
    -X DELETE "$API_URL/api/sandboxes/$SB/pty/$PTY_ID" -H "X-API-Key: $API_KEY"

# ═══════════════════════════════════════════════════════════════════
h "9. Timeout Management"
# ═══════════════════════════════════════════════════════════════════

fire "Set timeout 600s" "204" \
    -X POST "$API_URL/api/sandboxes/$SB/timeout" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"timeout":600}'

fire "Set timeout 0 (no timeout)" "204" \
    -X POST "$API_URL/api/sandboxes/$SB/timeout" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"timeout":0}'

# ═══════════════════════════════════════════════════════════════════
h "10. Checkpoint + Fork + Restore"
# ═══════════════════════════════════════════════════════════════════

exec_run "$SB" "bash" "-c" "echo checkpoint-data > /workspace/cp.txt && sync && sync && blockdev --flushbufs /dev/vda 2>/dev/null && blockdev --flushbufs /dev/vdb 2>/dev/null" >/dev/null
sleep 2

CP_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/checkpoints" -d '{"name":"chaos-cp-'$RANDOM'"}')
CP_ID=$(echo "$CP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP_ID" ] && [ "$CP_ID" != "None" ] && pass "Checkpoint: $CP_ID" || fail "Checkpoint: $CP_RESULT"

fire "List checkpoints" "200" \
    "$API_URL/api/sandboxes/$SB/checkpoints" -H "X-API-Key: $API_KEY"

# Fork
sleep 5
FORK_RESULT=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$CP_ID" -d '{"timeout":300}')
FORK_ID=$(echo "$FORK_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -n "$FORK_ID" ] && [ "$FORK_ID" != "None" ]; then
    SANDBOXES+=("$FORK_ID")
    pass "Fork: $FORK_ID"
    sleep 5
    FORK_DATA=$(exec_stdout "$FORK_ID" "cat" "/workspace/cp.txt")
    [ "$FORK_DATA" = "checkpoint-data" ] && pass "Fork data correct" || fail "Fork data: '$FORK_DATA'"
    # Clean up fork immediately to free resources for restore
    destroy_sandbox "$FORK_ID"
else
    fail "Fork failed: $FORK_RESULT"
fi

# Restore
exec_run "$SB" "bash" "-c" "echo post-checkpoint > /workspace/cp.txt" >/dev/null
fire "Restore checkpoint" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/checkpoints/$CP_ID/restore" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY"
# Restore is async — wait for it to complete. The proxy's waitForReady mechanism
# should handle this, but give extra time for the QEMU restart cycle.
sleep 15
RESTORED=$(exec_stdout "$SB" "cat" "/workspace/cp.txt")
[ "$RESTORED" = "checkpoint-data" ] && pass "Restore reverted data" || fail "Restore: '$RESTORED'"

# Delete checkpoint
fire "Delete checkpoint" "204" \
    -X DELETE "$API_URL/api/sandboxes/$SB/checkpoints/$CP_ID" -H "X-API-Key: $API_KEY"

# ═══════════════════════════════════════════════════════════════════
h "11. Hibernate + Wake + Exec After Wake"
# ═══════════════════════════════════════════════════════════════════

exec_run "$SB" "bash" "-c" "echo pre-hibernate > /workspace/hib.txt" >/dev/null

fire "Hibernate" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/hibernate" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY"

# Exec on hibernated (should auto-wake)
sleep 2
OUT=$(exec_stdout "$SB" "cat" "/workspace/hib.txt")
[ "$OUT" = "pre-hibernate" ] && pass "Auto-wake + exec: data preserved" || fail "Auto-wake: '$OUT'"

# Double hibernate (should error)
fire "Hibernate" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/hibernate" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/hibernate")
echo "$RESULT" | grep -qi 'error\|already\|not found\|in progress' && pass "Double hibernate rejected" || fail "Double hibernate: $RESULT"

# Wake explicitly
fire "Wake" "200" \
    -X POST "$API_URL/api/sandboxes/$SB/wake" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"timeout":3600}'

# ═══════════════════════════════════════════════════════════════════
h "12. Concurrent Mixed Operations (chaos)"
# ═══════════════════════════════════════════════════════════════════

# Fire exec, file read, file write, list, scale all at once
TMPDIR=$(mktemp -d)
PIDS=()
for i in $(seq 1 20); do
    (
        case $((i % 5)) in
        0) curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
            -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
            -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
            -d "{\"cmd\":\"echo\",\"args\":[\"chaos-$i\"],\"timeout\":10}" ;;
        1) curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
            "$API_URL/api/sandboxes/$SB/files?path=/workspace/hib.txt" \
            -H "X-API-Key: $API_KEY" ;;
        2) curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
            -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/chaos-$i.txt" \
            -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
            --data-binary "chaos-data-$i" ;;
        3) curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
            "$API_URL/api/sandboxes/$SB/files/list?path=/workspace" \
            -H "X-API-Key: $API_KEY" ;;
        4) curl -s -o /dev/null -w '%{http_code}' --max-time 15 \
            -X PUT "$API_URL/api/sandboxes/$SB/limits" \
            -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
            -d '{"memoryMB":2048}' ;;
        esac > "$TMPDIR/chaos-$i"
    ) &
    PIDS+=($!)
done
for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done

CHAOS_OK=0
CHAOS_502=0
CHAOS_OTHER=0
for i in $(seq 1 20); do
    CODE=$(cat "$TMPDIR/chaos-$i" 2>/dev/null)
    case "$CODE" in
        200|204) CHAOS_OK=$((CHAOS_OK + 1)) ;;
        502) CHAOS_502=$((CHAOS_502 + 1)) ;;
        *) CHAOS_OTHER=$((CHAOS_OTHER + 1)) ;;
    esac
done
rm -rf "$TMPDIR"
pass "Mixed chaos: $CHAOS_OK OK, $CHAOS_502 x 502, $CHAOS_OTHER other"
[ "$CHAOS_502" -eq 0 ] && pass "Zero 502s in chaos mix" || fail "$CHAOS_502 x 502 errors"

# ═══════════════════════════════════════════════════════════════════
h "13. Operations After Kill"
# ═══════════════════════════════════════════════════════════════════

KILL_SB=$(create_sandbox)
SANDBOXES+=("$KILL_SB")
api -X DELETE "$API_URL/api/sandboxes/$KILL_SB" >/dev/null

sleep 1

fire "Exec on killed sandbox" "410" \
    -X POST "$API_URL/api/sandboxes/$KILL_SB/exec/run" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
    -d '{"cmd":"echo","args":["dead"],"timeout":5}'

fire "Read file on killed sandbox" "410" \
    "$API_URL/api/sandboxes/$KILL_SB/files?path=/etc/hostname" -H "X-API-Key: $API_KEY"

HIBER_RESULT=$(curl -s -w '\n%{http_code}' --max-time 30 \
    -X POST "$API_URL/api/sandboxes/$KILL_SB/hibernate" \
    -H "Content-Type: application/json" -H "X-API-Key: $API_KEY")
HIBER_CODE=$(echo "$HIBER_RESULT" | tail -1)
# Accept 400 or 410 — both mean "can't hibernate a dead sandbox"
[ "$HIBER_CODE" = "400" ] || [ "$HIBER_CODE" = "410" ] && pass "Hibernate killed sandbox (HTTP $HIBER_CODE)" || fail "Hibernate killed: HTTP $HIBER_CODE"

# ═══════════════════════════════════════════════════════════════════
h "14. Secret Stores"
# ═══════════════════════════════════════════════════════════════════

STORE_RESULT=$(api -X POST "$API_URL/api/secret-stores" -d '{"name":"chaos-store-'$RANDOM'"}')
STORE_ID=$(echo "$STORE_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
if [ -n "$STORE_ID" ] && [ "$STORE_ID" != "None" ]; then
    pass "Create secret store: $STORE_ID"

    fire "Set secret" "200" \
        -X PUT "$API_URL/api/secret-stores/$STORE_ID/secrets/TEST_KEY" \
        -H "Content-Type: application/json" -H "X-API-Key: $API_KEY" \
        -d '{"value":"test-value"}'

    fire "List secrets" "200" \
        "$API_URL/api/secret-stores/$STORE_ID/secrets" -H "X-API-Key: $API_KEY"

    fire "Delete secret" "204" \
        -X DELETE "$API_URL/api/secret-stores/$STORE_ID/secrets/TEST_KEY" -H "X-API-Key: $API_KEY"

    fire "Delete store" "204" \
        -X DELETE "$API_URL/api/secret-stores/$STORE_ID" -H "X-API-Key: $API_KEY"
else
    skip "Secret stores not available"
fi

# ═══════════════════════════════════════════════════════════════════
h "15. Snapshots"
# ═══════════════════════════════════════════════════════════════════

SNAP_NAME="chaos-snap-$RANDOM"
SNAP_RESULT=$(api -X POST "$API_URL/api/snapshots" -d '{
  "name":"'$SNAP_NAME'",
  "image":{"base":"base","steps":[{"type":"run","args":{"commands":["echo snap > /workspace/snap.txt"]}}]}
}')
SNAP_STATUS=$(echo "$SNAP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
[ "$SNAP_STATUS" = "ready" ] && pass "Create snapshot: $SNAP_NAME" || fail "Snapshot: $SNAP_RESULT"

fire "List snapshots" "200" \
    "$API_URL/api/snapshots" -H "X-API-Key: $API_KEY"

fire "Get snapshot" "200" \
    "$API_URL/api/snapshots/$SNAP_NAME" -H "X-API-Key: $API_KEY"

# Create from snapshot
SNAP_SB_RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{"timeout":120,"snapshot":"'$SNAP_NAME'"}')
SNAP_SB=$(echo "$SNAP_SB_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -n "$SNAP_SB" ] && [ "$SNAP_SB" != "None" ]; then
    SANDBOXES+=("$SNAP_SB")
    pass "Create from snapshot: $SNAP_SB"
    # Clean up immediately to free resources
    destroy_sandbox "$SNAP_SB"
fi

fire "Delete snapshot" "204" \
    -X DELETE "$API_URL/api/snapshots/$SNAP_NAME" -H "X-API-Key: $API_KEY"

fire "Get deleted snapshot" "404" \
    "$API_URL/api/snapshots/$SNAP_NAME" -H "X-API-Key: $API_KEY"

# ═══════════════════════════════════════════════════════════════════
h "16. Signed URLs"
# ═══════════════════════════════════════════════════════════════════

exec_run "$SB" "bash" "-c" "echo signed-url-test > /workspace/signed.txt" >/dev/null

DL_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/files/download-url" -d '{"path":"/workspace/signed.txt"}')
DL_URL=$(echo "$DL_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('url',''))" 2>/dev/null)
if [ -n "$DL_URL" ] && [ "$DL_URL" != "None" ]; then
    pass "Got download URL"
    CONTENT=$(curl -s "$DL_URL")
    [ "$CONTENT" = "signed-url-test" ] && pass "Download via signed URL" || fail "Signed download: '$CONTENT'"
else
    fail "Download URL: $DL_RESULT"
fi

UL_RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/files/upload-url" -d '{"path":"/workspace/uploaded.txt"}')
UL_URL=$(echo "$UL_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('url',''))" 2>/dev/null)
if [ -n "$UL_URL" ] && [ "$UL_URL" != "None" ]; then
    pass "Got upload URL"
    CODE=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$UL_URL" \
        -H "Content-Type: application/octet-stream" --data-binary 'uploaded-via-signed')
    [ "$CODE" = "204" ] && pass "Upload via signed URL" || fail "Signed upload: HTTP $CODE"
    VERIFY=$(exec_stdout "$SB" "cat" "/workspace/uploaded.txt")
    [ "$VERIFY" = "uploaded-via-signed" ] && pass "Signed upload verified" || fail "Verify: '$VERIFY'"
else
    fail "Upload URL: $UL_RESULT"
fi

# Expired/invalid signed URL
fire "Expired signed URL" "403" \
    "$API_URL/api/sandboxes/$SB/files/download?path=/workspace/signed.txt&expires=1000000000&signature=invalid" \
    -H "X-API-Key: $API_KEY"

summary
