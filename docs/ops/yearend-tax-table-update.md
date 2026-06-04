# 年末調整 税率表・控除限度額 年次更新 SOP

> **前提**: 本手順書に記載された税率・控除限度額の具体値はすべてプレースホルダ
> または近似値です。国税庁が毎年公表する「給与所得の源泉徴収税額表」「年末調整の
> しかた」等の一次法令資料と、**社会保険労務士（社労士）または税理士による確認**
> を経た上で値を確定してください。本手順書は法的助言ではありません。

---

## 1. 概要

年末調整に関わる税率・控除額は、所得税法および租税特別措置法の改正により
毎年変更される可能性があります。本システムでは以下の二層で管理しています。

| 層 | 場所 | 内容 |
|---|---|---|
| ハードコードブラケット | `backend/internal/yearend/service.go` の `computeIncomeTax` 関数 | 速算表（超過累進税率）のブラケット — **フォールバック用デフォルト値** |
| テナント設定 JSON | DB テーブル `yearend_settings` の `rate_table_json` / `deduction_limits_json` | テナントごとに上書き可能な年次設定 |

本番環境では **`yearend_settings`（DB）の値が優先** されるため、通常の年次
更新は DB を更新するだけで対応できます。`computeIncomeTax` の変更はソース
コード改修が必要なため、法改正に伴う抜本的な速算表変更がある場合のみ実施
してください。

---

## 2. 年次更新チェックリスト

```
[ ] 国税庁「給与所得の源泉徴収税額表（令和XX年分）」を入手
[ ] 国税庁「年末調整のしかた（令和XX年分）」を入手
[ ] 社労士 / 税理士が一次法令を確認・承認
[ ] 速算表ブラケットに変更があれば service.go を改修（下記 §3）
[ ] 給与所得控除・基礎控除・各種控除上限を確認し DB 設定を更新（§4）
[ ] ステージング環境で計算結果を目視確認
[ ] 本番適用・監査ログ確認
```

---

## 3. `computeIncomeTax` の速算表ブラケット変更（ソース改修）

### 対象ファイル

```
backend/internal/yearend/service.go
```

### 対象関数

```go
// computeIncomeTax applies the 所得税速算表 brackets.
func computeIncomeTax(taxableIncome int64) int64 { ... }
```

### 現行プレースホルダ値（2024年度近似）

> 以下の値は近似値です。国税庁の一次資料で必ず確認してください。

| 課税所得（円以下） | 税率 | 控除額（円） |
|---|---|---|
| 1,950,000 | 5% | 0 |
| 3,300,000 | 10% | 97,500 |
| 6,950,000 | 20% | 427,500 |
| 9,000,000 | 23% | 636,000 |
| 18,000,000 | 33% | 1,536,000 |
| 40,000,000 | 40% | 2,796,000 |
| 40,000,000超 | 45% | 4,796,000 |

### 改修手順

1. 国税庁の最新速算表と上記プレースホルダ値を比較する。
2. 変更がある場合は `computeIncomeTax` の `switch` ブロックを修正する。
3. コメント内の PLACEHOLDER 注記と年度表記を更新する。
4. 単体テスト `backend/internal/yearend/yearend_test.go` の計算ケースを
   新ブラケットに合わせて更新する。
5. `go test ./internal/yearend/... -p 1 -count=1` でテストが通ることを確認する。
6. PR を作成し、社労士または税理士のレビューコメントを添付する。

### 復興特別所得税

`CalculateTax` では `annualTax = incomeTax + (incomeTax * 21 / 1000)` として
2.1% を加算しています（令和19年まで適用）。期限延長・廃止がある場合はここも
合わせて改修してください。

---

## 4. `yearend_settings` テーブルの DB 更新（管理 API 経由）

速算表ブラケットのコード改修を伴わない控除限度額・給与所得控除額の変更は、
`PUT /api/v1/yearend/settings` API または DB 直接更新で対応します。

### エンドポイント（管理者向け）

```
PUT /api/v1/yearend/settings
Content-Type: application/json
Authorization: Bearer <admin-token>

{
  "tax_year": 202X,
  "rate_table_json": {
    // 速算表ブラケット（将来的に computeIncomeTax を置き換える設計）
    // 現時点では参照実装未統合。下記形式で保存し監査証跡を残すこと。
    "brackets": [
      { "limit": 1950000,  "rate": 0.05, "deduction": 0 },
      { "limit": 3300000,  "rate": 0.10, "deduction": 97500 },
      { "limit": 6950000,  "rate": 0.20, "deduction": 427500 },
      { "limit": 9000000,  "rate": 0.23, "deduction": 636000 },
      { "limit": 18000000, "rate": 0.33, "deduction": 1536000 },
      { "limit": 40000000, "rate": 0.40, "deduction": 2796000 },
      { "limit": null,     "rate": 0.45, "deduction": 4796000 }
    ]
  },
  "deduction_limits_json": {
    // 以下はすべてプレースホルダ。一次法令で確認すること。
    "basic_deduction":                480000,
    "employment_deduction_table": [
      { "income_limit": 1625000,  "deduction": 550000 },
      { "income_limit": 1800000,  "deduction_rate": 0.40, "min": 650000 },
      { "income_limit": 3600000,  "deduction_rate": 0.30, "adjustment": 180000 },
      { "income_limit": 6600000,  "deduction_rate": 0.20, "adjustment": 540000 },
      { "income_limit": 8500000,  "deduction_rate": 0.10, "adjustment": 1200000 },
      { "income_limit": null,     "deduction": 1950000 }
    ],
    "life_insurance_deduction_limit":       120000,
    "earthquake_insurance_deduction_limit":  50000,
    "spouse_deduction_limit":               380000,
    "dependent_deduction_per_person":       380000,
    "surtax_rate":                            0.021
  }
}
```

> **注意**: `rate_table_json` / `deduction_limits_json` の値は現状の
> `CalculateTax` / `computeIncomeTax` では参照されていません（ハードコード
> された速算表が使用されます）。DB への保存は **設定の証跡記録と将来の
> 設定ドリブン化に備えるため**です。将来的に `computeIncomeTax` をこの
> JSON から動的に読み込む設計へ改修する際は、関連する design doc と
> migration を別途作成してください。

### DB 直接更新（メンテナンス時のみ）

```sql
-- 必ず BEGIN / COMMIT でラップし、ロールバックできる状態で実行すること。
BEGIN;

INSERT INTO yearend_settings (id, tenant_id, tax_year, rate_table_json, deduction_limits_json)
VALUES (
  gen_random_uuid(),
  '<tenant_uuid>',
  202X,
  '<rate_table_json>'::jsonb,
  '<deduction_limits_json>'::jsonb
)
ON CONFLICT (tenant_id, tax_year) DO UPDATE
  SET rate_table_json      = EXCLUDED.rate_table_json,
      deduction_limits_json = EXCLUDED.deduction_limits_json,
      updated_at            = now();

-- 確認
SELECT tax_year, rate_table_json, deduction_limits_json, updated_at
FROM yearend_settings
WHERE tenant_id = '<tenant_uuid>' AND tax_year = 202X;

COMMIT;
```

---

## 5. 監査・確認事項

- `yearend_settings` の変更は `audit_logs` テーブルに `yearend_settings.updated`
  として記録されます。変更後に監査ログの存在を確認してください。
- ステージング環境で代表的な年収パターン（300万・500万・800万・1000万・1500万円）の
  計算結果を手計算値と照合してください。
- 照合結果と社労士承認メモを PR / 変更管理チケットに添付してください。

---

## 6. 関連ファイル・参照先

| 対象 | パス |
|---|---|
| 計算エンジン | `backend/internal/yearend/service.go` |
| 速算表関数 | `service.go` → `computeIncomeTax` / `CalculateTax` |
| テスト | `backend/internal/yearend/yearend_test.go` |
| API ハンドラ | `backend/internal/yearend/handler.go` |
| DB スキーマ | `backend/db/migrations/` → `yearend_settings` テーブル定義 |
| 国税庁 | <https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/2260.htm> |
| 年末調整のしかた | <https://www.nta.go.jp/publication/pamph/gensen/nencho2024/01.htm> |

---

*本ドキュメントは毎年改正サイクルに合わせて更新してください。更新者・更新日・
参照した国税庁資料のURL・承認者（社労士氏名または税理士事務所名）を PR 説明に
記載することを必須とします。*
