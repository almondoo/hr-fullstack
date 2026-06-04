# environments/production/main.tf
#
# 本番環境 root モジュール
# TODO(#9 GAP-01): クラウドプロバイダ決定後に provider ブロックと
# module 参照を実装する。

# TODO(#9): provider ブロックを追加する
# provider "aws" { region = var.region }
# provider "google" { project = var.project_id; region = var.region }
# provider "azurerm" { features {} }

# TODO(#9): 以下の module ブロックを有効化し、各変数を埋める

# module "network" {
#   source      = "../../modules/network"
#   environment = "production"
#   vpc_cidr    = "10.0.0.0/16"
#   region      = var.region
# }

# module "database" {
#   source                = "../../modules/database"
#   environment           = "production"
#   subnet_ids            = module.network.private_subnet_ids
#   instance_class        = "TODO: プロバイダ選定後に決定"
#   backup_retention_days = 35  # RPO 目標確定後に調整 (#16)
# }

# module "secrets" {
#   source           = "../../modules/secrets"
#   environment      = "production"
#   app_identity_arn = module.container.task_role_arn
# }

# module "container" {
#   source             = "../../modules/container"
#   environment        = "production"
#   image_tag          = var.image_tag
#   min_instances      = 2   # SLA 99.9% のため最小 2 (#19)
#   max_instances      = 10
#   private_subnet_ids = module.network.private_subnet_ids
# }
