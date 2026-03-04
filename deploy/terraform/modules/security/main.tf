###############################################################################
# Security module — conditional SG creation based on environment
#
# Dev mode  (var.environment == "dev"):  single permissive SG
# Prod mode (var.environment != "dev"):  ALB, Server, Worker, RDS, Redis SGs
###############################################################################

locals {
  is_dev  = startswith(var.environment, "dev")
  is_prod = !local.is_dev
}

###############################################################################
# DEV MODE — single security group
###############################################################################

resource "aws_security_group" "dev" {
  count = local.is_dev ? 1 : 0

  name        = "${var.project_name}-${var.environment}-dev-sg"
  description = "Dev-mode security group: API + SSH inbound, all outbound"
  vpc_id      = var.vpc_id

  # Server API
  ingress {
    description = "Server API"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # SSH
  ingress {
    description = "SSH"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # All outbound
  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.project_name}-${var.environment}-dev-sg"
    Environment = var.environment
  }
}

###############################################################################
# PROD MODE — ALB security group
###############################################################################

resource "aws_security_group" "alb" {
  count = local.is_prod ? 1 : 0

  name        = "${var.project_name}-${var.environment}-alb-sg"
  description = "ALB security group: HTTP + HTTPS inbound"
  vpc_id      = var.vpc_id

  ingress {
    description = "HTTP"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.project_name}-${var.environment}-alb-sg"
    Environment = var.environment
  }
}

###############################################################################
# PROD MODE — Server security group
###############################################################################

resource "aws_security_group" "server" {
  count = local.is_prod ? 1 : 0

  name        = "${var.project_name}-${var.environment}-server-sg"
  description = "Server security group: API from ALB, SSH from VPC"
  vpc_id      = var.vpc_id

  ingress {
    description     = "API from ALB"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb[0].id]
  }

  ingress {
    description = "SSH from VPC"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr_block]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.project_name}-${var.environment}-server-sg"
    Environment = var.environment
  }
}

###############################################################################
# PROD MODE — Worker security group
###############################################################################

resource "aws_security_group" "worker" {
  count = local.is_prod ? 1 : 0

  name        = "${var.project_name}-${var.environment}-worker-sg"
  description = "Worker security group: API + gRPC + metrics from Server, SSH from VPC"
  vpc_id      = var.vpc_id

  ingress {
    description     = "Worker API from Server"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.server[0].id]
  }

  ingress {
    description     = "gRPC from Server"
    from_port       = 9090
    to_port         = 9090
    protocol        = "tcp"
    security_groups = [aws_security_group.server[0].id]
  }

  ingress {
    description     = "Metrics from Server"
    from_port       = 9091
    to_port         = 9091
    protocol        = "tcp"
    security_groups = [aws_security_group.server[0].id]
  }

  ingress {
    description = "SSH from VPC"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr_block]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.project_name}-${var.environment}-worker-sg"
    Environment = var.environment
  }
}

###############################################################################
# PROD MODE — RDS security group
###############################################################################

resource "aws_security_group" "rds" {
  count = local.is_prod ? 1 : 0

  name        = "${var.project_name}-${var.environment}-rds-sg"
  description = "RDS security group: PostgreSQL from Server + Worker"
  vpc_id      = var.vpc_id

  ingress {
    description     = "PostgreSQL from Server"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.server[0].id]
  }

  ingress {
    description     = "PostgreSQL from Worker"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.worker[0].id]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.project_name}-${var.environment}-rds-sg"
    Environment = var.environment
  }
}

###############################################################################
# PROD MODE — Redis security group
###############################################################################

resource "aws_security_group" "redis" {
  count = local.is_prod ? 1 : 0

  name        = "${var.project_name}-${var.environment}-redis-sg"
  description = "Redis security group: Redis from Server + Worker"
  vpc_id      = var.vpc_id

  ingress {
    description     = "Redis from Server"
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_security_group.server[0].id]
  }

  ingress {
    description     = "Redis from Worker"
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [aws_security_group.worker[0].id]
  }

  egress {
    description = "All outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.project_name}-${var.environment}-redis-sg"
    Environment = var.environment
  }
}
