# 年末調整 税率表・控除限度額 年次更新 SOP

> **前提・免責**: 本手順書の税率・控除額は、国税庁が公表する一次法令資料
> (下記 §6 参照) との突合により確認した値を記載していますが、年度改正により
> 随時変更されます。**社会保険労務士（社労士）または税理士による一次法令源と
> の確認が前提**です。本手順書は法的助言ではありません。実運用前に必ず
> 専門家の確認を取得してください。

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

### 確認済み速算表値（令和6年分・令和7年分 — 不変）

> 出典: 国税庁 No.2260 所得税の税率
> <https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/2260.htm>
>
> **注**: 速算表だけでは最終所得税額にならない。復興特別所得税（基準所得税額×2.1%、
> 2013–2037年適用）を別途上乗せすること（`CalculateTax` で計上済み）。
> 令和7年以後の超高所得者サーチャージ（課税所得330百万円超部分）は別建て（未実装）。

| 課税所得（円） | 税率 | 控除額（円） |
|---|---|---|
| 1,000 〜 1,949,000 | 5% | 0 |
| 1,950,000 〜 3,299,000 | 10% | 97,500 |
| 3,300,000 〜 6,949,000 | 20% | 427,500 |
| 6,950,000 〜 8,999,000 | 23% | 636,000 |
| 9,000,000 〜 17,999,000 | 33% | 1,536,000 |
| 18,000,000 〜 39,999,000 | 40% | 2,796,000 |
| 40,000,000 以上 | 45% | 4,796,000 |

> **万円丸め禁止**: 境界値は円単位で正確に管理すること。

### 令和7年度税制改正（施行=令和7年12月1日、令和7年分以後適用）

速算表ブラケット自体は不変ですが、**以下の控除項目が年度依存ロジックを要します**。
現状はコード内 TODO コメントで記録済み。完全実装は別 issue で対応してください。

| 改正項目 | 令和6以前 | 令和7以後 | 出典 |
|---|---|---|---|
| 基礎控除（合計所得2,400万以下） | 48万円（一律） | 段階制（132万以下=95万〜655万超=58万）※時限 | [国税庁](https://www.nta.go.jp/users/gensen/2025kiso/index.htm) / [No.1199](https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1199.htm) |
| 給与所得控除の最低保障額 | 55万円 | 65万円 | 国税庁 No.1410 |
| 扶養・配偶者の合計所得要件 | 48万円 | 58万円 | [No.1191](https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1191.htm) |

> 基礎控除の中間層上乗せ（132万超655万以下）は**令和7・8年分のみの時限措置**。
> 令和9年分以後は一律58万に戻る予定。年度別分岐の実装が必要。

### 据置項目（令和6・令和7とも不変）

| 項目 | 内容 | 出典 |
|---|---|---|
| 配偶者控除額 | 本人合計所得900万以下: 一般38万/老人(70歳以上)48万<br>900万超950万以下: 26万/32万<br>950万超1,000万以下: 13万/16万<br>1,000万超: 不適用 | 国税庁 No.1191 |
| 生命保険料控除(所得税) | 新契約(H24.1.1以後): 3区分各最高4万円<br>旧契約(H23.12.31以前): 各最高5万円<br>新旧通算合計上限: 12万円<br>**住民税は計算式・上限(計7万円)が異なる** | [No.1140](https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1140.htm) |

### 改修手順

1. 国税庁の最新速算表(No.2260)と上記確認済み値を比較する。
2. 変更がある場合は `computeIncomeTax` の `switch` ブロックを修正する。
3. コメント内の年度表記・出典URLを更新する。
4. 単体テスト `backend/internal/yearend/yearend_test.go` の計算ケースを
   新ブラケットに合わせて更新する。
5. `go test ./internal/yearend/... -p 1 -count=1` でテストが通ることを確認する。
6. PR を作成し、社労士または税理士のレビューコメントを添付する。

### 復興特別所得税

`CalculateTax` では `annualTax = incomeTax + (incomeTax * 21 / 1000)` として
2.1% を加算しています（平成25(2013)年〜令和19(2037)年まで適用）。
期限延長・廃止がある場合はここも合わせて改修してください。

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
      { "limit": 1949000,  "rate": 0.05, "deduction": 0 },
      { "limit": 3299000,  "rate": 0.10, "deduction": 97500 },
      { "limit": 6949000,  "rate": 0.20, "deduction": 427500 },
      { "limit": 8999000,  "rate": 0.23, "deduction": 636000 },
      { "limit": 17999000, "rate": 0.33, "deduction": 1536000 },
      { "limit": 39999000, "rate": 0.40, "deduction": 2796000 },
      { "limit": null,     "rate": 0.45, "deduction": 4796000 }
    ]
  },
  "deduction_limits_json": {
    // 注: 基礎控除は令和7年度改正で年度・合計所得依存の段階制に変更。
    // 合計所得655万超2350万以下の一律値(58万)を記録するが、
    // 中間層上乗せ(令和7・8年分のみ時限)は年度別ロジックで別途実装が必要。
    // 出典: 国税庁 https://www.nta.go.jp/users/gensen/2025kiso/index.htm
    "basic_deduction_reiwa7_below_1320000":  950000,
    "basic_deduction_reiwa7_below_3360000":  880000,
    "basic_deduction_reiwa7_below_4890000":  680000,
    "basic_deduction_reiwa7_below_6550000":  630000,
    "basic_deduction_reiwa7_below_23500000": 580000,
    "basic_deduction_reiwa6_below_24000000": 480000,
    // 給与所得控除の最低保障額: 令和6以前=55万 / 令和7以後=65万
    "employment_deduction_minimum_reiwa7": 650000,
    "employment_deduction_minimum_reiwa6": 550000,
    "employment_deduction_table": [
      { "income_limit": 1625000,  "deduction": 650000 },
      { "income_limit": 1800000,  "deduction_rate": 0.40, "min": 650000 },
      { "income_limit": 3600000,  "deduction_rate": 0.30, "adjustment": 180000 },
      { "income_limit": 6600000,  "deduction_rate": 0.20, "adjustment": 540000 },
      { "income_limit": 8500000,  "deduction_rate": 0.10, "adjustment": 1200000 },
      { "income_limit": null,     "deduction": 1950000 }
    ],
    // 生命保険料控除(所得税): 新契約各最高4万/旧契約各最高5万/合計上限12万
    // 住民税は計算式・上限(合計7万)が異なるため混同しない
    // 出典: 国税庁 No.1140 https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1140.htm
    "life_insurance_deduction_limit":       120000,
    "earthquake_insurance_deduction_limit":  50000,
    // 配偶者控除: 本人合計所得900万以下=38万/老人配偶者48万(令和6・7とも不変)
    // 扶養・配偶者の合計所得要件: 令和6以前48万 / 令和7以後58万
    "spouse_deduction_limit":               380000,
    "dependent_income_limit_reiwa7":         580000,
    "dependent_income_limit_reiwa6":         480000,
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
| 国税庁 No.2260 所得税の税率(速算表) | <https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/2260.htm> |
| 国税庁 No.1199 基礎控除(令和7改正) | <https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1199.htm> |
| 国税庁 令和7年分基礎控除特設 | <https://www.nta.go.jp/users/gensen/2025kiso/index.htm> |
| 国税庁 No.1191 配偶者控除 | <https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1191.htm> |
| 国税庁 No.1140 生命保険料控除 | <https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1140.htm> |
| 年末調整のしかた | <https://www.nta.go.jp/publication/pamph/gensen/nencho2024/01.htm> |

---

*本ドキュメントは毎年改正サイクルに合わせて更新してください。更新者・更新日・
参照した国税庁資料のURL・承認者（社労士氏名または税理士事務所名）を PR 説明に
記載することを必須とします。*
