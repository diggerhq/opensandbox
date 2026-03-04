###############################################################################
# Dev mode output — populated only when environment == "dev"
###############################################################################

output "dev_sg_id" {
  description = "Security group ID for the dev-mode combined SG (empty string in prod)"
  value       = length(aws_security_group.dev) > 0 ? aws_security_group.dev[0].id : ""
}

###############################################################################
# Prod mode outputs — populated only when environment != "dev"
###############################################################################

output "alb_sg_id" {
  description = "Security group ID for the ALB (empty string in dev)"
  value       = length(aws_security_group.alb) > 0 ? aws_security_group.alb[0].id : ""
}

output "server_sg_id" {
  description = "Security group ID for the Server instances (empty string in dev)"
  value       = length(aws_security_group.server) > 0 ? aws_security_group.server[0].id : ""
}

output "worker_sg_id" {
  description = "Security group ID for the Worker instances (empty string in dev)"
  value       = length(aws_security_group.worker) > 0 ? aws_security_group.worker[0].id : ""
}

output "rds_sg_id" {
  description = "Security group ID for the RDS instance (empty string in dev)"
  value       = length(aws_security_group.rds) > 0 ? aws_security_group.rds[0].id : ""
}

output "redis_sg_id" {
  description = "Security group ID for the Redis cluster (empty string in dev)"
  value       = length(aws_security_group.redis) > 0 ? aws_security_group.redis[0].id : ""
}
