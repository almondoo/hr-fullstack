# modules/database/main.tf
#
# TODO(#9 GAP-01): クラウドプロバイダ決定後に実装する。
#
# 実装すべきリソース:
#   AWS   : aws_db_instance (PostgreSQL), aws_db_subnet_group,
#            aws_db_parameter_group, aws_secretsmanager_secret (DB パスワード)
#   GCP   : google_sql_database_instance (POSTGRES_15), google_sql_database,
#            google_sql_user, google_secret_manager_secret (DB パスワード)
#   Azure : azurerm_postgresql_flexible_server, azurerm_postgresql_flexible_server_database,
#            azurerm_key_vault_secret (DB パスワード)

# ---------------------------------------------------------------------------
# 設計方針 (プロバイダ非依存)
# ---------------------------------------------------------------------------
# - エンジン: PostgreSQL 15+ (移行 SQL は backend/db/migrations/ で管理)
# - 接続: SSL 必須 (sslmode=require)。平文接続を拒否する設定を有効化。
# - 認証: DB パスワードはシークレットマネージャ経由でインジェクト。コードにハードコード禁止。
# - バックアップ: 自動バックアップ有効 (保持期間 7-35 日)、PITR 有効 (→ #16 DR 参照)
# - 最小権限: アプリ用 DB ロールは SELECT/INSERT/UPDATE/DELETE のみ。DDL は migration ロールで分離。
# - RLS: 全テナント分離テーブルで ROW LEVEL SECURITY FORCE を維持 (backend/db/migrations/ 参照)
# - マルチ AZ / HA: 本番は Multi-AZ または High Availability モードを有効化

variable "environment" {
  type        = string
  description = "環境名"
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
  description = "DB インスタンスサイズ — 環境ごとに environments/ 側で指定する"
  # TODO(#9): production=db.t3.medium 以上, staging=db.t3.small 程度
}

variable "subnet_ids" {
  type        = list(string)
  description = "DB を配置するプライベートサブネット ID"
}

variable "backup_retention_days" {
  type        = number
  description = "自動バックアップの保持日数 (#16 RPO/RTO 参照)"
  default     = 7
  # TODO(#16): RPO 目標確定後に本番は 35 日 (RDS 上限) / 365 日 (アーカイブ) を検討
}

# TODO(#9): 以下にプロバイダ固有のリソースブロックを追加する
# resource "aws_db_instance" "main" { ... }
# resource "google_sql_database_instance" "main" { ... }
# resource "azurerm_postgresql_flexible_server" "main" { ... }

output "db_endpoint" {
  description = "アプリが使用するデータベースエンドポイント"
  value       = "TODO: プロバイダリソース決定後に参照を設定する"
  sensitive   = true
}
