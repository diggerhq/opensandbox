###############################################################################
# Data Sources
###############################################################################

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 2)

  public_subnet_cidrs  = ["10.0.1.0/24", "10.0.2.0/24"]
  private_subnet_cidrs = ["10.0.10.0/24", "10.0.11.0/24"]

  common_tags = {
    Project     = var.project_name
    Environment = var.environment
    ManagedBy   = "terraform"
  }
}

###############################################################################
# VPC
###############################################################################

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-vpc"
  })
}

###############################################################################
# Internet Gateway
###############################################################################

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-igw"
  })
}

###############################################################################
# Public Subnets
###############################################################################

resource "aws_subnet" "public" {
  count = 2

  vpc_id                  = aws_vpc.main.id
  cidr_block              = local.public_subnet_cidrs[count.index]
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = true

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-public-${local.azs[count.index]}"
    Tier = "public"
  })
}

###############################################################################
# Private Subnets
###############################################################################

resource "aws_subnet" "private" {
  count = 2

  vpc_id            = aws_vpc.main.id
  cidr_block        = local.private_subnet_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-private-${local.azs[count.index]}"
    Tier = "private"
  })
}

###############################################################################
# NAT Gateway (conditional — prod mode only)
###############################################################################

resource "aws_eip" "nat" {
  count  = var.create_nat_gateway ? 1 : 0
  domain = "vpc"

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-nat-eip"
  })
}

resource "aws_nat_gateway" "main" {
  count = var.create_nat_gateway ? 1 : 0

  allocation_id = aws_eip.nat[0].id
  subnet_id     = aws_subnet.public[0].id

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-nat-gw"
  })

  depends_on = [aws_internet_gateway.main]
}

###############################################################################
# Public Route Table
###############################################################################

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-public-rt"
  })
}

resource "aws_route_table_association" "public" {
  count = 2

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

###############################################################################
# Private Route Table
###############################################################################

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.common_tags, {
    Name = "${var.project_name}-${var.environment}-private-rt"
  })
}

resource "aws_route" "private_nat" {
  count = var.create_nat_gateway ? 1 : 0

  route_table_id         = aws_route_table.private.id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.main[0].id
}

resource "aws_route_table_association" "private" {
  count = 2

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}
