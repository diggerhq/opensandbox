-- NOTE: irreversible data loss — the backfill to 500 for existing free orgs
-- cannot be restored by dropping the column. This down migration only reverts
-- the schema change.
ALTER TABLE orgs DROP COLUMN IF EXISTS free_credits_remaining_cents;
