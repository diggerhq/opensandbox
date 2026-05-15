-- Track whether a hibernation's S3 upload actually completed.
-- Pre-fix: size_bytes was hardcoded to 0 by qemu hibernate, so the DB had no
-- signal of upload success. Async upload failures were only logged. This made
-- the missing-blob bug invisible — sandboxes appeared hibernated but couldn't
-- be woken cross-worker because the blob was never uploaded.

ALTER TABLE sandbox_hibernations
  ADD COLUMN uploaded_at  TIMESTAMPTZ,
  ADD COLUMN upload_error TEXT;

-- Find hibernations that have not finished uploading. Useful for monitoring
-- and for the periodic backstop scan.
CREATE INDEX idx_hibernations_pending_upload
  ON sandbox_hibernations (hibernated_at)
  WHERE uploaded_at IS NULL AND upload_error IS NULL AND expired_at IS NULL;
