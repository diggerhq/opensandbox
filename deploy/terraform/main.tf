# --- Data Sources ---

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

# Look up latest Ubuntu 24.04 LTS x86_64 AMI if none provided
data "aws_ami" "ubuntu" {
  count       = var.ami_id == "" ? 1 : 0
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }

  filter {
    name   = "state"
    values = ["available"]
  }

  filter {
    name   = "architecture"
    values = ["x86_64"]
  }
}

locals {
  ami_id     = var.ami_id != "" ? var.ami_id : data.aws_ami.ubuntu[0].id
  is_dev     = startswith(var.environment, "dev")
  is_prod    = !startswith(var.environment, "dev")
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.region
}

# --- Always Created ---

module "networking" {
  source = "./modules/networking"

  project_name       = var.project_name
  environment        = var.environment
  vpc_cidr           = var.vpc_cidr
  create_nat_gateway = local.is_prod
}

module "security" {
  source = "./modules/security"

  project_name   = var.project_name
  environment    = var.environment
  vpc_id         = module.networking.vpc_id
  vpc_cidr_block = module.networking.vpc_cidr_block
}

# --- Dev Mode: Single Host (just VPC + SG + EC2) ---

module "dev_host" {
  count  = local.is_dev ? 1 : 0
  source = "./modules/dev_host"

  project_name      = var.project_name
  environment       = var.environment
  subnet_id         = module.networking.public_subnet_ids[0]
  security_group_id = module.security.dev_sg_id
  instance_type     = var.dev_host_instance_type
  key_pair_name     = var.key_pair_name
  ami_id            = local.ami_id
}

# --- Prod Mode: Separate Components ---

module "ecr" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/ecr"

  project_name = var.project_name
  environment  = var.environment
}

module "secrets" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/secrets"

  project_name        = var.project_name
  environment         = var.environment
  database_url        = module.database[0].connection_url
  redis_url           = module.redis[0].connection_url
  jwt_secret          = var.jwt_secret
  api_key             = var.api_key
  ecr_server_repo_arn = module.ecr[0].server_repo_arn
  ecr_worker_repo_arn = module.ecr[0].worker_repo_arn
}

module "database" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/database"

  project_name      = var.project_name
  environment       = var.environment
  subnet_ids        = module.networking.private_subnet_ids
  security_group_id = module.security.rds_sg_id
  instance_class    = var.db_instance_class
  db_name           = var.db_name
  db_username       = var.db_username
}

module "redis" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/redis"

  project_name      = var.project_name
  environment       = var.environment
  subnet_ids        = module.networking.private_subnet_ids
  security_group_id = module.security.redis_sg_id
  node_type         = var.redis_node_type
}

module "server" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/server"

  project_name          = var.project_name
  environment           = var.environment
  subnet_id             = module.networking.private_subnet_ids[0]
  security_group_id     = module.security.server_sg_id
  instance_type         = var.server_instance_type
  key_pair_name         = var.key_pair_name
  instance_profile_name = module.secrets[0].instance_profile_name
  server_secret_arn     = module.secrets[0].server_secret_arn
  ecr_server_repo_url   = module.ecr[0].server_repo_url
  server_image_tag      = var.server_image_tag
  database_url          = module.database[0].connection_url
  redis_url             = module.redis[0].connection_url
  worker_private_ip     = module.worker[0].private_ip
  ami_id                = local.ami_id
}

module "alb" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/alb"

  project_name        = var.project_name
  environment         = var.environment
  vpc_id              = module.networking.vpc_id
  subnet_ids          = module.networking.public_subnet_ids
  security_group_id   = module.security.alb_sg_id
  acm_certificate_arn = var.acm_certificate_arn
  server_instance_id  = module.server[0].instance_id
}

# --- Route 53 alias record (prod only, when domain is configured) ---

resource "aws_route53_record" "app" {
  count = local.is_prod && var.hosted_zone_id != "" && var.domain_name != "" ? 1 : 0

  zone_id = var.hosted_zone_id
  name    = var.domain_name
  type    = "A"

  alias {
    name                   = module.alb[0].dns_name
    zone_id                = module.alb[0].zone_id
    evaluate_target_health = true
  }
}

module "worker" {
  count  = local.is_prod ? 1 : 0
  source = "./modules/worker"

  project_name          = var.project_name
  environment           = var.environment
  subnet_id             = module.networking.private_subnet_ids[0]
  security_group_id     = module.security.worker_sg_id
  instance_type         = var.worker_instance_type
  key_pair_name         = var.key_pair_name
  instance_profile_name = module.secrets[0].instance_profile_name
  worker_secret_arn     = module.secrets[0].worker_secret_arn
  database_url          = module.database[0].connection_url
  redis_url             = module.redis[0].connection_url
  ami_id                = local.ami_id
}
