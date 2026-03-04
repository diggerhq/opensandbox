###############################################################################
# AMI lookup – Amazon Linux 2023 ARM64 (used when var.ami_id is empty)
###############################################################################
data "aws_ami" "al2023_arm64" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-arm64"]
  }

  filter {
    name   = "architecture"
    values = ["arm64"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

locals {
  ami_id    = var.ami_id != "" ? var.ami_id : data.aws_ami.al2023_arm64[0].id
  ecr_image = "${var.ecr_server_repo_url}:${var.server_image_tag}"

  # Extract the ECR registry URL (everything before the first '/') for docker login
  ecr_registry = regex("^([^/]+)", var.ecr_server_repo_url)[0]
  aws_region   = regex("\\.([a-z0-9-]+)\\.amazonaws\\.com", var.ecr_server_repo_url)[0]
}

###############################################################################
# EC2 Instance – OpenSandbox Server
###############################################################################
resource "aws_instance" "server" {
  ami                    = local.ami_id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [var.security_group_id]
  key_name               = var.key_pair_name
  iam_instance_profile   = var.instance_profile_name

  associate_public_ip_address = false

  user_data = base64encode(<<-EOF
    #!/bin/bash
    set -euo pipefail

    # Install Docker
    dnf install -y docker
    systemctl enable docker
    systemctl start docker

    # ECR login
    aws ecr get-login-password --region ${local.aws_region} | docker login --username AWS --password-stdin ${local.ecr_registry}

    # Pull and run the server container
    docker pull ${local.ecr_image}

    docker run -d \
      --name opensandbox-server \
      --restart unless-stopped \
      -p 8080:8080 \
      -e OPENSANDBOX_MODE=server \
      -e OPENSANDBOX_SECRETS_ARN=${var.server_secret_arn} \
      -e OPENSANDBOX_DATABASE_URL=${var.database_url} \
      -e OPENSANDBOX_REDIS_URL=${var.redis_url} \
      -e OPENSANDBOX_WORKER_INTERNAL_URL=http://${var.worker_private_ip}:8080 \
      ${local.ecr_image}
  EOF
  )

  tags = {
    Name        = "${var.project_name}-${var.environment}-server"
    Project     = var.project_name
    Environment = var.environment
    Role        = "server"
  }
}
