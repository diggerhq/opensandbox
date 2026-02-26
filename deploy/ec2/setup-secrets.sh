#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# OpenSandbox — AWS Secrets Manager + IAM Setup (Digger account)
#
# This script creates:
#   1. Two Secrets Manager secrets (worker + server)
#   2. An IAM policy for EC2 instances (Secrets Manager + S3 + ECR)
#   3. An IAM role + instance profile for EC2 instances
#   4. Attaches the instance profile to running instances
#
# Prerequisites:
#   - AWS CLI v2 with "digger" profile configured
#   - jq installed (brew install jq / apt install jq)
#
# Usage:
#   ./deploy/ec2/setup-secrets.sh
# =============================================================================

AWS_PROFILE="digger"
REGION="us-east-2"
WORKER_SECRET_NAME="opensandbox/worker"
SERVER_SECRET_NAME="opensandbox/server"
S3_BUCKET="opensandbox-checkpoints-digger"
ROLE_NAME="opensandbox-worker-role"
POLICY_NAME="opensandbox-worker-policy"
INSTANCE_PROFILE_NAME="opensandbox-worker"
ECR_REGISTRY="739940681129.dkr.ecr.us-east-2.amazonaws.com"
ECR_REPOSITORY="opensandbox-templates"

# Known infrastructure — pulled from running production instances
REDIS_URL="redis://opensandbox-redis.c1cbz5.0001.use2.cache.amazonaws.com:6379"
RDS_HOST="opensandbox-pg.chwjcxjqzouh.us-east-2.rds.amazonaws.com"
RDS_PASSWORD="OpnSbx2026SecurePG"
JWT_SECRET="${JWT_SECRET}"
DATABASE_URL="postgres://opensandbox:${RDS_PASSWORD}@${RDS_HOST}:5432/opensandbox?sslmode=require"

# WorkOS (server only) — set these env vars before running
WORKOS_API_KEY="${WORKOS_API_KEY}"
WORKOS_CLIENT_ID="${WORKOS_CLIENT_ID}"
WORKOS_REDIRECT_URI="${WORKOS_REDIRECT_URI:-https://app.opensandbox.ai/auth/callback}"
WORKOS_COOKIE_DOMAIN="${WORKOS_COOKIE_DOMAIN:-opensandbox.ai}"

# Shorthand — every aws call uses the digger profile
aws() { command aws --profile "$AWS_PROFILE" "$@"; }

# --- Colors ---
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}==> OpenSandbox Secrets Manager Setup (profile: ${AWS_PROFILE})${NC}"
echo ""

# --- Step 0: Verify credentials ---
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
echo -e "AWS Account: ${GREEN}${ACCOUNT_ID}${NC}"
echo -e "Region:      ${GREEN}${REGION}${NC}"
echo -e "Redis:       ${GREEN}${REDIS_URL}${NC}"
echo -e "RDS Host:    ${GREEN}${RDS_HOST}${NC}"
echo -e "S3 Bucket:   ${GREEN}${S3_BUCKET}${NC}"
echo -e "ECR:         ${GREEN}${ECR_REGISTRY}/${ECR_REPOSITORY}${NC}"
echo -e "S3 Auth:     ${GREEN}IAM instance profile (no static keys)${NC}"
echo ""

# --- Step 1: Create worker secret ---
echo -e "${YELLOW}==> Step 1: Creating worker secret (${WORKER_SECRET_NAME})...${NC}"

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

if aws secretsmanager describe-secret --secret-id "$WORKER_SECRET_NAME" --region "$REGION" &>/dev/null; then
  echo "Secret '$WORKER_SECRET_NAME' already exists — updating..."
  aws secretsmanager put-secret-value \
    --secret-id "$WORKER_SECRET_NAME" \
    --region "$REGION" \
    --secret-string "$WORKER_SECRET_JSON"
else
  aws secretsmanager create-secret \
    --name "$WORKER_SECRET_NAME" \
    --region "$REGION" \
    --description "OpenSandbox worker secrets (JWT, Redis, S3, DB, ECR)" \
    --secret-string "$WORKER_SECRET_JSON"
fi

WORKER_SECRET_ARN=$(aws secretsmanager describe-secret \
  --secret-id "$WORKER_SECRET_NAME" \
  --region "$REGION" \
  --query 'ARN' --output text)

echo -e "Worker Secret ARN: ${GREEN}${WORKER_SECRET_ARN}${NC}"
echo ""

# --- Step 2: Create server secret ---
echo -e "${YELLOW}==> Step 2: Creating server secret (${SERVER_SECRET_NAME})...${NC}"

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

# --- Step 3: Create IAM policy ---
echo -e "${YELLOW}==> Step 3: Creating IAM policy...${NC}"

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
  # Delete oldest non-default version if at limit (max 5 versions)
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

echo -e "Policy ARN: ${GREEN}${POLICY_ARN}${NC}"
echo ""

# --- Step 4: Create IAM role + instance profile ---
echo -e "${YELLOW}==> Step 4: Creating IAM role and instance profile...${NC}"

TRUST_POLICY=$(cat <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF
)

if aws iam get-role --role-name "$ROLE_NAME" &>/dev/null; then
  echo "Role '$ROLE_NAME' already exists"
else
  aws iam create-role \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document "$TRUST_POLICY" \
    --description "OpenSandbox EC2 role (workers + server)"
  echo "Created role '$ROLE_NAME'"
fi

aws iam attach-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-arn "$POLICY_ARN" 2>/dev/null || true
echo "Attached policy to role"

if aws iam get-instance-profile --instance-profile-name "$INSTANCE_PROFILE_NAME" &>/dev/null; then
  echo "Instance profile '$INSTANCE_PROFILE_NAME' already exists"
else
  aws iam create-instance-profile \
    --instance-profile-name "$INSTANCE_PROFILE_NAME"
  echo "Created instance profile '$INSTANCE_PROFILE_NAME'"

  aws iam add-role-to-instance-profile \
    --instance-profile-name "$INSTANCE_PROFILE_NAME" \
    --role-name "$ROLE_NAME"
  echo "Added role to instance profile"
fi

echo ""

# --- Step 5: Attach to existing instances ---
echo -e "${YELLOW}==> Step 5: Attaching instance profile to running instances...${NC}"

ALL_INSTANCES=$(aws ec2 describe-instances \
  --region "$REGION" \
  --filters "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[?Tags[?Key==`Name` && contains(Value, `opensandbox`)]].InstanceId' \
  --output text 2>/dev/null || echo "")

if [ -n "$ALL_INSTANCES" ] && [ "$ALL_INSTANCES" != "None" ]; then
  for INSTANCE_ID in $ALL_INSTANCES; do
    echo -n "  Attaching to $INSTANCE_ID... "
    aws ec2 associate-iam-instance-profile \
      --region "$REGION" \
      --instance-id "$INSTANCE_ID" \
      --iam-instance-profile Name="$INSTANCE_PROFILE_NAME" 2>/dev/null \
      && echo "done" \
      || echo "(already attached or error)"
  done
else
  echo "No running opensandbox instances found."
fi

echo ""

# --- Done ---
echo -e "${GREEN}============================================${NC}"
echo -e "${GREEN} Setup complete!${NC}"
echo ""
echo "Worker Secret ARN: $WORKER_SECRET_ARN"
echo "Server Secret ARN: $SERVER_SECRET_ARN"
echo "IAM Role:          $ROLE_NAME"
echo "Instance Profile:  $INSTANCE_PROFILE_NAME"
echo ""
echo "Next steps:"
echo ""
echo "  Worker — update systemd unit to use Secrets Manager:"
echo "    sudo systemctl edit opensandbox-worker --force"
echo "    # Replace inline secrets with:"
echo "    #   Environment=OPENSANDBOX_SECRETS_ARN=${WORKER_SECRET_ARN}"
echo ""
echo "  Server — update env file to use Secrets Manager:"
echo "    Add to /etc/opensandbox/server.env:"
echo "      OPENSANDBOX_SECRETS_ARN=${SERVER_SECRET_ARN}"
echo "    Then remove: DATABASE_URL, OPENSANDBOX_REDIS_URL, OPENSANDBOX_JWT_SECRET,"
echo "      OPENSANDBOX_S3_*, OPENSANDBOX_ECR_*, WORKOS_*"
echo ""
echo -e "${GREEN}============================================${NC}"
