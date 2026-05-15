DROP INDEX IF EXISTS idx_hibernations_pending_upload;

ALTER TABLE sandbox_hibernations
  DROP COLUMN IF EXISTS uploaded_at,
  DROP COLUMN IF EXISTS upload_error;
