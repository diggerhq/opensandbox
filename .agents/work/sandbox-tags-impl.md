# Sandbox tags + per-tag/per-sandbox usage — implementation plan

Working doc. Design is at `.agents/design/sandbox-tags-and-usage.md`,
signed off. This doc is the delivery plan + a flag list of weak or
fuzzy spots in the design that surfaced during code exploration.

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

**F3. Tenancy PK on `sandbox_tags`.** Design uses
`PRIMARY KEY (sandbox_id, key)`. Sandbox IDs look globally unique
(`sb-...`), but nothing in the schema enforces that across orgs, and
`sandbox_sessions` deliberately allows multiple rows per sandbox_id
(session history). Recommendation: **keep the PK as designed** — the
PUT handler must still verify `sandbox_sessions.org_id = caller_org`
before mutating, which closes the cross-tenant write path. Leave
`org_id` in the row as a denormalization for the indexed lookup in
`GET /tags` (which filters on `org_id`). Noted for the reviewer.

**F4. Key-namespace parsing with `:` in keys.** Validation allows `:`
in keys. That collides with the `tag:<key>` syntax in
`groupBy`/`filter`. If a user sets key `team:payments`, then
`filter[tag:team:payments]=...` parses ambiguously unless we
`SplitN(s, ":", 2)`. Doable — just document the rule ("after the
first `:`, everything is the tag key") and SplitN. No design change.

**F5. Reconciliation test needs a live Postgres fixture we don't
have.** `internal/api/sandbox_test.go` explicitly notes the repo has
no PG fixture yet. Options: (a) build the query builder as a pure Go
function returning `(sql, args)` and assert SQL text/args — no DB
needed — **plus** a separate `go test -tags=pgfixture` test that hits
a real Postgres if `TEST_DATABASE_URL` is set, and the reconciliation
assertion lives there. (b) Add the PG fixture to the repo in this PR
(scope explosion). **Recommendation: (a).** Call out the
tags/scope-limit gap in the PR body and flag it as the missing piece
to revisit with the broader test-infra work.

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
PR touches all four code paths, not just the server ones. Minor but
easy to miss.

## Decisions on the design's three open questions

Per the handover, these are mine to pick; noting here so the PR body
cites the rationale.

**D1. `firstStartedAt` / `lastEndedAt` source** — use
`sandbox_sessions` (`MIN(started_at)` / `MAX(COALESCE(stopped_at,
now()))`), scoped to the query window. Matches user intent of "when
did this sandbox exist" rather than "when was it actively billed."
Session-level is also stable across scale events, which can churn
every time memory changes.

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
   `ListSandboxTags`, `ReplaceSandboxTags` (transactional
   delete-then-insert), `ListTagsForSandboxes` (bulk for list
   responses), `GetTagsLastUpdatedAt`, `ListOrgTagKeys`.
3. **Usage query builder** `internal/db/usage_query.go` — single
   pure function `BuildUsageQuery(orgID, window, groupBy, filters,
   sort, cursor, limit) (sql string, args []any)`. Reuses the
   `COALESCE(ended_at, LEAST(now(), $to)) - GREATEST(started_at, $from)`
   idiom from `GetOrgUsage`. Disk overage computed inline with the
   same `max(0, disk_mb - 20480) / 1024 * duration` formula from
   `DiskOverageGBSeconds`.
4. **Handlers**
   - `internal/api/usage.go` — `GET /usage`, `GET /tags`,
     `GET /sandboxes/:id/usage`.
   - `internal/api/sandbox_tags.go` — `GET /sandboxes/:id/tags`,
     `PUT /sandboxes/:id/tags`.
5. **Router wiring** in `internal/api/router.go` inside the
   `api := e.Group("/api")` block.
6. **Additive responses** — touch all four sandbox-read paths:
   `getSandbox`, `getSandboxRemote`, `listSandboxes`,
   `listSandboxesRemote`. Hydrate tags with `ListTagsForSandboxes`
   (one batched DB call per list response).
7. **Tests**
   - Pure-Go query-builder tests for each `groupBy × filter × sort`
     combination (snapshot SQL + args).
   - PUT validation tests: reserved prefix, key/value length limits,
     50-tag cap, malformed keys — handler-level, no DB.
   - Integration reconciliation test behind a `pgfixture` build tag
     gated on `TEST_DATABASE_URL` — asserts
     `Σ by-sandbox = Σ by-tag + untagged = GetOrgUsage(org)` within
     float epsilon. Skipped by default; documented in PR.
8. **SDKs** — TS (`sdks/typescript/src/`) and Python
   (`sdks/python/opencomputer/`): usage and tags stubs. Convenience
   wrappers (`usage.byTag`, `usage.bySandbox`) designed in the SDK,
   not the server.
9. **Docs** — new pages under `docs/api-reference/sandboxes/` for
   tags endpoints and `docs/api-reference/usage.mdx` for the
   aggregator. Wire into the existing "Sandboxes" navigation group in
   `docs/mint.json`, plus a new "Usage" entry at the right spot.
10. **PR ready-for-review** after CI green. Draft → ready.

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
  tag to CI if a `TEST_DATABASE_URL` is available, otherwise call out
  the gap in the PR body so a reviewer signs off on the gap.
- **`ReconcileWorkerSessions` staleness (F2)** is inherited, not
  introduced. Document and move on.
- **`alias` coupling to config JSON (F1)** is a one-line SQL concern
  but worth a second look in review.
