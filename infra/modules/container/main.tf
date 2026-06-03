# modules/container/main.tf
#
# TODO(#9 GAP-01): クラウドプロバイダ決定後に実装する。
#
# 実装すべきリソース:
#   AWS   : aws_ecs_cluster, aws_ecs_task_definition, aws_ecs_service,
#            aws_lb, aws_lb_listener, aws_lb_target_group,
#            aws_iam_role (タスク実行ロール・最小権限)
#   GCP   : google_cloud_run_v2_service, google_compute_global_forwarding_rule,
#            google_compute_backend_service, google_service_account (最小権限)
#   Azure : azurerm_container_app, azurerm_container_app_environment,
#            azurerm_lb (または Application Gateway), azurerm_user_assigned_identity

# ---------------------------------------------------------------------------
# 設計方針 (プロバイダ非依存)
# ---------------------------------------------------------------------------
# - コンテナイメージ: CI がビルドしてレジストリにプッシュ。タグはコミット SHA。
# - ヘルスチェック: /healthz (起動確認) と /readyz (トラフィック受け入れ可否) を使用
#   (実装済み — backend/internal/server/ 参照)
# - 環境変数 / シークレット: すべてシークレットマネージャ / Key Vault から実行時インジェクト
#   (FIELD_ENCRYPTION_KEY, DATABASE_URL, SESSION_KEY, CSRF_KEY を含む)
# - スケーリング: 本番は最小 2 インスタンス (SLA 99.9% 目標 → #19 参照)
# - ロールバック: 前タスク定義に即時ロールバック可能なデプロイ戦略を設定
# - IAM: タスク実行ロールは最小権限。シークレット読み取りのみ許可。

variable "environment" {
  type        = string
  description = "環境名"
}

variable "image_tag" {
  type        = string
  description = "コンテナイメージタグ (コミット SHA 等)"
  default     = "latest"
  # TODO(#9): CI/CD から渡す。本番に latest タグを使わない。
}

variable "min_instances" {
  type        = number
  description = "最小インスタンス数 (#19 SLA 参照)"
  default     = 2
}

variable "max_instances" {
  type        = number
  description = "最大インスタンス数 (オートスケーリング上限)"
  default     = 10
}

variable "cpu" {
  type        = string
  description = "コンテナ CPU 割り当て (例: '256' for Fargate vCPU units, '1' for Cloud Run)"
  default     = "256"
}

variable "memory" {
  type        = string
  description = "コンテナメモリ割り当て (例: '512' MB)"
  default     = "512"
}

variable "private_subnet_ids" {
  type        = list(string)
  description = "コンテナを配置するプライベートサブネット ID"
}

# TODO(#9): 以下にプロバイダ固有のリソースブロックを追加する

output "service_url" {
  description = "デプロイされたサービスの URL"
  value       = "TODO: プロバイダリソース決定後に参照を設定する"
}
