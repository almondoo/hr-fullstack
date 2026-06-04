# modules/network/main.tf
#
# AWS VPC / サブネット / ルーティング / NAT / セキュリティグループ
# Issue #9 (GAP-01 解決), #17 (WAF/レート制限/DDoS), #19 (SLA/可用性)
#
# 設計方針:
#   - パブリックサブネット : ALB のみ配置
#   - プライベートサブネット: ECS Fargate タスク・RDS を配置
#   - NAT ゲートウェイ     : プライベートサブネットからの送信トラフィック用
#   - マルチ AZ            : 本番は最低 2 AZ (SLA 99.9% → #19)
#   - セキュリティグループ : 最小権限・ステートフルルール

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# ---------------------------------------------------------------------------
# 変数
# ---------------------------------------------------------------------------

variable "environment" {
  type        = string
  description = "環境名 (production / staging / development)"
}

variable "vpc_cidr" {
  type        = string
  description = "VPC の CIDR ブロック"
  default     = "10.0.0.0/16"
  # NOTE: 本番環境では既存ネットワークと重複しないよう確認すること
}

variable "region" {
  type        = string
  description = "AWS リージョン (例: ap-northeast-1)"
}

variable "availability_zones" {
  type        = list(string)
  description = "使用する AZ のリスト (最低 2 つ推奨)"
  default     = []
  # デフォルト空リストの場合は data source で AZ を取得する
}

variable "public_subnet_cidrs" {
  type        = list(string)
  description = "パブリックサブネットの CIDR ブロックリスト"
  default     = ["10.0.1.0/24", "10.0.2.0/24"]
}

variable "private_subnet_cidrs" {
  type        = list(string)
  description = "プライベートサブネットの CIDR ブロックリスト"
  default     = ["10.0.11.0/24", "10.0.12.0/24"]
}

variable "enable_nat_gateway" {
  type        = bool
  description = "NAT ゲートウェイを有効にするか (コスト削減のため dev では false も可)"
  default     = true
}

variable "single_nat_gateway" {
  type        = bool
  description = "true: NAT GW を 1 つに集約 (コスト重視), false: AZ ごとに作成 (可用性重視)"
  default     = false
  # 本番では false (AZ ごとに NAT GW) を推奨。SLA 99.9% 維持のため。
}

variable "app_port" {
  type        = number
  description = "アプリケーションコンテナのポート番号"
  default     = 8080
}

variable "tags" {
  type        = map(string)
  description = "すべてのリソースに付与するタグ"
  default     = {}
}

# ---------------------------------------------------------------------------
# データソース
# ---------------------------------------------------------------------------

data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = length(var.availability_zones) > 0 ? var.availability_zones : slice(data.aws_availability_zones.available.names, 0, 2)

  common_tags = merge(
    {
      Environment = var.environment
      ManagedBy   = "terraform"
      Project     = "hr-fullstack"
    },
    var.tags,
  )
}

# ---------------------------------------------------------------------------
# VPC
# ---------------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-vpc" })
}

# ---------------------------------------------------------------------------
# インターネットゲートウェイ
# ---------------------------------------------------------------------------

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-igw" })
}

# ---------------------------------------------------------------------------
# パブリックサブネット (ALB 用)
# ---------------------------------------------------------------------------

resource "aws_subnet" "public" {
  count = length(var.public_subnet_cidrs)

  vpc_id                  = aws_vpc.main.id
  cidr_block              = var.public_subnet_cidrs[count.index]
  availability_zone       = local.azs[count.index % length(local.azs)]
  map_public_ip_on_launch = false # ALB の ENI は EIP/ALB が担う。EC2 直接公開なし。

  tags = merge(local.common_tags, {
    Name = "hr-${var.environment}-public-${count.index + 1}"
    Tier = "public"
  })
}

# ---------------------------------------------------------------------------
# プライベートサブネット (ECS Fargate / RDS 用)
# ---------------------------------------------------------------------------

resource "aws_subnet" "private" {
  count = length(var.private_subnet_cidrs)

  vpc_id            = aws_vpc.main.id
  cidr_block        = var.private_subnet_cidrs[count.index]
  availability_zone = local.azs[count.index % length(local.azs)]

  tags = merge(local.common_tags, {
    Name = "hr-${var.environment}-private-${count.index + 1}"
    Tier = "private"
  })
}

# ---------------------------------------------------------------------------
# NAT ゲートウェイ (プライベートサブネットからの egress 用)
# ---------------------------------------------------------------------------

resource "aws_eip" "nat" {
  count  = var.enable_nat_gateway ? (var.single_nat_gateway ? 1 : length(var.public_subnet_cidrs)) : 0
  domain = "vpc"

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-nat-eip-${count.index + 1}" })

  depends_on = [aws_internet_gateway.main]
}

resource "aws_nat_gateway" "main" {
  count = var.enable_nat_gateway ? (var.single_nat_gateway ? 1 : length(var.public_subnet_cidrs)) : 0

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-natgw-${count.index + 1}" })

  depends_on = [aws_internet_gateway.main]
}

# ---------------------------------------------------------------------------
# ルートテーブル
# ---------------------------------------------------------------------------

# パブリック: IGW 経由
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-rt-public" })
}

resource "aws_route_table_association" "public" {
  count = length(aws_subnet.public)

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# プライベート: NAT GW 経由
resource "aws_route_table" "private" {
  count  = var.enable_nat_gateway ? length(var.private_subnet_cidrs) : 1
  vpc_id = aws_vpc.main.id

  dynamic "route" {
    for_each = var.enable_nat_gateway ? [1] : []
    content {
      cidr_block     = "0.0.0.0/0"
      nat_gateway_id = var.single_nat_gateway ? aws_nat_gateway.main[0].id : aws_nat_gateway.main[count.index % length(aws_nat_gateway.main)].id
    }
  }

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-rt-private-${count.index + 1}" })
}

resource "aws_route_table_association" "private" {
  count = length(aws_subnet.private)

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[var.enable_nat_gateway ? count.index : 0].id
}

# ---------------------------------------------------------------------------
# セキュリティグループ
# ---------------------------------------------------------------------------

# ALB: インターネットから HTTP/HTTPS を受け付ける
resource "aws_security_group" "alb" {
  name        = "hr-${var.environment}-alb-sg"
  description = "ALB inbound HTTP/HTTPS from internet"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTPS from internet"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  ingress {
    description = "HTTP from internet (redirect to HTTPS)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "Allow all outbound (ALB to targets)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-alb-sg" })
}

# ECS タスク: ALB からのトラフィックのみ受け付ける
resource "aws_security_group" "app" {
  name        = "hr-${var.environment}-app-sg"
  description = "ECS Fargate tasks: inbound from ALB only"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "App port from ALB"
    from_port       = var.app_port
    to_port         = var.app_port
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  egress {
    description = "Allow all outbound (to DB, Secrets Manager, ECR)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-app-sg" })
}

# RDS: ECS タスクからのみ 5432 を受け付ける
resource "aws_security_group" "rds" {
  name        = "hr-${var.environment}-rds-sg"
  description = "RDS: inbound PostgreSQL from app only"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "PostgreSQL from app SG"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.app.id]
  }

  egress {
    description = "No outbound needed for RDS"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-rds-sg" })
}

# ---------------------------------------------------------------------------
# VPC Flow Logs (セキュリティ監査用)
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "vpc_flow_log" {
  name              = "/aws/vpc/flowlogs/hr-${var.environment}"
  retention_in_days = 90

  tags = local.common_tags
}

resource "aws_iam_role" "vpc_flow_log" {
  name = "hr-${var.environment}-vpc-flow-log-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "vpc-flow-logs.amazonaws.com" }
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy" "vpc_flow_log" {
  name = "hr-${var.environment}-vpc-flow-log-policy"
  role = aws_iam_role.vpc_flow_log.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "logs:CreateLogGroup",
        "logs:CreateLogStream",
        "logs:PutLogEvents",
        "logs:DescribeLogGroups",
        "logs:DescribeLogStreams",
      ]
      Resource = "*"
    }]
  })
}

resource "aws_flow_log" "main" {
  vpc_id          = aws_vpc.main.id
  traffic_type    = "ALL"
  iam_role_arn    = aws_iam_role.vpc_flow_log.arn
  log_destination = aws_cloudwatch_log_group.vpc_flow_log.arn

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "vpc_id" {
  description = "VPC の ID"
  value       = aws_vpc.main.id
}

output "public_subnet_ids" {
  description = "パブリックサブネット ID のリスト"
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "プライベートサブネット ID のリスト"
  value       = aws_subnet.private[*].id
}

output "alb_security_group_id" {
  description = "ALB セキュリティグループ ID"
  value       = aws_security_group.alb.id
}

output "app_security_group_id" {
  description = "ECS タスクセキュリティグループ ID"
  value       = aws_security_group.app.id
}

output "rds_security_group_id" {
  description = "RDS セキュリティグループ ID"
  value       = aws_security_group.rds.id
}
