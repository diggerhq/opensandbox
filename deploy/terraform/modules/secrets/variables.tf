variable "project_name" {
  description = "Name of the project, used as a prefix for resource naming"
  type        = string
}

variable "environment" {
  description = "Deployment environment (e.g. dev, staging, prod)"
  type        = string
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

variable "jwt_secret" {
  description = "JWT signing secret shared between server and worker"
  type        = string
  sensitive   = true
}

variable "api_key" {
  description = "API key for the server"
  type        = string
  sensitive   = true
}

variable "ecr_server_repo_arn" {
  description = "ARN of the ECR repository for server images"
  type        = string
}

variable "ecr_worker_repo_arn" {
  description = "ARN of the ECR repository for worker images"
  type        = string
}
