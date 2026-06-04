# infra/ — IaC (Issue #9 / #10 / #16 / #17 / #19)

> **ステータス**: AWS (ap-northeast-1) を採用。Terraform 実装済み。
> GAP-01 (クラウドプロバイダ選定) 解決: **AWS ECS Fargate + RDS for PostgreSQL**。

## ディレクトリ構成

```
infra/
├── README.md                  ← 本ファイル
├── modules/                   ← 再利用可能な Terraform モジュール
│   ├── network/               ← VPC / パブリック・プライベートサブネット / NAT GW / SG
│   ├── database/              ← RDS for PostgreSQL (Multi-AZ / PITR / KMS 暗号化)
│   ├── container/             ← ECS Fargate / ALB / ECR / オートスケーリング
│   ├── secrets/               ← AWS Secrets Manager + KMS CMK (列暗号 KEK / RDS)
│   └── waf/                   ← AWS WAFv2 WebACL (OWASP CRS / レートルール)
└── environments/              ← 環境ごとの root モジュール
    ├── production/            ← main.tf / versions.tf / terraform.tfvars.example
    ├── staging/               ← main.tf / versions.tf / terraform.tfvars.example
    └── development/           ← main.tf / versions.tf
```

## クラウドプロバイダ選定基準 (Issue #9 より)

GAP-01 で以下の観点から一つを選択する。選定後に各モジュールのプレースホルダを埋める。

| 観点 | AWS (ECS + RDS) | GCP (Cloud Run + Cloud SQL) | Azure (Container Apps + Flexible Server) |
|------|----------------|-----------------------------|------------------------------------------|
| チームの既存スキル | **最重視** | | |
| 既存の企業契約・割引 | | | |
| 月額コスト試算 (小規模 SaaS) | \$200-400/月 程度 | \$150-350/月 程度 | \$180-380/月 程度 |
| マネージド PostgreSQL | RDS for PostgreSQL (自動バックアップ・PITR 標準) | Cloud SQL for PostgreSQL (同上) | Azure Database for PostgreSQL Flexible Server (同上) |
| シークレット管理 | AWS Secrets Manager / KMS | GCP Secret Manager / Cloud KMS | Azure Key Vault |
| WAF / DDoS | AWS WAF + Shield Standard | Cloud Armor | Azure DDoS Protection + WAF |
| コンテナオーケストレーション | ECS Fargate (最小運用) / EKS (将来) | Cloud Run (サーバーレス) | Container Apps (サーバーレス) |
| 国内リージョン | ap-northeast-1 (東京) | asia-northeast1 (東京) | japaneast (東日本) |

**選定結果: AWS (ECS Fargate + RDS for PostgreSQL)**

CI に `terraform fmt -check` + `terraform validate` を追加済み (`.github/workflows/ci.yml`)。

実際の `terraform apply` は `.tfvars` を用意した後に手動または CD パイプラインで実施すること。

## Terraform バージョンポリシー

- Terraform >= 1.6 (OpenTofu 互換) を推奨。
- バージョンは `infra/environments/*/versions.tf` の `required_version` で固定する。
- State バックエンド: クラウド選定後に S3 / GCS / Azure Blob を設定する。**ローカル State は本番で使わない。**

## セキュリティ原則 (CLAUDE.local.md 準拠)

- シークレット・鍵・パスワードをコードにハードコードしない。すべて変数参照またはシークレットマネージャ経由。
- RLS / テナント分離 / RBAC の DB 設定は `modules/database/` で管理し、手作業変更を禁止。
- State ファイルには暗号化を有効化すること (S3: `encrypt = true` / GCS: CMEK / Azure Blob: SSE)。
