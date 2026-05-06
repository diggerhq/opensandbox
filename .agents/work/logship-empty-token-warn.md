# Logship: warn loudly when the Axiom token is empty at server startup / worker bake

> ⚠️ **NO PII IN GIT.** Customer emails, person/org names, ticket
> identifiers tying activity to a specific individual must never
> appear in any committed file. Audit before every Write/Edit. Full
> rule in project memory `feedback_no_pii_in_git.md`.

Status: **planned, not started.** Builds on the merged sandbox-
session-logs feature (PR #226, `2af73c4`) and the team's overnight
fix `d6c2985` (Brian) which mapped the Axiom secrets through the
Key Vault loader.

## Problem

After PR #226 merged and was rolled to prod, the team (overnight)
verified that **prod workers were not shipping logs**. Their
analysis pinned the root cause to the Azure-pool worker bake
template:

```
1. cmd/server/main.go renders worker cloud-init using cfg.AxiomIngestToken.
2. cfg is populated from os.Getenv at server startup.
3. The control plane was running long before the AXIOM_INGEST_TOKEN
   secret was added to Key Vault, so cfg.AxiomIngestToken was empty.
4. Workers spawned by that server baked an empty token into
   /etc/opensandbox/worker.env.
5. Worker's configureLogshipForSandbox sees the empty token and
   skips ConfigureLogship. Logs never ship.
```

The team also found the related cause: `internal/config/keyvault.go`
silently skips KV entries that aren't in `secretMapping`. Until
Brian's `d6c2985`, `AXIOM_*` weren't in the map, so the values
never even got into the server's process env in the first place.

`d6c2985` closed the KV-mapping gap. **This working doc is about
the second gap: even with mapping in place, a server that started
*before* the secret was added (or a freshly-rotated secret) leaves
the running cfg stale, and every spawned-worker silently bakes
empty.** Both gaps were silent — there was no log line to tell
ops that anything was wrong.

## Why this is worth fixing now (vs documenting it)

The class of bug is "silent empty propagation across a process
boundary." Each individual bug here is small, but the cluster has
already burnt one prod rollout. A single startup log + a single
spawn-time warning catches the next instance in seconds. The fix
is small enough that "documenting the gotcha" is more code than
the actual code.

It also matches what we did on the worker side during the original
feature work: `configureLogshipForSandbox` logs every code path
(token-not-set, manager-doesn't-implement, RPC-failed, RPC-sent)
specifically because we had hit silent-empty during dev testing.
The server has never had the equivalent.

## Fix scope

Three small changes, all in `cmd/server/main.go` and the working
doc / deploy README — no proto, no schema, no UI.

### 1. Server startup log: log shipping config state

`cmd/server/main.go` already calls `server.SetAxiomQueryConfig(...)`
and logs `"sandbox session logs read API enabled (dataset=...)"`
when the *query* token is set. Add the symmetric line for the
*ingest* / worker-bake side:

```go
if cfg.AxiomIngestToken != "" {
    log.Printf("opensandbox: workers spawned by this server will ship logs to Axiom (dataset=%s)", cfg.AxiomDataset)
} else {
    log.Printf("opensandbox: WARNING: AXIOM_INGEST_TOKEN empty — workers spawned by this server will NOT ship logs")
}
```

If the empty case fires in prod logs, ops sees it on the first
deploy or restart. If the populated case fires, we know the bake
will be correct.

### 2. Spawn-time warning when bake template would emit empty token

In the Azure-pool path where `workerEnv` is rendered, log a warning
when `cfg.AxiomIngestToken == ""` — same string per-spawn so
operators can spot the moment a worker gets baked without a token.
Cheap because spawning is rare (sub-minute is unusual).

```go
if cfg.AxiomIngestToken == "" {
    log.Printf("opensandbox: WARNING: spawning worker with empty AXIOM_INGEST_TOKEN — this worker will not ship sandbox session logs (workerName=%s)", workerName)
}
```

This catches the case where the server was started without the
token but the secret has since been added to KV — no restart yet,
so cfg is stale, and workers being baked right now will silently
fail. The warning gives ops a paged-on-able signal.

### 3. Operational note in deploy README

Add to `deploy/ec2/README.md` (and equivalent in
`.agents/work/sandbox-session-logs-impl.md` Status Log) the
operational fact that the server **must be restarted** after any
`AXIOM_*` secret rotation. Same applies to KV-backed prod and
EnvironmentFile-based dev. Two-line addition; no real text needed
beyond stating the rule.

## What's deliberately NOT in this fix

- **No re-read-on-spawn of os.Getenv.** Tempting to read
  `os.Getenv("AXIOM_INGEST_TOKEN")` directly inside the bake
  template instead of `cfg.AxiomIngestToken`. Rejected: process env
  in Go is also frozen-ish (changes to it from outside the process
  don't propagate without explicit os.Setenv). The right fix for
  rotation-without-restart is a periodic config reload, which is
  scope creep and a real architectural decision.
- **No fail-fast on empty token.** Tempting to refuse to spawn
  workers if the token is empty, but the server has many
  legitimate reasons to be running without log shipping (dev mode,
  combined mode, deliberate kill-switch). Warning loudly is the
  right level.
- **No metric.** A `logship_workers_baked_empty_total` counter
  would be the principled fix, but we don't have a metric pipeline
  for control-plane-side counters yet, and the log line is enough
  for now.
- **No retroactive fix for already-spawned workers.** Workers with
  empty tokens in `/etc/opensandbox/worker.env` are dead-on-arrival
  for log shipping; they need to be respawned. Operationally
  documented in `sandbox-session-logs-impl.md`.

## Verification

On the EC2 dev host (or any dev environment):

1. Unset `AXIOM_INGEST_TOKEN` in the relevant `.env`, daemon-reload,
   restart server. Confirm the WARNING log line fires once at
   startup.
2. Re-set the token, daemon-reload, restart server. Confirm the
   "workers will ship logs" line fires.
3. With token unset: spawn a worker (or simulate the bake-template
   path). Confirm the per-spawn warning fires.
4. Re-set the token, restart, spawn a worker. Confirm no warning.

No new tests in `_test.go` — the change is just `log.Printf` in
two places, hard to unit-test usefully without mocking `log`.

## Related

- **Sandbox session logs feature:** PR #226 (merged, `2af73c4`).
- **KV-mapping fix:** `d6c2985` (Brian) — adds Axiom secrets to
  `secretMapping`. Without that, KV-backed deployments couldn't
  have loaded the token at all.
- **Worker bake template fix:** `d2df72d` — the original change
  that bakes `AXIOM_INGEST_TOKEN` into Azure-pool worker
  cloud-init. Without that, even a non-empty server-side cfg
  wouldn't reach workers.
- **Operational gotchas already captured:** see Status Log in
  `.agents/work/sandbox-session-logs-impl.md` (systemd
  daemon-reload after env-file edits; rootfs not rebuilt by
  deploy-qemu-dev.sh after agent change; golden snapshot caches
  in-memory agent across replacements).

## Status log (fill in as we go)

- *2026-05-06* — branch + working doc created. No code changes
  yet. Waiting for review before implementation.
