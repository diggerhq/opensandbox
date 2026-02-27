ALTER TABLE orgs
  ADD COLUMN custom_domain TEXT,
  ADD COLUMN cf_hostname_id TEXT,
  ADD COLUMN domain_verification_status TEXT DEFAULT 'none',
  ADD COLUMN domain_ssl_status TEXT DEFAULT 'none',
  ADD COLUMN verification_txt_name TEXT,
  ADD COLUMN verification_txt_value TEXT,
  ADD COLUMN ssl_txt_name TEXT,
  ADD COLUMN ssl_txt_value TEXT;
