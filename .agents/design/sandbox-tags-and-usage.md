# Sandbox tags and usage visibility

API-only surface for attributing sandbox spend to the customer's own
groupings (team, env, customer, etc.) and drilling down to individual
sandboxes. GB-seconds, not dollars — Stripe stays the pricing source
of truth.

## Why: today's billing is org-rollup only

`GetOrgUsage` aggregates `sandbox_scale_events` for the whole org and
reports totals per memory tier to Stripe. There is no way for a customer
to see which sandboxes or which of their groupings drove the bill.
Dashboards, chargebacks, and "why is spend up this month" investigations
are all impossible without this surface.

Dashboard/CLI come later. v1 is API only.

## Tags reuse existing `metadata` JSONB — no new field

`sandbox_sessions.metadata JSONB` has been user-settable since migration
001, via `POST /sandboxes` body and `oc sandbox create --metadata k=v`.
Audit of the codebase: the field is persisted and round-tripped on read,
but no handler filters, groups, or otherwise acts on it. It's
write-only decoration today.

We rescope metadata as tags rather than adding a parallel `Tags` field.

- No SDK break — existing `--metadata` flag and `SandboxConfig.Metadata`
  keep working and now carry real semantics.
- No semantic overlap users have to disambiguate ("when do I use tags vs
  metadata?").
- The only risk is retroactive bucketing of free-form data users may
  have stuffed in metadata. Communicated in release notes; users can
  clear it via the new PATCH.

Implications:

- `PATCH /sandboxes/{id}` is new — metadata is create-only today.
- Add GIN index on `sandbox_sessions.metadata jsonb_path_ops`.
- Current tags attribute all historical spend. Retagging re-buckets.
  Simpler than snapshotting tags onto every scale event; revisit only
  if stable historical attribution becomes a real ask.

## Unit is GB-seconds (memory + disk overage)

Stripe owns pricing — rates vary by plan, trial, grandfathering. A
server-computed dollar figure would be misleading and would silently
drift from the invoice. We surface the physical quantities:

- `memoryGbSeconds = memory_mb/1024 × duration_s`, summed over
  `sandbox_scale_events` in the window.
- `diskOverageGbSeconds = max(0, disk_mb-20480)/1024 × duration_s`.
  Matches what's actually billed (`DiskOverageGBSeconds` in
  `internal/billing/pricing.go`), not raw disk — raw disk doesn't tie
  to anything.
- Active events count, capped at `now()` — running sandboxes show live
  accrual, which is what users expect from a spend view.
- CPU not exposed: deterministic from memory (1 vCPU per 4GB), so
  memory GB-seconds already captures it.

## One aggregator, one discovery endpoint, one drilldown, one PATCH

```
GET   /usage                        # aggregator, dimensioned
GET   /tags                         # tag-key discovery
GET   /sandboxes/{id}/usage         # single-sandbox drilldown
PATCH /sandboxes/{id}               # tag updates (body: metadata)
```

### Design middle: dimensions as data, not routes

We explicitly considered two poles before landing here:

- **Narrow endpoints** (`/usage/by-sandbox`, `/usage/by-tag`) — baked in
  one dimension per route. Adding `status`, `template`, `region` later
  means new routes. `/by-tag` privileges tags over other dimensions.
- **Composable query DSL** (`POST /usage/query` with
  `{metrics, groupBy, filter, orderBy}`) — ELK/Prometheus/Cost Explorer
  shape. Maximum flexibility, but reinvents a query engine for a
  problem that doesn't yet have multi-dim demand.

The middle: **one REST aggregator with `groupBy` and `filter[]` as
query params**. Dimensions are strings from a small known set
(`sandbox`, `tag:<key>`, future `status`, `template`, `region`). Adding
a dimension later costs one string, not a new route. No query body, no
filter tree, no nested aggregations — the shape of Stripe's list
endpoints or GitHub's search.

### `GET /usage` — aggregator

Query parameters:

| Param | Values | Notes |
|---|---|---|
| `groupBy` | `sandbox`, `tag:<key>` | Required. One dimension. |
| `filter[<dim>]` | any | Repeatable, AND-ed. `filter[tag:team]=` (empty) = "key absent". |
| `from`, `to` | ISO8601 | Defaults to last 30 days. |
| `sort` | `-memoryGbSeconds` (default), `-diskOverageGbSeconds`, `sandboxCount` | |
| `limit`, `cursor` | default 50, max 500 | Opaque cursor. |

Response shape when `groupBy=tag:team`:

```json
{
  "from": "2026-03-23T00:00:00Z",
  "to":   "2026-04-22T00:00:00Z",
  "groupBy": "tag:team",
  "total": { "memoryGbSeconds": 19000, "diskOverageGbSeconds": 340 },
  "items": [
    { "tag:team": "payments", "memoryGbSeconds": 8000,
      "diskOverageGbSeconds": 120, "sandboxCount": 12 },
    { "tag:team": "growth",   "memoryGbSeconds": 4000,
      "diskOverageGbSeconds":  50, "sandboxCount":  5 },
    { "tag:team": null,       "memoryGbSeconds": 1000,
      "diskOverageGbSeconds":  20, "sandboxCount":  2 }
  ],
  "nextCursor": null
}
```

The `null` row is the untagged bucket. Sum of groups + null row = total
— dashboards can reconcile.

When `groupBy=sandbox`, items include `sandboxId`, `alias`, `status`
(`running|hibernated|stopped|destroyed`), `tags` (the metadata map), and
the metrics.

### `GET /tags` — discovery

```json
{ "keys": [
  { "key": "team", "sandboxCount": 17, "valueCount": 4 },
  { "key": "env",  "sandboxCount": 23, "valueCount": 3 }
] }
```

Powers "Group by ___" selectors. Backed by
`SELECT jsonb_object_keys(metadata), COUNT(...) FROM sandbox_sessions
WHERE org_id = $1 GROUP BY 1`, org-scoped.

### `GET /sandboxes/{id}/usage` — drilldown

```json
{
  "sandboxId": "sbx_abc",
  "alias": "my-agent",
  "from": "...", "to": "...",
  "memoryGbSeconds": 12345.6,
  "diskOverageGbSeconds": 789.0,
  "tags": { "env": "prod", "team": "payments" },
  "status": "running",
  "firstStartedAt": "...",
  "lastEndedAt": null
}
```

Works for destroyed sandboxes too — `sandbox_scale_events` persists
after sandbox teardown.

### `PATCH /sandboxes/{id}` — tag update

Body: `{ "metadata": { "team": "growth", "customer": null } }`. Merge
semantics: `null` value deletes the key, missing keys are left alone.
Scope of other PATCHable fields is out of this design.

## Data freshness: live to the minute

Running sandboxes are summed with `COALESCE(ended_at, now())`, so
scale-event-derived metrics (memory + disk GB-seconds) are fresh to the
minute. Sample-derived metrics (if any are added later) would lag by
the 5-minute flush interval. Document this in the endpoint reference.

## Explicit non-goals for v1

- **Dollars.** Stripe owns invoicing.
- **Time series / bucketing.** Will drop in as `?interval=1d` on the
  same endpoint, same response shape plus per-bucket rows. Not now.
- **Multi-dimensional group-by** (`groupBy=tag:team,tag:env` cross-tab).
  Extends `groupBy` to comma-separated later — additive, non-breaking.
- **Filter trees** (OR/NOT/nested). Flat AND covers the real use cases
  we've imagined. Add only on demand.
- **Per-event tag snapshotting.** Current tags drive all history.
- **Tag audit log.**
- **CSV / bulk export.** Paginated JSON.
- **Dashboard, CLI.** API only for v1.

## Implementation notes

- **Migration**: one migration adding
  `CREATE INDEX ... ON sandbox_sessions USING GIN (metadata jsonb_path_ops)`.
  No schema change.
- **Query builder**: one function
  `(orgID, window, groupBy, filters, sort, cursor) → rows` compiling to
  SQL that joins `sandbox_scale_events` to `sandbox_sessions` for tag
  access. Reuse the duration/GB-second math from `GetOrgUsage` and
  `DiskOverageGBSeconds`.
- **Handlers**: new `internal/api/usage.go` for `/usage`, `/tags`, and
  `/sandboxes/{id}/usage`; extend `internal/api/sandbox.go` for PATCH.
  Wire in `internal/api/router.go` inside the authed group.
- **Tenancy**: every query scopes on `auth.GetOrgID(c)` — same pattern
  as existing handlers.
- **Tests**: unit-test the query builder for each `groupBy × filter`
  combo using fixtures in `sandbox_scale_events`; integration tests for
  the three core flows (top-N sandboxes, group-by-team,
  per-sandbox-drilldown).

## Open questions for implementation

- `firstStartedAt` / `lastEndedAt` on single-sandbox response: derive
  from first/last scale event, or from `sandbox_sessions.started_at`?
  Session-level is probably more intuitive but needs a check for
  sandboxes with multiple sessions.
- Empty-filter "missing key" convention
  (`filter[tag:team]=`) — document once; reuse on both `groupBy=sandbox`
  and drilldowns so the untagged bucket from `GET /usage?groupBy=tag:team`
  is always addressable.
- Org-level API key sandboxes (no user) — `GetSandboxOwner` handles them
  for billing; confirm the new queries are consistent.
- SDK convenience wrappers (`usage.byTag(key)`, `usage.bySandbox()`) —
  design in the SDK layer, not the server.
