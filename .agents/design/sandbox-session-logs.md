# Sandbox session logs: capture, ship, and surface

## TL;DR

Give every sandbox a "Logs" tab in the dashboard that shows
**everything happening inside it** — every line written to
`/var/log/*`, every byte of stdout/stderr from every command exec'd
through the platform — with live tail and free-text search.

The shape: a small in-guest log forwarder embedded in `osb-agent`
ships events directly to **Axiom** over HTTPS, using a long-lived
**dataset-scoped ingest-only token** baked into sandbox env at
creation. The control plane is **not on the hot path** for log bytes
— it only mints the token at sandbox-create and proxies read queries
back to the UI. The UI's Logs tab is a thin SSE consumer over a new
`GET /api/sandboxes/:id/logs` endpoint that runs APL queries against
Axiom server-side.

This is a strict superset of what comparable sandbox platforms
surface today (process lifecycle only — `pid X started`, `pid X
ended` — no command text, no stdout, no network, no file ops).
Closing this gap is high-leverage: the same five lines of output
that resolve "why did my LLM call fail?" today require an
SSH-grade investigation.

A small precedent for the same architecture already exists in our
own agentbox-poc workspace (Go log-shipper, ~150 lines, identical
batch-and-POST shape). We're copying that shape, narrowing the
integration to osb-agent's existing exec path, and adding a
control-plane-proxied read API.

## Problem

Today, "what's happening inside my sandbox?" has no answer in the UI.

The closest existing surfaces are all partial:

- `command_logs` (`internal/db/migrations/001_initial.up.sql:58-68`,
  populated from `internal/db/store.go:821-848`) captures command +
  exit_code + duration_ms — no stdout/stderr, and never queried by
  the dashboard.
- `pty_session_logs` captures only metadata (bytes in/out, timestamps)
  — not the actual PTY bytes.
- `agent_events` / `agent_operations` (sessions-api) cover lifecycle
  for *agents*, not *sandboxes*; they're sparse and structured.
- The Logs tab on `AgentDetail.tsx` (line 322-419) tails
  `/tmp/openclaw-gateway.log` for OpenClaw-only managed agents via a
  per-call `getAgentLogs` SDK invocation, and the `GET
  /v1/agents/:id/logs` endpoint it calls **doesn't exist server-side**
  — that tab silently 404s.

What users actually need to see:

1. **Their own application logs.** Whatever they write to
   `/var/log/myapp.log` or stdout from a server they started.
2. **Command output.** Every `exec` they (or their SDK code) ran:
   command, args, stdout, stderr, exit code.
3. **System events that affect them.** OOM kills, kernel messages,
   the start/stop of services they configured.

A recent customer-support investigation hinges on exactly this gap:
a curl from inside a customer's sandbox to an external URL returned
an opaque upstream error, but nothing in the OC dashboard showed the
request, the response, or the failure mode. Resolving the ticket
required us to manually reproduce the call from a fresh sandbox in
another org. With a Logs tab, the user could have self-served — a
search for the destination hostname would have returned the line.

## Proposed architecture

Five components, three new:

```
┌──────────────────────── sandbox VM ────────────────────────┐
│                                                            │
│  /var/log/*   ──┐                                          │
│  Exec stdout ───┤   osb-agent (existing PID 1)             │
│  Exec stderr ───┤   ┌──────────────────────────────┐       │
│                 └─► │ log forwarder goroutine pool │ ──┐   │
│                     │ - tails /var/log via fsnotify│   │   │
│                     │ - tees from Exec/ExecStream  │   │   │
│                     │ - batches: 100 lines/200ms   │   │   │
│                     └──────────────────────────────┘   │   │
│                                                        │   │
└────────────────────────────────────────────────────────┼───┘
                                                         │
    worker → agent gRPC at boot:                        │
      ConfigureLogship(                                  │
        ingest_token, dataset,                           │ HTTPS
        sandbox_id, org_id)                              │ POST
                                                         │
                                                         ▼
                                              ┌──────────────────┐
                                              │      Axiom       │
                                              │ dataset:         │
                                              │ oc-sandbox-logs  │
                                              └────────┬─────────┘
                                                       │
                                                       │ APL query
                                                       │ (server-side
                                                       │ token, never
                                                       │ leaves OC)
                                                       │
┌──────── OC control plane ──────────────────────┐     │
│                                                │     │
│  GET /api/sandboxes/:id/logs    ◄──────────────┼─────┘
│    - auth: org owns sandbox                    │
│    - APL: where sandbox_id == :id              │
│    - SSE: emit historical batch + poll loop    │
│                                                │
└────────────────────┬───────────────────────────┘
                     │ EventSource
                     ▼
             SessionDetail.tsx → Logs tab
                  (live tail + search)
```

### 1. In-guest log forwarder (in `osb-agent`)

A new goroutine pool inside the existing `osb-agent` (PID 1 of every
sandbox VM). Lives in `cmd/agent/internal/logship/` (new package).

**Why in-process, not a separate binary:** half the data we want to
ship — exec stdout/stderr — is already produced inside the agent
(`Exec` / `ExecStream` RPC handlers). Routing it through IPC to a
sidecar would duplicate buffering and add a second crash surface.
The other half (`/var/log/*` tailing) is a generic concern but
small. A separate `osb-log-shipper` binary buys us nothing useful
and adds a process-supervision problem inside the VM.

**Data sources:**

- **`/var/log/**`** via `fsnotify`: on start, list and tail every
  regular file under `/var/log`. On `IN_CREATE` for new files
  (rotation, app-created), start a new tailer. On `IN_MODIFY`, read
  appended bytes. Standard inotify-tail pattern; total state is
  one file descriptor + offset per file.
- **Exec output (Exec, ExecStream, ExecSession*)**: a one-line tee
  in each handler. Existing handlers already produce stdout/stderr
  buffers; we hand the buffers to a non-blocking channel send into
  the forwarder. The current behaviour (returning stdout/stderr to
  the gRPC caller) is preserved unchanged.
- **PTY output**: out of scope for v1. PTY content is interactive
  (passwords, secrets typed by the user); shipping it by default
  has a different consent shape. We can add it behind an opt-in
  flag later.

**Event schema** (one Axiom row per line):

```json
{
  "_time":      "2026-05-05T16:55:51.123456789Z",
  "sandbox_id": "sb-0a1b2c3d",
  "org_id":     "org-7e8f9a",
  "source":     "var_log" | "exec_stdout" | "exec_stderr" | "agent",
  "line":       "May  5 16:55:51 sandbox sshd[1234]: ...",

  // when source == "var_log":
  "path":       "/var/log/syslog",

  // when source == "exec_*":
  "exec_id":    "ulid-of-this-exec",
  "command":    "curl",
  "argv":       ["-sS", "https://example.com"],
  "exit_code":  0
}
```

`exit_code` is set on the *final* line of the exec's output (the
EOF marker), not on every line — that lets the UI render
"command X exited 0/1" without joining to a separate table.

**Batching:** 100 lines or 200ms, whichever fires first. Same
discipline as agentbox's shipper. POST to
`https://api.axiom.co/v1/datasets/${AXIOM_DATASET}/ingest` with
`Authorization: Bearer ${AXIOM_INGEST_TOKEN}`.

**Backpressure:** the channel feeding the shipper has a fixed
buffer (10k events ≈ a few MB). If full, drop **oldest** with a
counter (`logship_dropped_total`). Drop-oldest matches user
intuition for "tail" — the recent stuff is what they're looking at;
the old stuff is the loss. The counter is itself shipped as an
event so the UI can show "logs dropped: N" if it ever happens.

**Reliability:** if Axiom returns 5xx or the network is partitioned,
retry with backoff up to 30s. After 30s, drop the batch with a
counter event. We do **not** spool to disk in v1: it adds an
unbounded-disk-usage failure mode (a partitioned sandbox running
for days could fill the disk) and the failure case it covers
(brief Axiom blip) is already handled by retry.

**Hot-upgrade:** none. The agent.proto stability contract
(`proto/agent/agent.proto:11-34`) means old sandboxes keep their
old agent forever. Sandboxes created before this lands won't have
log shipping. That's fine — the feature is opt-in by virtue of
sandbox-create timing, and old sandboxes typically don't live
longer than a week or two anyway.

### 2. osb-agent tee on Exec/ExecStream

Modify the four exec entry points to route output through the
forwarder in addition to the existing return path:

- `Exec` (`agent.proto:37`): collects full stdout/stderr buffers;
  on completion, hand both to the forwarder split into lines, with
  one final EOF event carrying `exit_code`.
- `ExecStream` (`agent.proto:40`): wraps the existing chunk
  pump with a goroutine that line-buffers and forwards.
- `ExecSessionAttach` (`agent.proto:72`): same wrap, but the exec
  session can outlive the attach — the forwarder needs to keep
  reading scrollback even after the gRPC client disconnects, so
  this hooks at the session level not the attach level.
- `PTYAttach` (`agent.proto:68`): **NOT** wrapped in v1. PTY content
  is interactive and includes typed passwords / secrets.

In all four, every line generated also gets a unique `exec_id`
(ULID generated when the exec starts), the `command` and `argv`,
and on the final EOF line the `exit_code`. Schema above.

Implementation note: do this with a `io.Writer` adaptor wrapped
around the existing stdout/stderr pipes. The forwarder is
`io.Writer`; chain it as a `io.MultiWriter` alongside the existing
buffer. Zero new buffering, ~20 lines of code per entry point.

### 3. Axiom dataset & schema

One dataset: `oc-sandbox-logs`. Same dataset across all OC
sandboxes (per-sandbox isolation enforced at query time, not
storage time — a separate dataset per sandbox would not scale).

**Token:** a single, long-lived, **dataset-scoped, ingest-only**
Axiom token. Stored in OC's secret store; injected into every
sandbox at create time as `AXIOM_INGEST_TOKEN`. Ingest tokens in
Axiom can only POST to `/ingest` for the named dataset — they
cannot read or query, cannot list other datasets, cannot administer.
A sandbox compromised by a malicious user can at worst write
spurious events claiming arbitrary `sandbox_id` / `org_id`, which
is an **annoyance, not a breach** — the read API filters on
`sandbox_id` matching the sandbox the user is viewing, and that
filter is enforced server-side based on the user's authenticated
session, not on data inside the event.

Spoofing isolation: a user *could* pollute another org's view by
emitting events with someone else's `sandbox_id`. We mitigate by
adding `_event_source` on ingest equal to the sandbox's worker
hostname, validated server-side at query time:

```apl
['oc-sandbox-logs']
  | where sandbox_id == "sb-0a1b2c3d"
  | where _event_source startswith "worker-"   // dropped events that
                                                // claim to be from
                                                // somewhere unexpected
  | sort by _time asc
```

This isn't airtight but it's the level of defence-in-depth that
matches the threat model: Axiom is not the source of truth for
billing or auth; it's a debugging surface.

Retention: whatever Axiom's default is for the plan we're on
(30 days at the time of writing). Out of scope for v1; we'll
revisit when we look at cost.

### 4. Configuration injection at sandbox boot

There is no existing path to inject env vars into the *agent
process's own* environment at PID 1 startup. The existing `SetEnvs`
RPC (`proto/agent/agent.proto:78-79`) stores envs in agent memory
**for user commands run via Exec/ExecStream** — it does not modify
the agent's own `os.Environ()`. The agent at PID 1 startup has only
the kernel-supplied minimal env (`PATH` is set explicitly in
`cmd/agent/main.go:26`, nothing else).

So we add **one new RPC** to `proto/agent/agent.proto` —
the only proto change in this design. Additive, allowed under the
proto stability contract:

```proto
// ConfigureLogship sets log-shipping configuration. Worker calls
// this immediately after VM boot, before any user-facing operations.
// Sandboxes whose worker doesn't call this never ship logs (kill-
// switch by omission).
rpc ConfigureLogship(ConfigureLogshipRequest) returns (ConfigureLogshipResponse);

message ConfigureLogshipRequest {
  string ingest_token = 1; // empty = disable; treated as kill-switch
  string dataset      = 2;
  string sandbox_id   = 3;
  string org_id       = 4;
}

message ConfigureLogshipResponse {}
```

Flow:

1. Add `AXIOM_INGEST_TOKEN` and `AXIOM_DATASET` to the worker's
   environment (deploy plumbing — the worker process reads these
   at startup, not the agent).
2. Worker calls `ConfigureLogship` over the worker→agent gRPC
   channel right after sandbox boot, passing the token, dataset,
   and the per-sandbox `sandbox_id` / `org_id` identifiers it
   already has in scope.
3. The agent receives the call and hands the four values to the
   forwarder, which lazy-initialises and starts shipping.
4. If `AXIOM_INGEST_TOKEN` is unset on the worker, the worker
   skips the call. Agent's forwarder stays dormant. This is the
   rollback / kill-switch — clear the env var, redeploy workers,
   new sandboxes don't ship.

**Brief startup window where shipping is inactive** (~50–100ms
between agent ready and worker's `ConfigureLogship` call). Logs
written in that window are not captured. Acceptable: we don't
backfill `/var/log` history anyway (we tail from current EOF), and
no exec runs before the worker is talking to the agent.

### 5. Read API: control-plane-proxied SSE

New endpoint: `GET /api/sandboxes/:sandboxId/logs`.

**Auth:** existing dashboard auth path (cookie / JWT). The handler
verifies `sandboxId` belongs to an org the user is a member of —
same check `sandbox.go` already performs for other dashboard reads.

**Query params:**

- `tail=true|false` (default `true`): if true, after returning
  historical batch, keep the SSE open and poll for new events.
- `since=<rfc3339>` / `until=<rfc3339>`: time window. Default is
  `since=sandbox.created_at`, `until=now`.
- `q=<text>`: free-text search; appends `| where line contains "..."`
  to the APL query (with escaping).
- `source=var_log|exec_stdout|exec_stderr|agent`: filter by source.
- `limit=<n>` (default 1000, max 10000): cap on the historical
  batch. Live tail isn't capped.

**Response shape:** SSE stream. Each `data:` line is a JSON event
matching the ingest schema (sans `_time` parsing — leave the wire
format identical so the client doesn't have to know two shapes).
A `comment` line every 15s as an SSE keepalive.

**Live tail loop:** poll Axiom every 1s with `since=<last_event_time>`,
emit any new rows. Same shape as agentbox's
`packages/api/src/routes/sessions.ts:240-275`. We can revisit this
to use Axiom's [streaming query API](https://axiom.co/docs/restapi/endpoints/queryApl)
once we confirm it's stable, but the polling path is fine for v1
and matches the proven precedent.

**Why not a direct Axiom query from the browser?** Two reasons:
(a) it would require shipping a query token to the client, which
is an entirely separate auth surface to manage; (b) we want one
chokepoint where we enforce "this user can only see their org's
sandboxes." The control plane already owns that check; let it own
the query.

### 6. UI: Logs tab on SessionDetail

`web/src/pages/SessionDetail.tsx` gets a new tab next to the
existing Terminal button. Components:

- **Stream viewer:** a virtualised list (the same library
  `AgentDetail` uses for its broken Logs tab — we can lift that
  component). Each row is one event, color-coded by `source`
  (stdout neutral, stderr red-tinted, var_log gray, agent purple).
  Auto-scroll-to-bottom unless the user scrolls up; pause/resume
  toggle.
- **Header bar:** time-range picker (last 1h / 6h / 24h / since
  sandbox boot / custom), source-filter chips, a search box, and
  a small status indicator showing live-tail state.
- **Search:** debounced 200ms, sends the query string to the
  endpoint, replaces the visible stream with the filtered batch.
  When cleared, returns to live tail.
- **Per-event drill-in:** click a `source=exec_*` row to scroll the
  view to all rows with the same `exec_id` — equivalent to "show
  me the full output of this command."

Out of scope for v1: log download, retention controls, alert rules,
saved searches. We can add any of these later without re-architecting.

## Tradeoffs

| | **Tee in osb-agent + Axiom (proposed)** | **Run rsyslog/journald + ship via Vector** | **Stream over existing gRPC, store in OC's DB** |
|---|---|---|---|
| Hot path through control plane | None — guest pushes direct | None — guest pushes direct | Yes — every byte through worker → control plane |
| New infra to operate | One Axiom dataset | rsyslog + Vector + storage backend | Schema, retention, indexing on our DB |
| Search latency | <1s (Axiom indexed) | depends on backend | minutes-to-hours unless we add OpenSearch |
| Time to v1 | 3-5 days | 2-3 weeks | 2-3 weeks + ongoing index ops |
| Vendor lock-in | Yes — Axiom | Low — Vector swaps backends | None |
| Cost at our scale (today) | small (decide later) | similar | high (storage + index) |
| Supports live tail | Yes (1s poll) | Yes if backend supports | Yes if we build it |
| Supports search | Yes (APL) | Yes if backend supports | Painful without index |
| Captures ad-hoc user processes | No (only platform-exec'd) | Yes (everything goes through syslog) | No (only platform-exec'd) |

The sharpest tradeoff is **vendor lock-in for ingest and query
shape**. Mitigation: the forwarder's egress is one HTTP POST with a
small JSON shape. If we ever want to move off Axiom, we
re-target the URL and rewrite the query path; no code outside the
forwarder and the read endpoint depends on Axiom-specifics.

The "captures ad-hoc user processes" gap is real — if a user
runs `nohup my-server &` inside an interactive shell, the platform
never sees the process and its stdout never reaches the tee. The
auditd-based process tracking I sketched in chat-history is the
follow-up that closes this; that's a Tier-3 enhancement on top of
this design, not a prerequisite.

## Open design questions

These are deliberately deferred:

- **Cost.** Ingest is cheap; query / retention is the variable.
  Need a back-of-envelope based on the GB-hours data we already
  have, plus a per-sandbox rate cap (e.g. 1MB/s ingest). Can do
  this once we have a week of v1 data.
- **Retention.** Default is whatever Axiom defaults to. We may
  want shorter (cheaper, less risk for sensitive data) or longer
  (deeper investigation window). Decide once we see the cost
  envelope.
- **Privacy / opt-out.** Some customers will run sensitive code
  inside sandboxes. Default-on log capture without a per-org
  opt-out is a soft commitment we should think about before broad
  GA. Easy to add: a flag on `orgs` that, when set, makes the
  worker skip injecting `AXIOM_INGEST_TOKEN` for that org's
  sandboxes.
- **PTY content.** Excluded from v1 for the consent reasons above.
  If we want to add it, the natural shape is a per-sandbox flag
  ("Capture PTY output to logs?") visible in the UI when the user
  opens a terminal.
- **Per-org or per-tier rate caps.** A pathological sandbox can
  push hundreds of MB of logs/sec. The forwarder's drop-oldest
  semantics protect us from unbounded memory; a hard rate cap at
  the egress (e.g. drop events past N/sec) protects Axiom and our
  bill.
- **How `command_logs` and `agent_events` interact with this.**
  Log events overlap conceptually with `command_logs` (which has
  cmd + exit + duration) and `agent_events` (sparse error events).
  We don't propose deleting either — they're cheap, structured,
  and useful as authoritative summaries. Logs are the unstructured
  firehose for drill-in. The Logs tab and the existing Events tab
  on AgentDetail can coexist.

## Migration plan

Phased so each phase is independently shippable and revertible.

**Phase 0 — Axiom plumbing (no user-visible change).**
1. Provision the Axiom workspace (or add a dataset to the existing
   one used by agentbox, if we share). Create:
   - Dataset `oc-sandbox-logs` (30-day retention default).
   - One ingest token, dataset-scoped, ingest-only.
   - One query token, dataset-scoped, read-only — stored in OC
     control plane secret store.
2. Add `AXIOM_INGEST_TOKEN`, `AXIOM_DATASET`, `AXIOM_QUERY_TOKEN`
   to the deploy env templates.

**Phase 1 — osb-agent forwarder behind a flag.**
1. Implement `cmd/agent/internal/logship/` with `/var/log` tail and
   one `io.Writer` interface that wraps stdout/stderr.
2. Implement the in-`Exec` / `ExecStream` / `ExecSessionAttach`
   tees as a `io.MultiWriter` chain.
3. Gate with `AXIOM_INGEST_TOKEN`-empty-means-disabled; default off
   in dev, on in prod.
4. Bake into a new agent build; new sandboxes pick it up
   automatically. Test on a single dev worker first.

**Phase 2 — read API.**
1. Add `internal/api/sandbox_logs.go` with the SSE endpoint and the
   APL query builder.
2. Wire into `internal/api/router.go`.
3. Test end-to-end: spin up a sandbox, run a few execs, hit the
   endpoint, see events.

**Phase 3 — UI Logs tab.**
1. Add the tab to `web/src/pages/SessionDetail.tsx`.
2. Lift the virtualised list component pattern from
   `AgentDetail.tsx`'s Logs tab.
3. Implement the search box + source filter + time picker.
4. QA on a few real sandboxes; check tail latency, search latency,
   memory footprint of the page after 30 minutes of streaming.

**Phase 4 — turn it on broadly.**
1. Watch ingest volume and Axiom cost for a week.
2. Decide on rate cap, retention, and any per-org opt-out.
3. Document in the public docs.

Each phase is reversible by a config / deploy change without
schema migrations on our DB.

## What we're NOT changing

- `command_logs` table — keeps populating from
  `internal/db/store.go:821-848`. Still useful as authoritative
  cmd + exit + duration summary, especially for billing-adjacent
  audits.
- `agent_events` / `agent_operations` in sessions-api — unchanged.
- `AgentDetail.tsx` Logs tab — that's an OpenClaw-specific
  thing; stays as-is. (It's separately broken — the
  `GET /v1/agents/:id/logs` endpoint doesn't exist in sessions-api
  — but that's tracked separately and unrelated to sandbox-level
  logs.)
- The egress proxy (`internal/secretsproxy/`) — no proposed
  changes. We do propose teeing the proxy's request log to Axiom
  too as a follow-up (Tier-2), but it's not in v1 scope.
- The `osb-agent` ↔ worker proto — exactly **one** new RPC
  (`ConfigureLogship`, see §4). Log bytes themselves do not flow
  over this channel; they go out a separate egress path entirely
  (HTTPS to Axiom). The proto change is configuration only.
- Per-sandbox secret store — no new secrets per sandbox.
  `AXIOM_INGEST_TOKEN` is a single global config value.

## Decisions needed before phase 0

- **Dataset name and shape.** Proposed: `oc-sandbox-logs` (one
  dataset, one shape, every sandbox writes to it).
- **Reuse agentbox's Axiom workspace, or stand up our own?**
  Proposed: stand up our own — different product, different
  retention/cost tradeoffs likely, and we don't want a single
  Axiom outage to take down both.
- **Token rotation cadence.** Proposed: none. Long-lived, ingest-
  only, baked into worker env at deploy time. Rotate when the
  threat surface changes (e.g. if we suspect a leak), not on a
  schedule.
- **Should we ship to Axiom *and* our own DB?** Proposed: no.
  Axiom is the only store. `command_logs` and `agent_events` stay
  as their own narrow surfaces and don't try to be log stores.

Once these are agreed, phase 0 is a half-day of Axiom + deploy
plumbing; phase 1-3 is the bulk of the work (3-5 days of focused
engineering).
