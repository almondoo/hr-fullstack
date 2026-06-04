# environments/staging/main.tf
#
# ステージング環境 root モジュール — AWS (ap-northeast-1 東京)
# 本番と同一の構成 (ミラー環境) でデプロイ検証。コストを抑えるため小さめのサイズ。
# Issue #9, #10, #16, #17, #19

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Environment = "staging"
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
  type        = string
  description = "デプロイするコンテナイメージタグ"
  default     = "latest"
}

variable "image_repository_url" {
  type        = string
  description = "ECR リポジトリ URL"
}

variable "certificate_arn" {
  type    = string
  default = null
}

variable "additional_kms_admins" {
  type    = list(string)
  default = []
}

# ---------------------------------------------------------------------------
# モジュール
# ---------------------------------------------------------------------------

module "network" {
  source = "../../modules/network"

  environment          = "staging"
  region               = var.region
  vpc_cidr             = "10.1.0.0/16"
  public_subnet_cidrs  = ["10.1.1.0/24", "10.1.2.0/24"]
  private_subnet_cidrs = ["10.1.11.0/24", "10.1.12.0/24"]
  enable_nat_gateway   = true
  single_nat_gateway   = true # コスト削減: 単一 NAT GW (staging は高可用性不要)
  app_port             = 8080
}

module "secrets" {
  source = "../../modules/secrets"

  environment                 = "staging"
  region                      = var.region
  app_task_role_arn           = module.container.task_role_arn
  app_task_execution_role_arn = module.container.task_execution_role_arn
  recovery_window_in_days     = 7 # staging は短めの回復ウィンドウ
  additional_kms_admins       = var.additional_kms_admins
}

module "database" {
  source = "../../modules/database"

  environment            = "staging"
  instance_class         = "db.t3.small"
  subnet_ids             = module.network.private_subnet_ids
  vpc_security_group_ids = [module.network.rds_security_group_id]
  kms_key_id             = module.secrets.rds_kms_key_arn

  backup_retention_days        = 7
  multi_az                     = false # コスト削減
  deletion_protection          = false
  skip_final_snapshot          = true
  performance_insights_enabled = false
}

module "container" {
  source = "../../modules/container"

  environment           = "staging"
  region                = var.region
  vpc_id                = module.network.vpc_id
  public_subnet_ids     = module.network.public_subnet_ids
  private_subnet_ids    = module.network.private_subnet_ids
  alb_security_group_id = module.network.alb_security_group_id
  app_security_group_id = module.network.app_security_group_id
  image_repository_url  = var.image_repository_url
  image_tag             = var.image_tag
  certificate_arn       = var.certificate_arn
  min_instances         = 1
  max_instances         = 3
  cpu                   = 256
  memory                = 512
  secrets_manager_arns  = module.secrets.all_secret_arns
  kms_key_arns          = module.secrets.all_kms_key_arns
}

module "waf" {
  source = "../../modules/waf"

  environment            = "staging"
  alb_arn                = module.container.alb_arn
  rate_limit_per_ip      = 600 # staging は緩め
  auth_rate_limit_per_ip = 100
  enable_logging         = true
  log_retention_days     = 30
}

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "alb_dns_name" {
  value = module.container.alb_dns_name
}

output "ecr_repository_url" {
  value = module.container.ecr_repository_url
}
