terraform {
  required_version = ">= 1.6"

  # 開発環境: ローカル State も許容 (CI 統合テスト用一時環境では S3 を使うこと)
  # 本番・ステージングと同様に S3 バックエンドを推奨:
  # backend "s3" {
  #   key     = "development/terraform.tfstate"
  #   region  = "ap-northeast-1"
  #   encrypt = true
  # }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
