output "server_repo_url" {
  description = "The URL of the server ECR repository"
  value       = aws_ecr_repository.server.repository_url
}

output "worker_repo_url" {
  description = "The URL of the worker ECR repository"
  value       = aws_ecr_repository.worker.repository_url
}

output "server_repo_arn" {
  description = "The ARN of the server ECR repository"
  value       = aws_ecr_repository.server.arn
}

output "worker_repo_arn" {
  description = "The ARN of the worker ECR repository"
  value       = aws_ecr_repository.worker.arn
}
