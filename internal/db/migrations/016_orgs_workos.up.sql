-- Link orgs to WorkOS organizations
ALTER TABLE orgs ADD COLUMN workos_org_id TEXT UNIQUE;
ALTER TABLE orgs ADD COLUMN is_personal BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE orgs ADD COLUMN owner_user_id UUID REFERENCES users(id);
ALTER TABLE orgs ADD COLUMN credit_balance_cents INT NOT NULL DEFAULT 3000;

-- Link users to WorkOS users
ALTER TABLE users ADD COLUMN workos_user_id TEXT UNIQUE;

-- Mark existing orgs as personal (backfill for pre-existing data)
UPDATE orgs SET is_personal = true, owner_user_id = u.id
FROM users u WHERE u.org_id = orgs.id;
