-- Persist the actual reason a checkpoint create failed.
--
-- Pre-fix: SetCheckpointFailed(ctx, id, reason) accepted a `reason` argument
-- but its UPDATE only set status='failed' — the reason string was silently
-- discarded. Operators and customers had no signal beyond "failed", which
-- was the entire detail Oliviero saw on the May 6 incident: "status
-- processing for ~280s → failed, no error detail".
--
-- error_msg holds the formatted error chain from CreateCheckpoint (timeout,
-- archive failure, upload failure, etc.). failed_at marks when the failure
-- was recorded so we can correlate with worker journals.

ALTER TABLE sandbox_checkpoints
  ADD COLUMN error_msg TEXT,
  ADD COLUMN failed_at TIMESTAMPTZ;
