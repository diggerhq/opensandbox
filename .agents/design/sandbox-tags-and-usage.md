# Sandbox tags and usage visibility

API-only surface for attributing sandbox spend to customer-defined
groupings (team, env, customer, etc.) and drilling down to individual
sandboxes. GB-seconds, not dollars — Stripe stays the pricing source
of truth.

## Why: today's billing is org-rollup only

`GetOrgUsage` aggregates `sandbox_scale_events` for the whole org and
reports totals per memory tier to Stripe. There is no way for a
customer to see which sandboxes or which of their groupings drove
the bill. Dashboards, chargebacks, and "why is spend up this month"
investigations are all impossible without this surface.

Dashboard/CLI come later. v1 is API only.

## Tags live in a new table, not on session metadata

First instinct was to reuse `sandbox_sessions.metadata JSONB` — a
user-settable column that is persisted but never queried. On closer
inspection the semantics don't line up: **a sandbox owns many
`sandbox_sessions` rows** (one per start/resume), and each captures
the metadata passed to THAT create call. The column is "per-session
metadata at creation", not "sandbox-level tags." Reusing it would
silently require `ORDER BY started_at DESC LIMIT 1` on every tag
query to pick a winner, and PATCHing it would overwrite an
otherwise-immutable create-time snapshot. Wrong shape.

Instead:

```sql
CREATE TABLE sandbox_tags (
  org_id      UUID        NOT NULL,
  sandbox_id  TEXT        NOT NULL,
  key         TEXT        NOT NULL,
  value       TEXT        NOT NULL,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (sandbox_id, key)
);
CREATE INDEX ON sandbox_tags (org_id, key, value);
```

Row-per-tag keeps grouping SQL clean (`LEFT JOIN sandbox_tags t ON
t.sandbox_id = e.sandbox_id AND t.key = 'team' GROUP BY t.value`),
makes tag-count limits trivial to enforce, and cleanly separates
"current tags" (mutable) from session metadata (historical snapshot).

`sandbox_sessions.metadata` is left intact — still captured at create
time, still unread, still round-trips. No SDK surface for it changes.

## Attribution is live: retagging rewrites history

Queries join live `sandbox_tags`. A retag changes all historical
attribution for that sandbox going forward. Fine for ops and
"where is the money going this month"; hazardous for chargebacks.

v1 does not snapshot tags onto scale events. Mitigation in the
response: every sandbox-level payload carries `tagsLastUpdatedAt`
(= max `updated_at` across the sandbox's tags) so dashboards can
annotate "tags edited on …" and stakeholders don't mistake
retagging for spend movement.

If stable historical attribution ever becomes required (chargeback
automation, invoice reconciliation), the upgrade path is a
`sandbox_tag_changes` audit table or inline snapshotting onto scale
events. Out of scope for v1.

## Unit is GB-seconds — identical math to the Stripe pipeline

Stripe owns pricing — rates vary by plan, trial, grandfathering.
A server-computed dollar figure would drift from the invoice. We
surface the physical quantities the invoice is computed from, using
**bit-for-bit identical math to `GetOrgUsage` and
`DiskOverageGBSeconds`:**

- `memoryGbSeconds = SUM(memory_mb/1024 × EXTRACT(EPOCH FROM duration))`
- `diskOverageGbSeconds = SUM(max(0, disk_mb-20480)/1024 × duration)`
- Active events (`ended_at IS NULL`) use `COALESCE(ended_at, now())`
  — running sandboxes show live accrual.
- Float math throughout (no rounding). Matches the existing Stripe
  pipeline so per-tag sums always reconcile to the org total.
- CPU not exposed: deterministic from memory tier (1 vCPU per 4GB).

A reconciliation test (see Implementation notes) asserts
`Σ by-sandbox = Σ by-tag + untagged = GetOrgUsage(org)` within float
epsilon. If `GetOrgUsage` math ever changes, this code changes in
lockstep.

## Data freshness: fresh on clean shutdown, reconciled otherwise

Scale events close on two paths:

- **Clean transitions** via `UpdateSandboxSessionStatus` — immediate.
- **Worker heartbeat reconciliation** via `ReconcileWorkerSessions`
  — closes zombie rows on worker crash or sandbox death.

So "fresh to the minute" holds for clean shutdowns; worst case is
reconciliation-lagged (sandbox dies, usage keeps accruing until the
next heartbeat sweep closes the row). Call it out on the endpoint
reference.

## Surface

```
GET /usage                          # aggregator (dimensions as data)
GET /tags                           # org-wide tag-key discovery
GET /sandboxes/{id}/usage           # per-sandbox drilldown
GET /sandboxes/{id}/tags            # read current tags
PUT /sandboxes/{id}/tags            # full-replace tag set
```

Plus an additive change: `GET /sandboxes` and `GET /sandboxes/{id}`
responses gain `tags` and `tagsLastUpdatedAt`.

### `GET /usage` — aggregator

Dimensions are data, not URL segments. Adding `status`, `template`,
`region` later costs one string in `groupBy`, not a new route. One
dimension at a time in v1 — multi-dim extends via comma separation
(`groupBy=tag:team,tag:env`) additively.

Alternatives considered: narrow per-dimension routes
(`/usage/by-sandbox`, `/usage/by-tag`) privilege tags in URLs and
grow a route per future dimension; a full composable DSL
(`POST /usage/query` with filter trees, nested aggs) reinvents a
query engine before there's multi-dim demand.

Query parameters:

| Param | Values | Notes |
|---|---|---|
| `groupBy` | `sandbox`, `tag:<key>` | Required. |
| `filter[<dim>]` | any | Repeatable, AND-ed. `filter[tag:team]=` (empty) = "key absent". |
| `from`, `to` | ISO8601 | Default: last 30 days. Max window: 90 days. |
| `sort` | `-memoryGbSeconds` (default), `-diskOverageGbSeconds` | Secondary sort by `sandboxId` / `tagValue` for cursor determinism. |
| `limit`, `cursor` | default 50, max 500 | Opaque cursor. |

Response, `groupBy=tag:team`:

```json
{
  "from": "...", "to": "...",
  "groupBy": "tag:team",
  "total":    { "memoryGbSeconds": 19000, "diskOverageGbSeconds": 340 },
  "untagged": { "memoryGbSeconds": 1000,  "diskOverageGbSeconds":  20,
                "sandboxCount": 2 },
  "items": [
    { "tagKey": "team", "tagValue": "payments",
      "memoryGbSeconds": 8000, "diskOverageGbSeconds": 120,
      "sandboxCount": 12 },
    { "tagKey": "team", "tagValue": "growth",
      "memoryGbSeconds": 4000, "diskOverageGbSeconds":  50,
      "sandboxCount":  5 }
  ],
  "nextCursor": null
}
```

`untagged` is a sibling field, not a null-valued item — typed SDKs
don't null-check items. `Σ items + untagged = total` by construction.

Response, `groupBy=sandbox`:

```json
{
  "from": "...", "to": "...",
  "groupBy": "sandbox",
  "total": { "memoryGbSeconds": 20000, "diskOverageGbSeconds": 360 },
  "items": [
    { "sandboxId": "sbx_abc", "alias": "my-agent",
      "status": "running",
      "tags": { "env": "prod", "team": "payments" },
      "tagsLastUpdatedAt": "2026-04-19T14:02:00Z",
      "memoryGbSeconds": 8000, "diskOverageGbSeconds": 120 }
  ],
  "nextCursor": null
}
```

No `sandboxCount` per row (always 1). `status` values reconcile with
the existing state machine: `running | hibernated | stopped | error`.

### `GET /tags` — discovery

```json
{ "keys": [
  { "key": "team", "sandboxCount": 17, "valueCount": 4 },
  { "key": "env",  "sandboxCount": 23, "valueCount": 3 }
] }
```

Backed by `SELECT key, COUNT(DISTINCT sandbox_id),
COUNT(DISTINCT value) FROM sandbox_tags WHERE org_id = $1 GROUP BY
key`, org-scoped.

### `GET /sandboxes/{id}/usage` — drilldown

```json
{
  "sandboxId": "sbx_abc", "alias": "my-agent",
  "status": "running",
  "from": "...", "to": "...",
  "memoryGbSeconds": 12345.6, "diskOverageGbSeconds": 789.0,
  "tags": { "env": "prod", "team": "payments" },
  "tagsLastUpdatedAt": "2026-04-19T14:02:00Z",
  "firstStartedAt": "...", "lastEndedAt": null
}
```

Works for torn-down sandboxes — `sandbox_scale_events` and
`sandbox_tags` persist.

### `GET /sandboxes/{id}/tags`, `PUT /sandboxes/{id}/tags`

```
GET → { "tags": { "env": "prod", "team": "payments" },
        "tagsLastUpdatedAt": "2026-04-19T14:02:00Z" }

PUT body: { "env": "staging", "team": "growth" }
→ full replace, returns new state. `{}` clears all tags.
```

Deliberately narrow. A broader `PATCH /sandboxes/{id}` would invite
feature creep (alias, memory, etc.), each with its own semantic
issues. Scoping to `/tags` keeps that door shut.

### Validation (applied on PUT)

- ≤ 50 tag keys per sandbox.
- Key: 1–128 chars, `[A-Za-z0-9_.-]` plus `:` for user namespacing.
- Value: 0–256 chars, UTF-8.
- `oc:` key prefix reserved for future system-set tags — PUT with
  such keys returns 400.

## Explicit non-goals for v1

- **Dollars.** Stripe owns invoicing.
- **Time series / bucketing.** Drops in as `?interval=1d` on `/usage`.
  Additive, non-breaking.
- **Multi-dim group-by** (`groupBy=tag:team,tag:env`). Extends
  `groupBy` to comma-separated. Additive, non-breaking.
- **Filter trees** (OR / NOT / nested). Flat AND covers the real
  flows.
- **Historical tag snapshotting / audit log.** `tagsLastUpdatedAt` is
  the v1 mitigation; upgrade path is a separate audit table.
- **`GET /sandboxes?tag=k:v` filter.** Obvious follow-up on the same
  `sandbox_tags` backend; tracked as a separate PR.
- **CSV / bulk export.** Paginated JSON covers it.
- **CLI / dashboard.** API only for v1.

## Implementation notes

- **Migration**: one migration creating `sandbox_tags` (table +
  composite index). No change to existing tables.
- **Query builder**: one function
  `(orgID, window, groupBy, filters, sort, cursor) → rows`, compiled
  to SQL joining `sandbox_scale_events` to `sandbox_tags`. Reuse the
  duration / GB-second math from `GetOrgUsage` /
  `DiskOverageGBSeconds` — do not reimplement.
- **Handlers**: new `internal/api/usage.go` (`/usage`, `/tags`,
  `/sandboxes/{id}/usage`); new `internal/api/sandbox_tags.go` (GET
  and PUT on `/sandboxes/{id}/tags`). Wire in
  `internal/api/router.go` inside the authed group.
- **Response additions**: `GET /sandboxes` list and `GET
  /sandboxes/{id}` gain `tags` + `tagsLastUpdatedAt`. Additive.
- **Tenancy**: every query scopes on `auth.GetOrgID(c)` — same as
  every existing handler. No sub-org visibility; an org-scoped API
  key sees the whole org's spend (pre-existing model).
- **Query guardrails**: reject `to - from > 90d`; reject `limit >
  500`; handler-level timeout on `/usage` (10s suggested).
- **Tests**:
  - Query builder across each `groupBy × filter` combination.
  - **Reconciliation test**: assert `Σ by-sandbox = Σ by-tag +
    untagged = GetOrgUsage(org)` within float epsilon.
  - Integration tests for top-N sandboxes, group-by-team,
    drilldown.
  - PUT validation tests (size limits, reserved prefix, malformed
    keys).
- **SDKs**: TS + Python SDK stubs for the new endpoints. Convenience
  wrappers (`usage.byTag`, `usage.bySandbox`) designed in the SDK
  layer, not the server.
- **Docs**: reference entries under `docs/api-reference/` per
  endpoint; wire into `docs/mint.json`.

## Open questions for implementation

- `firstStartedAt` / `lastEndedAt` on single-sandbox response: derive
  from first/last scale event, or from `sandbox_sessions.started_at`?
  Session-level is probably more intuitive but needs a check for
  sandboxes with multiple sessions.
- Sandbox-destroy behavior for tag rows: tombstone or leave. Leaving
  preserves the drilldown for historical reports but slightly
  overstates `/tags` counts; tombstoning flips the trade-off. Lean
  leave; revisit if orgs complain.
- Whether to support a merge-mode (`PUT ...?mode=merge`) or keep
  strict full-replace and force GET+PUT for partial updates.
