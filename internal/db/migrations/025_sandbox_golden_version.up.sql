-- Store golden snapshot version on sandbox sessions so the scaler can read it
-- from PG instead of relying on in-memory state via gRPC.
ALTER TABLE sandbox_sessions ADD COLUMN IF NOT EXISTS golden_version TEXT;
