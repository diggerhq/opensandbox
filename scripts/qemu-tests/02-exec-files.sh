#!/usr/bin/env bash
# 02-exec-files.sh — Exec commands, file read/write/list
source "$(dirname "$0")/common.sh"

h "Exec & File I/O"

SB=$(create_sandbox)
trap "destroy_sandbox $SB" EXIT

# Basic exec
OUT=$(exec_stdout "$SB" "echo" "hello world")
[ "$OUT" = "hello world" ] && pass "exec echo" || fail "exec echo: '$OUT'"

# Python exec
OUT=$(exec_stdout "$SB" "python3" "-c" "print(1+1)")
[ "$OUT" = "2" ] && pass "exec python3" || fail "exec python3: '$OUT'"

# Exec with env
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -d '{"cmd":"bash","args":["-c","echo $FOO"],"envs":{"FOO":"bar123"},"timeout":5}')
OUT=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin)['stdout'].strip())")
[ "$OUT" = "bar123" ] && pass "exec with envs" || fail "exec with envs: '$OUT'"

# Exec with cwd
OUT=$(exec_stdout "$SB" "pwd")
# default cwd
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -d '{"cmd":"pwd","args":[],"cwd":"/workspace","timeout":5}')
OUT=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin)['stdout'].strip())")
[ "$OUT" = "/workspace" ] && pass "exec with cwd" || fail "exec with cwd: '$OUT'"

# Exec timeout
START=$(date +%s)
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/exec/run" \
    -d '{"cmd":"sleep","args":["30"],"timeout":3}')
END=$(date +%s)
ELAPSED=$((END - START))
[ "$ELAPSED" -lt 10 ] && pass "exec timeout (${ELAPSED}s)" || fail "exec timeout took ${ELAPSED}s"

# Write file
api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/test.txt" \
    -H "Content-Type: application/octet-stream" \
    --data-binary "hello world" >/dev/null
pass "write file"

# Read file
CONTENT=$(api "$API_URL/api/sandboxes/$SB/files?path=/workspace/test.txt")
[ "$CONTENT" = "hello world" ] && pass "read file" || fail "read file: '$CONTENT'"

# List dir
LIST=$(api "$API_URL/api/sandboxes/$SB/files/list?path=/workspace")
echo "$LIST" | grep -q "test.txt" && pass "list dir" || fail "list dir"

# Large file (10MB)
dd if=/dev/urandom bs=1M count=10 2>/dev/null | base64 > /tmp/qemu-test-large.txt
api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/large.bin" \
    -H "Content-Type: application/octet-stream" \
    --data-binary @/tmp/qemu-test-large.txt >/dev/null
SIZE=$(exec_stdout "$SB" "stat" "-c" "%s" "/workspace/large.bin")
EXPECTED=$(wc -c < /tmp/qemu-test-large.txt | tr -d ' ')
[ "$SIZE" = "$EXPECTED" ] && pass "large file (${SIZE} bytes)" || fail "large file: got $SIZE expected $EXPECTED"
rm -f /tmp/qemu-test-large.txt

summary
