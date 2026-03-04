output "dns_name" {
  description = "DNS name of the Application Load Balancer"
  value       = aws_lb.main.dns_name
}

output "zone_id" {
  description = "Canonical hosted zone ID of the ALB (for Route 53 alias records)"
  value       = aws_lb.main.zone_id
}

output "target_group_arn" {
  description = "ARN of the target group"
  value       = aws_lb_target_group.server.arn
}

output "arn" {
  description = "ARN of the Application Load Balancer"
  value       = aws_lb.main.arn
}
