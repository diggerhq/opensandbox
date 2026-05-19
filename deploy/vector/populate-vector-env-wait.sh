#!/bin/bash
# populate-vector-env-wait.sh — Background poller that waits for cloud-init
# to write /etc/opensandbox/worker.env (or server.env), then re-runs the
# main populator to fetch real Axiom creds from Key Vault and restarts
# vector.service so it picks them up.
#
# Started imperatively by populate-vector-env.sh when neither worker.env
# nor server.env exists at populator-run time (Azure first-boot race —
# cloud-final.service writes worker.env *after* multi-user.target, so the
# main populator can't synchronously wait for it without deadlocking the
# boot).
#
# Not WantedBy=multi-user.target — boot doesn't wait on this. Runs as a
# long-lived Type=simple service so systemd tracks it cleanly and the
# logs land in journald under populate-vector-env-wait.service.
set -uo pipefail

ROLE_WORKER=/etc/opensandbox/worker.env
ROLE_SERVER=/etc/opensandbox/server.env
DEADLINE_SECONDS=${POPULATE_WAIT_DEADLINE_SECONDS:-1800}
POLL_SECONDS=${POPULATE_WAIT_POLL_SECONDS:-5}

log() { logger -t populate-vector-env-wait "$*"; echo "$*"; }

deadline=$(($(date +%s) + DEADLINE_SECONDS))
log "polling for $ROLE_WORKER or $ROLE_SERVER every ${POLL_SECONDS}s (deadline ${DEADLINE_SECONDS}s)"

while [ "$(date +%s)" -lt "$deadline" ]; do
    if [ -f "$ROLE_WORKER" ] || [ -f "$ROLE_SERVER" ]; then
        log "role env appeared after $(( $(date +%s) - (deadline - DEADLINE_SECONDS) ))s, repopulating vector.env"
        if /usr/local/bin/populate-vector-env.sh; then
            log "populator succeeded; reset-failed + restart vector.service to pick up real creds"
            # reset-failed: clears any failed state from vector's earlier
            # start with the stub env (axiom sink healthcheck failures may
            # have tripped Restart=always's burst budget).
            systemctl reset-failed vector.service 2>/dev/null || true
            systemctl restart vector.service 2>/dev/null || log "vector.service restart returned non-zero"
        else
            log "populator exited non-zero; leaving vector.service with stub env (axiom sink will keep buffering)"
        fi
        exit 0
    fi
    sleep "$POLL_SECONDS"
done

log "role env never appeared after ${DEADLINE_SECONDS}s; giving up (vector.service stays on stub env, axiom sink keeps buffering)"
exit 0
