# Logship: warn loudly when the Axiom token is empty

> ⚠️ **NO PII IN GIT.** Audit before every Write/Edit. Rule:
> `feedback_no_pii_in_git.md` in project memory.

Status: **planned, not started.** Builds on PR #226 (the
sandbox-session-logs feature, merged `2af73c4`) and Brian's
follow-up `d6c2985` (KV mapping for Axiom secrets).

## Context

After PR #226 merged and rolled out, prod workers silently weren't
shipping logs. Two independent gaps:

1. **KV-mapping gap** — `internal/config/keyvault.go` only loads
   secrets in `secretMapping`; `AXIOM_*` weren't there. Fixed by
   `d6c2985`.
2. **Stale-cfg gap** — `cfg.AxiomIngestToken` is read from
   `os.Getenv` once at server startup. A server that started
   before the secret was added to KV (or before any rotation)
   keeps an empty cfg and bakes empty tokens into every worker.

Both were silent. This PR is three log lines that make the second
gap loud the next time it happens.

## Code in this PR (all in `cmd/server/main.go`)

1. **Startup log** mirroring the existing `"sandbox session logs
   read API enabled"` line on the query-token side. Empty
   ingest-token logs `WARNING`; populated logs the ok message.
2. **Spawn-time warning** when `cfg.AxiomIngestToken == ""` and
   the Azure-pool path renders `workerEnv` — catches stale-cfg
   post-rotation.
3. **One-line `deploy/ec2/README.md` note**: any `AXIOM_*` rotation
   requires a server restart.

~10 lines of code, no proto, no schema, no UI, no tests.

## Rollout — what happens after this PR merges

The actual prod recovery doesn't need any manual ops; it falls
out of the standard deploy. Sequence:

1. **Merge + deploy.** CI builds and ships the new control-plane
   binary. `deploy-server.sh:97` runs `systemctl restart
   opensandbox-server`.
2. **Server restart re-reads cfg.** With `d6c2985` already in main,
   `LoadSecretsFromKeyVault` populates `AXIOM_INGEST_TOKEN` /
   `AXIOM_QUERY_TOKEN` / `AXIOM_DATASET` from KV before
   `config.Load`. New diagnostic should print
   `"opensandbox: workers spawned by this server will ship logs
   to Axiom (dataset=oc-sandbox-logs)"`. If `WARNING` fires
   instead, the secret isn't in KV — fix KV before continuing.
3. **`targetWorkerVersion` bumps.** Server learns the new SHA
   from the deploy and the autoscaler's
   `internal/controlplane/scaler.go:118` field updates. Every
   currently-running worker now reports a `WorkerVersion` that
   doesn't match.
4. **Rolling-replace runs automatically.** `scaler.go:823
   rollingReplace` — quota-aware loop: pick the lightest stale
   worker, drain it (no new sandboxes routed there), live-
   migrate its existing sandboxes onto a peer, destroy the
   Azure VM, spawn a fresh one. Repeat until no stale workers
   remain. Wall-clock takes minutes-to-hours depending on pool
   size + sandbox migration latency, but unattended.
5. **Fresh workers bake the correct env.** Each spawn renders
   `workerEnv` from the now-populated `cfg.AxiomIngestToken`.
   Worker boots → `cmd/worker/main.go` logs `"sandbox session log
   shipping enabled (dataset=oc-sandbox-logs)"`.
6. **New sandboxes ship.** Any sandbox created on a fresh worker
   gets `ConfigureLogship` with the real token. In-VM agent
   activates the shipper. Logs flow to Axiom and the dashboard
   Logs panel within seconds of the events being produced.

**No prod SSH, no manual drain, no admin destroy endpoint, no
Azure CLI required.**

## What stays broken — and why we're not fixing it in this PR

**Existing sandboxes never start shipping.** Their in-VM agent
already received `ConfigureLogship` once with an empty token; the
shipper is dormant in agent memory and stays that way for the
sandbox's lifetime. Hibernate + wake preserves the dormant state
(memory snapshot). Live migration to a fresh worker preserves it
too — the agent's struct moves with the VM.

The only way to flip an existing sandbox to shipping is to
recreate it. We're choosing not to push customers to do that,
because:
- It's not a regression — these sandboxes never shipped logs.
- New sandboxes do ship.
- Customer-visible only in the dashboard Logs panel; pre-fix
  sandboxes show empty.

If a customer specifically asks why their old sandbox has no
logs, the workflow is: kill + recreate. That's a docs/CS thing,
not a code thing.

## Deliberately out of scope (PR-internal)

- **Re-reading `os.Getenv` per spawn.** Right fix for
  rotation-without-restart is a periodic config reloader; scope
  creep.
- **Fail-fast on empty.** Server has legit reasons to run
  without logship (dev, combined mode, kill-switch).
- **A metric.** No control-plane-side metric pipeline.
- **Retroactive sandbox fix.** See above.

## Verification

On the EC2 dev host:
- Unset `AXIOM_INGEST_TOKEN`, daemon-reload, restart server →
  expect WARNING line in journalctl.
- Re-set, restart → expect the ok line.
- Spawn-time warning fires only on the Azure-pool path; not
  reachable from EC2 dev. Verified by code inspection.

In prod (after merge + deploy):
- Watch journalctl on control plane for the new ok line on
  startup.
- Watch worker journalctl as rolling-replace progresses for the
  existing `"sandbox session log shipping enabled"` line on each
  fresh worker.
- Open dashboard Logs tab on a sandbox created post-rollout
  (created on a fresh worker) → expect content rows.

## Status log

- *2026-05-06* — branch + working doc created. No code yet.
  Awaiting review before implementation.
- *2026-05-06* — code changes landed: startup log + spawn-time
  warning in `cmd/server/main.go`, README rotation note in
  `deploy/ec2/README.md`. ~15 lines net. `go build ./cmd/server/...`
  clean. Ready for review.
