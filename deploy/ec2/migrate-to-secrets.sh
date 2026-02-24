#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# OpenSandbox — Migrate running instances to use AWS Secrets Manager
#
# Current state (after setup-secrets.sh was run):
#   - opensandbox/worker secret exists (JWT, Redis, DB, S3) — needs ECR added
#   - opensandbox-ec2 instance profile is attached to both instances
#   - opensandbox-ec2 role only has ECR access — needs Secrets Manager + S3
#   - No server secret exists yet
#   - Both instances use inline secrets in systemd/env files
#
# This script:
#   1. Updates the worker secret to include ECR values
#   2. Creates a server secret with WorkOS values
#   3. Updates the opensandbox-ec2 role with Secrets Manager + S3 + ECR policy
#   4. SSHes into both instances and migrates them to OPENSANDBOX_SECRETS_ARN
#
# Prerequisites:
#   - AWS CLI v2 with "digger" profile configured
#   - SSH access via opensandbox-digger.pem
#   - jq installed
# =============================================================================

AWS_PROFILE="digger"
REGION="us-east-2"
SSH_KEY="$HOME/.ssh/opensandbox-digger.pem"

# Existing infra
WORKER_SECRET_NAME="opensandbox/worker"
ROLE_NAME="opensandbox-ec2"  # the role actually attached to instances
S3_BUCKET="opensandbox-checkpoints-digger"
ECR_REGISTRY="739940681129.dkr.ecr.us-east-2.amazonaws.com"
ECR_REPOSITORY="opensandbox-templates"

# Server secret (new)
SERVER_SECRET_NAME="opensandbox/server"

# Production values — from running worker systemd unit
REDIS_URL="redis://opensandbox-redis.c1cbz5.0001.use2.cache.amazonaws.com:6379"
RDS_HOST="opensandbox-pg.chwjcxjqzouh.us-east-2.rds.amazonaws.com"
RDS_PASSWORD="OpnSbx2026SecurePG"
JWT_SECRET="${JWT_SECRET}"
DATABASE_URL="postgres://opensandbox:${RDS_PASSWORD}@${RDS_HOST}:5432/opensandbox?sslmode=require"

# WorkOS (server only) — from /etc/opensandbox/server.env
WORKOS_API_KEY="${WORKOS_API_KEY}"
WORKOS_CLIENT_ID="${WORKOS_CLIENT_ID}"
WORKOS_REDIRECT_URI="https://app.opensandbox.ai/auth/callback"
WORKOS_COOKIE_DOMAIN="opensandbox.ai"

# Instance IPs
WORKER_IP="18.219.23.64"
SERVER_IP="3.135.246.117"

aws() { command aws --profile "$AWS_PROFILE" "$@"; }

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

echo -e "${GREEN}==> OpenSandbox: Migrate to Secrets Manager${NC}"
echo ""

# --- Step 1: Update worker secret to include ECR ---
echo -e "${YELLOW}==> Step 1: Updating worker secret with ECR values...${NC}"

WORKER_SECRET_JSON=$(jq -n \
  --arg jwt "$JWT_SECRET" \
  --arg redis "$REDIS_URL" \
  --arg db "$DATABASE_URL" \
  --arg s3bucket "$S3_BUCKET" \
  --arg s3region "$REGION" \
  --arg ecr_registry "$ECR_REGISTRY" \
  --arg ecr_repo "$ECR_REPOSITORY" \
  '{
    "OPENSANDBOX_JWT_SECRET": $jwt,
    "OPENSANDBOX_REDIS_URL": $redis,
    "DATABASE_URL": $db,
    "OPENSANDBOX_S3_BUCKET": $s3bucket,
    "OPENSANDBOX_S3_REGION": $s3region,
    "OPENSANDBOX_ECR_REGISTRY": $ecr_registry,
    "OPENSANDBOX_ECR_REPOSITORY": $ecr_repo
  }')

aws secretsmanager put-secret-value \
  --secret-id "$WORKER_SECRET_NAME" \
  --region "$REGION" \
  --secret-string "$WORKER_SECRET_JSON"

WORKER_SECRET_ARN=$(aws secretsmanager describe-secret \
  --secret-id "$WORKER_SECRET_NAME" \
  --region "$REGION" \
  --query 'ARN' --output text)

echo -e "Worker Secret ARN: ${GREEN}${WORKER_SECRET_ARN}${NC}"
echo ""

# --- Step 2: Create server secret ---
echo -e "${YELLOW}==> Step 2: Creating server secret...${NC}"

SERVER_SECRET_JSON=$(jq -n \
  --arg jwt "$JWT_SECRET" \
  --arg redis "$REDIS_URL" \
  --arg db "$DATABASE_URL" \
  --arg s3bucket "$S3_BUCKET" \
  --arg s3region "$REGION" \
  --arg ecr_registry "$ECR_REGISTRY" \
  --arg ecr_repo "$ECR_REPOSITORY" \
  --arg workos_key "$WORKOS_API_KEY" \
  --arg workos_client "$WORKOS_CLIENT_ID" \
  --arg workos_redirect "$WORKOS_REDIRECT_URI" \
  --arg workos_cookie "$WORKOS_COOKIE_DOMAIN" \
  '{
    "OPENSANDBOX_JWT_SECRET": $jwt,
    "OPENSANDBOX_REDIS_URL": $redis,
    "DATABASE_URL": $db,
    "OPENSANDBOX_S3_BUCKET": $s3bucket,
    "OPENSANDBOX_S3_REGION": $s3region,
    "OPENSANDBOX_ECR_REGISTRY": $ecr_registry,
    "OPENSANDBOX_ECR_REPOSITORY": $ecr_repo,
    "WORKOS_API_KEY": $workos_key,
    "WORKOS_CLIENT_ID": $workos_client,
    "WORKOS_REDIRECT_URI": $workos_redirect,
    "WORKOS_COOKIE_DOMAIN": $workos_cookie
  }')

if aws secretsmanager describe-secret --secret-id "$SERVER_SECRET_NAME" --region "$REGION" &>/dev/null; then
  echo "Secret '$SERVER_SECRET_NAME' already exists — updating..."
  aws secretsmanager put-secret-value \
    --secret-id "$SERVER_SECRET_NAME" \
    --region "$REGION" \
    --secret-string "$SERVER_SECRET_JSON"
else
  aws secretsmanager create-secret \
    --name "$SERVER_SECRET_NAME" \
    --region "$REGION" \
    --description "OpenSandbox server secrets (JWT, Redis, S3, DB, ECR, WorkOS)" \
    --secret-string "$SERVER_SECRET_JSON"
fi

SERVER_SECRET_ARN=$(aws secretsmanager describe-secret \
  --secret-id "$SERVER_SECRET_NAME" \
  --region "$REGION" \
  --query 'ARN' --output text)

echo -e "Server Secret ARN: ${GREEN}${SERVER_SECRET_ARN}${NC}"
echo ""

# --- Step 3: Update opensandbox-ec2 role with Secrets Manager + S3 policy ---
echo -e "${YELLOW}==> Step 3: Updating IAM role '${ROLE_NAME}' with Secrets Manager + S3 policy...${NC}"

POLICY_NAME="opensandbox-secrets-s3-ecr"
POLICY_DOC=$(cat <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SecretsManagerAccess",
      "Effect": "Allow",
      "Action": "secretsmanager:GetSecretValue",
      "Resource": [
        "${WORKER_SECRET_ARN}",
        "${SERVER_SECRET_ARN}"
      ]
    },
    {
      "Sid": "S3CheckpointAccess",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:DeleteObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::${S3_BUCKET}",
        "arn:aws:s3:::${S3_BUCKET}/*"
      ]
    },
    {
      "Sid": "ECRAccess",
      "Effect": "Allow",
      "Action": [
        "ecr:GetAuthorizationToken",
        "ecr:BatchCheckLayerAvailability",
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchGetImage"
      ],
      "Resource": "*"
    }
  ]
}
EOF
)

EXISTING_POLICY_ARN=$(aws iam list-policies \
  --query "Policies[?PolicyName=='${POLICY_NAME}'].Arn" \
  --output text 2>/dev/null || echo "")

if [ -n "$EXISTING_POLICY_ARN" ] && [ "$EXISTING_POLICY_ARN" != "None" ]; then
  echo "Policy '$POLICY_NAME' already exists — creating new version..."
  OLD_VERSION=$(aws iam list-policy-versions \
    --policy-arn "$EXISTING_POLICY_ARN" \
    --query 'Versions[?IsDefaultVersion==`false`].VersionId | [0]' \
    --output text 2>/dev/null || echo "None")
  if [ -n "$OLD_VERSION" ] && [ "$OLD_VERSION" != "None" ]; then
    aws iam delete-policy-version \
      --policy-arn "$EXISTING_POLICY_ARN" \
      --version-id "$OLD_VERSION" 2>/dev/null || true
  fi
  aws iam create-policy-version \
    --policy-arn "$EXISTING_POLICY_ARN" \
    --policy-document "$POLICY_DOC" \
    --set-as-default
  POLICY_ARN="$EXISTING_POLICY_ARN"
else
  POLICY_ARN=$(aws iam create-policy \
    --policy-name "$POLICY_NAME" \
    --description "OpenSandbox: Secrets Manager + S3 + ECR access" \
    --policy-document "$POLICY_DOC" \
    --query 'Policy.Arn' --output text)
fi

aws iam attach-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-arn "$POLICY_ARN" 2>/dev/null || true

echo -e "Policy ARN: ${GREEN}${POLICY_ARN}${NC}"
echo "Attached to role '$ROLE_NAME'"
echo ""

# --- Step 4: Migrate worker instance ---
echo -e "${YELLOW}==> Step 4: Migrating worker instance (${WORKER_IP})...${NC}"

ssh -o StrictHostKeyChecking=no -i "$SSH_KEY" "ubuntu@${WORKER_IP}" bash <<REMOTE_WORKER
set -euo pipefail

# Write new minimal systemd unit that uses Secrets Manager
sudo tee /etc/systemd/system/opensandbox-worker.service > /dev/null <<'UNIT'
[Unit]
Description=OpenSandbox Worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=/sbin/modprobe inet_diag
ExecStartPre=/sbin/modprobe tcp_diag
ExecStartPre=/sbin/modprobe udp_diag
ExecStartPre=/sbin/modprobe unix_diag
ExecStartPre=/sbin/modprobe netlink_diag
ExecStart=/usr/local/bin/opensandbox-worker
Restart=always
RestartSec=5

Environment=HOME=/root
Environment=OPENSANDBOX_MODE=worker
Environment=OPENSANDBOX_PORT=8080
Environment=OPENSANDBOX_REGION=use2
Environment=OPENSANDBOX_DATA_DIR=/data/sandboxes
Environment=OPENSANDBOX_WORKER_ID=w-use2-1
Environment=OPENSANDBOX_HTTP_ADDR=http://${WORKER_IP}:8080
Environment=OPENSANDBOX_GRPC_ADVERTISE=10.10.1.54:9090
Environment=OPENSANDBOX_SANDBOX_DOMAIN=workers.opensandbox.ai
Environment=OPENSANDBOX_SECRETS_ARN=${WORKER_SECRET_ARN}

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
echo "Worker systemd unit updated. Secrets will be fetched from Secrets Manager at next restart."
REMOTE_WORKER

echo -e "${GREEN}Worker migrated.${NC}"
echo ""

# --- Step 5: Migrate server instance ---
echo -e "${YELLOW}==> Step 5: Migrating server instance (${SERVER_IP})...${NC}"

ssh -o StrictHostKeyChecking=no -i "$SSH_KEY" "ubuntu@${SERVER_IP}" bash <<REMOTE_SERVER
set -euo pipefail

# Write new minimal server.env that uses Secrets Manager
sudo tee /etc/opensandbox/server.env > /dev/null <<'ENV'
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_REGION=use2
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_WORKER_ID=cp-use2-1
OPENSANDBOX_HTTP_ADDR=http://${SERVER_IP}:8080
OPENSANDBOX_SANDBOX_DOMAIN=workers.opensandbox.ai
OPENSANDBOX_SECRETS_ARN=${SERVER_SECRET_ARN}
ENV

echo "Server env file updated. Secrets will be fetched from Secrets Manager at next restart."
REMOTE_SERVER

echo -e "${GREEN}Server migrated.${NC}"
echo ""

# --- Done ---
echo -e "${GREEN}============================================${NC}"
echo -e "${GREEN} Migration complete!${NC}"
echo ""
echo "Worker Secret ARN: $WORKER_SECRET_ARN"
echo "Server Secret ARN: $SERVER_SECRET_ARN"
echo "IAM Policy:        $POLICY_ARN"
echo ""
echo "Both instances are configured but NOT restarted."
echo "Inline secrets have been replaced with OPENSANDBOX_SECRETS_ARN."
echo ""
echo "To apply (restart services):"
echo "  Worker: ssh -i $SSH_KEY ubuntu@${WORKER_IP} 'sudo systemctl restart opensandbox-worker'"
echo "  Server: ssh -i $SSH_KEY ubuntu@${SERVER_IP} 'sudo systemctl restart opensandbox-server'"
echo ""
echo "Or restart both:"
echo "  ssh -i $SSH_KEY ubuntu@${WORKER_IP} 'sudo systemctl restart opensandbox-worker' && \\"
echo "  ssh -i $SSH_KEY ubuntu@${SERVER_IP} 'sudo systemctl restart opensandbox-server'"
echo ""
echo -e "${GREEN}============================================${NC}"
