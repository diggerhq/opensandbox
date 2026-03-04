output "endpoint" {
  description = "RDS instance endpoint (host:port)"
  value       = aws_db_instance.this.endpoint
}

output "connection_url" {
  description = "Full PostgreSQL connection URL"
  value       = "postgres://${aws_db_instance.this.username}:${random_password.db_password.result}@${aws_db_instance.this.endpoint}/${aws_db_instance.this.db_name}"
  sensitive   = true
}

output "db_password" {
  description = "Generated database password"
  value       = random_password.db_password.result
  sensitive   = true
}
