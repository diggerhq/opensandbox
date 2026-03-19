#!/usr/bin/env bash
# common.sh — Shared helpers for QEMU backend test scripts
set -euo pipefail

API_URL="${OPENSANDBOX_API_URL:?Set OPENSANDBOX_API_URL}"
API_KEY="${OPENSANDBOX_API_KEY:?Set OPENSANDBOX_API_KEY}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/opensandbox-digger.pem}"
WORKER_HOST="${WORKER_HOST:?Set WORKER_HOST}"

PASS=0
FAIL=0
SKIP=0

h() { printf '\n\033[1;34m=== %s ===\033[0m\n' "$*"; }
pass() { PASS=$((PASS+1)); printf '  \033[32m✓ %s\033[0m\n' "$*"; }
fail() { FAIL=$((FAIL+1)); printf '  \033[31m✗ %s\033[0m\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  \033[33m⊘ %s\033[0m\n' "$*"; }
summary() {
    echo ""
    printf '\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$PASS" "$FAIL" "$SKIP"
    [ "$FAIL" -eq 0 ] && exit 0 || exit 1
}

api() {
    curl -s --max-time "${TIMEOUT:-30}" \
        -H "Content-Type: application/json" \
        -H "X-API-Key: $API_KEY" \
        "$@"
}

create_sandbox() {
    local timeout="${1:-3600}"
    api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":$timeout}" | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])"
}

destroy_sandbox() {
    api -X DELETE "$API_URL/api/sandboxes/$1" >/dev/null 2>&1 || true
}

exec_run() {
    local sb="$1"; shift
    local cmd="$1"; shift
    local args=""
    if [ $# -gt 0 ]; then
        args=$(printf '%s\n' "$@" | python3 -c "import sys,json; print(json.dumps([l.strip() for l in sys.stdin]))")
    else
        args="[]"
    fi
    api -X POST "$API_URL/api/sandboxes/$sb/exec/run" \
        -d "{\"cmd\":\"$cmd\",\"args\":$args,\"timeout\":30}"
}

exec_stdout() {
    local result
    result=$(exec_run "$@")
    echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('stdout','').strip())" 2>/dev/null
}

exec_exit_code() {
    local result
    result=$(exec_run "$@")
    echo "$result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('exitCode',-999))" 2>/dev/null
}

wait_for_sandbox() {
    local sb="$1" max="${2:-30}"
    for i in $(seq 1 "$max"); do
        local status
        status=$(api "$API_URL/api/sandboxes/$sb" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
        [ "$status" = "running" ] && return 0
        sleep 1
    done
    return 1
}

ssh_worker() {
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -i "$SSH_KEY" "ubuntu@$WORKER_HOST" "$@"
}
