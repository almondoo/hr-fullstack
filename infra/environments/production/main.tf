# environments/production/main.tf
#
# 本番環境 root モジュール — AWS (ap-northeast-1 東京)
# Issue #9 (AWS 確定), #10 (Secrets/KMS), #16 (バックアップ/DR), #17 (WAF), #19 (SLA)
#
# 使用方法:
#   terraform init -backend-config="bucket=<tfstate-bucket>" \
#                  -backend-config="dynamodb_table=<lock-table>"
#   terraform plan -var-file="terraform.tfvars"
#   terraform apply -var-file="terraform.tfvars"
#
# NOTE: terraform.tfvars は .gitignore 済み。terraform.tfvars.example を参照して作成すること。

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Environment = "production"
      ManagedBy   = "terraform"
      Project     = "hr-fullstack"
    }
  }
}

# クロスリージョンレプリカ用プロバイダ (enable_cross_region_replica=true 時に使用)
provider "aws" {
  alias  = "replica"
  region = var.replica_region

  default_tags {
    tags = {
      Environment = "production"
      ManagedBy   = "terraform"
      Project     = "hr-fullstack"
      Role        = "cross-region-replica"
    }
  }
}

# ---------------------------------------------------------------------------
# 変数
# ---------------------------------------------------------------------------

variable "region" {
  type        = string
  description = "AWS リージョン"
  default     = "ap-northeast-1"
}

variable "replica_region" {
  type        = string
  description = "クロスリージョンレプリカのリージョン (#16 DR)"
  default     = "ap-northeast-3"
}

variable "image_tag" {
  type        = string
  description = "デプロイするコンテナイメージタグ (コミット SHA 推奨)"
  # CI/CD から -var image_tag=<sha> で渡すこと。本番に latest を使わない。
}

variable "image_repository_url" {
  type        = string
  description = "ECR リポジトリ URL (アカウント ID 含む)"
  # NOTE: アカウント ID は実値。.gitignore 済みの tfvars で渡すこと。
}

variable "certificate_arn" {
  type        = string
  description = "ALB HTTPS リスナー用 ACM 証明書 ARN"
  default     = null
}

variable "additional_kms_admins" {
  type        = list(string)
  description = "KMS キー管理者 IAM ARN リスト (DevOps ロール等)"
  default     = []
}

# ---------------------------------------------------------------------------
# ネットワーク (#9)
# ---------------------------------------------------------------------------

module "network" {
  source = "../../modules/network"

  environment          = "production"
  region               = var.region
  vpc_cidr             = "10.0.0.0/16"
  public_subnet_cidrs  = ["10.0.1.0/24", "10.0.2.0/24"]
  private_subnet_cidrs = ["10.0.11.0/24", "10.0.12.0/24"]
  enable_nat_gateway   = true
  single_nat_gateway   = false # AZ ごとに NAT GW (SLA 99.9% → #19)
  app_port             = 8080
}

# ---------------------------------------------------------------------------
# シークレット / KMS (#10) — container より先に apply が必要
# ---------------------------------------------------------------------------

module "secrets" {
  source = "../../modules/secrets"

  environment                 = "production"
  region                      = var.region
  app_task_role_arn           = module.container.task_role_arn
  app_task_execution_role_arn = module.container.task_execution_role_arn
  recovery_window_in_days     = 30
  additional_kms_admins       = var.additional_kms_admins
}

# ---------------------------------------------------------------------------
# データベース (#9 / #16 バックアップ / #19 Multi-AZ)
# ---------------------------------------------------------------------------

module "database" {
  source = "../../modules/database"

  environment            = "production"
  instance_class         = "db.t3.medium"
  subnet_ids             = module.network.private_subnet_ids
  vpc_security_group_ids = [module.network.rds_security_group_id]
  kms_key_id             = module.secrets.rds_kms_key_arn

  # バックアップ / PITR (#16)
  # TODO(#16): RPO 目標確定後に backup_retention_days を調整 (最大 35 日)
  # 推奨 RPO: 1 時間 (PITR で対応), RTO: 4 時間 (Multi-AZ 自動フェイルオーバー 30-120 秒)
  backup_retention_days = 35

  # Multi-AZ / SLA (#19)
  # 推奨 SLA: 99.9% — Multi-AZ + 複数インスタンス + 自動スケーリング
  multi_az = true

  deletion_protection = true
  skip_final_snapshot = false

  performance_insights_enabled = true

  # クロスリージョンレプリカ (#16 DR — コスト・RPO 要件確定後に有効化)
  # 有効化する場合は infra/modules/database/main.tf のコメント例を参照して
  # resource "aws_db_instance" "replica" を以下に追加すること。
}

# ---------------------------------------------------------------------------
# コンテナ / ECS Fargate / ALB (#9 / #19)
# ---------------------------------------------------------------------------

module "container" {
  source = "../../modules/container"

  environment           = "production"
  region                = var.region
  vpc_id                = module.network.vpc_id
  public_subnet_ids     = module.network.public_subnet_ids
  private_subnet_ids    = module.network.private_subnet_ids
  alb_security_group_id = module.network.alb_security_group_id
  app_security_group_id = module.network.app_security_group_id
  image_repository_url  = var.image_repository_url
  image_tag             = var.image_tag
  certificate_arn       = var.certificate_arn

  # SLA 99.9% 維持のため最小 2 タスク (#19)
  min_instances = 2
  max_instances = 10
  cpu           = 512
  memory        = 1024

  # シークレット参照 (#10)
  secrets_manager_arns = module.secrets.all_secret_arns
  kms_key_arns         = module.secrets.all_kms_key_arns
}

# ---------------------------------------------------------------------------
# WAF (#17)
# ---------------------------------------------------------------------------

module "waf" {
  source = "../../modules/waf"

  environment            = "production"
  alb_arn                = module.container.alb_arn
  rate_limit_per_ip      = 300 # 300 req / 5 分 = 60 req/min 相当
  auth_rate_limit_per_ip = 50  # 50 req / 5 分 = 10 req/min 相当 (ブルートフォース対策)
  enable_logging         = true
  log_retention_days     = 90

  # allowed_ip_cidrs = []  # VPN / オフィス IP を tfvars で設定 (実値をコードに書かない)
  # blocked_ip_cidrs = []  # 拒否リストを tfvars で設定
}

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "alb_dns_name" {
  description = "ALB DNS 名 (Route 53 エイリアスレコードのターゲット)"
  value       = module.container.alb_dns_name
}

output "ecr_repository_url" {
  description = "ECR リポジトリ URL (CI/CD で使用)"
  value       = module.container.ecr_repository_url
}

output "db_endpoint" {
  description = "RDS エンドポイント"
  value       = module.database.db_endpoint
  sensitive   = true
}

output "field_encryption_kms_key_arn" {
  description = "列暗号 KEK 用 KMS キー ARN"
  value       = module.secrets.field_encryption_kms_key_arn
}

output "app_secrets_arn" {
  description = "アプリシークレット ARN"
  value       = module.secrets.app_secrets_arn
  sensitive   = true
}
