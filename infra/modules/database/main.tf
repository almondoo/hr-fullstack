# modules/database/main.tf
#
# AWS RDS for PostgreSQL — Multi-AZ / 自動バックアップ / PITR / 暗号化
# Issue #9 (AWS 確定), #16 (バックアップ/DR), #19 (SLA/可用性), #10 (KMS 暗号化)
#
# 設計方針:
#   - PostgreSQL 15 / Multi-AZ 有効 (本番デフォルト)
#   - PITR: backup_retention_period で制御 (RPO 目標確定後に調整 → #16)
#   - 接続: SSL 必須 (sslmode=require), 平文接続拒否
#   - 認証: DB パスワードはシークレットマネージャ経由 (コードにハードコード禁止)
#   - 暗号化: KMS CMK による保存時暗号化
#   - 最小権限: アプリ用 DB ロールは SELECT/INSERT/UPDATE/DELETE のみ
#   - RLS: backend/db/migrations/ で管理

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

variable "db_name" {
  type        = string
  description = "PostgreSQL データベース名"
  default     = "hrdb"
}

variable "db_version" {
  type        = string
  description = "PostgreSQL エンジンバージョン"
  default     = "15"
}

variable "instance_class" {
  type        = string
  description = "RDS インスタンスタイプ (例: db.t3.medium)"
  # production: db.t3.medium 以上 / staging: db.t3.small / dev: db.t3.micro
}

variable "allocated_storage" {
  type        = number
  description = "ストレージ初期サイズ (GiB)"
  default     = 20
}

variable "max_allocated_storage" {
  type        = number
  description = "ストレージオートスケーリング上限 (GiB, 0 で無効)"
  default     = 100
}

variable "subnet_ids" {
  type        = list(string)
  description = "RDS を配置するプライベートサブネット ID"
}

variable "vpc_security_group_ids" {
  type        = list(string)
  description = "RDS セキュリティグループ ID リスト"
}

variable "backup_retention_days" {
  type        = number
  description = "自動バックアップ保持日数 (#16 RPO 目標確定後に調整)"
  default     = 7
  # TODO(#16): 本番の RPO 目標確定後に 35 日 (RDS 上限) へ変更を検討。
  #            労基法 5 年保管要件対応の場合は S3 Glacier へのエクスポートを検討。
  # 推奨値: production=35, staging=7, development=1
}

variable "backup_window" {
  type        = string
  description = "バックアップウィンドウ (UTC, HH:MM-HH:MM 形式)"
  default     = "17:00-18:00"
  # 17:00 UTC = 02:00 JST (低トラフィック時間帯)
}

variable "maintenance_window" {
  type        = string
  description = "メンテナンスウィンドウ (UTC)"
  default     = "Mon:18:00-Mon:19:00"
  # 月曜 03:00-04:00 JST
}

variable "multi_az" {
  type        = bool
  description = "Multi-AZ 有効 (SLA 99.9% 維持のため本番は true 推奨 → #19)"
  default     = true
}

variable "kms_key_id" {
  type        = string
  description = "RDS 保存時暗号化用 KMS キー ARN (#10)"
  default     = null
  # null の場合は AWS マネージドキーを使用 (本番では CMK 推奨)
}

variable "deletion_protection" {
  type        = bool
  description = "削除保護 (本番は true 必須)"
  default     = true
}

variable "skip_final_snapshot" {
  type        = bool
  description = "削除時の最終スナップショットをスキップするか (本番は false 必須)"
  default     = false
}

variable "db_username" {
  type        = string
  description = "DB 管理ユーザー名 (実値はシークレットマネージャで管理)"
  default     = "hrapp"
  # NOTE: パスワードは aws_db_instance の manage_master_user_password で
  #       Secrets Manager に自動管理させる。ここに実値を書かないこと。
}

variable "tags" {
  type        = map(string)
  description = "すべてのリソースに付与するタグ"
  default     = {}
}

variable "performance_insights_enabled" {
  type        = bool
  description = "Performance Insights を有効化するか"
  default     = true
}

# ---------------------------------------------------------------------------
# ローカル
# ---------------------------------------------------------------------------

locals {
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
# DB サブネットグループ
# ---------------------------------------------------------------------------

resource "aws_db_subnet_group" "main" {
  name        = "hr-${var.environment}-db-subnet-group"
  description = "Subnet group for hr-${var.environment} RDS"
  subnet_ids  = var.subnet_ids

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-db-subnet-group" })
}

# ---------------------------------------------------------------------------
# DB パラメータグループ (SSL 必須化)
# ---------------------------------------------------------------------------

resource "aws_db_parameter_group" "main" {
  name        = "hr-${var.environment}-pg${var.db_version}"
  family      = "postgres${var.db_version}"
  description = "HR SaaS PostgreSQL ${var.db_version} parameter group"

  parameter {
    name  = "rds.force_ssl"
    value = "1"
    # 1 = SSL 必須 / 平文接続拒否 (設計方針に準拠)
    apply_method = "immediate"
  }

  parameter {
    name         = "log_connections"
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name         = "log_disconnections"
    value        = "1"
    apply_method = "immediate"
  }

  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
    # 1 秒以上のクエリをログ記録 (パフォーマンス監視)
    apply_method = "immediate"
  }

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-pg${var.db_version}" })
}

# ---------------------------------------------------------------------------
# RDS インスタンス (本番用: Multi-AZ / 暗号化 / PITR)
# ---------------------------------------------------------------------------

resource "aws_db_instance" "main" {
  identifier = "hr-${var.environment}-postgres"

  # エンジン
  engine               = "postgres"
  engine_version       = var.db_version
  instance_class       = var.instance_class
  db_name              = var.db_name
  username             = var.db_username
  parameter_group_name = aws_db_parameter_group.main.name
  db_subnet_group_name = aws_db_subnet_group.main.name

  # パスワードは Secrets Manager で自動管理 (#10 設計方針)
  # manage_master_user_password = true を使うと RDS が自動的に SM シークレットを作成・ローテーション
  manage_master_user_password = true

  # ストレージ
  allocated_storage     = var.allocated_storage
  max_allocated_storage = var.max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_id

  # ネットワーク
  publicly_accessible    = false
  vpc_security_group_ids = var.vpc_security_group_ids
  multi_az               = var.multi_az

  # バックアップ / PITR (#16)
  backup_retention_period  = var.backup_retention_days
  backup_window            = var.backup_window
  copy_tags_to_snapshot    = true
  delete_automated_backups = false
  maintenance_window       = var.maintenance_window

  # パフォーマンス
  performance_insights_enabled = var.performance_insights_enabled

  # アップグレード
  auto_minor_version_upgrade = true
  apply_immediately          = false

  # 保護
  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = var.skip_final_snapshot ? null : "hr-${var.environment}-final-snapshot"

  tags = merge(local.common_tags, { Name = "hr-${var.environment}-postgres" })

  lifecycle {
    # パスワードの変更はシークレットマネージャ側で管理するため ignore
    ignore_changes = [password]
  }
}

# ---------------------------------------------------------------------------
# クロスリージョンリードレプリカ (DR 用 — #16)
# ---------------------------------------------------------------------------
# NOTE: クロスリージョンレプリカは Terraform の provider alias がルートモジュールに
#       存在しないと child module 内で使えない制約があるため、このモジュールでは
#       定義せず、environments/production/main.tf で直接 aws_db_instance.replica を
#       定義すること (#16 参照)。
#       実装例 (environments/production/main.tf):
#
#   resource "aws_db_instance" "replica" {
#     provider            = aws.replica
#     identifier          = "hr-production-postgres-replica"
#     replicate_source_db = module.database.db_instance_arn
#     instance_class      = "db.t3.medium"
#     publicly_accessible = false
#     storage_encrypted   = true
#     skip_final_snapshot = true
#     deletion_protection = false
#   }
#
# 昇格手順: aws rds promote-read-replica --db-instance-identifier <id>

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "db_endpoint" {
  description = "RDS エンドポイント (アプリの DATABASE_URL に使用)"
  value       = aws_db_instance.main.endpoint
  sensitive   = true
}

output "db_port" {
  description = "RDS ポート番号"
  value       = aws_db_instance.main.port
}

output "db_name" {
  description = "データベース名"
  value       = aws_db_instance.main.db_name
}

output "db_resource_id" {
  description = "RDS リソース ID (IAM 認証用)"
  value       = aws_db_instance.main.resource_id
}

output "db_instance_arn" {
  description = "RDS インスタンス ARN"
  value       = aws_db_instance.main.arn
}

output "master_user_secret_arn" {
  description = "RDS マスターパスワードの Secrets Manager ARN"
  value       = aws_db_instance.main.master_user_secret[0].secret_arn
  sensitive   = true
}
