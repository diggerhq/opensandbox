#!/bin/sh
# OpenClaw launcher used by the digger/openclaw-managed image.
#
# Runs at container start. Sources the per-agent env file (mounted from the
# sandbox at /home/node/.openclaw/env), then execs the upstream OpenClaw
# gateway. Replacing the default upstream entrypoint with a sourcing wrapper
# is the cleanest way to inject per-agent secrets without baking them into
# the image or relying on docker-run --env-file (which would require the
# adapter to set every var explicitly).
#
# If the env file doesn't exist (e.g. someone runs the image standalone for
# debugging), the gateway boots with whatever's already in the environment;
# upstream will fail closed if OPENROUTER_API_KEY is missing.

set -eu

ENV_FILE=/home/node/.openclaw/env
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

# Hand off to upstream's openclaw binary. It picks up its config from
# /home/node/.openclaw/openclaw.json (mounted) and listens on 18789.
exec openclaw gateway run --bind 0.0.0.0 --port 18789
