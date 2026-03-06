###############################################################################
# Data Sources
###############################################################################

# Look up the latest Ubuntu 24.04 LTS arm64 AMI when no ami_id is provided.
data "aws_ami" "ubuntu" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*"]
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
  ami_id = var.ami_id != "" ? var.ami_id : data.aws_ami.ubuntu[0].id

  common_tags = {
    Project     = var.project_name
    Environment = var.environment
    ManagedBy   = "terraform"
  }
}

###############################################################################
# EC2 Instance
###############################################################################

resource "aws_instance" "dev_host" {
  ami                    = local.ami_id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [var.security_group_id]
  key_name               = var.key_pair_name

  associate_public_ip_address = true

  root_block_device {
    volume_size           = 100
    volume_type           = "gp3"
    delete_on_termination = true
  }

  # Data volume for Firecracker sandbox storage (rootfs + workspace images).
  # Each sandbox uses ~20GB for its workspace.ext4, so size this based on
  # how many concurrent sandboxes you need.
  ebs_block_device {
    device_name           = "/dev/sdf"
    volume_size           = var.data_volume_size_gb
    volume_type           = "gp3"
    delete_on_termination = true
  }

  # Format and mount the data volume on first boot
  user_data = <<-USERDATA
    #!/usr/bin/env bash
    set -euo pipefail

    # Format data volume if not already formatted
    if ! blkid /dev/nvme1n1 2>/dev/null; then
      mkfs.ext4 -L opensandbox-data /dev/nvme1n1
    fi

    # Mount data volume
    mkdir -p /data
    echo 'LABEL=opensandbox-data /data ext4 defaults,nofail 0 2' >> /etc/fstab
    mount -a
    mkdir -p /data/sandboxes /data/firecracker/images
  USERDATA

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-dev-host"
  })

  lifecycle {
    ignore_changes = [ami]
  }
}
