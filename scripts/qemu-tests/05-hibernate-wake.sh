#!/usr/bin/env bash
# 05-hibernate-wake.sh — Hibernate/wake with data persistence
source "$(dirname "$0")/common.sh"

TIMEOUT=120

h "Hibernate / Wake"

SB=$(create_sandbox)
# No trap — we manage cleanup manually

# Write marker file
exec_run "$SB" "bash" "-c" "echo persist-test-42 > /workspace/marker.txt" >/dev/null
pass "Write marker file"

# Hibernate
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/hibernate")
echo "$RESULT" | grep -q '"hibernated"' && pass "Hibernate" || { fail "Hibernate: $RESULT"; destroy_sandbox "$SB"; exit 1; }

# Wake
RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/wake" -d '{"timeout":3600}')
echo "$RESULT" | grep -q '"running"' && pass "Wake" || { fail "Wake: $RESULT"; destroy_sandbox "$SB"; exit 1; }

# Verify workspace file
OUT=$(exec_stdout "$SB" "cat" "/workspace/marker.txt")
[ "$OUT" = "persist-test-42" ] && pass "Workspace file survived" || fail "Workspace file: '$OUT'"

# Rootfs persistence (apt install)
h "Rootfs Persistence"
exec_run "$SB" "bash" "-c" "apt-get update -qq && apt-get install -y -qq cowsay 2>/dev/null" >/dev/null
OUT=$(exec_stdout "$SB" "bash" "-c" "/usr/games/cowsay test 2>/dev/null | head -1")
[ -n "$OUT" ] && pass "cowsay installed" || fail "cowsay install"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/hibernate")
echo "$RESULT" | grep -q '"hibernated"' && pass "Hibernate (with cowsay)" || fail "Hibernate 2"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/wake" -d '{"timeout":3600}')
echo "$RESULT" | grep -q '"running"' && pass "Wake (2nd)" || fail "Wake 2"

OUT=$(exec_stdout "$SB" "bash" "-c" "/usr/games/cowsay survived 2>/dev/null | head -1")
[ -n "$OUT" ] && pass "cowsay survived hibernate" || fail "cowsay after wake"

# Workspace large file with hash
h "Large File Integrity"
HASH_BEFORE=$(exec_stdout "$SB" "bash" "-c" "dd if=/dev/urandom of=/workspace/big.bin bs=1M count=100 2>/dev/null && sha256sum /workspace/big.bin | cut -d' ' -f1")
[ -n "$HASH_BEFORE" ] && pass "Write 100MB (hash: ${HASH_BEFORE:0:16}...)" || fail "Write 100MB"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/hibernate")
echo "$RESULT" | grep -q '"hibernated"' && pass "Hibernate (100MB)" || fail "Hibernate 3"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/wake" -d '{"timeout":3600}')
echo "$RESULT" | grep -q '"running"' && pass "Wake (3rd)" || fail "Wake 3"

HASH_AFTER=$(exec_stdout "$SB" "bash" "-c" "sha256sum /workspace/big.bin | cut -d' ' -f1")
[ "$HASH_BEFORE" = "$HASH_AFTER" ] && pass "100MB hash matches after wake" || fail "Hash mismatch: $HASH_BEFORE vs $HASH_AFTER"

# Scaled memory persists
h "Memory Persists Across Hibernate"
api -X PUT "$API_URL/api/sandboxes/$SB/limits" \
    -d '{"maxMemoryMB":2048,"cpuPercent":200,"maxPids":256}' >/dev/null
sleep 1
MEM_BEFORE=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
pass "Scaled to 2GB: ${MEM_BEFORE}MB"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/hibernate")
echo "$RESULT" | grep -q '"hibernated"' && pass "Hibernate (2GB)" || fail "Hibernate 4"

RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/wake" -d '{"timeout":3600}')
echo "$RESULT" | grep -q '"running"' && pass "Wake (4th)" || fail "Wake 4"

MEM_AFTER=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
[ "$MEM_AFTER" -gt 1800 ] && pass "Memory persisted: ${MEM_AFTER}MB" || fail "Memory after wake: ${MEM_AFTER}MB"

# Clock sync after wake
GUEST=$(exec_stdout "$SB" "date" "+%s")
HOST=$(date +%s)
DRIFT=$((GUEST - HOST))
[ "$DRIFT" -ge -2 ] && [ "$DRIFT" -le 2 ] && pass "Clock sync after wake: drift=${DRIFT}s" || fail "Clock drift: ${DRIFT}s"

destroy_sandbox "$SB"
summary
