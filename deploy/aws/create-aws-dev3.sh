#!/usr/bin/env bash
# create-aws-dev3.sh — Create a third dev OpenSandbox cell on AWS.
#
# This is the AWS counterpart to deploy/azure/create-azure-dev2.sh. It creates
# a fully isolated cell (VPC + subnet + IAM + EC2 instances + Secrets Manager)
# in AWS so we can validate cross-cloud cell behaviour against the existing
# Azure dev2 cell.
#
# Cost estimate (idle): ~$280/mo with default sizing. Use the `stop` command
# to deallocate when not testing.
#
# Prerequisites:
#   - aws CLI installed and authenticated (aws sts get-caller-identity should work)
#   - SSH key at ~/.ssh/opensandbox-digger.pem (same key as Azure dev for consistency)
#
# Usage:
#   bash deploy/aws/create-aws-dev3.sh create   # provision (run once)
#   bash deploy/aws/create-aws-dev3.sh deploy   # build + deploy binaries
#   bash deploy/aws/create-aws-dev3.sh status   # show IPs and status
#   bash deploy/aws/create-aws-dev3.sh ssh-cp   # SSH to control plane
#   bash deploy/aws/create-aws-dev3.sh ssh-wk   # SSH to worker
#   bash deploy/aws/create-aws-dev3.sh start    # start both instances (after stop)
#   bash deploy/aws/create-aws-dev3.sh stop     # stop instances to save cost
#   bash deploy/aws/create-aws-dev3.sh destroy  # tear down everything

set -euo pipefail

# ── Configuration ──
REGION="us-east-1"
NAME_PREFIX="osb-dev3"
VPC_CIDR="10.120.0.0/16"
SUBNET_CIDR="10.120.1.0/24"
SG_CP_NAME="${NAME_PREFIX}-cp"
SG_WK_NAME="${NAME_PREFIX}-wk"
CP_NAME="${NAME_PREFIX}-cp"
WK_NAME="${NAME_PREFIX}-worker-1"

# Instance sizing — NVMe-equipped, cost-optimised for a dev testbed
CP_TYPE="t3.medium"   # 2 vCPU / 4GB / EBS only — postgres + redis + CP at dev scale
WK_TYPE="m6id.large"  # 2 vCPU / 8GB / 118GB local NVMe — small worker for cross-region tests
WK_DATA_GB=128         # Additional persistent EBS volume mounted at /data

# Ubuntu 24.04 LTS amd64 (latest from Canonical's account 099720109477) — overridable
AMI_OWNER="099720109477"
AMI_FILTER="ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"

# CF-cutover specifics (cell ID encodes cloud + region for clarity)
CELL_ID="aws-us-east-1-cell-a"
CF_EVENT_ENDPOINT="https://opensandbox-events-ingest-dev.brian-124.workers.dev/ingest"

SSH_KEY="$HOME/.ssh/opensandbox-digger.pem"
SSH_PUB=$(ssh-keygen -y -f "$SSH_KEY" 2>/dev/null)
ADMIN_USER="ubuntu"
DOMAIN="dev3.opensandbox.ai"

STATE_FILE="$HOME/.opensandbox-aws-dev3-state"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

log() { echo "[dev3-aws] $*"; }
err() { echo "[dev3-aws] ERROR: $*" >&2; }

save_state() { echo "$1=$2" >> "$STATE_FILE"; }
load_state() {
    if [ -f "$STATE_FILE" ]; then
        # shellcheck disable=SC1090
        source "$STATE_FILE"
    fi
}

# ── Create ──
cmd_create() {
    log "Creating dev3 stack in $REGION..."

    rm -f "$STATE_FILE"

    # Generate secrets up front
    API_KEY="osb_$(openssl rand -hex 32)"
    JWT_SECRET=$(openssl rand -hex 32)
    PG_PASSWORD=$(openssl rand -hex 16)
    SESSION_JWT_SECRET=$(openssl rand -hex 32)
    CF_EVENT_SECRET=$(openssl rand -hex 32)
    CF_ADMIN_SECRET=$(openssl rand -hex 32)
    save_state "API_KEY" "$API_KEY"
    save_state "JWT_SECRET" "$JWT_SECRET"
    save_state "PG_PASSWORD" "$PG_PASSWORD"
    save_state "SESSION_JWT_SECRET" "$SESSION_JWT_SECRET"
    save_state "CF_EVENT_SECRET" "$CF_EVENT_SECRET"
    save_state "CF_ADMIN_SECRET" "$CF_ADMIN_SECRET"

    # Resolve latest Ubuntu 24.04 AMI
    log "Finding latest Ubuntu 24.04 AMI..."
    AMI_ID=$(aws ec2 describe-images --region "$REGION" \
        --owners "$AMI_OWNER" \
        --filters "Name=name,Values=$AMI_FILTER" "Name=state,Values=available" \
        --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' \
        --output text)
    if [ -z "$AMI_ID" ] || [ "$AMI_ID" = "None" ]; then
        err "Could not resolve Ubuntu 24.04 AMI in $REGION"
        exit 1
    fi
    save_state "AMI_ID" "$AMI_ID"
    log "AMI: $AMI_ID"

    # ── VPC ──
    log "Creating VPC ($VPC_CIDR)..."
    VPC_ID=$(aws ec2 create-vpc --region "$REGION" \
        --cidr-block "$VPC_CIDR" \
        --tag-specifications "ResourceType=vpc,Tags=[{Key=Name,Value=${NAME_PREFIX}-vpc}]" \
        --query 'Vpc.VpcId' --output text)
    save_state "VPC_ID" "$VPC_ID"
    aws ec2 modify-vpc-attribute --region "$REGION" --vpc-id "$VPC_ID" --enable-dns-hostnames

    # Subnet
    SUBNET_ID=$(aws ec2 create-subnet --region "$REGION" \
        --vpc-id "$VPC_ID" --cidr-block "$SUBNET_CIDR" \
        --tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=${NAME_PREFIX}-subnet}]" \
        --query 'Subnet.SubnetId' --output text)
    save_state "SUBNET_ID" "$SUBNET_ID"
    aws ec2 modify-subnet-attribute --region "$REGION" --subnet-id "$SUBNET_ID" --map-public-ip-on-launch

    # Internet gateway
    IGW_ID=$(aws ec2 create-internet-gateway --region "$REGION" \
        --tag-specifications "ResourceType=internet-gateway,Tags=[{Key=Name,Value=${NAME_PREFIX}-igw}]" \
        --query 'InternetGateway.InternetGatewayId' --output text)
    save_state "IGW_ID" "$IGW_ID"
    aws ec2 attach-internet-gateway --region "$REGION" --internet-gateway-id "$IGW_ID" --vpc-id "$VPC_ID"

    # Route table
    RTB_ID=$(aws ec2 create-route-table --region "$REGION" --vpc-id "$VPC_ID" \
        --tag-specifications "ResourceType=route-table,Tags=[{Key=Name,Value=${NAME_PREFIX}-rtb}]" \
        --query 'RouteTable.RouteTableId' --output text)
    save_state "RTB_ID" "$RTB_ID"
    aws ec2 create-route --region "$REGION" --route-table-id "$RTB_ID" --destination-cidr-block 0.0.0.0/0 --gateway-id "$IGW_ID" >/dev/null
    aws ec2 associate-route-table --region "$REGION" --route-table-id "$RTB_ID" --subnet-id "$SUBNET_ID" >/dev/null

    # ── Security groups ──
    log "Creating security groups..."
    SG_CP=$(aws ec2 create-security-group --region "$REGION" \
        --group-name "$SG_CP_NAME" --description "$NAME_PREFIX control plane" \
        --vpc-id "$VPC_ID" --query 'GroupId' --output text)
    save_state "SG_CP" "$SG_CP"
    for port in 22 8080 9090; do
        aws ec2 authorize-security-group-ingress --region "$REGION" \
            --group-id "$SG_CP" --protocol tcp --port "$port" --cidr 0.0.0.0/0 >/dev/null
    done
    # Postgres + Redis from within the VPC only
    for port in 5432 6379; do
        aws ec2 authorize-security-group-ingress --region "$REGION" \
            --group-id "$SG_CP" --protocol tcp --port "$port" --cidr "$VPC_CIDR" >/dev/null
    done

    SG_WK=$(aws ec2 create-security-group --region "$REGION" \
        --group-name "$SG_WK_NAME" --description "$NAME_PREFIX worker" \
        --vpc-id "$VPC_ID" --query 'GroupId' --output text)
    save_state "SG_WK" "$SG_WK"
    aws ec2 authorize-security-group-ingress --region "$REGION" \
        --group-id "$SG_WK" --protocol tcp --port 22 --cidr 0.0.0.0/0 >/dev/null
    for port in 9090 8081; do
        aws ec2 authorize-security-group-ingress --region "$REGION" \
            --group-id "$SG_WK" --protocol tcp --port "$port" --cidr "$VPC_CIDR" >/dev/null
    done

    # ── Key pair ──
    log "Creating key pair..."
    aws ec2 import-key-pair --region "$REGION" \
        --key-name "$NAME_PREFIX" \
        --public-key-material "fileb://<(echo "$SSH_PUB")" >/dev/null 2>&1 || \
    aws ec2 import-key-pair --region "$REGION" \
        --key-name "$NAME_PREFIX" \
        --public-key-material "$(echo -n "$SSH_PUB" | base64)" >/dev/null

    # ── S3 bucket for checkpoints ──
    BUCKET="osb-dev3-checkpoints-$(echo $RANDOM | md5 2>/dev/null | head -c 8 || echo $RANDOM | md5sum | head -c 8)"
    log "Creating S3 bucket $BUCKET..."
    aws s3api create-bucket --region "$REGION" --bucket "$BUCKET" \
        $([ "$REGION" != "us-east-1" ] && echo "--create-bucket-configuration LocationConstraint=$REGION") >/dev/null
    save_state "BUCKET" "$BUCKET"

    # ── Control Plane EC2 ──
    log "Launching control plane ($CP_TYPE)..."
    CP_USERDATA=$(cat <<'CPUD'
#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq docker.io jq curl
systemctl enable --now docker
mkdir -p /etc/opensandbox /usr/local/bin /data/postgres
CPUD
)
    CP_ID=$(aws ec2 run-instances --region "$REGION" \
        --image-id "$AMI_ID" --instance-type "$CP_TYPE" \
        --key-name "$NAME_PREFIX" --subnet-id "$SUBNET_ID" \
        --security-group-ids "$SG_CP" \
        --associate-public-ip-address \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$CP_NAME},{Key=opensandbox:role,Value=controlplane}]" \
        --user-data "$CP_USERDATA" \
        --query 'Instances[0].InstanceId' --output text)
    save_state "CP_ID" "$CP_ID"
    log "Waiting for control plane to enter running state..."
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$CP_ID"
    CP_PUBLIC_IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$CP_ID" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    CP_PRIVATE_IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$CP_ID" \
        --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
    save_state "CP_PUBLIC_IP" "$CP_PUBLIC_IP"
    save_state "CP_PRIVATE_IP" "$CP_PRIVATE_IP"
    log "Control plane: public=$CP_PUBLIC_IP private=$CP_PRIVATE_IP"

    # ── Worker EC2 ──
    log "Launching worker ($WK_TYPE)..."
    WK_ID=$(aws ec2 run-instances --region "$REGION" \
        --image-id "$AMI_ID" --instance-type "$WK_TYPE" \
        --key-name "$NAME_PREFIX" --subnet-id "$SUBNET_ID" \
        --security-group-ids "$SG_WK" \
        --associate-public-ip-address \
        --block-device-mappings "DeviceName=/dev/sdb,Ebs={VolumeSize=$WK_DATA_GB,VolumeType=gp3,DeleteOnTermination=true}" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$WK_NAME},{Key=opensandbox:role,Value=worker}]" \
        --query 'Instances[0].InstanceId' --output text)
    save_state "WK_ID" "$WK_ID"
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$WK_ID"
    WK_PUBLIC_IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$WK_ID" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    WK_PRIVATE_IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$WK_ID" \
        --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
    save_state "WK_PUBLIC_IP" "$WK_PUBLIC_IP"
    save_state "WK_PRIVATE_IP" "$WK_PRIVATE_IP"
    log "Worker: public=$WK_PUBLIC_IP private=$WK_PRIVATE_IP"

    # ── Wait for SSH ──
    log "Waiting for SSH on control plane..."
    until ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
        "$ADMIN_USER@$CP_PUBLIC_IP" "echo ready" 2>/dev/null; do
        sleep 5
    done

    # ── Provision control plane (Postgres + Redis containers) ──
    log "Provisioning control plane..."
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" "PG_PASSWORD='$PG_PASSWORD' bash -s" <<'CPSETUP'
set -euo pipefail
sudo apt-get update -qq
sudo apt-get install -y -qq docker.io jq curl
sudo systemctl enable --now docker
sudo mkdir -p /data/postgres /etc/opensandbox /usr/local/bin

sudo docker run -d --name postgres \
    --restart unless-stopped \
    --shm-size=2g \
    -p 5432:5432 \
    -e POSTGRES_USER=opensandbox \
    -e POSTGRES_PASSWORD="$PG_PASSWORD" \
    -e POSTGRES_DB=opensandbox \
    -v /data/postgres:/var/lib/postgresql/data \
    postgres:16 \
    postgres -c shared_buffers=512MB -c effective_cache_size=2GB -c synchronous_commit=off -c max_connections=100

sudo docker run -d --name redis --restart unless-stopped -p 6379:6379 redis:7-alpine

for i in $(seq 1 30); do
    sudo docker exec postgres pg_isready -U opensandbox 2>/dev/null && break
    sleep 2
done
echo "Control plane provisioned."
CPSETUP

    # ── Provision worker (mount NVMe + install QEMU host packages) ──
    log "Waiting for SSH on worker..."
    until ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
        "$ADMIN_USER@$WK_PUBLIC_IP" "echo ready" 2>/dev/null; do
        sleep 5
    done
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        "$SCRIPT_DIR/../azure/setup-azure-host.sh" "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/setup-host.sh"
    log "Provisioning worker (running setup-host.sh)..."
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "bash -s" <<'WKSETUP'
set -euo pipefail
# Mount instance NVMe (m6id.large exposes /dev/nvme1n1) at /data, fall back to EBS
if ! mountpoint -q /data 2>/dev/null; then
    DATA_DISK=""
    for d in /dev/nvme1n1 /dev/nvme2n1 /dev/sdb /dev/xvdb; do
        if [ -b "$d" ] && ! blkid "$d" >/dev/null 2>&1; then
            DATA_DISK="$d"
            break
        fi
    done
    if [ -n "$DATA_DISK" ]; then
        sudo mkfs.xfs -f -m reflink=1 "$DATA_DISK"
        sudo mkdir -p /data
        echo "$DATA_DISK /data xfs defaults,nofail 0 2" | sudo tee -a /etc/fstab
        sudo mount /data
    fi
fi
sudo bash /tmp/setup-host.sh
WKSETUP

    # ── Env files ──
    log "Writing environment files..."
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" "sudo tee /etc/opensandbox/server.env > /dev/null" <<CPENV
OPENSANDBOX_MODE=server
OPENSANDBOX_PORT=8080
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:$PG_PASSWORD@localhost:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://localhost:6379
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_API_KEY=$API_KEY
OPENSANDBOX_REGION=$REGION
OPENSANDBOX_SANDBOX_DOMAIN=$DOMAIN
OPENSANDBOX_CELL_ID=$CELL_ID
OPENSANDBOX_CF_EVENT_ENDPOINT=$CF_EVENT_ENDPOINT
OPENSANDBOX_CF_EVENT_SECRET=$CF_EVENT_SECRET
OPENSANDBOX_CF_ADMIN_SECRET=$CF_ADMIN_SECRET
OPENSANDBOX_SESSION_JWT_SECRET=$SESSION_JWT_SECRET
OPENSANDBOX_COMPUTE_PROVIDER=aws
CPENV

    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" "sudo tee /etc/opensandbox/worker.env > /dev/null" <<WKENV
OPENSANDBOX_MODE=worker
OPENSANDBOX_VM_BACKEND=qemu
OPENSANDBOX_QEMU_BIN=qemu-system-x86_64
OPENSANDBOX_DATA_DIR=/data/sandboxes
OPENSANDBOX_KERNEL_PATH=/opt/opensandbox/vmlinux
OPENSANDBOX_IMAGES_DIR=/data/firecracker/images
OPENSANDBOX_GRPC_ADVERTISE=$WK_PRIVATE_IP:9090
OPENSANDBOX_HTTP_ADDR=http://$WK_PRIVATE_IP:8081
OPENSANDBOX_JWT_SECRET=$JWT_SECRET
OPENSANDBOX_WORKER_ID=w-aws-${REGION}-dev3-1
OPENSANDBOX_REGION=$REGION
OPENSANDBOX_MAX_CAPACITY=10
OPENSANDBOX_PORT=8081
OPENSANDBOX_DEFAULT_SANDBOX_MEMORY_MB=1024
OPENSANDBOX_DEFAULT_SANDBOX_CPUS=2
OPENSANDBOX_DATABASE_URL=postgres://opensandbox:$PG_PASSWORD@$CP_PRIVATE_IP:5432/opensandbox?sslmode=disable
OPENSANDBOX_REDIS_URL=redis://$CP_PRIVATE_IP:6379
OPENSANDBOX_S3_BUCKET=$BUCKET
OPENSANDBOX_S3_REGION=$REGION
OPENSANDBOX_CELL_ID=$CELL_ID
WKENV

    # Postgres VPC access
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" <<PGFIX
sudo docker exec postgres bash -c "echo 'host all all $VPC_CIDR md5' >> /var/lib/postgresql/data/pg_hba.conf"
sudo docker exec postgres mkdir -p /var/lib/postgresql/data/conf.d 2>/dev/null || true
sudo docker exec postgres bash -c "echo \"listen_addresses = '*'\" > /var/lib/postgresql/data/conf.d/listen.conf"
sudo docker restart postgres
PGFIX

    log ""
    log "=== dev3-aws stack created ==="
    log "Region:         $REGION"
    log "Cell ID:        $CELL_ID"
    log "Control plane:  $CP_PUBLIC_IP (private: $CP_PRIVATE_IP)"
    log "Worker:         $WK_PUBLIC_IP (private: $WK_PRIVATE_IP)"
    log "S3 bucket:      $BUCKET"
    log "API key:        $API_KEY"
    log ""
    log "EVENT_SECRET to push to CF events-ingest (matches what's baked here):"
    log "  echo '$CF_EVENT_SECRET' | npx wrangler secret put EVENT_SECRET"
    log "  (note: same Worker handles both cells; secret should match Azure dev2's)"
    log ""
    log "Next: bash deploy/aws/create-aws-dev3.sh deploy"
}

# ── Deploy ──
cmd_deploy() {
    load_state
    log "Building and deploying binaries to dev3-aws..."

    cd "$PROJECT_ROOT"

    log "Building server..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/opensandbox-server-dev3 ./cmd/server/

    log "Building worker..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/opensandbox-worker-dev3 ./cmd/worker/

    log "Building agent..."
    GOOS=linux GOARCH=amd64 go build -o /tmp/osb-agent-dev3 ./cmd/agent/

    log "Deploying to control plane ($CP_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/opensandbox-server-dev3 "$ADMIN_USER@$CP_PUBLIC_IP:/tmp/opensandbox-server"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP" \
        'sudo systemctl stop opensandbox-server 2>/dev/null || true; sudo cp /tmp/opensandbox-server /usr/local/bin/ && sudo systemctl start opensandbox-server || sudo /usr/local/bin/opensandbox-server &'

    log "Deploying to worker ($WK_PUBLIC_IP)..."
    scp -i "$SSH_KEY" -o StrictHostKeyChecking=no \
        /tmp/opensandbox-worker-dev3 /tmp/osb-agent-dev3 \
        "$ADMIN_USER@$WK_PUBLIC_IP:/tmp/"
    ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP" <<'DEPLOY'
set -euo pipefail
sudo systemctl stop opensandbox-worker 2>/dev/null || true
sudo cp /tmp/opensandbox-worker-dev3 /usr/local/bin/opensandbox-worker
sudo cp /tmp/osb-agent-dev3 /usr/local/bin/osb-agent
sudo systemctl start opensandbox-worker || true
DEPLOY

    log ""
    log "=== Deployed to dev3-aws ==="
    log "Smoke test: curl -s http://$CP_PUBLIC_IP:8080/api/sandboxes -H \"X-API-Key: $API_KEY\""
}

cmd_status() {
    load_state
    log "=== dev3-aws stack ==="
    log "Region:        $REGION"
    log "Cell ID:       $CELL_ID"
    log "VPC:           ${VPC_ID:-?}"
    log ""
    log "Control plane: ${CP_PUBLIC_IP:-?} (private: ${CP_PRIVATE_IP:-?})"
    aws ec2 describe-instances --region "$REGION" --instance-ids "${CP_ID:-}" \
        --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "  (not found)"
    log ""
    log "Worker:        ${WK_PUBLIC_IP:-?} (private: ${WK_PRIVATE_IP:-?})"
    aws ec2 describe-instances --region "$REGION" --instance-ids "${WK_ID:-}" \
        --query 'Reservations[0].Instances[0].State.Name' --output text 2>/dev/null || echo "  (not found)"
    log ""
    log "S3 bucket:     ${BUCKET:-?}"
    log "API key:       ${API_KEY:-?}"
}

cmd_ssh_cp() {
    load_state
    exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$CP_PUBLIC_IP"
}

cmd_ssh_wk() {
    load_state
    exec ssh -i "$SSH_KEY" -o StrictHostKeyChecking=no "$ADMIN_USER@$WK_PUBLIC_IP"
}

cmd_stop() {
    load_state
    log "Stopping dev3-aws instances (use 'start' to resume)..."
    aws ec2 stop-instances --region "$REGION" --instance-ids "$CP_ID" "$WK_ID" >/dev/null
    log "Stop initiated."
}

cmd_start() {
    load_state
    log "Starting dev3-aws instances..."
    aws ec2 start-instances --region "$REGION" --instance-ids "$CP_ID" "$WK_ID" >/dev/null
    log "Start initiated. Note: public IPs may change unless you've allocated EIPs."
}

cmd_destroy() {
    load_state
    log "This will DELETE all dev3-aws resources (VPC, instances, security groups, S3 bucket)."
    read -p "Are you sure? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        log "Aborted."
        exit 0
    fi
    aws ec2 terminate-instances --region "$REGION" --instance-ids "$CP_ID" "$WK_ID" 2>/dev/null || true
    log "Waiting for instances to terminate..."
    aws ec2 wait instance-terminated --region "$REGION" --instance-ids "$CP_ID" "$WK_ID" 2>/dev/null || true
    aws ec2 delete-security-group --region "$REGION" --group-id "$SG_CP" 2>/dev/null || true
    aws ec2 delete-security-group --region "$REGION" --group-id "$SG_WK" 2>/dev/null || true
    aws ec2 disassociate-route-table --region "$REGION" --association-id "$(aws ec2 describe-route-tables --region $REGION --route-table-ids $RTB_ID --query 'RouteTables[0].Associations[0].RouteTableAssociationId' --output text)" 2>/dev/null || true
    aws ec2 delete-route-table --region "$REGION" --route-table-id "$RTB_ID" 2>/dev/null || true
    aws ec2 detach-internet-gateway --region "$REGION" --internet-gateway-id "$IGW_ID" --vpc-id "$VPC_ID" 2>/dev/null || true
    aws ec2 delete-internet-gateway --region "$REGION" --internet-gateway-id "$IGW_ID" 2>/dev/null || true
    aws ec2 delete-subnet --region "$REGION" --subnet-id "$SUBNET_ID" 2>/dev/null || true
    aws ec2 delete-vpc --region "$REGION" --vpc-id "$VPC_ID" 2>/dev/null || true
    aws s3 rb "s3://$BUCKET" --force 2>/dev/null || true
    aws ec2 delete-key-pair --region "$REGION" --key-name "$NAME_PREFIX" 2>/dev/null || true
    rm -f "$STATE_FILE"
    log "dev3-aws teardown complete."
}

case "${1:-}" in
    create)  cmd_create ;;
    deploy)  cmd_deploy ;;
    status)  cmd_status ;;
    ssh-cp)  cmd_ssh_cp ;;
    ssh-wk)  cmd_ssh_wk ;;
    start)   cmd_start ;;
    stop)    cmd_stop ;;
    destroy) cmd_destroy ;;
    *)
        echo "Usage: $0 {create|deploy|status|ssh-cp|ssh-wk|start|stop|destroy}"
        exit 1
        ;;
esac
