# Sandbox tags + per-tag/per-sandbox usage — implementation plan

Working doc. Design is at `.agents/design/sandbox-tags-and-usage.md`,
signed off. This doc is the delivery plan + a flag list of weak or
fuzzy spots in the design that surfaced during code exploration.

## Latest review of the implementation branch

The first review pass drove F3/F8/F9/F10/F11 changes, and those are now
landed on branch: tag storage is org-scoped, all sandbox read paths
hydrate tags, drilldown windows reject `to <= from` and clamp
timestamps, filter semantics are narrowed to one param per dimension,
and `groupBy=sandbox` session hydration is batched.

What still looks outstanding before merge:

**F12. Session lookups are still keyed by `sandbox_id` alone.**
The new tag store/query paths are keyed on `(org_id, sandbox_id)`, but
the feature's ownership and drilldown session reads still call the old
`GetSandboxSession(ctx, sandboxID)` helper from
`internal/api/sandbox_tags.go` and `internal/api/usage.go`. That helper
queries `sandbox_sessions` by `sandbox_id` alone, and this repo still
does not enforce global sandbox-ID uniqueness. Sandbox IDs are generated
as short `sb-xxxxxxxx` strings in multiple create paths. Result: on a
cross-org ID collision, one org can fail `ownsSandbox` for its own
sandbox and `/sandboxes/{id}/usage` can hydrate alias/status from the
wrong org's latest session. Recommendation: add an org-scoped latest
session lookup and use it everywhere this feature touches session state.

**F13. `SandboxUsageWindow` hides real errors from the session-bounds
query.** The first query correctly returns usage numbers or an error.
The second query (session MIN/MAX bounds) currently treats any
`row.Scan(...)` error as "no sessions in window" and returns nil
timestamps. That is too broad: a real DB/context failure on that query
would be downgraded into a partial `200` response with missing
`firstStartedAt` / `lastEndedAt`. Recommendation: only special-case the
empty-window case; surface real scan/query failures normally. A clean
way is to `COALESCE(BOOL_OR(...), false)` and scan into non-pointer
state instead of using scan failure as control flow.

**F14. The reconciliation invariant still lacks an execution-level
test.** `internal/db/usage_query_test.go` now covers builder shape and
some tenancy predicates, but it still does not prove the load-bearing
claim against real Postgres:
`Σ by-sandbox = Σ by-tag + untagged = ExecuteOrgTotals / GetOrgUsage`.
This remains the main safety gap for billing-adjacent math. The
`pgfixture` reconciliation test called out below is still not optional
for sign-off.

**F15. The Python SDK still documents the old duplicate-filter
contract.** The API and TS SDK now agree on "one filter param per
dimension, comma-separated OR values, reject repeated same-key params."
But `sdks/python/opencomputer/usage.py` still says duplicate keys are
preserved and expected. The runtime surface already takes a
`dict[str, str]`, so this is a reader-facing mismatch rather than a
server bug. Recommendation: update the Python helper docstring/comments
to match the narrowed v1 contract before merge.

## Issues flagged from fresh code reading

Listed in rough order of "needs a decision before merge." Numbers are
so reviewers can pick them out.

**F1. `alias` isn't a column; it lives inside `config` JSONB.**
The design shows `alias` in the `groupBy=sandbox` row and in
`GET /sandboxes/{id}/usage`. But `pkg/types/sandbox.go` puts `Alias` on
`SandboxConfig`, and that gets JSON-marshalled into
`sandbox_sessions.config`. There is no top-level `alias` column. Three
options: (a) extract via `config->>'alias'` in the query, (b) promote
`alias` to a real column in this PR (scope creep), (c) drop `alias`
from the response. **Recommendation: (a).** One JSON field access per
row is cheap and matches where alias already lives. Call out in the PR
body.

**F2. "Worker heartbeat reconciliation" is actually worker-startup
reconciliation.** `ReconcileWorkerSessions` is invoked once in
`cmd/worker/main.go:253` at boot. There is no periodic heartbeat
sweep; a silently-dead worker that never restarts leaves scale events
open indefinitely, and the `COALESCE(ended_at, now())` clamp will
happily accrue usage against it forever. This is a pre-existing
billing-pipeline issue, not something we introduce — `GetOrgUsage`
has the same behavior. But the design's freshness section paints it
as "reconciliation-lagged (minutes)," which is not accurate. Either
(a) fix the doc's wording ("lagged until worker restart; same
behavior as the Stripe rollup"), or (b) accept the inaccuracy. Zero
code impact. **Recommendation: (a).**

**F3. `org_id` must be in the keyspace, not just in a lookup index.**
The signed-off design used `PRIMARY KEY (sandbox_id, key)`. Fresh
review says that is unsafe in this repo. Sandbox IDs are currently
generated as `sb-` plus 8 hex chars in multiple create paths
(`internal/api/sandbox.go`, `internal/qemu/manager.go`,
`internal/firecracker/manager.go`) — a short 32-bit space, not a
schema-enforced globally unique namespace. With a `(sandbox_id, key)`
PK and store methods that read/write by `sandbox_id` alone, a single
cross-org ID collision aliases tag state across tenants. Recommendation:
**change the schema and all store/query paths to key on
`(org_id, sandbox_id, key)` and join on both `org_id` and
`sandbox_id`.** The handler ownership check remains necessary, but it is
not sufficient.

**F4. Key-namespace parsing with `:` in keys.** Validation allows `:`
in keys. That collides with the `tag:<key>` syntax in
`groupBy`/`filter`. If a user sets key `team:payments`, then
`filter[tag:team:payments]=...` parses ambiguously unless we
`SplitN(s, ":", 2)`. Doable — just document the rule ("after the
first `:`, everything is the tag key") and SplitN. No design change.

**F5. The reconciliation invariant needs a real Postgres merge gate.**
`internal/api/sandbox_test.go` explicitly notes the repo has no PG
fixture yet. Pure SQL-builder tests are still useful, but they do not
prove the load-bearing claim that
`Σ by-sandbox = Σ by-tag + untagged = GetOrgUsage(org)`. Recommendation:
keep the pure-Go builder tests **and** add a
`go test -tags=pgfixture` reconciliation test that runs against real
Postgres when `TEST_DATABASE_URL` is available. If CI cannot run that
path yet, treat the gap as release-blocking and call it out plainly in
the PR.

**F6. Empty-tag response shape.** Design shows `"tags": {...}` but is
silent on sandboxes with no tags. Emit `"tags": {}` (not null), and
`"tagsLastUpdatedAt": null`. Consistent with typed SDKs. Nothing to
change in the design — note in handler contracts.

**F7. `/tags` discovery is tag-state-only, not activity-filtered.**
Returns keys that exist in `sandbox_tags`, regardless of whether the
tagged sandbox was active in any window. This is consistent with
"live attribution" but may surprise users doing drilldowns on a
quarter where a tagged sandbox never ran. Accept as-is for v1; add a
`?activeIn=<window>` param later if asked.

**F8. Additive `tags` / `tagsLastUpdatedAt` on sandbox responses
lands in three places, not two.** `GET /sandboxes/{id}` has both a
local branch (`getSandbox` in worker mode) and a remote branch
(`getSandboxRemote` in server mode). `GET /sandboxes` has `listSandboxes`
and `listSandboxesRemote`. The design says "additive" — make sure the
PR touches all four code paths, not just three of them. `listSandboxesRemote`
is the easy miss because it still assembles the old response shape by
hand. Minor in code size, not minor in contract impact.

**F9. Drilldown timestamps need explicit clamping and invalid-window
rejection.** The design says `GET /sandboxes/{id}/usage` returns
`firstStartedAt` / `lastEndedAt` clamped to the query window. Saying
"use `sandbox_sessions` MIN/MAX" is not enough; without an explicit
clamp, long-lived sandboxes will leak timestamps outside `[from, to]`.
Likewise `to <= from` should be a 400, same as the aggregate path.
Recommendation: clamp in the store layer and validate the window in the
handler.

**F10. "Repeatable filters" conflicts with the natural SDK shape.**
The earlier draft said `filter[...]` was repeatable and AND-ed. In
practice, the SDK wants a map from dimension → value string, and the
useful v1 case is one param per dimension with comma-separated OR values
inside it. A true "repeat the same key multiple times" contract
complicates both the SDK and handler parsing for little gain.
Recommendation: narrow v1 to:
one filter param per dimension, comma-separated OR values within that
dimension, AND across dimensions.

**F11. `groupBy=sandbox` should not rely on per-row session lookups.**
The design promises up to 500 rows and a 10s handler timeout. Tag
hydration can batch cleanly; alias/status hydration should too. Doing
`GetSandboxSession` once per returned row is exactly the kind of
non-obvious latency multiplier that turns a good query surface into a
slow one. Recommendation: batch latest-session reads for the result set,
or fold alias/status into the aggregate query.

## Decisions on the design's three open questions

Per the handover, these are mine to pick; noting here so the PR body
cites the rationale.

**D1. `firstStartedAt` / `lastEndedAt` source** — use
`sandbox_sessions` (`MIN(started_at)` / `MAX(COALESCE(stopped_at,
now()))`), then clamp the resulting timestamps into the query window.
Matches user intent of "when did this sandbox exist" rather than "when
was it actively billed." Session-level is also stable across scale
events, which can churn every time memory changes.

**D2. Tag rows on sandbox destroy** — leave. Preserves drilldown for
historical reports. Minor overstatement of `/tags` key counts is
acceptable; revisit if an org complains. Design leans this way.

**D3. `PUT ?mode=merge`** — no. Keep strict full-replace. GET+PUT is
one extra round-trip and the atomicity story is cleaner. Consistent
with the "deliberately narrow" framing.

## Scope of this PR

New files / edits, in planned commit order:

1. **Migration** `026_sandbox_tags.up.sql` + `026_sandbox_tags.down.sql`.
   Append to the migration list in `internal/db/store.go:121`
   (becomes `{29, "migrations/026_sandbox_tags.up.sql"}`).
2. **Store layer** `internal/db/sandbox_tags.go` —
   org-scoped tag methods:
   `GetSandboxTags(ctx, orgID, sandboxID)`,
   `GetSandboxTagsMulti(ctx, orgID, sandboxIDs)`,
   `ReplaceSandboxTags` (transactional replace),
   `ListOrgTagKeys`.
3. **Usage query builder** `internal/db/usage_query.go` — single
   pure function `BuildUsageQuery(orgID, window, groupBy, filters,
   sort, cursor, limit) (sql string, args []any)`. Reuses the
   `COALESCE(ended_at, LEAST(now(), $to)) - GREATEST(started_at, $from)`
   idiom from `GetOrgUsage`. Disk overage computed inline with the
   same `max(0, disk_mb - 20480) / 1024 * duration` formula from
   `DiskOverageGBSeconds`. All tag joins key on both `org_id` and
   `sandbox_id`.
4. **Handlers**
   - `internal/api/usage.go` — `GET /usage`, `GET /tags`,
     `GET /sandboxes/:id/usage`.
   - `internal/api/sandbox_tags.go` — `GET /sandboxes/:id/tags`,
     `PUT /sandboxes/:id/tags`.
5. **Router wiring** in `internal/api/router.go` inside the
   `api := e.Group("/api")` block.
6. **Additive responses** — touch all four sandbox-read paths:
   `getSandbox`, `getSandboxRemote`, `listSandboxes`,
   `listSandboxesRemote`. `listSandboxesRemote` is the likely miss.
   Hydrate tags with one batched DB call per list response, and keep
   alias/status hydration batched too.
7. **Tests**
   - Pure-Go query-builder tests for each `groupBy × filter × sort`
     combination (snapshot SQL + args).
   - PUT validation tests: reserved prefix, key/value length limits,
     50-tag cap, malformed keys — handler-level, no DB.
   - Integration reconciliation test behind a `pgfixture` build tag
     gated on `TEST_DATABASE_URL` — asserts
     `Σ by-sandbox = Σ by-tag + untagged = GetOrgUsage(org)` within
     float epsilon. Not optional for sign-off; wire it into CI if a
     test DB is available.
8. **SDKs** — TS (`sdks/typescript/src/`) and Python
   (`sdks/python/opencomputer/`): usage and tags stubs. Convenience
   wrappers (`usage.byTag`, `usage.bySandbox`) designed in the SDK,
   not the server. Do not call the surface complete with TS-only parity.
9. **Docs** — new pages under `docs/api-reference/sandboxes/` for
   tags endpoints and `docs/api-reference/usage.mdx` for the
   aggregator. Wire into the existing "Sandboxes" navigation group in
   `docs/mint.json`, plus a new "Usage" entry at the right spot.
10. **PR ready-for-review** only after CI is green **and** the
    reconciliation path, SDK parity, and docs surface are all present.

Commit boundaries are each of the above; never amend previous commits.

## Validation rules (spec-extract for the handler)

- Key: 1–128 chars, regex `^[A-Za-z0-9_.\-:]+$` — allow `:` per
  design (see F4 for parsing).
- Key must not start with `oc:` (reserved) → 400.
- Value: 0–256 chars, any UTF-8.
- ≤ 50 tags per sandbox.
- PUT body must be a flat `map[string]string`; reject nested values
  with 400.

## Guardrails on `/usage`

- `to - from > 90d` → 400.
- `to <= from` → 400.
- `limit > 500` → 400.
- Handler-level timeout of 10s on `/usage`.
- Cursor is opaque base64 of `(sortValue, tiebreaker)` — standard
  pattern, pure in-process.

## Non-goals (from design, for the PR body)

Dollars, time-series bucketing, multi-dim groupBy, filter trees, tag
snapshot/audit, `GET /sandboxes?tag=k:v`, CSV export, CLI/dashboard.
All tracked as additive follow-ups.

## Checkpoints / risks

- **Test coverage for the reconciliation invariant is behind a
  pgfixture tag.** If we lose the invariant silently because the
  fixture isn't wired into CI, billing diverges. Mitigation: wire the
  tag to CI if a `TEST_DATABASE_URL` is available. If that cannot
  happen in this PR, leave the feature explicitly short of final
  sign-off.
- **`ReconcileWorkerSessions` staleness (F2)** is inherited, not
  introduced. Document and move on.
- **`alias` coupling to config JSON (F1)** is a one-line SQL concern
  but worth a second look in review.
- **Sandbox ID shortness makes org-scoped keying load-bearing.** The
  schema/store fix in F3 is not polish; it is the tenancy boundary.
