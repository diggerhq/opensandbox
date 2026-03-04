output "instance_id" {
  description = "ID of the worker EC2 instance"
  value       = aws_instance.worker.id
}

output "private_ip" {
  description = "Private IP address of the worker EC2 instance"
  value       = aws_instance.worker.private_ip
}
