# infra/ — IaC スケルトン (Issue #9)

> **ステータス**: クラウドプロバイダ未決定 (GAP-01)。
> このディレクトリはディレクトリ構造と選定基準の**足場**のみ。
> 実際のプロバイダリソースは GAP-01 解決後に埋める。

## ディレクトリ構成

```
infra/
├── README.md                  ← 本ファイル
├── modules/                   ← 再利用可能な Terraform モジュール
│   ├── network/               ← VPC / サブネット / セキュリティグループ
│   ├── database/              ← マネージド PostgreSQL (RDS / Cloud SQL / Flexible Server)
│   ├── container/             ← コンテナ実行基盤 (ECS / Cloud Run / Container Apps)
│   └── secrets/               ← シークレット / KMS 管理
└── environments/              ← 環境ごとの root モジュール
    ├── production/
    ├── staging/
    └── development/
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

**選定後の作業**:

1. `infra/modules/` 内の `provider.tf.example` を `provider.tf` にリネームし、プロバイダを記入。
2. `infra/environments/production/main.tf` の `TODO` を埋める。
3. `terraform init && terraform plan` を CI に追加 (`.github/workflows/ci.yml` 担当者へ依頼)。

## Terraform バージョンポリシー

- Terraform >= 1.6 (OpenTofu 互換) を推奨。
- バージョンは `infra/environments/*/versions.tf` の `required_version` で固定する。
- State バックエンド: クラウド選定後に S3 / GCS / Azure Blob を設定する。**ローカル State は本番で使わない。**

## セキュリティ原則 (CLAUDE.local.md 準拠)

- シークレット・鍵・パスワードをコードにハードコードしない。すべて変数参照またはシークレットマネージャ経由。
- RLS / テナント分離 / RBAC の DB 設定は `modules/database/` で管理し、手作業変更を禁止。
- State ファイルには暗号化を有効化すること (S3: `encrypt = true` / GCS: CMEK / Azure Blob: SSE)。
