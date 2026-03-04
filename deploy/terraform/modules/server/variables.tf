variable "project_name" {
  description = "Name of the project, used as a prefix for resource naming"
  type        = string
}

variable "environment" {
  description = "Deployment environment (e.g. dev, staging, prod)"
  type        = string
}

variable "subnet_id" {
  description = "ID of the private subnet to launch the instance in"
  type        = string
}

variable "security_group_id" {
  description = "ID of the security group to attach to the instance"
  type        = string
}

variable "instance_type" {
  description = "EC2 instance type for the server"
  type        = string
  default     = "t3.medium"
}

variable "key_pair_name" {
  description = "Name of the EC2 key pair for SSH access"
  type        = string
}

variable "instance_profile_name" {
  description = "Name of the IAM instance profile (from secrets module)"
  type        = string
}

variable "server_secret_arn" {
  description = "ARN of the Secrets Manager secret for the server"
  type        = string
}

variable "ecr_server_repo_url" {
  description = "Full ECR repository URL for the server image (e.g. 123456789.dkr.ecr.us-east-1.amazonaws.com/opensandbox-server)"
  type        = string
}

variable "server_image_tag" {
  description = "Docker image tag for the server (e.g. latest, v1.0.0, sha-abc1234)"
  type        = string
  default     = "latest"
}

variable "database_url" {
  description = "PostgreSQL connection string for the server"
  type        = string
  sensitive   = true
}

variable "redis_url" {
  description = "Redis connection string"
  type        = string
  sensitive   = true
}

variable "worker_private_ip" {
  description = "Private IP address of the worker instance"
  type        = string
}

variable "ami_id" {
  description = "AMI ID to use. If empty, the latest Amazon Linux 2023 ARM64 AMI is looked up automatically."
  type        = string
  default     = ""
}
