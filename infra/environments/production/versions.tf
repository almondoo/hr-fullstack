terraform {
  required_version = ">= 1.6"

  # TODO(#9): バックエンドをクラウド選定後に設定する
  # backend "s3" {
  #   bucket  = "TODO: tfstate バケット名"
  #   key     = "production/terraform.tfstate"
  #   region  = "TODO: リージョン"
  #   encrypt = true  # 必須: State ファイルの暗号化
  # }
  # backend "gcs" { bucket = "TODO"; prefix = "production" }
  # backend "azurerm" { resource_group_name = "TODO"; storage_account_name = "TODO"; container_name = "tfstate"; key = "production.tfstate" }

  required_providers {
    # TODO(#9): 選択したプロバイダのみを有効化する
    # aws = {
    #   source  = "hashicorp/aws"
    #   version = "~> 5.0"
    # }
    # google = {
    #   source  = "hashicorp/google"
    #   version = "~> 5.0"
    # }
    # azurerm = {
    #   source  = "hashicorp/azurerm"
    #   version = "~> 3.0"
    # }
  }
}
