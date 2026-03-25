#!/usr/bin/env bash
# 04-metadata.sh — Internal metadata API (169.254.169.254)
source "$(dirname "$0")/common.sh"

h "Metadata API (169.254.169.254)"

SB=$(create_sandbox)
trap "destroy_sandbox $SB" EXIT

# Status
STATUS=$(exec_stdout "$SB" "curl" "-s" "http://169.254.169.254/v1/status")
echo "$STATUS" | grep -q "\"sandboxId\":\"$SB\"" && pass "Status: sandboxId matches" || fail "Status sandboxId: $STATUS"
echo "$STATUS" | grep -q '"uptime"' && pass "Status: has uptime" || fail "Status uptime: $STATUS"

# Limits
LIMITS=$(exec_stdout "$SB" "curl" "-s" "http://169.254.169.254/v1/limits")
echo "$LIMITS" | grep -q '"memLimit"' && pass "Limits: has memLimit" || fail "Limits: $LIMITS"
echo "$LIMITS" | grep -q '"pids"' && pass "Limits: has pids" || fail "Limits pids: $LIMITS"

# Metadata
META=$(exec_stdout "$SB" "curl" "-s" "http://169.254.169.254/v1/metadata")
echo "$META" | grep -q '"region"' && pass "Metadata: has region" || fail "Metadata region: $META"
echo "$META" | grep -q '"template"' && pass "Metadata: has template" || fail "Metadata template: $META"

# Clock sync
GUEST_TIME=$(exec_stdout "$SB" "date" "+%s")
HOST_TIME=$(date +%s)
DRIFT=$((GUEST_TIME - HOST_TIME))
[ "$DRIFT" -ge -2 ] && [ "$DRIFT" -le 2 ] && pass "Clock sync: drift=${DRIFT}s" || fail "Clock drift: ${DRIFT}s"

summary
