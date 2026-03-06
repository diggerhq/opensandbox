variable "environment" {
  description = "Environment name: 'dev', 'dev-<name>' (e.g. dev-mohamed, dev-ci), or 'prod'. Use unique dev-<name> values to run multiple dev environments without clashes."
  type        = string
  default     = "dev"

  validation {
    condition     = startswith(var.environment, "dev") || var.environment == "prod"
    error_message = "Environment must be 'dev', 'dev-<name>' (e.g. dev-mohamed), or 'prod'."
  }
}

variable "aws_region" {
  description = "AWS region to deploy into"
  type        = string
  default     = "us-east-1"
}

variable "project_name" {
  description = "Project name used for resource naming"
  type        = string
  default     = "opensandbox"
}

# --- Networking ---

variable "vpc_cidr" {
  description = "CIDR block for the VPC"
  type        = string
  default     = "10.0.0.0/16"
}

# --- EC2 ---

variable "key_pair_name" {
  description = "Name of the EC2 key pair for SSH access"
  type        = string
  default     = "opensandbox-dev"
}

variable "server_instance_type" {
  description = "Instance type for the server EC2 (prod only)"
  type        = string
  default     = "t3.medium"
}

variable "worker_instance_type" {
  description = "Bare-metal instance type for the worker EC2"
  type        = string
  default     = "c5.metal"
}

variable "dev_host_instance_type" {
  description = "EC2 instance type for the dev single-host. Use bare-metal (e.g. c7g.metal) for Firecracker KVM support, or regular instances (e.g. c8i.xlarge) for control plane dev without sandboxes."
  type        = string
  default     = "a1.metal"
}

# --- Database (prod only) ---

variable "db_instance_class" {
  description = "RDS instance class"
  type        = string
  default     = "db.t4g.micro"
}

variable "db_name" {
  description = "PostgreSQL database name"
  type        = string
  default     = "opensandbox"
}

variable "db_username" {
  description = "PostgreSQL master username"
  type        = string
  default     = "opensandbox"
}

# --- Redis (prod only) ---

variable "redis_node_type" {
  description = "ElastiCache node type"
  type        = string
  default     = "cache.t4g.micro"
}

# --- ALB (prod only) ---

variable "acm_certificate_arn" {
  description = "ACM certificate ARN for HTTPS listener (prod only, optional)"
  type        = string
  default     = ""
}

variable "domain_name" {
  description = "Domain name for the server (used in ALB, Route 53, and outputs)"
  type        = string
  default     = ""
}

variable "hosted_zone_id" {
  description = "Route 53 hosted zone ID for creating an alias record pointing to the ALB (prod only)"
  type        = string
  default     = ""
}

# --- Secrets ---

variable "api_key" {
  description = "API key for authenticating requests. In dev mode, defaults to 'test-key' if not set."
  type        = string
  sensitive   = true
  default     = "test-key"
}

variable "jwt_secret" {
  description = "JWT secret for token signing. In dev mode, defaults to 'dev-secret' if not set."
  type        = string
  sensitive   = true
  default     = "dev-secret"
}

# --- AMI ---

variable "ami_id" {
  description = "AMI ID for EC2 instances (Ubuntu 24.04 arm64 recommended). If empty, latest Ubuntu 24.04 LTS arm64 is used."
  type        = string
  default     = ""
}

# --- Server Docker image ---

variable "server_image_tag" {
  description = "Docker image tag for the server (defaults to 'latest')"
  type        = string
  default     = "latest"
}
