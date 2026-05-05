# Sandbox session logs — implementation plan

> ⚠️ **NO PII IN GIT.** Customer emails, person/org names, ticket
> identifiers tying activity to a specific individual must never
> appear in any committed file. Use generic stand-ins ("a customer",
> anonymised IDs). Full rule in project memory
> `feedback_no_pii_in_git.md`. Audit before every Write/Edit.

Status: **design approved, implementation not started.** Builds on
[`.agents/design/sandbox-session-logs.md`](../design/sandbox-session-logs.md)
(design doc, draft PR #226) and the OC architecture as of `main` at
the time of writing.

This is a **complete reference** for the implementation. Future agents
should be able to work from this doc + the design doc without
needing conversation history.

Decisions locked in (from design doc; do not relitigate without
re-opening the design):

| Decision | Locked value |
|---|---|
| Log host | Axiom |
| Forwarder placement | In-process inside `osb-agent` (no sidecar) |
| Token model | One long-lived, dataset-scoped, **ingest-only** Axiom token, baked into worker env at deploy time |
| `/var/log` capture | Push-only, via `fsnotify`-based tail in the forwarder |
| Exec stdout/stderr capture | Tee in osb-agent's `Exec` / `ExecStream` / `ExecSessionAttach` handlers via `io.MultiWriter` |
| PTY content | **Excluded** from v1 (consent surface is different) |
| Read API | SSE-based, control-plane-proxied; never expose Axiom token to client |
| Auth on read | Existing dashboard auth + `org owns sandbox` check |
| Live tail mechanism | Poll Axiom every 1s (matches agentbox precedent; revisit if Axiom streaming-query proves stable) |
| MVP scope | Live tail + free-text search |
| Out of MVP | Retention controls, log download, alert rules, saved searches, per-org opt-out, rate caps, PTY capture, ad-hoc-process tracking |
| Backpressure | Drop-oldest with counter event; in-memory only (no on-disk spool) |

## What lives where

All work is in this repo (`opencomputer`). No cross-repo coordination
needed.

```
.agents/
├── design/sandbox-session-logs.md             (design doc — same PR)
└── work/sandbox-session-logs-impl.md          (this file)

proto/agent/
└── agent.proto                                (one new RPC: ConfigureLogship)

cmd/agent/                                     (the in-guest agent)
├── main.go                                    (wire shipper + tailer at startup)
└── internal/logship/                          (new package, all forwarder code)
    ├── shipper.go                             (Axiom HTTP client + batcher + drop-oldest)
    ├── varlog.go                              (fsnotify tail of /var/log/**)
    ├── tee.go                                 (io.Writer adapter for exec output)
    └── shipper_test.go                        (unit tests)

internal/agent/                                (in-VM agent server)
├── server.go                                  (add logshipCfg + shipper handle)
├── logship.go                                 (new: ConfigureLogship handler)
└── exec.go                                    (existing — wrap pipes with tee in Phase 1)

internal/config/
└── config.go                                  (add AxiomIngestToken + AxiomDataset)

internal/api/
├── router.go                                  (one new line: register sandbox_logs route)
└── sandbox_logs.go                            (new: SSE handler + APL query builder)

internal/worker/
└── grpc_server.go                             (call ConfigureLogship after sandbox create)

web/src/pages/SessionDetail.tsx                (add Logs tab)
```

No new tables in OC's Postgres. **One** new RPC in `agent.proto`
(additive — `ConfigureLogship` for config delivery; log bytes never
flow over this channel, only configuration). The forwarder is a
pure addition to the agent binary; the read API is a single new
HTTP handler; the UI gain is one tab.

## Pre-flight (do once, before any phase)

1. Confirm the design PR (#226) is approved/merged or that the team
   is OK with implementation proceeding off the draft.
2. Provision Axiom workspace + dataset:
   - Decide: shared-with-agentbox workspace, or new OC-only workspace?
     **Default: new workspace.** Agentbox is a different product
     with different retention/cost tradeoffs and shouldn't share
     blast radius.
   - Create dataset `oc-sandbox-logs` (default 30-day retention).
   - Create one **ingest-only** API token, scoped to the dataset.
     Note this is the ingest token that goes into every worker.
   - Create one **read-only** API token, scoped to the dataset.
     Note this is the query token that goes into the OC control
     plane secret store.
3. Add three secrets to OC deploy plumbing
   ([`deploy/server.env.example`](../../deploy/server.env.example)
   pattern, but actually wired through Azure Key Vault per the
   existing convention):
   - `AXIOM_INGEST_TOKEN` (workers need this)
   - `AXIOM_DATASET` (= `oc-sandbox-logs`)
   - `AXIOM_QUERY_TOKEN` (control plane needs this)

Phase 0 (next section) is the engineering work that consumes these.

---

## Phase 0 — config + new RPC plumbing (no behaviour change)

Goal: token reaches the worker process, `ConfigureLogship` RPC is
defined and wired through worker→agent, and the agent stores the
config in memory. **No actual shipping yet** — Phase 1 introduces
the forwarder that consumes the config.

### 0.1 Why a new RPC (and not env injection)

Initial assumption was "inject env vars at sandbox boot via the
existing mechanism." Verified false:

- The agent's own `os.Environ()` at PID 1 startup is minimal —
  only `PATH` is set explicitly in
  [`cmd/agent/main.go:26`](../../cmd/agent/main.go).
- The existing `SetEnvs` RPC
  ([`proto/agent/agent.proto:78-79`](../../proto/agent/agent.proto))
  stores envs in agent memory **for user commands only**
  (injected into `Exec`/`ExecStream`); it does not modify the
  agent's own process env.
- Implementations live in `internal/agent/server.go` and
  `internal/agent/exec.go:58-66, 88-96` — they append the stored
  envs to each user `cmd.Env`, never to the agent itself.

So the cleanest fix is one new RPC that the worker calls right
after VM boot. Additive proto change, allowed under the agent.proto
stability contract (`proto/agent/agent.proto:11-34`).

### 0.2 Worker config

Add two fields to OC's `Config` struct
([`internal/config/config.go:140`](../../internal/config/config.go)),
near the existing external-service config (Segment / Stripe):

```go
// Axiom — log shipping for sandbox session logs.
// Empty token = log shipping disabled (kill-switch).
AxiomIngestToken string
AxiomDataset     string
```

In `Load()` (around line 228, near `SegmentWriteKey`):

```go
AxiomIngestToken: os.Getenv("AXIOM_INGEST_TOKEN"),
AxiomDataset:     envOrDefault("AXIOM_DATASET", "oc-sandbox-logs"),
```

### 0.3 New proto RPC

Add to [`proto/agent/agent.proto`](../../proto/agent/agent.proto)
(near the `SetEnvs` RPC for adjacency, since they're conceptually
related):

```proto
// ConfigureLogship sets log-shipping configuration. Worker calls
// this immediately after VM boot, before any user-facing operations.
// If never called, the forwarder stays dormant — this is the
// kill-switch (worker doesn't call when ingest_token is unset).
rpc ConfigureLogship(ConfigureLogshipRequest) returns (ConfigureLogshipResponse);
```

```proto
// --- Logship ---

message ConfigureLogshipRequest {
  string ingest_token = 1;
  string dataset      = 2;
  string sandbox_id   = 3;
  string org_id       = 4;
}

message ConfigureLogshipResponse {}
```

Regenerate via the existing proto-gen pipeline (likely a `make
proto` or equivalent — verify in the Makefile / scripts before
running).

### 0.4 Agent-side handler

Add to `internal/agent/server.go` (or a new `logship.go` in the
same package) a handler that stores the config and exposes it to
the forwarder:

```go
type LogshipConfig struct {
    IngestToken string
    Dataset     string
    SandboxID   string
    OrgID       string
}

func (s *Server) ConfigureLogship(ctx context.Context, req *pb.ConfigureLogshipRequest) (*pb.ConfigureLogshipResponse, error) {
    s.logshipMu.Lock()
    s.logshipCfg = LogshipConfig{
        IngestToken: req.IngestToken,
        Dataset:     req.Dataset,
        SandboxID:   req.SandboxId,
        OrgID:       req.OrgId,
    }
    s.logshipMu.Unlock()

    // Phase 1 will: signal the forwarder goroutine that config is ready.
    // Phase 0 just stores the config.

    return &pb.ConfigureLogshipResponse{}, nil
}
```

Add `logshipMu sync.Mutex` and `logshipCfg LogshipConfig` fields to
the server struct.

### 0.5 Worker calls `ConfigureLogship` at sandbox boot

In `internal/worker/grpc_server.go`, in `CreateSandbox` after `sb,
err := s.manager.Create(ctx, cfg)` succeeds (around line 224 — verify
before editing), add:

```go
if s.cfg.AxiomIngestToken != "" {
    orgID, _ := s.store.GetSandboxOrgID(ctx, sb.ID)
    if err := s.callConfigureLogship(ctx, sb.ID, orgID); err != nil {
        // Don't fail sandbox create on a logship config failure —
        // shipping just stays dormant for this sandbox. Log loudly.
        log.Printf("grpc: ConfigureLogship for %s failed: %v", sb.ID, err)
    }
}
```

`callConfigureLogship` opens a (probably-cached) gRPC connection to
the agent inside the sandbox and calls the new RPC. The exact
plumbing follows whatever pattern the manager already uses for
agent RPCs (manager exposes an `agent client` per sandbox; reuse).

Same insertion in the `ForkFromCheckpoint` paths (lines ~169 and
~191) — restored sandboxes also need their forwarder configured.

### 0.6 Verify

Boot a dev sandbox with `AXIOM_INGEST_TOKEN=test-token` set on the
worker. Inside the sandbox, no env vars appear yet (correct — that
was the old design). Instead:

- Check the worker log shows `ConfigureLogship for <sb_id>` did not
  fail.
- Add temporary debug logging in the agent's `ConfigureLogship`
  handler to confirm it received the call with the right values.
- Remove the debug logging before commit.

### 0.7 Verification checklist

- [ ] Config struct has `AxiomIngestToken` and `AxiomDataset`
      fields, populated from env in `Load()`
- [ ] `proto/agent/agent.proto` has `ConfigureLogship` RPC and
      `ConfigureLogshipRequest` / `ConfigureLogshipResponse`
      messages with the right field numbers
- [ ] Generated proto code committed (`agent.pb.go` /
      `agent_grpc.pb.go`)
- [ ] Agent server handles `ConfigureLogship` and stores the
      config in `s.logshipCfg`
- [ ] Worker calls `ConfigureLogship` when token is set, both in
      fresh-create and fork-from-checkpoint paths
- [ ] Worker skips the call when token is unset (kill-switch works)
- [ ] Worker tolerates a `ConfigureLogship` failure (logs loudly,
      sandbox still boots successfully)
- [ ] No regression in existing sandbox boot path
- [ ] Three secrets visible in deploy templates / vault config

---

## Phase 1 — in-guest forwarder

Goal: every line written to `/var/log/**` and every line of stdout/
stderr produced by `Exec`/`ExecStream`/`ExecSessionAttach` lands in
Axiom under the schema in the design doc.

### 1.1 Package layout

`cmd/agent/internal/logship/`:

- **`shipper.go`** — owns the Axiom HTTP client, the in-memory
  ring buffer (drop-oldest), and the batch flusher.
- **`varlog.go`** — fsnotify-based tail of `/var/log/**`.
- **`tee.go`** — `io.Writer` that adapts a stream of bytes from
  exec handlers into line-segmented events.
- **`shipper_test.go`** — unit tests, especially for the tee
  line-buffering and the drop-oldest ring.

### 1.2 Core types

```go
package logship

type Source string

const (
    SourceVarLog     Source = "var_log"
    SourceExecStdout Source = "exec_stdout"
    SourceExecStderr Source = "exec_stderr"
    SourceAgent      Source = "agent"
)

type Event struct {
    Time      time.Time `json:"_time"`
    SandboxID string    `json:"sandbox_id"`
    OrgID     string    `json:"org_id"`
    Source    Source    `json:"source"`
    Line      string    `json:"line"`

    // optional, depending on source:
    Path     string   `json:"path,omitempty"`      // var_log
    ExecID   string   `json:"exec_id,omitempty"`   // exec_*
    Command  string   `json:"command,omitempty"`   // exec_*
    Argv     []string `json:"argv,omitempty"`      // exec_*
    ExitCode *int     `json:"exit_code,omitempty"` // exec_*, set on EOF only
}

type Shipper struct {
    cfg    Config
    in     chan Event       // bounded; drop-oldest when full
    closed atomic.Bool
}

type Config struct {
    IngestToken string
    Dataset     string
    SandboxID   string
    OrgID       string
    BatchSize   int           // default 100
    FlushEvery  time.Duration // default 200ms
    BufferSize  int           // default 10_000
    HTTPTimeout time.Duration // default 10s
}
```

### 1.3 Drop-oldest ring

Exact behaviour:

```go
func (s *Shipper) Send(ev Event) {
    if s.closed.Load() { return }
    select {
    case s.in <- ev:
        // delivered
    default:
        // buffer full — drop the oldest, retry once
        select {
        case <-s.in: // discard one old event
            atomic.AddUint64(&s.droppedTotal, 1)
        default:
        }
        select {
        case s.in <- ev:
        default:
            atomic.AddUint64(&s.droppedTotal, 1)
        }
    }
}
```

Every 30s the shipper synthesises a `source: "agent"` event with
`line: "logship: dropped N events since last report"` if N > 0,
and resets the counter. That way the UI sees the dropped count
without us building a separate metric pipeline.

### 1.4 Batch flusher

Standard ticker-or-batch-full pattern, modeled on
agentbox's `log-shipper/main.go:60-108` (in our local agentbox-poc
workspace):

```go
func (s *Shipper) run(ctx context.Context) {
    var batch []Event
    flush := func() {
        if len(batch) == 0 { return }
        s.post(ctx, batch)
        batch = batch[:0]
    }
    tick := time.NewTicker(s.cfg.FlushEvery)
    defer tick.Stop()
    for {
        select {
        case <-ctx.Done():
            flush()
            return
        case ev := <-s.in:
            batch = append(batch, ev)
            if len(batch) >= s.cfg.BatchSize {
                flush()
            }
        case <-tick.C:
            flush()
        }
    }
}
```

`post` does an HTTPS POST to
`https://api.axiom.co/v1/datasets/{dataset}/ingest` with
`Authorization: Bearer {token}`, JSON body of the batch slice. On
non-2xx, retry up to 30s of backoff (250ms → 500ms → 1s → 2s → 5s
caps), then drop the batch and emit a counter event. **Don't block
ingestion** while a POST is in flight — the loop should keep
draining `s.in` while a separate goroutine handles the POST.
Easiest shape: flusher pushes batches to a second buffered channel
that a small worker pool drains.

### 1.5 `/var/log` tail

```go
type VarLogTailer struct {
    shipper *Shipper
    root    string // "/var/log"
}

// On start: walk root, open every regular file at offset = file end
// (we don't backfill historical content), register fsnotify on root
// (recursive). On WRITE event for a tracked file: read appended
// bytes, split into lines, Send to shipper. On CREATE: start
// tracking. On RENAME (rotation): close current fd, the new file
// will trigger CREATE.
```

Edge cases:

- Files truncated in place (rare, some loggers do this): the read
  position becomes invalid. Handle by stat'ing on each read — if
  `size < lastOffset`, reset `lastOffset = 0`.
- Binary files in `/var/log` (unlikely but possible — `wtmp`,
  `btmp`): skip files we can't decode as UTF-8 (or where the first
  N bytes contain NUL). Decision: skip files whose name matches
  `(wtmp|btmp|lastlog|*.gz|*.[0-9])`. Compressed rotated logs
  don't get tailed.
- Nested directories: recursive watch. Some apps log to
  `/var/log/myapp/output.log`. Use `filepath.Walk` once on start;
  fsnotify recursive isn't built in but a small directory watcher
  on top of `IN_CREATE` for new dirs is straightforward.

### 1.6 Exec tee

The tee point is the exec handlers in the agent. Today they look
roughly (verify exact shape in
[`cmd/agent/main.go`](../../cmd/agent/main.go) before
editing):

```go
func (s *agentServer) ExecStream(req *pb.ExecRequest, stream pb.SandboxAgent_ExecStreamServer) error {
    cmd := exec.Command(...)
    stdout, _ := cmd.StdoutPipe()
    stderr, _ := cmd.StderrPipe()
    cmd.Start()
    // pump stdout/stderr into stream chunks ...
}
```

Insertion: wrap the pipes with `io.MultiWriter`-style tees that
also feed a per-exec `*logship.LineWriter`:

```go
execID := ulid.Make().String()
cmdName, argv := req.Command, req.Args
stdoutTee := logship.NewLineWriter(s.shipper, logship.SourceExecStdout,
    execID, cmdName, argv)
stderrTee := logship.NewLineWriter(s.shipper, logship.SourceExecStderr,
    execID, cmdName, argv)

stdoutR := io.TeeReader(stdout, stdoutTee)
stderrR := io.TeeReader(stderr, stderrTee)

// pump stdoutR/stderrR into stream chunks (existing logic) ...

// after cmd.Wait(), emit final EOF event with exit_code
exitCode := cmd.ProcessState.ExitCode()
stdoutTee.Close(exitCode)  // emits one final event with the code
stderrTee.Close(exitCode)
```

`LineWriter` line-buffers writes (split on `\n`, hold a partial
last line until next write or Close). On `Close(exitCode)`, flush
any partial line and emit one synthetic event with `exit_code`
set.

The same wrapper applies to `Exec` (no streaming — just operate on
the buffered stdout/stderr after `cmd.Run()`) and to
`ExecSessionAttach` (wraps the session-internal pty/pipe; needs
care because exec sessions can outlive a single attach call).

### 1.7 Wiring into agent main and the ConfigureLogship handler

The shipper is created up-front but **dormant** — it doesn't start
shipping until `ConfigureLogship` arrives from the worker.

```go
// cmd/agent/main.go (sketch)

func main() {
    // ... existing init

    shipper := logship.New() // dormant; no token yet
    go shipper.Run(ctx)      // run loop is a no-op until Activate is called

    tailer := logship.NewVarLogTailer(shipper, "/var/log")
    go tailer.Run(ctx)       // tails always; events queue but don't ship

    srv := newAgentServer(shipper)
    // ... existing serve loop (existing flow registers the new
    // ConfigureLogship handler alongside the rest)
}
```

The agent's `ConfigureLogship` handler (added in Phase 0; activated
in Phase 1) calls `shipper.Activate(cfg)`:

```go
// internal/agent/logship.go
func (s *Server) ConfigureLogship(ctx context.Context, req *pb.ConfigureLogshipRequest) (*pb.ConfigureLogshipResponse, error) {
    if req.IngestToken == "" {
        return &pb.ConfigureLogshipResponse{}, nil // kill-switch
    }
    s.shipper.Activate(logship.Config{
        IngestToken: req.IngestToken,
        Dataset:     req.Dataset,
        SandboxID:   req.SandboxId,
        OrgID:       req.OrgId,
    })
    return &pb.ConfigureLogshipResponse{}, nil
}
```

Why dormant-then-activate (vs. lazy-create-on-first-Configure):

- The `/var/log` tailer wants to start as early as possible (catch
  appends from supervisord, OS init scripts, etc.). Creating it
  only after `ConfigureLogship` arrives means we miss the boot
  window entirely.
- Pre-Activate, `shipper.Send(ev)` queues into the bounded ring
  exactly as it would otherwise; the run loop just doesn't POST
  yet. Once Activate fires, the buffered events get flushed
  with the worker-supplied `sandbox_id` / `org_id` populated
  on each event at flush time (not at Send time).
- If Activate is never called (kill-switch case), the ring drops
  oldest indefinitely. Negligible memory cost since `/var/log`
  on an idle sandbox is quiet.

### 1.8 Verification

End-to-end check on a dev worker pointing at a real (cheap) Axiom
dataset:

1. Boot a sandbox with all four env vars set.
2. From the host: `oc exec <sbx> -- echo "hello world"`.
3. Query Axiom:
   ```
   ['oc-sandbox-logs']
     | where sandbox_id == "<sbx_id>" and source == "exec_stdout"
     | sort by _time desc
     | limit 5
   ```
   Expect to see one row with `line: "hello world"` and the next
   row with `line: ""` and `exit_code: 0`.
4. Inside the sandbox: `echo "from inside" >> /var/log/my.log`.
5. Query Axiom for `source == "var_log" and path == "/var/log/my.log"`.
   Expect one row.
6. Inside the sandbox: `seq 1 100000 | tee /tmp/big.log`.
7. Verify the dropped-events counter event eventually shows up
   (or doesn't, if the buffer keeps up — both are valid; we want
   to confirm the path works).

### 1.9 Verification checklist

- [ ] `logship` package builds and unit tests pass
- [ ] LineWriter handles partial-line writes correctly (no
      duplicates, no losses)
- [ ] Drop-oldest ring confirmed under synthetic flood test
- [ ] `/var/log` tail picks up appends to existing files
- [ ] `/var/log` tail picks up newly created files
- [ ] `/var/log` tail handles rotation (file moved out, new file
      created with same name)
- [ ] Exec stdout reaches Axiom with correct `exec_id` and `command`
- [ ] Exit code appears on the synthetic EOF event for an exec
- [ ] Killswitch works: agent runs cleanly with `AXIOM_INGEST_TOKEN`
      unset, no panics, no goroutine leaks
- [ ] Existing exec contract unchanged: same stdout/stderr/exit_code
      on the gRPC return path

---

## Phase 2 — read API

Goal: `GET /api/sandboxes/:sandboxId/logs` returns historical batch
+ live tail via SSE, with auth and a control-plane-side query token.

### 2.1 Files

- New: `internal/api/sandbox_logs.go` — handler + APL query builder
- Modify: `internal/api/router.go` — register one new route

### 2.2 Handler skeleton

```go
package api

func (s *Server) sandboxLogs(c echo.Context) error {
    orgID := auth.GetOrgID(c)
    sandboxID := c.Param("sandboxId")

    // Existing pattern: verify the sandbox is in the caller's org.
    sb, err := s.db.GetSandbox(c.Request().Context(), sandboxID)
    if err != nil { return echo.ErrNotFound }
    if sb.OrgID != orgID { return echo.ErrNotFound }

    q := buildAPL(sandboxID, c.QueryParams())

    // SSE setup
    c.Response().Header().Set("Content-Type", "text/event-stream")
    c.Response().Header().Set("Cache-Control", "no-cache")
    c.Response().Header().Set("Connection", "keep-alive")
    c.Response().WriteHeader(http.StatusOK)
    flusher := c.Response().Writer.(http.Flusher)

    // Initial historical batch
    rows, err := axiomQuery(ctx, q.historical())
    if err != nil { return err }
    for _, r := range rows {
        writeSSE(c.Response(), r)
        flusher.Flush()
    }

    // Live tail loop (only if `tail=true`, default true)
    if q.tail {
        lastTime := rows[len(rows)-1].Time
        ticker := time.NewTicker(1 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-c.Request().Context().Done():
                return nil
            case <-ticker.C:
                newRows, _ := axiomQuery(ctx, q.tailSince(lastTime))
                for _, r := range newRows {
                    writeSSE(c.Response(), r)
                    flusher.Flush()
                    lastTime = r.Time
                }
                // SSE keepalive every 15s
                if time.Since(lastFlush) > 15*time.Second {
                    fmt.Fprint(c.Response(), ": keepalive\n\n")
                    flusher.Flush()
                }
            }
        }
    }
    return nil
}
```

### 2.3 APL query builder

```go
type aplQuery struct {
    sandboxID string
    since     time.Time
    until     time.Time
    text      string   // free-text search (escaped)
    sources   []string // filter by source
    limit     int
    tail      bool
}

func buildAPL(sandboxID string, qs url.Values) aplQuery {
    // parse query params; defaults:
    //   tail=true, since=sandbox.created_at, until=now, limit=1000
    //   text="", sources=[]  (no filter)
}

func (q aplQuery) historical() string {
    var b strings.Builder
    fmt.Fprintf(&b, "['oc-sandbox-logs']\n")
    fmt.Fprintf(&b, "  | where sandbox_id == %q\n", q.sandboxID)
    fmt.Fprintf(&b, "  | where _event_source startswith \"worker-\"\n")
    if !q.since.IsZero() {
        fmt.Fprintf(&b, "  | where _time >= datetime(%q)\n", q.since.Format(time.RFC3339Nano))
    }
    if !q.until.IsZero() {
        fmt.Fprintf(&b, "  | where _time <= datetime(%q)\n", q.until.Format(time.RFC3339Nano))
    }
    if q.text != "" {
        fmt.Fprintf(&b, "  | where line contains %q\n", q.text)
    }
    if len(q.sources) > 0 {
        // build  | where source in ('a','b')
    }
    fmt.Fprintf(&b, "  | sort by _time asc\n")
    fmt.Fprintf(&b, "  | limit %d\n", q.limit)
    return b.String()
}
```

**Important:** all string interpolation uses `%q` (Go's quoted
string). APL injection isn't a credential-leak risk (the token is
read-only and dataset-scoped), but it could let a user craft a
query that returns *another sandbox's* events by overriding the
`sandbox_id` filter. The `%q` rules combined with the `where
sandbox_id == "..."` first-line filter should be airtight, but
write a unit test that feeds malicious `?q=` values and confirms
the resulting APL still has the sandbox filter.

### 2.4 Axiom client

Tiny package (could live in `internal/axiom/` or stay inline in
`sandbox_logs.go` for v1):

```go
func axiomQuery(ctx context.Context, apl string) ([]Event, error) {
    body, _ := json.Marshal(map[string]any{
        "apl":       apl,
        "startTime": ..., // pulled from APL; Axiom requires both
        "endTime":   ...,
    })
    req, _ := http.NewRequestWithContext(ctx, "POST",
        "https://api.axiom.co/v1/datasets/_apl/query",
        bytes.NewReader(body))
    req.Header.Set("Authorization", "Bearer " + cfg.AxiomQueryToken)
    req.Header.Set("Content-Type", "application/json")
    resp, err := httpClient.Do(req)
    // parse resp.matches[].data into []Event
}
```

The Axiom APL query endpoint is `/v1/datasets/_apl/query` (note
`_apl`, not the dataset name — the dataset is referenced inside
the APL string itself). Confirm exact shape against current Axiom
docs at implementation time.

### 2.5 Router wiring

In `internal/api/router.go`, alongside other dashboard routes:

```go
e.GET("/api/sandboxes/:sandboxId/logs", s.sandboxLogs,
    auth.PGAPIKeyMiddleware(s.db))
```

Use the same auth middleware that protects the existing dashboard
sandbox routes.

### 2.6 Verification checklist

- [ ] `GET /api/sandboxes/:id/logs` returns 404 for sandbox not
      in caller's org
- [ ] Returns 200 + SSE for own sandbox
- [ ] Initial batch contains historical events (verified against
      direct Axiom query)
- [ ] Live tail emits new events within ~2s of ingest
- [ ] `?q=foo` filters server-side
- [ ] `?source=exec_stdout` filters server-side
- [ ] `?tail=false` returns batch and closes
- [ ] APL injection unit test passes (malicious `q` cannot bypass
      sandbox-id filter)
- [ ] Keepalive comments arrive every 15s on idle streams

---

## Phase 3 — UI Logs tab

Goal: `web/src/pages/SessionDetail.tsx` gains a Logs tab next to the
existing Terminal button. Live tail by default; search box; source
filter chips; time-range picker.

### 3.1 Files

- Modify: `web/src/pages/SessionDetail.tsx` (add tab)
- New: `web/src/components/LogsTab.tsx` (the tab body)
- Possibly new: `web/src/components/LogStreamView.tsx` (the
  virtualised list — could be inline if small)
- Modify: `web/src/api/client.ts` (one new method)

### 3.2 API client

```ts
// web/src/api/client.ts
export function streamSandboxLogs(
    sandboxId: string,
    opts: { tail?: boolean; q?: string; source?: string; since?: string }
): EventSource {
    const url = new URL(`/api/sandboxes/${sandboxId}/logs`, location.origin);
    if (opts.tail !== undefined) url.searchParams.set("tail", String(opts.tail));
    if (opts.q) url.searchParams.set("q", opts.q);
    if (opts.source) url.searchParams.set("source", opts.source);
    if (opts.since) url.searchParams.set("since", opts.since);
    return new EventSource(url.toString());
}
```

### 3.3 Component sketch

```tsx
// web/src/components/LogsTab.tsx
export function LogsTab({ sandboxId }: { sandboxId: string }) {
    const [events, setEvents] = useState<LogEvent[]>([]);
    const [query, setQuery] = useState("");
    const [source, setSource] = useState<string | undefined>();
    const [paused, setPaused] = useState(false);

    const debouncedQuery = useDebounce(query, 200);

    useEffect(() => {
        if (paused) return;
        const es = streamSandboxLogs(sandboxId, {
            q: debouncedQuery, source, tail: true,
        });
        setEvents([]); // clear on filter change
        es.onmessage = e => {
            const ev = JSON.parse(e.data) as LogEvent;
            setEvents(prev => [...prev, ev]);
        };
        return () => es.close();
    }, [sandboxId, debouncedQuery, source, paused]);

    return (
        <div className="logs-tab">
            <header>
                <SearchBox value={query} onChange={setQuery} />
                <SourceFilter value={source} onChange={setSource} />
                <PauseToggle value={paused} onToggle={() => setPaused(!paused)} />
            </header>
            <LogStreamView events={events} />
        </div>
    );
}
```

### 3.4 Stream view

Virtualised list (use `react-window` if not already a dep —
otherwise lift the same approach `AgentDetail.tsx` uses for its
existing Logs tab pattern). Each row:

- Time (relative or absolute, toggle)
- Source chip (color-coded: stdout neutral, stderr red-tinged,
  var_log gray, agent purple)
- Line content (monospace, truncated with click-to-expand for long
  lines)
- For `exec_*` events: small badge with truncated `command` and a
  link/click that scrolls to the matching `exec_id` window

Auto-scroll-to-bottom unless the user scrolls up. Standard pattern:
track `userIsAtBottom` state, only auto-scroll when true.

### 3.5 Verification checklist

- [ ] Logs tab appears in SessionDetail next to Terminal
- [ ] Live events arrive without page refresh
- [ ] Search filters server-side (verify Axiom query in network tab)
- [ ] Source filter chips work
- [ ] Pause/resume freezes the stream client-side
- [ ] Page doesn't OOM after 30 min of streaming (virtualised list
      working)
- [ ] EventSource reconnects on transient network blips

---

## Phase 4 — turn it on

Once phases 0-3 are reviewed and merged:

1. Set the three Axiom secrets in prod's vault.
2. Deploy worker (picks up `AXIOM_INGEST_TOKEN`).
3. Existing sandboxes don't ship — they have the old agent binary
   without the forwarder. **Only sandboxes created after the
   worker deploy** will start shipping. This is fine and expected.
4. Watch for one week:
   - Axiom ingest volume per day
   - Axiom query latency p50 / p95
   - Any errors in the agent forwarder log
   - Any user-reported issues with the Logs tab
5. After one week, decide on:
   - Rate cap (likely 1MB/s ingest per sandbox, but informed by
     observed volumes)
   - Retention (whatever makes cost sense)
   - Per-org opt-out (if any customer asks for it)
   - Whether to extend ingest-token rotation discipline

## Open questions / decisions still pending

These are not blocking phases 0-3 but should be answered before
phase 4 broad turn-on:

- **Cost model.** Axiom charges per GB ingested. Need a back-of-
  envelope from the GB-hours data we already have to predict
  monthly Axiom bill at current sandbox volume. If it's >$X/month
  (decide threshold), rate-cap before turning on.
- **Retention default.** Stick with Axiom's default for v1; revisit
  once we see costs.
- **What about sandboxes that run for weeks/months?** The forwarder
  is in-memory; we never spool to disk. Long-lived sandboxes
  with bursty activity should be fine. Long-lived sandboxes with
  *steady high* output volume could mask drops if the user only
  occasionally checks the Logs tab. Acceptable for v1.
- **Defence-in-depth on `_event_source`.** Design doc proposes
  filtering on `_event_source startswith "worker-"` server-side
  to defeat trivial spoofing. Need to confirm that Axiom's `_event_source`
  field is indeed reliable / whether we need to use
  `_ingester` or a different metadata field. Verify at phase 1 time.
- **Should we capture egress proxy access logs?** The secrets
  proxy at `internal/secretsproxy/proxy.go` already logs every
  outbound request (verify). Teeing that to Axiom under
  `source: "egress_proxy"` would surface the highest-leverage
  failure mode (network errors that look like app bugs). Out of v1
  scope but a strong tier-2 add.
- **Should we capture user-launched ad-hoc processes?** Not via
  this design. The auditd-based process-watcher I sketched in
  prior conversation is a separate tier-3 enhancement; scope it
  separately if/when there's demand.

## Verification: end-to-end

Once all four phases land, the full test:

1. Boot a fresh sandbox.
2. Open the dashboard → Sessions → that sandbox → Logs tab.
3. From the SDK or CLI, run a few execs:
   `echo "hello"; ls -la /tmp; curl -sS https://example.com`.
4. Inside the sandbox, write to `/var/log/test.log`.
5. Within ~2s, all four lines should appear in the Logs tab
   in the right order with the right source colours.
6. Type "hello" into the search box; only the first row remains.
7. Clear search; live tail resumes.
8. Click the `curl` row; tab scrolls to the EOF event showing
   `exit_code: 0`.

If all of that works, we're done with v1 and can start the
phase-4 turn-on watch.

## Status log (fill in as we go)

- *2026-05-05* — design doc merged-pending (PR #226 draft).
  Implementation plan written. No code changes yet. Phase 0 is
  next.
