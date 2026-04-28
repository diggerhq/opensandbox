-- Append-only ledger of reserved-capacity commitments.
-- One row per (reservation_id, resource, starts_at). Multiple rows share a
-- reservation_id when the customer commits to several 15-minute buckets in
-- a single POST. The ledger is the source of truth for the calendar
-- aggregation, the audit-list endpoint, and (in phase 2) the per-second
-- overage allocator.
CREATE TABLE IF NOT EXISTS capacity_reservation_intervals (
    reservation_id      UUID        NOT NULL,
    org_id              UUID        NOT NULL,
    resource            TEXT        NOT NULL,
    starts_at           TIMESTAMPTZ NOT NULL,
    ends_at             TIMESTAMPTZ NOT NULL,
    capacity_gb         INTEGER     NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    idempotency_key_id  UUID,
    PRIMARY KEY (reservation_id, resource, starts_at),
    -- 1 GB-hour grain: capacity in 4 GB × 15 min units. Reject anything else
    -- at the schema level so a buggy handler can't write fractional grain.
    CHECK (capacity_gb > 0 AND capacity_gb % 4 = 0),
    -- Bucket must be exactly 15 minutes; multi-interval spans are rejected
    -- at the API layer but the schema enforces it as a final guard.
    CHECK (ends_at = starts_at + INTERVAL '15 minutes'),
    -- v1 only knows 'memory_gb'. Future resources extend this.
    CHECK (resource IN ('memory_gb'))
);

-- Calendar aggregation reads (org_id, resource, starts_at) range scans and
-- sums capacity_gb. List pagination orders by (created_at DESC,
-- reservation_id DESC). Two indexes to serve both shapes.
CREATE INDEX IF NOT EXISTS idx_capacity_intervals_org_resource_starts
    ON capacity_reservation_intervals (org_id, resource, starts_at);
CREATE INDEX IF NOT EXISTS idx_capacity_intervals_org_created
    ON capacity_reservation_intervals (org_id, created_at DESC, reservation_id DESC);
