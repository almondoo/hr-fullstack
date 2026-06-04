terraform {
  required_version = ">= 1.6"

  # State バックエンド: S3 + DynamoDB ロック (#9)
  # NOTE: バケット名・リージョン・テーブル名は実環境に合わせて設定すること。
  #       値はこのファイルに書かずに、init 時に -backend-config フラグで渡すこと。
  #       例:
  #         terraform init \
  #           -backend-config="bucket=<your-tfstate-bucket>" \
  #           -backend-config="key=production/terraform.tfstate" \
  #           -backend-config="region=ap-northeast-1" \
  #           -backend-config="dynamodb_table=<your-tfstate-lock-table>"
  backend "s3" {
    # bucket         = "<your-tfstate-bucket>"       # 手動設定 (init -backend-config 推奨)
    key     = "production/terraform.tfstate"
    region  = "ap-northeast-1"
    encrypt = true # 必須: State ファイルの暗号化 (README.md セキュリティ原則)
    # dynamodb_table = "<your-tfstate-lock-table>"   # 手動設定
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
