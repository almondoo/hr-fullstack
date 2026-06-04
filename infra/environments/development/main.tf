# environments/development/main.tf
#
# 開発環境 root モジュール (クラウド開発環境 — 通常はローカル docker-compose を優先)
#
# 使用場面:
#   - CI 統合テスト用の一時環境
#   - 外部連携テスト (決済 / 外部 API) が必要な場合
#
# NOTE: 通常のローカル開発は docker-compose.yml を使用すること。
#       このモジュールはクラウドリソースが必要な場合にのみ使用する。

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Environment = "development"
      ManagedBy   = "terraform"
      Project     = "hr-fullstack"
    }
  }
}

# ---------------------------------------------------------------------------
# 変数
# ---------------------------------------------------------------------------

variable "region" {
  type    = string
  default = "ap-northeast-1"
}

variable "image_tag" {
  type    = string
  default = "latest"
}

variable "image_repository_url" {
  type        = string
  description = "ECR リポジトリ URL"
}

# ---------------------------------------------------------------------------
# モジュール (最小コスト構成)
# ---------------------------------------------------------------------------

module "network" {
  source = "../../modules/network"

  environment          = "development"
  region               = var.region
  vpc_cidr             = "10.2.0.0/16"
  public_subnet_cidrs  = ["10.2.1.0/24", "10.2.2.0/24"]
  private_subnet_cidrs = ["10.2.11.0/24", "10.2.12.0/24"]
  enable_nat_gateway   = true
  single_nat_gateway   = true # コスト最優先
  app_port             = 8080
}

module "secrets" {
  source = "../../modules/secrets"

  environment                 = "development"
  region                      = var.region
  app_task_role_arn           = module.container.task_role_arn
  app_task_execution_role_arn = module.container.task_execution_role_arn
  recovery_window_in_days     = 7
}

module "database" {
  source = "../../modules/database"

  environment            = "development"
  instance_class         = "db.t3.micro"
  subnet_ids             = module.network.private_subnet_ids
  vpc_security_group_ids = [module.network.rds_security_group_id]
  kms_key_id             = module.secrets.rds_kms_key_arn

  backup_retention_days        = 1
  multi_az                     = false
  deletion_protection          = false
  skip_final_snapshot          = true
  performance_insights_enabled = false
}

module "container" {
  source = "../../modules/container"

  environment           = "development"
  region                = var.region
  vpc_id                = module.network.vpc_id
  public_subnet_ids     = module.network.public_subnet_ids
  private_subnet_ids    = module.network.private_subnet_ids
  alb_security_group_id = module.network.alb_security_group_id
  app_security_group_id = module.network.app_security_group_id
  image_repository_url  = var.image_repository_url
  image_tag             = var.image_tag
  min_instances         = 1
  max_instances         = 2
  cpu                   = 256
  memory                = 512
  secrets_manager_arns  = module.secrets.all_secret_arns
  kms_key_arns          = module.secrets.all_kms_key_arns
}

# WAF は開発環境では任意 (コスト削減のためコメントアウト)
# module "waf" {
#   source    = "../../modules/waf"
#   environment = "development"
#   alb_arn   = module.container.alb_arn
# }

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "alb_dns_name" {
  value = module.container.alb_dns_name
}
