###############################################################################
# Data Sources
###############################################################################

# Look up the latest Amazon Linux 2023 ARM64 AMI when no ami_id is provided.
data "aws_ami" "al2023_arm64" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-kernel-*-arm64"]
  }

  filter {
    name   = "architecture"
    values = ["arm64"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }

  filter {
    name   = "state"
    values = ["available"]
  }
}

###############################################################################
# Locals
###############################################################################

locals {
  ami_id = var.ami_id != "" ? var.ami_id : data.aws_ami.al2023_arm64[0].id

  common_tags = {
    Project     = var.project_name
    Environment = var.environment
    ManagedBy   = "terraform"
  }
}

###############################################################################
# EC2 Instance — bare-metal worker (private subnet, no public IP)
###############################################################################

resource "aws_instance" "worker" {
  ami                    = local.ami_id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [var.security_group_id]
  key_name               = var.key_pair_name
  iam_instance_profile   = var.instance_profile_name

  associate_public_ip_address = false

  root_block_device {
    volume_size           = 200
    volume_type           = "gp3"
    delete_on_termination = true
  }

  user_data = <<-USERDATA
    #!/usr/bin/env bash
    set -euo pipefail

    # ---------------------------------------------------------------
    # 1. Enable KVM
    # ---------------------------------------------------------------
    modprobe kvm
    # Use the appropriate KVM module for the CPU vendor
    modprobe kvm_intel 2>/dev/null || modprobe kvm_amd 2>/dev/null || true
    chmod 666 /dev/kvm

    # ---------------------------------------------------------------
    # 2. Sysctl tuning for Firecracker VM networking
    # ---------------------------------------------------------------
    cat > /etc/sysctl.d/99-opensandbox.conf << 'SYSCTL'
    net.ipv4.ip_forward = 1
    net.ipv4.conf.all.proxy_arp = 1
    SYSCTL
    sysctl --system

    # ---------------------------------------------------------------
    # 3. Create /opt/opensandbox directory structure
    # ---------------------------------------------------------------
    mkdir -p /opt/opensandbox/data
    mkdir -p /opt/opensandbox/rootfs

    # ---------------------------------------------------------------
    # 4. Download Firecracker-compatible kernel
    # ---------------------------------------------------------------
    wget -q -O /opt/opensandbox/vmlinux \
      https://github.com/diggerhq/opencomputer/releases/download/kernel-v1/vmlinux-arm64

    # ---------------------------------------------------------------
    # 5. Create systemd unit for opensandbox-worker
    # ---------------------------------------------------------------
    cat > /etc/systemd/system/opensandbox-worker.service << 'SVC'
    [Unit]
    Description=OpenSandbox Worker (Firecracker)
    After=network-online.target
    Wants=network-online.target

    [Service]
    Type=simple
    ExecStart=/opt/opensandbox/opensandbox-worker
    Restart=always
    RestartSec=5
    WorkingDirectory=/opt/opensandbox

    Environment=OPENSANDBOX_MODE=worker
    Environment=OPENSANDBOX_PORT=8080
    Environment=OPENSANDBOX_SECRETS_ARN=${var.worker_secret_arn}
    Environment=DATABASE_URL=${var.database_url}
    Environment=OPENSANDBOX_REDIS_URL=${var.redis_url}
    Environment=OPENSANDBOX_DATA_DIR=/opt/opensandbox/data

    KillMode=process
    TimeoutStopSec=300
    LimitNOFILE=1000000
    LimitNPROC=65536

    [Install]
    WantedBy=multi-user.target
    SVC

    systemctl daemon-reload
    # NOTE: Worker binary is deployed separately via Makefile.
    # Once the binary is placed at /opt/opensandbox/opensandbox-worker, enable
    # and start the service with:
    #   systemctl enable --now opensandbox-worker
  USERDATA

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-worker"
  })

  lifecycle {
    ignore_changes = [ami]
  }
}
