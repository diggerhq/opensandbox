###############################################################################
# Secrets Manager – server secret
###############################################################################
resource "aws_secretsmanager_secret" "server" {
  name                    = "${var.project_name}-${var.environment}-server"
  description             = "Server secrets for ${var.project_name} ${var.environment}"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "server" {
  secret_id = aws_secretsmanager_secret.server.id
  secret_string = jsonencode({
    OPENSANDBOX_DATABASE_URL = var.database_url
    OPENSANDBOX_REDIS_URL    = var.redis_url
    OPENSANDBOX_JWT_SECRET   = var.jwt_secret
    OPENSANDBOX_API_KEY      = var.api_key
  })
}

###############################################################################
# Secrets Manager – worker secret
###############################################################################
resource "aws_secretsmanager_secret" "worker" {
  name                    = "${var.project_name}-${var.environment}-worker"
  description             = "Worker secrets for ${var.project_name} ${var.environment}"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "worker" {
  secret_id = aws_secretsmanager_secret.worker.id
  secret_string = jsonencode({
    OPENSANDBOX_JWT_SECRET = var.jwt_secret
    DATABASE_URL           = var.database_url
    OPENSANDBOX_REDIS_URL  = var.redis_url
  })
}

###############################################################################
# IAM role for EC2 instances
###############################################################################
data "aws_iam_policy_document" "ec2_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "ec2_opensandbox" {
  name               = "ec2-opensandbox-${var.environment}"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json
}

###############################################################################
# Inline policy – secrets, ECR, S3
###############################################################################
data "aws_iam_policy_document" "ec2_permissions" {
  # Secrets Manager access – scoped to the two secrets
  statement {
    effect = "Allow"
    actions = [
      "secretsmanager:GetSecretValue",
    ]
    resources = [
      aws_secretsmanager_secret.server.arn,
      aws_secretsmanager_secret.worker.arn,
    ]
  }

  # ECR access – token + image pull
  statement {
    effect = "Allow"
    actions = [
      "ecr:GetAuthorizationToken",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
    ]
    resources = ["*"]
  }

  # S3 access – rootfs / kernel storage
  statement {
    effect = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "ec2_opensandbox" {
  name   = "ec2-opensandbox-${var.environment}-policy"
  role   = aws_iam_role.ec2_opensandbox.id
  policy = data.aws_iam_policy_document.ec2_permissions.json
}

###############################################################################
# Instance profile
###############################################################################
resource "aws_iam_instance_profile" "ec2_opensandbox" {
  name = "ec2-opensandbox-${var.environment}"
  role = aws_iam_role.ec2_opensandbox.name
}
