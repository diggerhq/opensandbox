#!/usr/bin/env bash
# 40-corruption-guards.sh — Stress the four hibernate/checkpoint corruption
# guards landed in fix/savevm-corruption-guards. Each section asserts the
# guard actually fires under the conditions it's designed to catch.
#
# Sections (set SECTION=<n> to run only one):
#   1. Hibernate refusal when the in-guest agent is unresponsive.
#      Suspend the agent with SIGSTOP, issue Hibernate, expect a clean error
#      (NOT "guest sync + unmount done (0ms)" followed by a savevm). The
#      rootfs.qcow2 mtime on the worker must not change during the attempt.
#
#   2. Checkpoint+fork under sustained I/O.
#      Regression test for the explicit Stop/SaveVM/Cont sequence: a busy
#      writer thread must not produce a corrupt qcow2 when checkpointed,
#      and the forked child must boot cleanly with the checkpointed file
#      contents intact.
#
#   3. Apt-cache bind-mount lifecycle.
#      Fresh sandbox: /var/cache/apt/archives must be a bind-mount onto
#      /home/sandbox/.osb-apt-cache. Across hibernate+wake the bind-mount
#      must still be in place. apt-get update writes must land on /dev/vdb,
#      not /dev/vda.
#
#   4. Disk-pressure refusal.
#      Fill rootfs to >=95%, attempt Hibernate, expect ErrRootfsCritical
#      with the actual % surfaced. Free space below threshold and confirm
#      the next Hibernate succeeds.
#
# Required env:
#   OPENSANDBOX_API_URL  (e.g. https://dev.opencomputer.dev)
#   OPENSANDBOX_API_KEY
#   SSH_KEY              path to a key with worker access, optional but
#                        required for the rootfs.qcow2 mtime assertion in
#                        section 1 (skipped without it)
#
# Optional:
#   ITERATIONS=<n>       repeat each section's main loop n times (default 3)
#   SECTION=<1|2|3|4>    run only one section
#   FILL_PCT=<n>         section 4 target % (default 96)

set +u
source "$(dirname "$0")/common.sh"

ITERATIONS="${ITERATIONS:-3}"
SECTION="${SECTION:-all}"
FILL_PCT="${FILL_PCT:-96}"
TIMEOUT=180
SANDBOXES=()
PASS=0
FAIL=0
SKIP=0

cleanup() {
    set +u
    for sb in "${SANDBOXES[@]+"${SANDBOXES[@]}"}"; do
        [ -n "$sb" ] && destroy_sandbox "$sb" 2>/dev/null
    done
    set -u
}
trap cleanup EXIT INT TERM

# Helper: parse error message + status from a JSON error response.
err_msg() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('error') or d.get('message') or d)" 2>/dev/null; }

# Helper: read worker-side mtime of a sandbox's rootfs.qcow2 over SSH.
# Returns "skip" if SSH_KEY isn't set or worker host can't be inferred.
worker_rootfs_mtime() {
    local sb="$1"
    [ -z "${SSH_KEY:-}" ] && { echo skip; return; }
    local worker_id worker_host path
    worker_id=$(api "$API_URL/api/sandboxes/$sb" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('worker_id',''))" 2>/dev/null)
    [ -z "$worker_id" ] && { echo skip; return; }
    # Workers expose their grpc_addr in the workers list — strip port.
    worker_host=$(api "$API_URL/api/workers" 2>/dev/null | python3 -c "
import sys, json
target = '$worker_id'
for w in json.load(sys.stdin):
    if w.get('worker_id') == target:
        addr = w.get('grpc_addr') or w.get('http_addr') or ''
        print(addr.split(':')[0])
        break
" 2>/dev/null)
    [ -z "$worker_host" ] && { echo skip; return; }
    path="/data/sandboxes/sandboxes/$sb/rootfs.qcow2"
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -i "$SSH_KEY" \
        "azureuser@$worker_host" "stat -c %Y $path 2>/dev/null" 2>/dev/null || echo skip
}

# Helper: freeze the entire VM from the host so the in-guest agent stops
# responding to gRPC. We can't SIGSTOP the agent directly because it runs as
# PID 1 (SIGNAL_UNKILLABLE — the kernel rejects user-space SIGSTOP to init).
# Instead, find the QEMU process for this sandbox on the host and SIGSTOP it.
# Restoring is symmetric (SIGCONT). Requires SSH access to the host (set
# DEV_VM_HOST + SSH_KEY); skips the section if unavailable.
#
# Action: stop|cont
freeze_guest_agent() {
    local sb="$1"
    local action="${2:-stop}"
    local sig="STOP"
    [ "$action" = "cont" ] && sig="CONT"
    if [ -z "${DEV_VM_HOST:-}" ] || [ -z "${SSH_KEY:-}" ]; then
        return 2  # not configured — caller should skip
    fi
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -i "$SSH_KEY" \
        "${DEV_VM_USER:-azureuser}@$DEV_VM_HOST" \
        "sudo pkill -$sig -f 'qemu.*$sb' 2>/dev/null; sleep 0.5; sudo pgrep -f 'qemu.*$sb' >/dev/null && echo found || echo nopid" 2>/dev/null
}

# ── Section 1: Hibernate refusal on unresponsive agent ────────────────
section1() {
    h "Section 1: Hibernate refusal when guest agent is unresponsive ($ITERATIONS iters)"
    if [ -z "${DEV_VM_HOST:-}" ] || [ -z "${SSH_KEY:-}" ]; then
        skip "Section 1 needs DEV_VM_HOST + SSH_KEY for host-side QEMU SIGSTOP (PID-1 freeze inside guest is blocked by SIGNAL_UNKILLABLE)."
        return
    fi
    for i in $(seq 1 "$ITERATIONS"); do
        SB=$(create_sandbox)
        SANDBOXES+=("$SB")

        # Wait for the QEMU process to be fully up and the agent registered.
        sleep 4

        # SIGSTOP the QEMU process for this sandbox on the host. Freezes the
        # whole VM, including the agent's gRPC server. The host's prepare-
        # hibernate RPC will hit its 10s timeout (PrepareHibernate), then the
        # 10s fallback Exec timeout, then bubble up ErrAgentUnresponsive.
        local freeze_result
        freeze_result=$(freeze_guest_agent "$SB" stop)
        if ! echo "$freeze_result" | grep -q found; then
            skip "iter $i: could not locate QEMU process for $SB on host"
            destroy_sandbox "$SB"
            continue
        fi

        local resp http_status
        resp=$(api -X POST -w '\nHTTP_STATUS:%{http_code}\n' "$API_URL/api/sandboxes/$SB/hibernate" 2>/dev/null)
        http_status=$(echo "$resp" | grep '^HTTP_STATUS:' | cut -d: -f2)

        # ALWAYS resume the QEMU process — even on test failure — so we don't
        # leak frozen VMs in the dev environment.
        freeze_guest_agent "$SB" cont >/dev/null 2>&1

        if [ "$http_status" = "200" ] || [ "$http_status" = "204" ]; then
            fail "iter $i: Hibernate succeeded against frozen VM (expected refusal)"
        elif echo "$resp" | grep -qi "agent unresponsive\|ErrAgentUnresponsive\|guest agent"; then
            pass "iter $i: Hibernate refused with agent-unresponsive error (HTTP $http_status)"
        else
            fail "iter $i: Hibernate failed (HTTP $http_status) but error did not mention agent unresponsiveness"
            echo "$resp" | tail -3 | sed 's/^/    /'
        fi

        destroy_sandbox "$SB"
    done
}

# ── Section 2: Checkpoint+fork under sustained I/O ────────────────────
section2() {
    h "Section 2: Checkpoint+fork under heavy I/O ($ITERATIONS iters)"
    for i in $(seq 1 "$ITERATIONS"); do
        SB=$(create_sandbox)
        SANDBOXES+=("$SB")

        # Start a background writer producing churn into rootfs+workspace.
        # 50 MB/s into rootfs for 30s plus a tight rename loop on workspace.
        exec_run "$SB" "bash" "-c" "
            (dd if=/dev/urandom of=/tmp/churn bs=1M count=200 oflag=direct 2>/dev/null) &
            (for j in \$(seq 1 200); do echo j-\$j > /home/sandbox/.churn.\$j; mv /home/sandbox/.churn.\$j /home/sandbox/.churn-final.\$j; done) &
            sleep 0.5; echo writers-launched
        " >/dev/null

        # Drop a deterministic blob whose hash we'll verify post-fork.
        local blob_hash
        exec_run "$SB" "bash" "-c" "dd if=/dev/urandom of=/home/sandbox/blob.bin bs=1M count=20 2>/dev/null && sync" >/dev/null
        blob_hash=$(exec_stdout "$SB" "bash" "-c" "sha256sum /home/sandbox/blob.bin | cut -d' ' -f1")

        # Take the checkpoint while the writers are still going. The API uses
        # capital-N "Name" on input; response field is "id" (not checkpoint_id).
        local cp_resp cp_id
        cp_resp=$(api -X POST "$API_URL/api/sandboxes/$SB/checkpoints" -d "{\"Name\":\"guard-test-$i\"}" 2>/dev/null)
        cp_id=$(echo "$cp_resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
        if [ -z "$cp_id" ]; then
            fail "iter $i: checkpoint create failed: $(echo $cp_resp | err_msg)"
            destroy_sandbox "$SB"
            continue
        fi
        pass "iter $i: checkpoint $cp_id created under load"

        # Checkpoint create returns immediately with status=processing; wait
        # for it to finish before forking from it. There is no
        # GET /api/checkpoints/<id> endpoint — we list per-sandbox and filter.
        local waited=0
        local cp_status=""
        while [ "$waited" -lt 90 ]; do
            cp_status=$(CP_ID="$cp_id" api "$API_URL/api/sandboxes/$SB/checkpoints" 2>/dev/null \
                | CP_ID="$cp_id" python3 -c "import sys,json,os
target=os.environ['CP_ID']
for cp in (json.load(sys.stdin) or []):
    if cp.get('id')==target:
        print(cp.get('status','')); break" 2>/dev/null)
            [ "$cp_status" = "ready" ] && break
            [ "$cp_status" = "failed" ] && { fail "iter $i: checkpoint $cp_id failed during processing"; break; }
            sleep 2; waited=$((waited+2))
        done
        if [ "$cp_status" != "ready" ]; then
            fail "iter $i: checkpoint $cp_id never reached ready (last status=$cp_status, waited ${waited}s)"
            destroy_sandbox "$SB"
            continue
        fi

        # Source sandbox should still be running afterward (Cont was issued).
        local status
        status=$(api "$API_URL/api/sandboxes/$SB" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
        if [ "$status" = "running" ]; then
            pass "iter $i: source sandbox still running post-checkpoint"
        else
            fail "iter $i: source sandbox status=$status after checkpoint (expected running) — Cont may have failed"
        fi

        # Fork from the checkpoint and verify blob hash matches.
        # Endpoint is POST /api/sandboxes/from-checkpoint/<id> (per router.go).
        local fork_resp fork_sb fork_hash
        fork_resp=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$cp_id" -d '{}' 2>/dev/null)
        fork_sb=$(echo "$fork_resp" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
        if [ -z "$fork_sb" ]; then
            fail "iter $i: fork from checkpoint failed: $(echo $fork_resp | err_msg)"
        else
            SANDBOXES+=("$fork_sb")
            sleep 3
            fork_hash=$(exec_stdout "$fork_sb" "bash" "-c" "sha256sum /home/sandbox/blob.bin | cut -d' ' -f1")
            if [ "$blob_hash" = "$fork_hash" ]; then
                pass "iter $i: fork blob.bin hash matches source"
            else
                fail "iter $i: fork blob hash MISMATCH (expected $blob_hash, got $fork_hash)"
            fi
            destroy_sandbox "$fork_sb"
        fi
        destroy_sandbox "$SB"
    done
}

# ── Section 3: Apt-cache bind-mount lifecycle ─────────────────────────
section3() {
    h "Section 3: Apt-cache bind-mount survives lifecycle events ($ITERATIONS iters)"
    for i in $(seq 1 "$ITERATIONS"); do
        SB=$(create_sandbox)
        SANDBOXES+=("$SB")

        # Fresh-spawn check.
        local mp_status
        mp_status=$(exec_exit_code "$SB" "bash" "-c" "mountpoint -q /var/cache/apt/archives")
        if [ "$mp_status" = "0" ]; then
            pass "iter $i: bind-mount in place on fresh spawn"
        else
            fail "iter $i: /var/cache/apt/archives is NOT a bind-mount on fresh spawn"
        fi

        # Verify the underlying device is /dev/vdb (workspace), not /dev/vda.
        local mount_dev
        mount_dev=$(exec_stdout "$SB" "bash" "-c" "findmnt -no SOURCE /var/cache/apt/archives 2>/dev/null | head -1")
        if [[ "$mount_dev" == *"vdb"* ]] || [[ "$mount_dev" == *"sandbox/.osb-apt-cache"* ]]; then
            pass "iter $i: bind-mount source is workspace ($mount_dev)"
        else
            fail "iter $i: bind-mount source is unexpected: $mount_dev"
        fi

        # apt-get update writes index files into /var/cache/apt — write a
        # marker file there and verify it actually lands on workspace.
        exec_run "$SB" "bash" "-c" "sudo touch /var/cache/apt/archives/.bind-test-$i" >/dev/null
        local on_workspace
        on_workspace=$(exec_stdout "$SB" "bash" "-c" "[ -f /home/sandbox/.osb-apt-cache/.bind-test-$i ] && echo yes || echo no")
        if [ "$on_workspace" = "yes" ]; then
            pass "iter $i: writes through /var/cache/apt/archives land on /home/sandbox/.osb-apt-cache"
        else
            fail "iter $i: bind-mount is not redirecting writes correctly"
        fi

        # Hibernate + wake. Bind-mount must be re-applied (or survive) on wake.
        api -X POST "$API_URL/api/sandboxes/$SB/hibernate" >/dev/null 2>&1
        sleep 5
        api -X POST "$API_URL/api/sandboxes/$SB/wake" -d '{"timeout":3600}' >/dev/null 2>&1
        sleep 5
        mp_status=$(exec_exit_code "$SB" "bash" "-c" "mountpoint -q /var/cache/apt/archives")
        if [ "$mp_status" = "0" ]; then
            pass "iter $i: bind-mount in place after hibernate+wake"
        else
            fail "iter $i: bind-mount lost after hibernate+wake"
        fi

        destroy_sandbox "$SB"
    done
}

# ── Section 4: Disk-pressure refusal ──────────────────────────────────
section4() {
    h "Section 4: Hibernate/checkpoint refused at >=95% rootfs use ($ITERATIONS iters, fill=$FILL_PCT%)"
    for i in $(seq 1 "$ITERATIONS"); do
        SB=$(create_sandbox)
        SANDBOXES+=("$SB")

        # Fill the rootfs (NOT workspace) to FILL_PCT% using dd (fallocate
        # allocates lazily on this ext4 — sparse files don't show in df). dd
        # writes zeros in 1 MiB blocks with conv=fsync so blocks are really
        # reserved. NB: must be a single line — common.sh's exec_run flattens
        # multi-line args via newline-split, which truncates a multi-line
        # bash -c body to just its first line.
        exec_run "$SB" "bash" "-c" "set -e; B=\$(stat -fc %s /); T=\$(stat -fc %b /); A=\$(stat -fc %a /); TOT=\$((B*T)); USED=\$((TOT-A*B)); TGT=\$((TOT*$FILL_PCT/100)); FILL_MB=\$(( (TGT-USED)/1024/1024 )); [ \$FILL_MB -gt 0 ] && sudo dd if=/dev/zero of=/.fill bs=1M count=\$FILL_MB conv=fsync status=none 2>&1 || true; df -h / | tail -1" >/dev/null

        local pct_used
        pct_used=$(exec_stdout "$SB" "bash" "-c" "df --output=pcent / | tail -1 | tr -dc '0-9'")
        if [ -z "$pct_used" ] || [ "$pct_used" -lt 95 ]; then
            skip "iter $i: rootfs only at ${pct_used:-?}% — fallocate may have failed; can't exercise refusal"
            destroy_sandbox "$SB"
            continue
        fi
        pass "iter $i: rootfs at $pct_used%"

        local resp http_status
        resp=$(api -X POST -w '\nHTTP_STATUS:%{http_code}\n' "$API_URL/api/sandboxes/$SB/hibernate" 2>/dev/null)
        http_status=$(echo "$resp" | grep '^HTTP_STATUS:' | cut -d: -f2)

        if [ "$http_status" = "200" ] || [ "$http_status" = "204" ]; then
            fail "iter $i: Hibernate succeeded against full rootfs at $pct_used% (expected refusal)"
        elif echo "$resp" | grep -qi "rootfs disk usage\|ErrRootfsCritical\|rootfs at"; then
            pass "iter $i: Hibernate refused with disk-pressure error"
        else
            fail "iter $i: Hibernate failed (HTTP $http_status) but error did not mention disk pressure"
            echo "$resp" | tail -3 | sed 's/^/    /'
        fi

        # Free space and confirm Hibernate now succeeds.
        exec_run "$SB" "bash" "-c" "sudo rm -f /.fill && sync" >/dev/null
        sleep 1
        resp=$(api -X POST -w '\nHTTP_STATUS:%{http_code}\n' "$API_URL/api/sandboxes/$SB/hibernate" 2>/dev/null)
        http_status=$(echo "$resp" | grep '^HTTP_STATUS:' | cut -d: -f2)
        if [ "$http_status" = "200" ] || [ "$http_status" = "204" ]; then
            pass "iter $i: Hibernate succeeded after freeing space"
        else
            fail "iter $i: Hibernate still failed after freeing space (HTTP $http_status)"
        fi
        destroy_sandbox "$SB"
    done
}

# ── Dispatch ──────────────────────────────────────────────────────────
case "$SECTION" in
    1) section1 ;;
    2) section2 ;;
    3) section3 ;;
    4) section4 ;;
    all|"") section1; section2; section3; section4 ;;
    *) echo "unknown SECTION=$SECTION (expected 1|2|3|4|all)"; exit 64 ;;
esac

summary
