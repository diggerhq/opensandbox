locals {
  _is_dev = startswith(var.environment, "dev")
}

# Dev mode outputs
output "dev_host_public_ip" {
  description = "Public IP of the dev host (dev mode only)"
  value       = local._is_dev ? module.dev_host[0].public_ip : null
}

output "dev_server_url" {
  description = "Server URL for dev mode"
  value       = local._is_dev ? "http://${module.dev_host[0].public_ip}:8080" : null
}

output "server_url" {
  description = "Server URL (ALB in prod, direct IP in dev)"
  value = local._is_dev ? (
    "http://${module.dev_host[0].public_ip}:8080"
    ) : (
    var.domain_name != "" ? "https://${var.domain_name}" : "http://${module.alb[0].dns_name}"
  )
}

output "api_key" {
  description = "API key for authenticating requests"
  value       = var.api_key
  sensitive   = true
}

# Prod mode outputs
output "ecr_server_repo_url" {
  description = "ECR repository URL for the server image (prod only)"
  value       = local._is_dev ? null : module.ecr[0].server_repo_url
}

output "ecr_worker_repo_url" {
  description = "ECR repository URL for the worker image (prod only)"
  value       = local._is_dev ? null : module.ecr[0].worker_repo_url
}

output "alb_dns_name" {
  description = "ALB DNS name (prod mode only)"
  value       = local._is_dev ? null : module.alb[0].dns_name
}

output "server_private_ip" {
  description = "Server private IP (prod mode only)"
  value       = local._is_dev ? null : module.server[0].private_ip
}

output "worker_private_ip" {
  description = "Worker private IP (prod mode only)"
  value       = local._is_dev ? null : module.worker[0].private_ip
}

output "database_endpoint" {
  description = "RDS endpoint (prod mode only)"
  value       = local._is_dev ? null : module.database[0].endpoint
}

output "redis_endpoint" {
  description = "ElastiCache endpoint (prod mode only)"
  value       = local._is_dev ? null : module.redis[0].endpoint
}

output "secrets_server_arn" {
  description = "ARN of the server secrets in Secrets Manager (prod only)"
  value       = local._is_dev ? null : module.secrets[0].server_secret_arn
}

output "secrets_worker_arn" {
  description = "ARN of the worker secrets in Secrets Manager (prod only)"
  value       = local._is_dev ? null : module.secrets[0].worker_secret_arn
}
