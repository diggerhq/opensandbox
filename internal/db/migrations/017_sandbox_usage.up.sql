-- Track resource scaling events for billing.
-- Each row represents a period at a specific resource tier.
-- When a sandbox scales, the current row gets ended_at and a new row is inserted.
CREATE TABLE IF NOT EXISTS sandbox_scale_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    sandbox_id  TEXT NOT NULL,
    org_id      UUID NOT NULL,
    memory_mb   INT NOT NULL,       -- memory tier at this point
    cpu_percent INT NOT NULL,       -- CPU allocation (100 = 1 vCPU)
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at    TIMESTAMPTZ         -- NULL = current tier
);

CREATE INDEX IF NOT EXISTS idx_scale_events_sandbox ON sandbox_scale_events(sandbox_id);
CREATE INDEX IF NOT EXISTS idx_scale_events_org ON sandbox_scale_events(org_id, started_at);

-- Periodic usage samples for detailed billing and monitoring.
-- Collected every 60s by the worker, batch inserted every 5 min.
CREATE TABLE IF NOT EXISTS sandbox_usage_samples (
    sandbox_id    TEXT NOT NULL,
    org_id        UUID NOT NULL,
    sampled_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    memory_mb     INT NOT NULL,       -- current tier
    cpu_usec      BIGINT NOT NULL,    -- cumulative CPU usage from cgroup cpu.stat
    memory_bytes  BIGINT NOT NULL,    -- current memory usage from cgroup memory.current
    pids          INT NOT NULL,       -- current PID count from cgroup pids.current
    PRIMARY KEY (sandbox_id, sampled_at)
);

CREATE INDEX IF NOT EXISTS idx_usage_samples_org ON sandbox_usage_samples(org_id, sampled_at);
