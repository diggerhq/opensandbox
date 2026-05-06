# Logship: warn loudly when the Axiom token is empty

> ⚠️ **NO PII IN GIT.** Audit before every Write/Edit. Rule:
> `feedback_no_pii_in_git.md` in project memory.

Status: **planned, not started.** Builds on PR #226 (the
sandbox-session-logs feature, merged `2af73c4`) and Brian's
follow-up `d6c2985` (KV mapping for Axiom secrets).

## Context

After PR #226 merged and rolled out, prod workers silently weren't
shipping logs. Two independent gaps caused it; both were silent.

1. **KV-mapping gap** — `internal/config/keyvault.go` only loads
   secrets that exist in `secretMapping`; `AXIOM_*` weren't in the
   map. **Closed by Brian's `d6c2985`.**
2. **Stale-cfg gap** — `cfg.AxiomIngestToken` is read from
   `os.Getenv` once at server startup. A server that started
   *before* the secret was added to KV (or before any rotation)
   keeps an empty cfg and bakes empty tokens into every worker
   it spawns. No log line told anyone.

The actual prod recovery is automatic: the next deploy restarts
the control plane (re-reads cfg from KV) and bumps
`targetWorkerVersion`; the autoscaler's rolling-replace
(`internal/controlplane/scaler.go:823`) then recycles every stale
worker. **No prod SSH, no manual drain. The deploy is the fix.**

This PR is **not the fix for that incident** — it's three small
log lines so the *next* silent-empty case is paged-on-able within
seconds instead of overnight.

## Changes (all in `cmd/server/main.go`)

1. **Startup log** mirroring the existing `"sandbox session logs
   read API enabled"` line on the query-token side. Empty
   ingest-token logs `WARNING: workers spawned by this server
   will NOT ship logs`. Populated logs the symmetric ok message.
2. **Spawn-time warning** when `cfg.AxiomIngestToken == ""` and
   the Azure-pool path is rendering `workerEnv`. Catches the
   stale-cfg case where the server hasn't been restarted
   post-rotation.
3. **One-line README addition** in `deploy/ec2/README.md`: any
   `AXIOM_*` rotation requires a server restart.

Total: ~10 lines of code + ~2 lines of docs. No proto, no schema,
no UI, no tests (just `log.Printf`).

## Deliberately out of scope

- **Re-reading `os.Getenv` per spawn.** Process env is also
  frozen-ish; right fix for rotation-without-restart is a periodic
  reloader, scope creep.
- **Fail-fast on empty.** Server has many legitimate reasons to
  run without logship (dev, combined mode, kill-switch). Warning
  is the right level.
- **A metric.** No control-plane-side metric pipeline yet; log
  line is enough for ops paging on regex.
- **Retroactive fix for already-running sandboxes.** Their in-VM
  agent has a dormant shipper; only new sandboxes ship. No code
  fix possible — it's a property of when ConfigureLogship was
  called.

## Verification

On the EC2 dev host: unset `AXIOM_INGEST_TOKEN`, daemon-reload,
restart server → confirm WARNING fires once. Set the token,
restart → confirm ok line fires. Spawn-time warning fires when
the bake template path runs with empty token (only reachable via
the Azure-pool spawn — verified by code inspection).

## Status log

- *2026-05-06* — branch + working doc created. No code yet.
  Awaiting review before implementation.
