#!/bin/sh
# OpenClaw launcher used by the digger/openclaw-managed image.
#
# Runs at container start. Sources the per-agent env file (mounted from
# the sandbox at /home/node/.openclaw/env), then execs the openclaw
# gateway. We use the globally-installed `openclaw` binary that npm
# places at /usr/local/bin/openclaw — that's the slim path. The non-slim
# path that goes through `docker-entrypoint.sh node openclaw.mjs gateway`
# is only valid on the upstream docker image, which we no longer base on.

set -eu

ENV_FILE=/home/node/.openclaw/env
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

# --bind lan: listen on all interfaces inside the container so the
# host port-publish reaches the gateway. Combined with auth.mode=token
# (set in /home/node/.openclaw/openclaw.json) and
# controlUi.dangerouslyAllowHostHeaderOriginFallback the gateway accepts
# the LAN bind without complaining.
exec /usr/local/bin/openclaw gateway run --bind lan --port 18789
