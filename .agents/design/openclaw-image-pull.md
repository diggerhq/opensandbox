# OpenClaw managed-agent runtime: image-pull instead of checkpoint-fork

## TL;DR

Move OpenClaw managed agents from "fork from a baked OC checkpoint" to
"pull a Docker image into a fresh sandbox at agent-create time." The
checkpoint path is the leading source of paywall-flow incidents in
prod (silent fork failures, golden-version rebase races, snapshot drift
between OpenClaw releases). We trade ~1s warm forks for ~10s cold
pulls in exchange for two layers we can debug independently: a Docker
registry pull, and an in-sandbox boot of a known image. The OpenClaw
team's image becomes the source of truth for the runtime, and we stop
re-baking a snapshot every release.

The Stripe paywall, sandbox-billing exemption, agent metadata, and
sessions-api adapter shape are all unchanged — only the provisioning
path is.

## Problem

Today's flow:

```
sessions-api POST /v1/agents
  → OC POST /api/sandboxes/from-checkpoint/<OPENCLAW_CHECKPOINT_ID>
    → worker fork-from-checkpoint
      → restored sandbox has supervisord + openclaw running with build-time placeholder env
        → entrypoint writes ~/.openclaw/env, supervisorctl restart openclaw
          → adapter.waitReady polls /health
```

Recurring failure modes in prod, each of which has fired this week:

- **Fork race / orphan reaper.** worker's orphan-reaper kills the
  forked QEMU process before the VM registry sees it
  (commit `9fb6c9a` introduced this). User sees
  `instance_boot: rpc error ... QMP cont: connection reset by peer`
  and the agent stays in `error`. Repro is intermittent under load.
- **Golden-version rebase loops.** When we rebuild the rootfs, every
  in-flight checkpoint references the old golden. The worker
  downloads the prior rootfs to `bases/<ver>/default.ext4`; on slow
  blob storage this stalls the first fork of every new agent for 30+
  seconds and occasionally times out.
- **Snapshot drift between OpenClaw releases.** OpenClaw 2026.4.16+
  changed the channel-config schema; our adapter writes the old
  shape; OpenClaw silently rolls writes back. We pin to 2026.4.15 to
  avoid this and have to rebuild the snapshot for every bump.
- **Reboot recovery is supervisord-restart-dependent.** A guest reboot
  wipes everything; we need PID-1 (osb-agent) to start supervisord
  via the `f7f57a7` hook, then need ~18s of openclaw-gateway warm-up
  before chat works. Users hit "Agent ended the turn without sending
  any text" if they message during the warm-up window.
- **Cross-checkpoint org leakage.** Public-checkpoint forks via the
  `is_public` relaxation work, but checkpoint authorship is brittle —
  if we accidentally rebuild a checkpoint as private the entire user
  base hits 403.

The common thread: we treat the OpenClaw runtime as part of our
infra, but we ship it from a different repo on a different release
cadence. Every interaction between them (config schema, supervisord
program shape, checkpoint format) is a place state can diverge.

## Proposed flow

```
sessions-api POST /v1/agents (core=openclaw)
  → OC POST /api/sandboxes  (fresh sandbox from default rootfs, secretStore=agent:<id>)
    → exec.run "docker pull diggerhq/openclaw-managed:<version>"
      → exec.run "docker run -d --name openclaw \
          -p 18789:18789 -p 8787:8787 \
          -v /home/sandbox/.openclaw:/data \
          --env-file /home/sandbox/.openclaw/env \
          --restart unless-stopped \
          diggerhq/openclaw-managed:<version>"
        → adapter.waitReady polls http://localhost:18789/health
```

Two new pieces of plumbing:

### 1. `digger/openclaw-managed:<version>` image

Built nightly via a workflow that pulls the official upstream
OpenClaw image, layers our supervisord + reload hooks + agent
metadata on top, and pushes to a registry. Layout sketch:

```dockerfile
ARG OPENCLAW_VERSION=2026.5.1
FROM openclaw/runtime:${OPENCLAW_VERSION}

# Base config — no per-agent secrets; those come from /data/env at runtime.
COPY openclaw.json /etc/openclaw/openclaw.json

# Launcher script that sources /data/env and execs the gateway in foreground.
COPY launcher.sh /usr/local/bin/openclaw-launcher
RUN chmod +x /usr/local/bin/openclaw-launcher

# Healthcheck baked in so docker can mark the container unhealthy without
# us shipping an external probe.
HEALTHCHECK --interval=10s --timeout=3s --start-period=30s --retries=3 \
  CMD curl -sf http://localhost:18789/health || exit 1

EXPOSE 18789 8787
ENTRYPOINT ["/usr/local/bin/openclaw-launcher"]
```

The image is the only place the OpenClaw runtime version lives.
Bumping OpenClaw is a tag bump, not a checkpoint rebuild. Rollback
is `docker run` with a previous tag — no rootfs surgery. CI tests
can `docker pull` and curl `/health` before promoting a tag.

### 2. Sessions-api adapter rewrite

`src/lib/cores.ts:openclaw` changes from "fork checkpoint, run
entrypoint" to "create sandbox, install image, start container."
The "core ready" gate becomes:

```ts
async function bootOpenclaw(sandbox, agentId, env) {
  await sandbox.files.makeDir("/home/sandbox/.openclaw");
  await sandbox.files.write(
    "/home/sandbox/.openclaw/env",
    Object.entries(env).map(([k, v]) => `${k}=${v}`).join("\n"),
  );
  await sandbox.exec.run(
    "docker pull diggerhq/openclaw-managed:" + OPENCLAW_VERSION,
    { timeout: 120 },
  );
  await sandbox.exec.run(
    `docker run -d --name openclaw --restart unless-stopped \
       -p 18789:18789 -p 8787:8787 \
       -v /home/sandbox/.openclaw:/data \
       --env-file /home/sandbox/.openclaw/env \
       diggerhq/openclaw-managed:${OPENCLAW_VERSION}`,
    { timeout: 30 },
  );
  await waitReady(sandbox);
}
```

Adapter methods that exist today (`restart`, `setModel`, `wireChannel`,
`waitChannelReady`) get simpler — they shell into the running container
or call its HTTP endpoints. Reboot recovery is free: `--restart
unless-stopped` brings the container back when the sandbox boots.

## Tradeoffs

| | Checkpoint fork (today) | Image pull (proposed) |
|---|---|---|
| Cold-start latency | ~3-5s warm fork + ~18s gateway | ~10s docker pull + ~5s warm boot |
| Warm-pool latency | ~1s | ~3s (cache hit) |
| Failure surface | rootfs/checkpoint/openclaw all coupled | layers debuggable independently |
| Runtime version control | rebuild snapshot, redeploy sessions-api | docker tag bump |
| Reboot recovery | osb-agent → supervisord → openclaw (~18s gap) | docker daemon → container (auto, ~3s) |
| Per-agent env | env file written post-fork, supervisorctl restart | env file mounted at container start |
| Cross-org issues | checkpoint ownership / `is_public` | none — image is a public dep |
| Worker disk pressure | one rootfs per template | rootfs + image cache (~500MB-1GB) |
| Storage growth | checkpoint blob per release | docker registry per release |

The honest cost: cold start is slower for the first agent on each
worker. A warm pool of "openclaw-image-prepulled" sandboxes mitigates
this — image is in the worker's overlay, fork-from-warmpool is fast,
agent-create just needs to run-the-container. The autoscaler already
maintains warm pools for the base template; we'd add a second pool
keyed on `template=openclaw-managed`.

The dishonest cost: nothing. We've been spending more eng-time on
checkpoint debugging this month than the latency difference can ever
charge back.

## Migration plan

Phased so we can roll back per-stage if anything misbehaves.

**Phase 0 — image registry plumbing (no user-visible change).**
1. Spin up `ghcr.io/diggerhq/openclaw-managed` (or push to the existing
   Azure Container Registry — `opencomputer-prod-acr` if it exists,
   easier vault story).
2. Author `Dockerfile.openclaw-managed` in this repo. Build via a new
   `.github/workflows/build-openclaw-image.yml` that runs nightly +
   on-demand, tags with `<openclaw-version>-<git-sha>`, pushes to
   registry.
3. Test: pull the image into a stock sandbox, `docker run`, curl
   `/health`. Verify Telegram + chat-completions still work. This is
   purely additive; no production code changes yet.

**Phase 1 — adapter behind a feature flag.**
1. Add `OPENCLAW_PROVISIONER` env to sessions-api: `checkpoint` (default,
   today's behaviour) or `image` (new).
2. New code path lives alongside the existing one in `src/lib/cores.ts`
   and `src/lib/adapters/openclaw.ts`. Old code keeps shipping.
3. Wire OC's billing exemption through metadata.agent_id same as today
   — provisioning path doesn't matter for billing.
4. Test on dev VM with `OPENCLAW_PROVISIONER=image`. Validate full
   create → connect Telegram → chat round-trip. Compare end-to-end
   timings.

**Phase 2 — flip prod, keep checkpoint as fallback.**
1. Set `OPENCLAW_PROVISIONER=image` on Fly's `bolt-platform` app.
2. Existing agents (created off the old checkpoint) continue to work
   — they're not re-provisioned. Only new agents take the image
   path.
3. Watch error rates, fork latencies (now image-pull latencies), and
   chat-turn latencies for 1 week.
4. If anything regresses, flip the flag back. No rollback needed
   beyond the env change.

**Phase 3 — retire the checkpoint path.**
1. Once a week of clean prod, delete the checkpoint code in
   `cores.ts` / adapter / `OPENCLAW_CHECKPOINT_ID` env.
2. Delete `scripts/build-openclaw-snapshot.ts`.
3. Remove the `is_public` checkpoint surface for OpenClaw (we keep
   it for any other public-checkpoint use cases).

**Phase 4 (optional) — warm pool for openclaw image.**
1. Teach the autoscaler to keep N idle sandboxes per region with the
   image already pulled. New OpenClaw agent grabs one off the pool;
   provisioning becomes "container start" only.
2. Trade-off is cost (idle worker capacity); only worth doing once
   we've got steady demand for managed agents.

## Open questions / unknowns

- **Does the official OpenClaw image exist?** If it doesn't, we'd
  build our own from source; same plan, more upstream-tracking
  obligation.
- **Networking for Telegram webhooks.** Today the sandbox listens on
  `:8787` directly; the outside world reaches it via OC's reverse
  proxy. With docker we'd port-publish 8787 from container to
  sandbox-host; same proxy contract holds. Want to confirm there's no
  bridge-mode quirk on the worker's docker daemon.
- **Image cache GC.** Workers will accumulate openclaw image layers;
  need an eviction policy on the worker. Probably reuse the existing
  base-image cleanup hook.
- **Versioning across regions.** If we eventually run multi-region,
  we want each region's worker pool pulling from the closest registry
  mirror to keep cold start under control.

## What we're NOT changing

- Stripe paywall + per-agent subscription gating (entitlement check
  still happens on `connect telegram`).
- Sandbox-billing exemption for subscribed-agent sandboxes
  (metadata.agent_id is set the same way regardless of provisioner).
- Sessions-api ↔ OC auth flow (identity JWTs, downstream tokens).
- CLI surface (`oc agent send/chat`, paywall preflight).
- Dashboard UX.

## Decision needed before phase 0

- Registry choice: `ghcr.io/diggerhq` or Azure ACR? ACR keeps the
  pull entirely on the Azure backbone for prod workers, which is
  faster and avoids GitHub rate limits.
- Image build cadence: nightly is fine; tag scheme like
  `<upstream-openclaw-version>-<our-build-sha>` makes per-build
  rollback trivial. Confirm before we ship.
