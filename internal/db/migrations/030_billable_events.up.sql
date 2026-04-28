-- Outbox of metered events for the unified billing pipeline.
-- The capacity allocator (phase 2) writes one row per (org, event_type,
-- memory_mb, bucket_start) after each 15-min bucket settles. The Stripe
-- sender (phase 3) reads delivery_state='pending' rows and ships them.
--
-- The (org_id, event_type, memory_mb, bucket_start) UNIQUE constraint
-- makes allocator reruns idempotent at the DB level — a crashed
-- allocator can replay any bucket without producing duplicate charges.
--
-- v1 event_types:
--   'reserved_usage'      — pre-booked capacity charge, single synthetic
--                           tier per (org, bucket); memory_mb = 0
--   'overage_usage'       — instant usage above the reserved floor,
--                           emitted per running sandbox tier with the
--                           tier as memory_mb
--   'disk_overage_usage'  — disk above the 20 GB allowance, org-level
--                           per bucket; memory_mb = 0
CREATE TABLE IF NOT EXISTS billable_events (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              UUID        NOT NULL,
    event_type          TEXT        NOT NULL,
    memory_mb           INTEGER     NOT NULL,
    gb_seconds          NUMERIC     NOT NULL,
    bucket_start        TIMESTAMPTZ NOT NULL,
    bucket_end          TIMESTAMPTZ NOT NULL,
    delivery_state      TEXT        NOT NULL DEFAULT 'pending',
    stripe_event_id     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at        TIMESTAMPTZ,
    UNIQUE (org_id, event_type, memory_mb, bucket_start),
    CHECK (event_type IN ('reserved_usage', 'overage_usage', 'disk_overage_usage')),
    CHECK (delivery_state IN ('pending', 'sent', 'failed')),
    CHECK (gb_seconds > 0),
    CHECK (memory_mb >= 0),
    CHECK (bucket_end = bucket_start + INTERVAL '15 minutes')
);

-- Sender query: pending rows in arrival order. Partial index keeps the
-- index small as 'sent' rows accumulate.
CREATE INDEX IF NOT EXISTS idx_billable_events_pending
    ON billable_events (created_at, id)
    WHERE delivery_state = 'pending';

-- Per-org scans (shadow-verify, audit, dashboard breakdown).
CREATE INDEX IF NOT EXISTS idx_billable_events_org_bucket
    ON billable_events (org_id, bucket_start);
