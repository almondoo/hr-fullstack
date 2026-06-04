terraform {
  required_version = ">= 1.6"

  # State バックエンド: S3 + DynamoDB ロック (#9)
  backend "s3" {
    key     = "staging/terraform.tfstate"
    region  = "ap-northeast-1"
    encrypt = true
    # bucket         = "<your-tfstate-bucket>"     # init -backend-config で渡す
    # dynamodb_table = "<your-tfstate-lock-table>" # init -backend-config で渡す
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}
