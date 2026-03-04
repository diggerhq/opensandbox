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
  description = "EC2 instance type for the worker (must support bare-metal / KVM)"
  type        = string
  default     = "c5.metal"
}

variable "key_pair_name" {
  description = "Name of the EC2 key pair for SSH access"
  type        = string
}

variable "instance_profile_name" {
  description = "Name of the IAM instance profile (from secrets module)"
  type        = string
}

variable "worker_secret_arn" {
  description = "ARN of the Secrets Manager secret for the worker"
  type        = string
}

variable "database_url" {
  description = "PostgreSQL connection string for the worker"
  type        = string
  sensitive   = true
}

variable "redis_url" {
  description = "Redis connection string"
  type        = string
  sensitive   = true
}

variable "ami_id" {
  description = "AMI ID to use. If empty, the latest Amazon Linux 2023 ARM64 AMI is looked up automatically."
  type        = string
  default     = ""
}
