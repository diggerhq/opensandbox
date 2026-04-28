-- Cached results of write requests keyed by Idempotency-Key header.
-- Replays return the original status_code + response_body verbatim, so a
-- cached 409 stays a 409. Same key with a different request body returns
-- idempotency_key_conflict (handler responsibility, not schema).
CREATE TABLE IF NOT EXISTS capacity_idempotency_keys (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id              UUID        NOT NULL,
    endpoint            TEXT        NOT NULL,
    key                 TEXT        NOT NULL,
    request_body_hash   BYTEA       NOT NULL,
    status_code         INTEGER     NOT NULL,
    response_body       JSONB       NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ NOT NULL,
    UNIQUE (org_id, endpoint, key)
);

-- Cleanup sweeps query expires_at < now().
CREATE INDEX IF NOT EXISTS idx_capacity_idempotency_expires
    ON capacity_idempotency_keys (expires_at);
