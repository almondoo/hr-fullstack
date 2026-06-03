# modules/network/main.tf
#
# TODO(#9 GAP-01): クラウドプロバイダ決定後に実装する。
# 以下のプレースホルダを選定したプロバイダのリソース定義で置き換える。
#
# 実装すべきリソース:
#   AWS   : aws_vpc, aws_subnet (public/private), aws_internet_gateway,
#            aws_nat_gateway, aws_security_group
#   GCP   : google_compute_network, google_compute_subnetwork,
#            google_compute_firewall, google_compute_router, google_compute_router_nat
#   Azure : azurerm_virtual_network, azurerm_subnet, azurerm_network_security_group

# ---------------------------------------------------------------------------
# 設計方針 (プロバイダ非依存)
# ---------------------------------------------------------------------------
# - パブリックサブネット: ロードバランサのみ配置
# - プライベートサブネット: アプリコンテナ・DB を配置 (インターネット直接公開しない)
# - NAT ゲートウェイ: プライベートサブネットからの送信トラフィック用
# - セキュリティグループ / ファイアウォール: 最小権限 (コンテナ→DB のみ許可)
# - マルチ AZ: 本番は最低 2 AZ でサブネットを作成

variable "vpc_cidr" {
  type        = string
  description = "VPC / VNet の CIDR ブロック"
  default     = "10.0.0.0/16"
  # TODO(#9): 本番環境では既存ネットワークと重複しないよう確認すること
}

variable "environment" {
  type        = string
  description = "環境名 (production / staging / development)"
}

variable "region" {
  type        = string
  description = "デプロイリージョン — クラウド選定後に environments/ 側で指定する"
  # TODO(#9): プロバイダ確定後に environments/production/main.tf で設定
}

# TODO(#9): 以下にプロバイダ固有のリソースブロックを追加する
# resource "aws_vpc" "main" { ... }
# resource "google_compute_network" "main" { ... }
# resource "azurerm_virtual_network" "main" { ... }

output "vpc_id" {
  description = "作成した VPC / VNet の ID"
  value       = "TODO: プロバイダリソース決定後に参照を設定する"
}

output "private_subnet_ids" {
  description = "プライベートサブネット ID のリスト"
  value       = []
  # TODO(#9): サブネットリソース追加後に更新する
}
