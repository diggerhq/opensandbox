output "certificate_arn" {
  description = "ARN of the validated ACM certificate"
  value       = aws_acm_certificate_validation.this.certificate_arn
}

output "domain_name" {
  description = "Full domain name (FQDN) for the certificate"
  value       = local.fqdn
}

output "subdomain" {
  description = "Random subdomain prefix"
  value       = random_string.subdomain.result
}
