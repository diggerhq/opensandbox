output "instance_id" {
  description = "ID of the dev host EC2 instance"
  value       = aws_instance.dev_host.id
}

output "public_ip" {
  description = "Public IP address of the dev host"
  value       = aws_instance.dev_host.public_ip
}

output "private_ip" {
  description = "Private IP address of the dev host"
  value       = aws_instance.dev_host.private_ip
}
