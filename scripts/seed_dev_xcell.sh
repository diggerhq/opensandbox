#!/usr/bin/env bash
# seed_dev_xcell.sh — seed the dev D1 database with the fixtures needed for a
# cross-cell test: two cells, one user, two orgs (one homed in each cell), and
# one API key per org.
#
# Idempotent (INSERT OR REPLACE) — safe to re-run, e.g. once dev3's CP URL is
# known you can re-run with the real value.
#
# Usage:
#   scripts/seed_dev_xcell.sh <DEV2_CP_URL> [DEV3_CP_URL]
#
# Example:
#   scripts/seed_dev_xcell.sh http://20.94.201.157:8080 http://3.91.x.x:8080
#
# Requires: wrangler logged in to the CF account that owns `opencomputer-dev`
# (run `wrangler login` first, or export CLOUDFLARE_API_TOKEN).

set -euo pipefail

D1_DB="opencomputer-dev"
DEV2_CP_URL="${1:?usage: seed_dev_xcell.sh <DEV2_CP_URL> [DEV3_CP_URL]}"
DEV3_CP_URL="${2:-http://DEV3-CP-NOT-PROVISIONED-YET:8080}"

# Fixed test fixtures — deterministic so the smoke test knows exactly what to send.
ORG_A_ID="aaaaaaaa-0000-0000-0000-000000000001"   # home_cell = azure-westus2-cell-b
ORG_B_ID="bbbbbbbb-0000-0000-0000-000000000001"   # home_cell = aws-us-east-1-cell-a
USER_ID="cccccccc-0000-0000-0000-000000000001"
KEY_A_ID="a11ce11a-0000-0000-0000-000000000001"
KEY_B_ID="b11ce11b-0000-0000-0000-000000000001"

# Plaintext API keys (osb_ + 64 hex). Obviously-test values; the edge looks up
# by sha256(plaintext) so only the hash is stored.
KEY_A_PLAINTEXT="osb_a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0"
KEY_B_PLAINTEXT="osb_b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1b1"

sha256_hex() { printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1; }
KEY_A_HASH="$(sha256_hex "$KEY_A_PLAINTEXT")"
KEY_B_HASH="$(sha256_hex "$KEY_B_PLAINTEXT")"
KEY_A_PREFIX="${KEY_A_PLAINTEXT:0:12}"
KEY_B_PREFIX="${KEY_B_PLAINTEXT:0:12}"

SQL_FILE="$(mktemp -t seed_dev_xcell.XXXXXX.sql)"
trap 'rm -f "$SQL_FILE"' EXIT

cat > "$SQL_FILE" <<SQL
-- Cells: the two regional control planes the edge will route between.
INSERT OR REPLACE INTO cells (cell_id, cloud, region, base_url, status, created_at) VALUES
  ('azure-westus2-cell-b', 'azure', 'westus2',   '${DEV2_CP_URL}', 'active', strftime('%s','now')),
  ('aws-us-east-1-cell-a', 'aws',   'us-east-1', '${DEV3_CP_URL}', 'active', strftime('%s','now'));

-- One user, owner of both test orgs.
INSERT OR REPLACE INTO users (id, email, name, created_at) VALUES
  ('${USER_ID}', 'xcell-test@opensandbox.dev', 'X-Cell Test', strftime('%s','now'));

-- Two orgs, each homed in a different cell — so a create with each org's key
-- exercises a different cell.
INSERT OR REPLACE INTO orgs (id, name, slug, plan, home_cell, is_personal, owner_user_id, created_at, updated_at) VALUES
  ('${ORG_A_ID}', 'X-Cell Test A (dev2)', 'xcell-test-a', 'pro', 'azure-westus2-cell-b', 0, '${USER_ID}', strftime('%s','now'), strftime('%s','now')),
  ('${ORG_B_ID}', 'X-Cell Test B (dev3)', 'xcell-test-b', 'pro', 'aws-us-east-1-cell-a', 0, '${USER_ID}', strftime('%s','now'), strftime('%s','now'));

INSERT OR REPLACE INTO org_memberships (org_id, user_id, role, created_at) VALUES
  ('${ORG_A_ID}', '${USER_ID}', 'owner', strftime('%s','now')),
  ('${ORG_B_ID}', '${USER_ID}', 'owner', strftime('%s','now'));

-- One API key per org.
INSERT OR REPLACE INTO api_keys (id, org_id, created_by, key_hash, key_prefix, name, scopes, created_at) VALUES
  ('${KEY_A_ID}', '${ORG_A_ID}', '${USER_ID}', '${KEY_A_HASH}', '${KEY_A_PREFIX}', 'xcell-test-a', 'sandbox:*', strftime('%s','now')),
  ('${KEY_B_ID}', '${ORG_B_ID}', '${USER_ID}', '${KEY_B_HASH}', '${KEY_B_PREFIX}', 'xcell-test-b', 'sandbox:*', strftime('%s','now'));
SQL

echo "Applying seed to D1 '${D1_DB}' (remote)..."
echo "  cell azure-westus2-cell-b -> ${DEV2_CP_URL}"
echo "  cell aws-us-east-1-cell-a -> ${DEV3_CP_URL}"
wrangler d1 execute "$D1_DB" --remote --file "$SQL_FILE"

cat <<DONE

Seed applied. Test credentials:

  Org A (home_cell=azure-westus2-cell-b, expect sandbox on dev2):
    X-API-Key: ${KEY_A_PLAINTEXT}

  Org B (home_cell=aws-us-east-1-cell-a, expect sandbox on dev3):
    X-API-Key: ${KEY_B_PLAINTEXT}

If dev3 isn't provisioned yet, re-run this with its real CP URL once it is:
  scripts/seed_dev_xcell.sh ${DEV2_CP_URL} http://<dev3-cp-ip>:8080
DONE
