variable "project_name" {
  description = "Project name used as a prefix for resource naming"
  type        = string
}

variable "environment" {
  description = "Environment name (dev, staging, prod). Dev mode creates a single SG; any other value creates prod-style SGs."
  type        = string
}

variable "vpc_id" {
  description = "ID of the VPC in which to create security groups"
  type        = string
}

variable "vpc_cidr_block" {
  description = "CIDR block of the VPC, used for intra-VPC SSH access in prod mode"
  type        = string
}
