#!/usr/bin/env bash
# deploy-aws-dev.sh — Deploy a dev environment on a single EC2 instance using AWS CLI
#
# Provisions a bare-metal ARM64 instance, sets up OpenSandbox (server + worker),
# and deploys the current codebase. No Terraform needed.
#
# Usage:
#   ./deploy/ec2/deploy-aws-dev.sh [create|deploy|status|ssh|destroy]
#
# Commands:
#   create   — Provision EC2 instance (VPC, SG, key pair, instance)
#   deploy   — Build and deploy code to the instance (re-runnable)
#   status   — Show instance status and connection info
#   ssh      — SSH into the instance
#   destroy  — Tear down all resources
#
# Configuration (env vars):
#   AWS_REGION          — AWS region (default: us-east-2)
#   INSTANCE_TYPE       — EC2 instance type (default: a1.metal)
#   KEY_NAME            — EC2 key pair name (default: opensandbox-dev-<region>)
#   API_KEY             — API key for the server (default: test-key)
#   DATA_VOLUME_SIZE_GB — EBS data volume size in GB (default: 200)
#
# State is stored in deploy/ec2/.dev-env-state to track created resources.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
STATE_FILE="$SCRIPT_DIR/.dev-env-state-${AWS_REGION}"

# Defaults
AWS_REGION="${AWS_REGION:-us-east-2}"
INSTANCE_TYPE="${INSTANCE_TYPE:-a1.metal}"
KEY_NAME="${KEY_NAME:-opensandbox-dev-${AWS_REGION}}"
API_KEY="${API_KEY:-test-key}"
DATA_VOLUME_SIZE_GB="${DATA_VOLUME_SIZE_GB:-200}"
PROJECT_TAG="opensandbox-dev"

log()  { echo "==> $*"; }
info() { echo "    $*"; }
err()  { echo "ERROR: $*" >&2; exit 1; }

# --- State management ---

save_state() {
    local key="$1" value="$2"
    if [ -f "$STATE_FILE" ] && grep -q "^${key}=" "$STATE_FILE" 2>/dev/null; then
        sed -i.bak "s|^${key}=.*|${key}=${value}|" "$STATE_FILE"
        rm -f "${STATE_FILE}.bak"
    else
        echo "${key}=${value}" >> "$STATE_FILE"
    fi
}

load_state() {
    local key="$1"
    if [ -f "$STATE_FILE" ]; then
        grep "^${key}=" "$STATE_FILE" 2>/dev/null | cut -d= -f2- || true
    fi
}

# --- AMI lookup ---

lookup_arm64_ami() {
    log "Looking up latest Ubuntu 24.04 ARM64 AMI in ${AWS_REGION}..." >&2
    local ami_id
    ami_id=$(aws ec2 describe-images \
        --region "$AWS_REGION" \
        --owners 099720109477 \
        --filters \
            "Name=name,Values=ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*" \
            "Name=architecture,Values=arm64" \
            "Name=state,Values=available" \
        --query 'Images | sort_by(@, &CreationDate) | [-1].ImageId' \
        --output text)

    if [ -z "$ami_id" ] || [ "$ami_id" = "None" ]; then
        err "Could not find Ubuntu 24.04 ARM64 AMI in ${AWS_REGION}"
    fi
    info "AMI: $ami_id" >&2
    echo "$ami_id"
}

# --- Create infrastructure ---

cmd_create() {
    # Check for existing state
    local existing_instance
    existing_instance=$(load_state INSTANCE_ID)
    if [ -n "$existing_instance" ]; then
        local state
        state=$(aws ec2 describe-instances \
            --region "$AWS_REGION" \
            --instance-ids "$existing_instance" \
            --query 'Reservations[0].Instances[0].State.Name' \
            --output text 2>/dev/null || echo "not-found")
        if [ "$state" = "running" ] || [ "$state" = "pending" ]; then
            info "Instance $existing_instance already exists (state: $state)"
            info "Run '$0 deploy' to deploy code, or '$0 destroy' first to recreate."
            cmd_status
            return 0
        fi
    fi

    # Pre-flight: verify instance type is available in this region
    log "Checking ${INSTANCE_TYPE} availability in ${AWS_REGION}..."
    local avail_azs
    avail_azs=$(aws ec2 describe-instance-type-offerings \
        --region "$AWS_REGION" \
        --location-type availability-zone \
        --filters "Name=instance-type,Values=${INSTANCE_TYPE}" \
        --query 'InstanceTypeOfferings[].Location' --output text)
    if [ -z "$avail_azs" ]; then
        err "${INSTANCE_TYPE} is not available in ${AWS_REGION}. Try us-east-1 or us-east-2."
    fi
    info "Available in: $avail_azs"

    save_state REGION "$AWS_REGION"
    save_state INSTANCE_TYPE "$INSTANCE_TYPE"

    # 1. VPC
    log "Creating VPC..."
    local vpc_id
    vpc_id=$(aws ec2 create-vpc \
        --region "$AWS_REGION" \
        --cidr-block 10.0.0.0/16 \
        --tag-specifications "ResourceType=vpc,Tags=[{Key=Name,Value=${PROJECT_TAG}-vpc},{Key=Project,Value=${PROJECT_TAG}}]" \
        --query 'Vpc.VpcId' --output text)
    save_state VPC_ID "$vpc_id"
    info "VPC: $vpc_id"

    # Enable DNS hostnames
    aws ec2 modify-vpc-attribute --region "$AWS_REGION" --vpc-id "$vpc_id" --enable-dns-hostnames '{"Value":true}'

    # 2. Internet Gateway
    log "Creating Internet Gateway..."
    local igw_id
    igw_id=$(aws ec2 create-internet-gateway \
        --region "$AWS_REGION" \
        --tag-specifications "ResourceType=internet-gateway,Tags=[{Key=Name,Value=${PROJECT_TAG}-igw},{Key=Project,Value=${PROJECT_TAG}}]" \
        --query 'InternetGateway.InternetGatewayId' --output text)
    aws ec2 attach-internet-gateway --region "$AWS_REGION" --internet-gateway-id "$igw_id" --vpc-id "$vpc_id"
    save_state IGW_ID "$igw_id"
    info "IGW: $igw_id"

    # 3. Subnet (public)
    log "Creating subnet..."
    # Pick first AZ where the instance type is available
    local az
    az=$(echo "$avail_azs" | tr '\t' '\n' | head -1)
    local subnet_id
    subnet_id=$(aws ec2 create-subnet \
        --region "$AWS_REGION" \
        --vpc-id "$vpc_id" \
        --cidr-block 10.0.1.0/24 \
        --availability-zone "$az" \
        --tag-specifications "ResourceType=subnet,Tags=[{Key=Name,Value=${PROJECT_TAG}-public},{Key=Project,Value=${PROJECT_TAG}}]" \
        --query 'Subnet.SubnetId' --output text)
    aws ec2 modify-subnet-attribute --region "$AWS_REGION" --subnet-id "$subnet_id" --map-public-ip-on-launch
    save_state SUBNET_ID "$subnet_id"
    info "Subnet: $subnet_id ($az)"

    # 4. Route table
    log "Setting up route table..."
    local rtb_id
    rtb_id=$(aws ec2 create-route-table \
        --region "$AWS_REGION" \
        --vpc-id "$vpc_id" \
        --tag-specifications "ResourceType=route-table,Tags=[{Key=Name,Value=${PROJECT_TAG}-public-rt},{Key=Project,Value=${PROJECT_TAG}}]" \
        --query 'RouteTable.RouteTableId' --output text)
    aws ec2 create-route --region "$AWS_REGION" --route-table-id "$rtb_id" --destination-cidr-block 0.0.0.0/0 --gateway-id "$igw_id" > /dev/null
    aws ec2 associate-route-table --region "$AWS_REGION" --route-table-id "$rtb_id" --subnet-id "$subnet_id" > /dev/null
    save_state RTB_ID "$rtb_id"

    # 5. Security Group
    log "Creating security group..."
    local sg_id
    sg_id=$(aws ec2 create-security-group \
        --region "$AWS_REGION" \
        --group-name "${PROJECT_TAG}-sg" \
        --description "OpenSandbox dev: SSH + API" \
        --vpc-id "$vpc_id" \
        --tag-specifications "ResourceType=security-group,Tags=[{Key=Name,Value=${PROJECT_TAG}-sg},{Key=Project,Value=${PROJECT_TAG}}]" \
        --query 'GroupId' --output text)
    # SSH
    aws ec2 authorize-security-group-ingress --region "$AWS_REGION" --group-id "$sg_id" \
        --protocol tcp --port 22 --cidr 0.0.0.0/0 > /dev/null
    # API (server)
    aws ec2 authorize-security-group-ingress --region "$AWS_REGION" --group-id "$sg_id" \
        --protocol tcp --port 8080 --cidr 0.0.0.0/0 > /dev/null
    # Worker direct access (SDK connectURL)
    aws ec2 authorize-security-group-ingress --region "$AWS_REGION" --group-id "$sg_id" \
        --protocol tcp --port 8081 --cidr 0.0.0.0/0 > /dev/null
    save_state SG_ID "$sg_id"
    info "SG: $sg_id"

    # 6. Key pair
    log "Creating key pair..."
    local key_file="$SCRIPT_DIR/${KEY_NAME}.pem"
    if aws ec2 describe-key-pairs --region "$AWS_REGION" --key-names "$KEY_NAME" &>/dev/null; then
        info "Key pair '$KEY_NAME' already exists"
        if [ ! -f "$key_file" ]; then
            err "Key pair exists in AWS but local file $key_file not found. Delete the key pair in AWS or provide the .pem file."
        fi
    else
        aws ec2 create-key-pair \
            --region "$AWS_REGION" \
            --key-name "$KEY_NAME" \
            --query 'KeyMaterial' --output text > "$key_file"
        chmod 600 "$key_file"
        info "Key pair created: $key_file"
    fi
    save_state KEY_NAME "$KEY_NAME"
    save_state KEY_FILE "$key_file"

    # 7. AMI
    local ami_id
    ami_id=$(lookup_arm64_ami)

    # 8. Launch instance
    log "Launching ${INSTANCE_TYPE} instance..."
    local instance_id
    instance_id=$(aws ec2 run-instances \
        --region "$AWS_REGION" \
        --image-id "$ami_id" \
        --instance-type "$INSTANCE_TYPE" \
        --key-name "$KEY_NAME" \
        --subnet-id "$subnet_id" \
        --security-group-ids "$sg_id" \
        --block-device-mappings \
            "DeviceName=/dev/sda1,Ebs={VolumeSize=100,VolumeType=gp3,DeleteOnTermination=true}" \
            "DeviceName=/dev/sdf,Ebs={VolumeSize=${DATA_VOLUME_SIZE_GB},VolumeType=gp3,DeleteOnTermination=true}" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${PROJECT_TAG}},{Key=Project,Value=${PROJECT_TAG}}]" \
        --query 'Instances[0].InstanceId' --output text)
    save_state INSTANCE_ID "$instance_id"
    info "Instance: $instance_id"

    # Wait for running
    log "Waiting for instance to be running..."
    aws ec2 wait instance-running --region "$AWS_REGION" --instance-ids "$instance_id"

    local public_ip
    public_ip=$(aws ec2 describe-instances \
        --region "$AWS_REGION" \
        --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    save_state PUBLIC_IP "$public_ip"
    info "Public IP: $public_ip"

    # Wait for SSH
    log "Waiting for SSH to be ready (bare-metal instances take a few minutes)..."
    local max_attempts=60
    for i in $(seq 1 $max_attempts); do
        if ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
            -i "$key_file" ubuntu@"$public_ip" "echo ok" &>/dev/null; then
            info "SSH ready after ~$((i * 10))s"
            break
        fi
        if [ "$i" -eq "$max_attempts" ]; then
            err "SSH not ready after $((max_attempts * 10))s. Check instance console."
        fi
        sleep 10
    done

    # Format and mount data volume
    log "Setting up data volume..."
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" '
        # Find the data volume (second NVMe or xvdf)
        DATA_DEV=""
        for dev in /dev/nvme1n1 /dev/xvdf; do
            if [ -b "$dev" ]; then DATA_DEV="$dev"; break; fi
        done
        if [ -n "$DATA_DEV" ]; then
            if ! sudo blkid "$DATA_DEV" 2>/dev/null | grep -q ext4; then
                sudo mkfs.ext4 -q -L opensandbox-data "$DATA_DEV"
            fi
            sudo mkdir -p /data
            if ! mountpoint -q /data; then
                echo "LABEL=opensandbox-data /data ext4 defaults,nofail 0 2" | sudo tee -a /etc/fstab > /dev/null
                sudo mount -a
            fi
            sudo mkdir -p /data/sandboxes /data/firecracker/images /data/checkpoints
            echo "Data volume mounted at /data"
        else
            echo "WARNING: No data volume found, using root volume"
            sudo mkdir -p /data/sandboxes /data/firecracker/images /data/checkpoints
        fi
    '

    # Run setup script
    log "Provisioning instance with setup-single-host.sh..."
    rsync -az --delete \
        -e "ssh -o StrictHostKeyChecking=no -i $key_file" \
        --exclude='.git' \
        --exclude='bin/' \
        --exclude='node_modules/' \
        --exclude='web/dist/' \
        --exclude='.claude/' \
        "$PROJECT_ROOT/" "ubuntu@${public_ip}:~/opensandbox/"
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" \
        "sudo bash ~/opensandbox/deploy/ec2/setup-single-host.sh"

    echo ""
    echo "============================================"
    echo " Instance created and provisioned!"
    echo ""
    echo " Instance: $instance_id ($INSTANCE_TYPE)"
    echo " Region:   $AWS_REGION"
    echo " IP:       $public_ip"
    echo ""
    echo " Next: run '$0 deploy' to build and start services"
    echo "============================================"
}

# --- Deploy code ---

cmd_deploy() {
    local public_ip key_file
    public_ip=$(load_state PUBLIC_IP)
    key_file=$(load_state KEY_FILE)

    if [ -z "$public_ip" ] || [ -z "$key_file" ]; then
        err "No instance found. Run '$0 create' first."
    fi

    # Verify instance is reachable
    if ! ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 -o BatchMode=yes \
        -i "$key_file" ubuntu@"$public_ip" "echo ok" &>/dev/null; then
        err "Cannot reach $public_ip via SSH. Instance may be stopped or IP changed."
    fi

    local branch
    branch=$(git -C "$PROJECT_ROOT" rev-parse --abbrev-ref HEAD)
    log "Deploying branch '$branch' to $public_ip..."

    log "Step 1: Syncing code..."
    rsync -az --delete \
        -e "ssh -o StrictHostKeyChecking=no -i $key_file" \
        --exclude='.git' \
        --exclude='bin/' \
        --exclude='node_modules/' \
        --exclude='web/dist/' \
        --exclude='.claude/' \
        "$PROJECT_ROOT/" "ubuntu@${public_ip}:~/opensandbox/"

    log "Step 2: Building server + worker + agent on instance..."
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" "
        export PATH=\$PATH:/usr/local/go/bin
        cd ~/opensandbox
        mkdir -p bin
        echo '  Building opensandbox-server...'
        CGO_ENABLED=0 go build -o bin/opensandbox-server ./cmd/server/
        echo '  Building opensandbox-worker...'
        CGO_ENABLED=0 go build -o bin/opensandbox-worker ./cmd/worker/
        echo '  Building osb-agent...'
        CGO_ENABLED=0 go build -o bin/osb-agent ./cmd/agent/
        sudo systemctl stop opensandbox-server opensandbox-worker 2>/dev/null || true
        sudo cp bin/opensandbox-server /usr/local/bin/opensandbox-server
        sudo cp bin/opensandbox-worker /usr/local/bin/opensandbox-worker
        sudo cp bin/osb-agent /usr/local/bin/osb-agent
        sudo chmod +x /usr/local/bin/opensandbox-server /usr/local/bin/opensandbox-worker /usr/local/bin/osb-agent
        echo '  Binaries installed'
    "

    log "Step 3: Building rootfs (if needed)..."
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" "
        if [ -f /data/firecracker/images/default.ext4 ]; then
            echo '  Rootfs already exists, skipping (delete /data/firecracker/images/default.ext4 to rebuild)'
        else
            export PATH=\$PATH:/usr/local/go/bin
            cd ~/opensandbox
            echo '  Building rootfs with Docker (takes a few minutes)...'
            sudo bash ./deploy/ec2/build-rootfs-docker.sh /usr/local/bin/osb-agent /data/firecracker/images default
        fi
    "

    log "Step 4: Installing env files..."
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" \
        "sudo bash ~/opensandbox/deploy/ec2/setup-dev-env.sh $API_KEY"
    # Add sandbox domain for preview URLs (nip.io resolves to the public IP)
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" "
        echo 'OPENSANDBOX_SANDBOX_DOMAIN=${public_ip}.nip.io' | sudo tee -a /etc/opensandbox/server.env
        echo 'OPENSANDBOX_SANDBOX_DOMAIN=${public_ip}.nip.io' | sudo tee -a /etc/opensandbox/worker.env
    "

    log "Step 5: Starting/restarting services..."
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" "
        sudo systemctl restart opensandbox-server
        echo '  Server started'
        sudo systemctl restart opensandbox-worker
        echo '  Worker started'

        # Docker sets FORWARD policy to DROP — add rules so Firecracker VMs can reach the internet
        sudo iptables -C FORWARD -s 172.16.0.0/16 -j ACCEPT 2>/dev/null || \
            sudo iptables -I FORWARD -s 172.16.0.0/16 -j ACCEPT
        sudo iptables -C FORWARD -d 172.16.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
            sudo iptables -I FORWARD -d 172.16.0.0/16 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
        echo '  iptables FORWARD rules for VM subnet added'
    "

    log "Step 6: Waiting for migrations, then seeding database..."
    sleep 5
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" "
        for i in \$(seq 1 15); do
            sudo docker exec postgres psql -U opensandbox -d opensandbox -q -c 'SELECT 1 FROM orgs LIMIT 0' 2>/dev/null && break
            echo '  Waiting for migrations...'
            sleep 2
        done
        sudo docker exec postgres psql -U opensandbox -d opensandbox -q -c \"
            INSERT INTO orgs (name, slug) VALUES ('Test Org', 'test-org') ON CONFLICT (slug) DO NOTHING;
            INSERT INTO api_keys (org_id, key_hash, key_prefix, name)
            SELECT id, encode(sha256('${API_KEY}'::bytea), 'hex'), '${API_KEY}', 'Dev Key'
            FROM orgs WHERE slug = 'test-org'
            ON CONFLICT (key_hash) DO NOTHING;
        \" && echo '  Database seeded' || echo '  WARNING: Seed failed'
    "

    log "Step 7: Verifying health..."
    sleep 3
    ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" \
        "curl -sf http://localhost:8080/health > /dev/null 2>&1 && echo '  Server health: OK' || echo '  Server health: waiting...'"

    echo ""
    echo "============================================"
    echo " Deployment complete!"
    echo ""
    echo " Server URL: http://${public_ip}:8080"
    echo " API Key:    ${API_KEY}"
    echo ""
    echo " Test:"
    echo "   curl -X POST http://${public_ip}:8080/api/sandboxes \\"
    echo "     -H 'Content-Type: application/json' \\"
    echo "     -H 'X-API-Key: ${API_KEY}' \\"
    echo "     -d '{\"templateID\":\"default\"}'"
    echo ""
    echo " SSH:  $0 ssh"
    echo " Logs: $0 ssh -- sudo journalctl -u opensandbox-worker -f"
    echo "============================================"
}

# --- Status ---

cmd_status() {
    local instance_id public_ip region
    instance_id=$(load_state INSTANCE_ID)
    public_ip=$(load_state PUBLIC_IP)
    region=$(load_state REGION)

    if [ -z "$instance_id" ]; then
        echo "No dev environment found. Run '$0 create' to set one up."
        return 0
    fi

    local state
    state=$(aws ec2 describe-instances \
        --region "${region:-$AWS_REGION}" \
        --instance-ids "$instance_id" \
        --query 'Reservations[0].Instances[0].State.Name' \
        --output text 2>/dev/null || echo "not-found")

    # Refresh IP if running
    if [ "$state" = "running" ]; then
        public_ip=$(aws ec2 describe-instances \
            --region "${region:-$AWS_REGION}" \
            --instance-ids "$instance_id" \
            --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
        save_state PUBLIC_IP "$public_ip"
    fi

    echo ""
    echo "  Instance:  $instance_id"
    echo "  Type:      $(load_state INSTANCE_TYPE)"
    echo "  Region:    ${region:-$AWS_REGION}"
    echo "  State:     $state"
    echo "  Public IP: ${public_ip:-n/a}"
    echo "  Key file:  $(load_state KEY_FILE)"
    echo ""
    if [ "$state" = "running" ]; then
        echo "  Server:    http://${public_ip}:8080"
        echo "  SSH:       $0 ssh"
    fi
    echo ""
}

# --- SSH ---

cmd_ssh() {
    local public_ip key_file
    public_ip=$(load_state PUBLIC_IP)
    key_file=$(load_state KEY_FILE)

    if [ -z "$public_ip" ] || [ -z "$key_file" ]; then
        err "No instance found. Run '$0 create' first."
    fi

    # Pass extra args after -- to ssh
    shift 2>/dev/null || true
    if [ $# -gt 0 ] && [ "$1" = "--" ]; then
        shift
        ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip" "$@"
    else
        ssh -o StrictHostKeyChecking=no -i "$key_file" ubuntu@"$public_ip"
    fi
}

# --- Destroy ---

cmd_destroy() {
    local region instance_id vpc_id igw_id subnet_id sg_id rtb_id key_name
    region=$(load_state REGION)
    region="${region:-$AWS_REGION}"
    instance_id=$(load_state INSTANCE_ID)
    vpc_id=$(load_state VPC_ID)
    igw_id=$(load_state IGW_ID)
    subnet_id=$(load_state SUBNET_ID)
    sg_id=$(load_state SG_ID)
    rtb_id=$(load_state RTB_ID)
    key_name=$(load_state KEY_NAME)

    if [ -z "$instance_id" ] && [ -z "$vpc_id" ]; then
        echo "No dev environment found."
        return 0
    fi

    echo "This will destroy the dev environment in ${region}:"
    echo "  Instance: ${instance_id:-none}"
    echo "  VPC:      ${vpc_id:-none}"
    echo ""
    read -r -p "Are you sure? (y/N) " confirm
    if [[ ! "$confirm" =~ ^[yY]$ ]]; then
        echo "Cancelled."
        return 0
    fi

    # Terminate instance
    if [ -n "$instance_id" ]; then
        log "Terminating instance $instance_id..."
        aws ec2 terminate-instances --region "$region" --instance-ids "$instance_id" > /dev/null 2>&1 || true
        aws ec2 wait instance-terminated --region "$region" --instance-ids "$instance_id" 2>/dev/null || true
    fi

    # Delete key pair
    if [ -n "$key_name" ]; then
        log "Deleting key pair $key_name..."
        aws ec2 delete-key-pair --region "$region" --key-name "$key_name" 2>/dev/null || true
    fi

    # Delete subnet
    if [ -n "$subnet_id" ]; then
        log "Deleting subnet $subnet_id..."
        aws ec2 delete-subnet --region "$region" --subnet-id "$subnet_id" 2>/dev/null || true
    fi

    # Delete route table
    if [ -n "$rtb_id" ]; then
        log "Deleting route table $rtb_id..."
        # Disassociate first
        local assoc_id
        assoc_id=$(aws ec2 describe-route-tables --region "$region" --route-table-ids "$rtb_id" \
            --query 'RouteTables[0].Associations[?!Main].RouteTableAssociationId' --output text 2>/dev/null || true)
        if [ -n "$assoc_id" ] && [ "$assoc_id" != "None" ]; then
            aws ec2 disassociate-route-table --region "$region" --association-id "$assoc_id" 2>/dev/null || true
        fi
        aws ec2 delete-route-table --region "$region" --route-table-id "$rtb_id" 2>/dev/null || true
    fi

    # Detach and delete IGW
    if [ -n "$igw_id" ] && [ -n "$vpc_id" ]; then
        log "Deleting internet gateway $igw_id..."
        aws ec2 detach-internet-gateway --region "$region" --internet-gateway-id "$igw_id" --vpc-id "$vpc_id" 2>/dev/null || true
        aws ec2 delete-internet-gateway --region "$region" --internet-gateway-id "$igw_id" 2>/dev/null || true
    fi

    # Delete security group
    if [ -n "$sg_id" ]; then
        log "Deleting security group $sg_id..."
        aws ec2 delete-security-group --region "$region" --group-id "$sg_id" 2>/dev/null || true
    fi

    # Delete VPC
    if [ -n "$vpc_id" ]; then
        log "Deleting VPC $vpc_id..."
        aws ec2 delete-vpc --region "$region" --vpc-id "$vpc_id" 2>/dev/null || true
    fi

    # Clean up state file
    rm -f "$STATE_FILE"
    log "Dev environment destroyed."
}

# --- Main ---

CMD="${1:-}"
case "$CMD" in
    create)  cmd_create ;;
    deploy)  cmd_deploy ;;
    status)  cmd_status ;;
    ssh)     shift; cmd_ssh "$@" ;;
    destroy) cmd_destroy ;;
    *)
        echo "Usage: $0 {create|deploy|status|ssh|destroy}"
        echo ""
        echo "Quick start:"
        echo "  $0 create    # Provision a1.metal in us-east-2 (~5 min)"
        echo "  $0 deploy    # Build and deploy code (~3 min)"
        echo "  $0 status    # Show instance info"
        echo "  $0 ssh       # SSH into instance"
        echo "  $0 destroy   # Tear down everything"
        echo ""
        echo "Environment variables:"
        echo "  AWS_REGION=$AWS_REGION  INSTANCE_TYPE=$INSTANCE_TYPE  API_KEY=$API_KEY"
        exit 1
        ;;
esac
