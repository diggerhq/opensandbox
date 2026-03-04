output "endpoint" {
  description = "Primary endpoint address of the Redis replication group"
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "connection_url" {
  description = "Full Redis connection URL"
  value       = "redis://${aws_elasticache_replication_group.this.primary_endpoint_address}:6379"
}
