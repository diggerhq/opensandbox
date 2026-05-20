-- Free-tier halt tracking, mirrored from the CreditAccount DO via the
-- /admin/halt-org webhook. is_halted is the cell-local source of truth
-- for "should we refuse to wake this org's sandboxes?" — checked by the
-- wake handler in place of the (now removed) free_credits_remaining_cents
-- gate. The DO remains the global authority; this column is just the
-- projection the cell sees after a halt webhook lands.
--
-- halt_reason on sandbox_sessions lets ResumeOrg target only credit-
-- exhaustion halts and leave user-initiated hibernations alone.

ALTER TABLE orgs ADD COLUMN is_halted BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE orgs ADD COLUMN halted_at TIMESTAMPTZ;
CREATE INDEX idx_orgs_halted ON orgs(is_halted) WHERE is_halted = TRUE;

ALTER TABLE sandbox_sessions ADD COLUMN halt_reason TEXT;
CREATE INDEX idx_sandbox_sessions_halt_reason ON sandbox_sessions(org_id, halt_reason) WHERE halt_reason IS NOT NULL;
