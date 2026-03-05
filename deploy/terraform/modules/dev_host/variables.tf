variable "project_name" {
  description = "Name of the project, used as a prefix for resource naming"
  type        = string
}

variable "environment" {
  description = "Deployment environment (e.g. dev, staging, prod)"
  type        = string
}

variable "subnet_id" {
  description = "ID of the public subnet to launch the instance in"
  type        = string
}

variable "security_group_id" {
  description = "ID of the security group to attach to the instance"
  type        = string
}

variable "instance_type" {
  description = "EC2 instance type (must be bare-metal for Firecracker KVM, e.g. c6g.metal)"
  type        = string
}

variable "key_pair_name" {
  description = "Name of the EC2 key pair for SSH access"
  type        = string
}

variable "ami_id" {
  description = "AMI ID to use. If empty, the latest Ubuntu 24.04 LTS x86_64 AMI is looked up automatically."
  type        = string
  default     = ""
}

variable "data_volume_size_gb" {
  description = "Size of the EBS data volume for Firecracker sandbox storage (GB). Each sandbox needs ~20GB."
  type        = number
  default     = 200
}
