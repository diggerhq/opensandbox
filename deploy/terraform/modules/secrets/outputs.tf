output "server_secret_arn" {
  description = "ARN of the server Secrets Manager secret"
  value       = aws_secretsmanager_secret.server.arn
}

output "worker_secret_arn" {
  description = "ARN of the worker Secrets Manager secret"
  value       = aws_secretsmanager_secret.worker.arn
}

output "instance_profile_name" {
  description = "Name of the IAM instance profile for EC2 instances"
  value       = aws_iam_instance_profile.ec2_opensandbox.name
}

output "iam_role_arn" {
  description = "ARN of the IAM role for EC2 instances"
  value       = aws_iam_role.ec2_opensandbox.arn
}
