variable "aws_region" {
  description = "AWS region (ACM certs for ALB must be in the same region as the ALB)"
  type        = string
  default     = "us-east-1"
}

variable "hosted_zone_id" {
  description = "Route 53 hosted zone ID for the base domain"
  type        = string
  default     = "Z09553651UGBAZ18FIQUX"
}

variable "base_domain" {
  description = "Base domain under which the random subdomain is created"
  type        = string
  default     = "preview.workers.opensandbox.ai"
}
