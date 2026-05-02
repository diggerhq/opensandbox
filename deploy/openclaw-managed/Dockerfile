# Wrapper around the official OpenClaw runtime image. We add:
#
#   - A launcher script that sources /home/node/.openclaw/env (mounted from
#     the sandbox) so OPENROUTER_API_KEY (and other per-agent secrets) flow
#     into the gateway without being baked into the image.
#   - A Docker HEALTHCHECK so the runtime layer self-reports unhealthy (the
#     sessions-api adapter just polls /health on the published port today).
#
# We deliberately do NOT bake any per-agent secret here — the image is shared
# across every managed agent in the platform. Per-agent state lives on the
# bind-mounted /home/node/.openclaw volume.
#
# Build/push lives in .github/workflows/build-openclaw-image.yml; tag scheme
# is <upstream-version>-<our-build-sha> with a moving `latest-stable` promoted
# after smoke tests pass.

ARG OPENCLAW_VERSION=2026.2.26
FROM ghcr.io/openclaw/openclaw:${OPENCLAW_VERSION}

# Image runs as `node` (uid 1000) per the upstream Dockerfile; we keep that
# and only drop into root where strictly necessary.
USER root

# curl is used by the HEALTHCHECK below. The base image is Debian-slim per
# upstream; if upstream changes base distro this layer will need a port.
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl \
 && rm -rf /var/lib/apt/lists/*

# Per-agent env loader. The sessions-api adapter writes
# /home/node/.openclaw/env at agent-create time; we source it before exec'ing
# the gateway so the OpenRouter key is available without putting it in the
# image or the docker-run env.
COPY launcher.sh /usr/local/bin/openclaw-launcher
RUN chmod 0755 /usr/local/bin/openclaw-launcher

USER node
WORKDIR /home/node

# Healthcheck: gateway answers /health on 18789. start-period covers the
# ~10–18s upstream cold boot so docker doesn't mark the container unhealthy
# while it's still loading config.
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 \
  CMD curl -sf http://localhost:18789/health || exit 1

EXPOSE 18789

ENTRYPOINT ["/usr/local/bin/openclaw-launcher"]
