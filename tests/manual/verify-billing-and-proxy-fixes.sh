#!/usr/bin/env bash
# Verifies the two billing/proxy fixes deployed on a single-host dev VM.
#
# Required env (export before running):
#   OPENCOMPUTER_API_URL  — dev API URL, e.g. http://20.101.100.215:8080
#   OPENCOMPUTER_API_KEY  — dev API key, e.g. opensandbox-dev
#   DEV_VM                — public IP of the dev VM, e.g. 20.101.100.215
#   DEV_KEY               — path to SSH private key for the VM
#
# Optional env (with defaults shown):
#   DEV_USER=ubuntu
#   PG_USER=opensandbox
#   PG_DB=opensandbox
#   PG_PASS=opensandbox
#   OC=oc                 — path to the oc binary if not on PATH
#
# Exit code 0 = both PASS. Non-zero on any failure.
#
# Tests:
#   A) Scale event recorded on fork-from-checkpoint (worker grpc fix)
#   B) Allowlist-only secret store registers a proxy session and enforces
#      the allowlist (secrets-proxy + qemu fix)

set -euo pipefail

DEV_USER=${DEV_USER:-ubuntu}
PG_USER=${PG_USER:-opensandbox}
PG_DB=${PG_DB:-opensandbox}
PG_PASS=${PG_PASS:-opensandbox}
OC=${OC:-oc}

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[33m%s\033[0m\n' "$*"; }
hr()    { printf '%s\n' '────────────────────────────────────────────────────────────'; }

require() {
  local name=$1
  if [ -z "${!name:-}" ]; then
    red "missing required env: $name"
    exit 2
  fi
}

require OPENCOMPUTER_API_URL
require OPENCOMPUTER_API_KEY
require DEV_VM
require DEV_KEY

if ! command -v "$OC" >/dev/null 2>&1; then
  red "oc binary not found on PATH (set OC=/path/to/oc to override)"
  exit 2
fi
if [ ! -r "$DEV_KEY" ]; then
  red "DEV_KEY not readable: $DEV_KEY"
  exit 2
fi

ssh_dev() {
  ssh -i "$DEV_KEY" \
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=10 -o LogLevel=ERROR \
      "${DEV_USER}@${DEV_VM}" "$@"
}

psql_dev() {
  ssh_dev "PGPASSWORD=${PG_PASS} psql -h localhost -U ${PG_USER} -d ${PG_DB} -t -A -c \"$1\""
}

# Quick reachability checks
hr; yellow "Pre-flight checks"
ssh_dev 'echo VM reachable && systemctl is-active opensandbox-worker && systemctl is-active opensandbox-server' \
  || { red "ssh / services check failed"; exit 2; }
$OC sandbox list >/dev/null \
  || { red "oc API check failed (OPENCOMPUTER_API_URL=$OPENCOMPUTER_API_URL)"; exit 2; }
green "OK"

# State for cleanup
CREATED_SBS=()
CREATED_CHECKPOINTS=()
CREATED_STORES=()

cleanup() {
  hr; yellow "Cleanup"
  for sb in "${CREATED_SBS[@]:-}"; do
    [ -n "$sb" ] && $OC sandbox kill "$sb" 2>/dev/null || true
  done
  # NOTE: checkpoints are tied to source sandbox; killed source cleans up its own
  # checkpoints? If not, they'll remain in DB but stop being usable.
  for store in "${CREATED_STORES[@]:-}"; do
    [ -n "$store" ] && $OC secret-store delete "$store" 2>/dev/null || true
  done
  green "Cleanup done"
}
trap cleanup EXIT

# Helper: parse `Created sandbox sb-XXXXX (status: ...)` style output for the ID
extract_sb_id() {
  grep -oE 'sb-[a-f0-9]+' | head -1
}
extract_store_id() {
  grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1
}

############################################
# Test A — scale event recorded on fork
############################################
TEST_A_PASS=0
hr; yellow "Test A — scale event recorded on fork-from-checkpoint"

# Snapshot global count for sanity (not used as assertion)
PRE_TOTAL=$(psql_dev "SELECT COUNT(*) FROM sandbox_scale_events;" | tr -d ' ')
echo "  baseline sandbox_scale_events row count: $PRE_TOTAL"

# 1. Source sandbox
echo "  creating source sandbox..."
SRC_OUT=$($OC sandbox create --memory 1024 --cpu 1 --timeout 600 2>&1)
SRC=$(echo "$SRC_OUT" | extract_sb_id)
if [ -z "$SRC" ]; then
  red "  could not extract source sandbox id from: $SRC_OUT"
  exit 1
fi
CREATED_SBS+=("$SRC")
echo "  source sandbox: $SRC"

# Wait briefly for it to settle
sleep 3

# 2. Checkpoint
echo "  creating checkpoint..."
CK_OUT=$($OC checkpoint create "$SRC" --name verify-fork-test 2>&1)
CK_ID=$(echo "$CK_OUT" | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
if [ -z "$CK_ID" ]; then
  red "  could not extract checkpoint id from: $CK_OUT"
  exit 1
fi
CREATED_CHECKPOINTS+=("$CK_ID")
echo "  checkpoint: $CK_ID"

# Wait for checkpoint to be 'ready'
echo "  waiting for checkpoint to be ready..."
for i in $(seq 1 30); do
  STATUS=$(psql_dev "SELECT status FROM sandbox_checkpoints WHERE id='$CK_ID';" | tr -d ' ')
  [ "$STATUS" = "ready" ] && break
  sleep 2
done
if [ "$STATUS" != "ready" ]; then
  red "  checkpoint did not become ready (last status: $STATUS)"
  exit 1
fi

# 3. Spawn fork from checkpoint
echo "  spawning fork from checkpoint..."
FORK_OUT=$($OC checkpoint spawn "$CK_ID" --timeout 300 2>&1)
FORK=$(echo "$FORK_OUT" | extract_sb_id)
if [ -z "$FORK" ]; then
  red "  could not extract fork sandbox id from: $FORK_OUT"
  exit 1
fi
CREATED_SBS+=("$FORK")
echo "  fork: $FORK"

# 4. Assert: scale_event row exists for the fork (this is the bug being fixed)
sleep 2
FORK_ROW=$(psql_dev "SELECT sandbox_id||'|'||memory_mb||'|'||cpu_percent||'|'||disk_mb FROM sandbox_scale_events WHERE sandbox_id='$FORK';" | tr -d ' ')
if [ -z "$FORK_ROW" ]; then
  red "  FAIL: no sandbox_scale_events row for fork $FORK"
  echo "  query: SELECT * FROM sandbox_scale_events WHERE sandbox_id='$FORK';"
else
  green "  PASS: scale_event row present → $FORK_ROW"
  TEST_A_PASS=1
fi

############################################
# Test B — allowlist-only proxy session
############################################
TEST_B_PASS=0
hr; yellow "Test B — allowlist-only secret store registers proxy session"

# 1. Allowlist-only store
echo "  creating allowlist-only secret store..."
STORE_OUT=$($OC secret-store create --name "allowlist-verify-$$" \
              --egress-allowlist example.com,httpbin.org 2>&1)
STORE_ID=$(echo "$STORE_OUT" | extract_store_id)
if [ -z "$STORE_ID" ]; then
  red "  could not extract store id from: $STORE_OUT"
  exit 1
fi
CREATED_STORES+=("$STORE_ID")
echo "  store: $STORE_ID"

# 2. Sandbox using that store. --env required so HTTP_PROXY actually gets injected
#    (mirrors the real openclaw failure conditions).
echo "  creating sandbox with allowlist-only store + dummy env..."
ALLOW_OUT=$($OC sandbox create --secret-store "allowlist-verify-$$" \
              --memory 1024 --cpu 1 --env DUMMY=1 --timeout 300 2>&1)
ALLOW=$(echo "$ALLOW_OUT" | extract_sb_id)
if [ -z "$ALLOW" ]; then
  red "  could not extract sandbox id from: $ALLOW_OUT"
  exit 1
fi
CREATED_SBS+=("$ALLOW")
echo "  sandbox: $ALLOW"

# Wait for sandbox to be ready for exec
sleep 4

# 3. Allowed host → expect HTTP 200
# 4. Disallowed host → expect 403 from proxy
# Wrap in set +e so a non-zero exit from oc-exec / curl doesn't trip set -e
# and abort the script before we can read the HTTP status.
set +e
echo "  curl https://example.com/ (allowed) ..."
ALLOWED_HTTP=$($OC exec "$ALLOW" -- curl -s -o /dev/null -w '%{http_code}' \
                --max-time 10 https://example.com/ 2>/dev/null | tr -d '[:space:]' | tail -c 4)
echo "    HTTP ${ALLOWED_HTTP:-<none>}"

echo "  curl https://www.google.com/ (NOT allowed) ..."
DENIED_HTTP=$($OC exec "$ALLOW" -- curl -s -o /dev/null -w '%{http_code}' \
               --max-time 10 https://www.google.com/ 2>/dev/null | tr -d '[:space:]' | tail -c 4)
echo "    HTTP ${DENIED_HTTP:-<none>}"
set -e

# 5. Cross-check worker logs: should see action=connect for example.com,
#    action=blocked reason=allowlist for google.com.
echo "  recent secrets-proxy audit lines:"
ssh_dev "sudo journalctl -u opensandbox-worker --since '2 minutes ago' --no-pager 2>/dev/null \
         | grep 'secrets-proxy: audit' | tail -8" || true

# Pass conditions:
#   - Allowed host returned 200 (proxy session exists + allowlist permits)
#   - Disallowed host returned 403 (proxy session exists + allowlist blocks)
# Failure mode pre-fix would be 407 on BOTH.
if [ "$ALLOWED_HTTP" = "200" ] && [ "$DENIED_HTTP" = "403" ]; then
  green "  PASS: allowed=200, denied=403 → session registered + allowlist enforced"
  TEST_B_PASS=1
elif [ "$ALLOWED_HTTP" = "407" ]; then
  red "  FAIL: got 407 — the fix is not active (no session registered for allowlist-only store)"
elif [ "$ALLOWED_HTTP" = "200" ] && [ "$DENIED_HTTP" = "200" ]; then
  red "  FAIL: both allowed AND denied returned 200 — proxy is being bypassed entirely"
else
  red "  FAIL: unexpected combination allowed=$ALLOWED_HTTP denied=$DENIED_HTTP"
fi

############################################
# Summary
############################################
hr; yellow "Summary"
[ "$TEST_A_PASS" = 1 ] && green "  A) scale-event-on-fork:    PASS" || red "  A) scale-event-on-fork:    FAIL"
[ "$TEST_B_PASS" = 1 ] && green "  B) allowlist-only proxy:   PASS" || red "  B) allowlist-only proxy:   FAIL"
hr

if [ "$TEST_A_PASS" = 1 ] && [ "$TEST_B_PASS" = 1 ]; then
  exit 0
fi
exit 1
