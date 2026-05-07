# Cohort usage analysis

How to reconstruct *who* drove a usage figure on a given day, what
they did, and where they dropped off — using only the production
databases. Pattern arose from investigating a 10K GB-hour spike that
turned out to be ~30 users abandoning the agent product after hitting
provisioning errors. Same recipe applies to any "who's burning the
compute" or "where's the funnel breaking" question.

## When to use this

The question pattern: a metric (usage, signups, errors) jumped on a
given day, and you need to know which users contributed, what they
were doing, and what stopped them. Output is a per-user timeline
spreadsheet that a non-engineer can read.

Not for: aggregate billing reconciliation (use `sandbox_scale_events`
joined to Stripe meter events), real-time alerting, or anything
requiring sub-minute resolution. Sample resolution is 60s; agent
events are sparse and append-only.

## Two databases, joined on user ID

Two separate Postgres instances hold complementary state:

- **OpenComputer DB** (Azure-hosted, behind SSH bastion): orgs,
  users, sandboxes, scale events, usage samples, preview URLs,
  secret stores, hibernations. Connection details and tunnel
  pattern: see `~/.claude` memory `reference_prod_debugging.md`,
  or the `OPENSANDBOX_DB_*` block in `~/Digger/gstack/opencomputer/.env`.
- **sessions-api DB** (Xata-hosted, public reach with credentials):
  agents, instances, sessions, agent_events, agent_operations.
  Connection string: `DATABASE_URL` in
  `~/Digger/gstack/sessions-api/.env`. No bastion needed.

The join key is **OC `users.id`** (UUID). In sessions-api,
`agents.owner_id` carries that same UUID. Three things this is *not*:

- not `orgs.id` — agents are owned per-user, not per-org
- not `workos_user_id` — that lives on `users.workos_user_id`,
  unrelated
- not `app_users.id` in sessions-api — that table is mostly empty
  test data, ignore it for cohort joins

`sessions-api.app_users` looks tempting because of the `email` column,
but it has on the order of ten rows total, all of which are
smoke-test or pivot accounts. Don't join via email.

## Identify the cohort

Step one is "who's in the relevant set." For a usage spike, the
canonical filter is "every org that emitted at least one
`sandbox_usage_samples` row in the window." This is the closed
universe — `usage_collector` writes one sample per running sandbox
per 60s, and every billable second flows through this table, so
nothing else can contribute.

```sql
SELECT DISTINCT us.org_id
FROM sandbox_usage_samples us
WHERE us.sampled_at >= '<window_start>'
  AND us.sampled_at < '<window_end>';
```

Resolve to users and emails via `users JOIN orgs`. Decide whether
you want internal accounts in or out — internal here means
`orgs.name IN ('opencomputer', 'diggerhq')` plus any user whose
email ends in `@digger.dev`. Amplitude dashboards filter internal
out, so for parity with what stakeholders see, exclude them.

## Reconciliation: prove the cohort accounts for the metric

The first thing to do after defining the cohort is verify it sums
to the metric you're investigating. Without this step the analysis
is suggestive, not conclusive — there could be hidden contributors
or double-counting.

The key property of `sandbox_usage_samples` is that the universe of
contributors is *enumerable*: a closed `SELECT DISTINCT org_id`.
Per-org GB-hour sums therefore reconcile to the total exactly:

```sql
SELECT
  bucket,
  count(DISTINCT org_id) AS orgs,
  ROUND(SUM(memory_mb / 1024.0 * 60.0) / 3600.0, 1) AS gb_hours
FROM (
  SELECT
    us.org_id,
    us.memory_mb,
    CASE
      WHEN o.name IN ('opencomputer', 'diggerhq') THEN 'internal'
      WHEN EXISTS (SELECT 1 FROM users u
                   WHERE u.org_id = o.id AND u.email LIKE '%@digger.dev')
        THEN 'internal'
      ELSE 'external'
    END AS bucket
  FROM sandbox_usage_samples us
  JOIN orgs o ON o.id = us.org_id
  WHERE us.sampled_at >= '<window_start>' AND us.sampled_at < '<window_end>'
) x
GROUP BY bucket;
```

Sanity checks:

1. `count(DISTINCT org_id)` from the unfiltered samples query
   equals the sum of bucket org counts (no orgs leak through).
2. Sum of bucket GB-hours equals the total from the unfiltered query.
3. No orphaned org_ids:
   ```sql
   SELECT count(*) FROM (
     SELECT DISTINCT org_id FROM sandbox_usage_samples
     WHERE sampled_at >= '<window_start>' AND sampled_at < '<window_end>'
   ) a WHERE NOT EXISTS (SELECT 1 FROM orgs o WHERE o.id = a.org_id);
   ```
   Should be `0`.

Once these three hold, the cohort *is* the metric, by construction.
Don't move on until they do.

## Per-user event collection

For each user in the cohort, pull every high-signal event that maps
to either intentional usage or a discrete failure. Skip everything
that's noise (heartbeats, periodic samples themselves, keepalives).
The list below is what worked for the agent-product cohort and
generalises to most spike investigations.

From the OpenComputer DB:

| Source | Event | Signal type |
|---|---|---|
| `orgs.created_at` | signup | start anchor |
| `sandbox_sessions.started_at` / `.stopped_at` | sandbox lifecycle | usage |
| `sandbox_hibernations.hibernated_at` / `.restored_at` | reaper / wake | system action |
| `api_keys.created_at` / `.last_used` | API engagement | usage |
| `sandbox_checkpoints.created_at` | user saved state | strong intent |
| `sandbox_preview_urls.created_at` | user exposed a service | strong intent |
| `secret_store_entries.created_at` | user configured credential | strong intent |

From the sessions-api DB:

| Source | Event | Signal type |
|---|---|---|
| `agents.created_at` | created an agent | intent |
| `instances.created_at` | spun up agent runtime | usage |
| `agent_operations` (`kind`, `status`, `phase`) | lifecycle ops | success/failure |
| `agent_events` (`type`, `phase`, `code`, `message`) | platform-recorded outcomes | failures, recoveries |
| `sessions.created_at` | actual agent invocation | usage (terminal funnel step) |

`agent_operations.kind` values worth tracking: `create_instance`,
`install_package`, `connect_channel`. Statuses: `succeeded`,
`failed`. Phase names line up with `agent_events.phase` so the two
streams cross-reference cleanly.

`agent_events.type` is one of `error`, `warning`, `info`. Errors
are the dropoff causes. `warning` is mostly retry chatter
(`webhook_delivery_retrying`). `info` includes
`webhook_delivery_recovered`, which is a positive signal worth
counting separately from `succeeded` ops.

What *not* to pull as activity proxy:

- `sandbox_usage_samples.cpu_usec` is hardcoded `0` —
  `internal/worker/usage_collector.go` has a `TODO: parse from
  cgroup cpu.stat`. Looks like activity data, isn't.
- `sandbox_usage_samples.memory_bytes` is real RSS but reports the
  guest-OS baseline (~1 GB) for almost any idle sandbox. Useful as
  "is anything happening" proxy, not as activity volume.
- `command_logs` and `pty_session_logs` are zero across all
  agent-product sandboxes. The agent runtime executes via internal
  paths that don't surface to the public Exec/PTY APIs that get
  logged. Don't conclude "user did nothing" from these alone — they
  only catch direct sandbox API usage.

## Pitfalls

These cost real time the first time around. Capturing here so the
next investigation starts ahead.

**`sandbox_sessions.status` is current state, not point-in-time.**
A sandbox shown as `hibernated` was running while it emitted samples.
Falsifiable test: `SELECT count(*) FROM sandbox_usage_samples WHERE
sandbox_id = X AND sampled_at > <hibernated_at>` — returns zero.
Don't filter samples by session status — filter by `sampled_at`.

**`sandbox_scale_events` is undercount-prone.** It's append-only on
explicit scale operations, but several lifecycle paths (notably
restore-from-hibernation cycles, possibly others) skip the write.
On any given day a meaningful fraction of running sandboxes lack
covering scale_event rows. Use `sandbox_usage_samples` as the
source of truth for "what was running when"; use scale_events only
for what billing actually saw.

**Amplitude's number is the external bucket.** A "12K GB-hours"
metric in the DB and "10.8K" in Amplitude is the internal/external
split, not a discrepancy. Reconcile both.

**Sandboxes outlive their visible activity.** Agent product
sandboxes default to 16 GB / 4 CPU / 1 hour timeout, but with no
idle reaper they often persist for days after the user has
abandoned them. Their per-day GB-hour cost is real even when zero
real work is happening inside.

**The `agent:*` secret_store name is the agent product's
fingerprint.** `sandbox_sessions.config->>'secretStore' LIKE
'agent:%'` distinguishes agent-runtime sandboxes from direct
sandbox-API usage (`hyperaide-user-*`, `summun-browser-egress`,
custom names). When the question is product-specific, filter on
this.

**Internal orgs leak in via fresh employee accounts.** The
`opencomputer` and `diggerhq` orgs are obviously internal. New
internal users sometimes sign up with `<name>+test@digger.dev` or
similar and end up under their own personal workspace. The robust
filter is `users.email NOT LIKE '%@digger.dev'`, not just an
org-name allowlist.

## Output formats

Three artefacts, each useful for a different consumer:

- **Long-format event log** (`events.csv`): one row per event,
  columns `email | uid | abs_ts | min_since_signup | source | kind
  | detail`. Pivots well in any spreadsheet. Source for the
  others.
- **Wide funnel** (`funnel.csv`): one row per user, columns are
  per-stage milestones (signup, first sandbox, first agent, first
  instance, first install op, first install ok, first channel ok,
  first session, last activity) plus counts and the user's
  terminal failure phase. Good for "where did each user drop off"
  questions.
- **Calendar timeline** (`cohort_calendar.xlsx`): rows =
  users (sorted by signup), columns = real calendar days, sub-columns
  = first 15m / 15-60m / 1-3h / 3h+ of each day's activity (anchored
  to user's first event of that day). Plus a single "before
  &lt;start&gt;" column. Cells are descriptive multi-line text with
  counts (e.g. "openclaw binary missing on PATH \n install failed
  ×2 \n agent created"); colored red/orange/green/grey by severity;
  thin grey horizontal rules between users; medium grey vertical
  rules between days; first two columns and header rows frozen.

The xlsx is what stakeholders look at. Per-day sub-buckets anchored
to the user's first daily event capture intensity-of-engagement
("they did everything in 15 minutes vs spread across hours") which
plain-time bins miss.

## Reproducing the analysis

Concrete recipe. Adjust the date window and the cohort filter.

### 1. Open the OC tunnel

```bash
ssh -i ~/.ssh/opensandbox-shared -o ExitOnForwardFailure=yes \
    -L 15432:$OPENSANDBOX_DB_HOST:5432 -N -f \
    azureuser@$OPENSANDBOX_DB_BASTION_HOST
```

`$OPENSANDBOX_DB_HOST` and `$OPENSANDBOX_DB_BASTION_HOST` from
`~/Digger/gstack/opencomputer/.env`. Tear down with
`pkill -f "ssh.*15432:.*:5432"`.

### 2. Define the cohort

Build a `uid|email` map of the cohort users to a flat file:

```sql
-- run via psql against OC; emit user_id|email pairs
SELECT u.id::text || '|' || u.email
FROM users u JOIN orgs o ON o.id = u.org_id
WHERE o.id IN (
  SELECT DISTINCT org_id FROM sandbox_usage_samples
  WHERE sampled_at >= '<start>' AND sampled_at < '<end>'
)
AND u.email NOT LIKE '%@digger.dev'
AND o.name NOT IN ('opencomputer', 'diggerhq')
ORDER BY u.email;
```

### 3. Reconcile (sanity-check first)

Run the bucket reconciliation query above. Sum to the headline
metric (Amplitude / dashboard / wherever it came from). Verify
zero orphans. Don't proceed otherwise — the cohort is wrong and
the rest of the analysis will be too.

### 4. Build `events.csv`

Single Python script, two psql connections, one row per event,
parse timestamps, sort by `(email, abs_ts)`, write CSV with columns
`email,uid,abs_ts,min_since_signup,source,kind,detail`.

The query set:

- OC: `users JOIN orgs` for signup; `sandbox_sessions` for
  start/stop; `sandbox_hibernations` for hibernate/restore;
  `api_keys` for created/last_used; `sandbox_checkpoints` for
  saved state; `sandbox_preview_urls` for exposed services;
  `secret_store_entries JOIN secret_stores` for stored credentials.
- sessions-api: `agents` for creation; `instances` for runtime
  starts; `agent_operations` (use `COALESCE(completed_at,
  started_at, created_at)` for the timestamp); `agent_events`
  for errors/warnings/info; `sessions` for actual invocations.

Tab-delimited psql output (`-tAF '\t'`) splits cleanly. NULLs at
the end of a row appear as missing trailing fields; pad to expected
column count.

### 5. Build `cohort_calendar.xlsx` from `events.csv`

Open the events log, group by user, then by calendar date. For
each (user, date), find first event of the day, bucket subsequent
events by relative offset (0-15m / 15-60m / 1-3h / 3h+).
Aggregate per cell as a `Counter` of (label, severity), render as
multi-line text with counts. Colour cells by worst severity in
the cell. Use `openpyxl`; freeze panes at C3; row height ~80pt;
column widths 22 for sub-buckets, 32 for email.

The describe() function — mapping (kind, detail) → (human label,
severity) — is the hand-tuned bit. The agent-product version
shipped with this analysis covers `error/warning/info` events
(with phase-specific labels), all the agent_operations
(succeeded/failed × create_instance/install_package/connect_channel),
and the OC lifecycle/intent events listed above. Extend
`describe()` for new failure modes as they appear; everything else
(severity ordering, cell formatting, layout) doesn't need changes.

Severity ordering for cell colour: `err > warn > ok > neutral >
info`. Cell text orders the same way (errors first, then warnings,
then successes, then lifecycle).

### 6. Verify the xlsx visually

A handful of users at the top by signup date should show signup
followed quickly by a cluster of activity. The right-hand columns
should thin out as users abandoned the product. If the right side
is unexpectedly busy, you have an unkilled-sandboxes long-tail
worth flagging separately. If a user's row is empty across all
columns, double-check the `users.id` → `agents.owner_id` join —
that's the most common mistake.
