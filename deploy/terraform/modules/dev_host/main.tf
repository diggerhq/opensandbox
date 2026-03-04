###############################################################################
# Data Sources
###############################################################################

# Look up the latest Ubuntu 24.04 LTS x86_64 AMI when no ami_id is provided.
data "aws_ami" "ubuntu" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
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

  cpu_options {
    nested_virtualization = "enabled"
  }

  root_block_device {
    volume_size           = 100
    volume_type           = "gp3"
    delete_on_termination = true
  }

  # No user_data — run `make deploy-dev` after terraform apply to provision.

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-dev-host"
  })

  lifecycle {
    ignore_changes = [ami]
  }
}
