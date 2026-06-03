# WAF / レート制限 / DDoS 対策設計メモ (Issue #17)

> **ステータス**: 設計検討フェーズ。エッジ/インフラ層はクラウドプロバイダ選定 (GAP-01) 後に実装。
> アプリケーション層のレート制限は**実装済み** (下記 §1 参照)。

---

## 1. 実装済み — アプリケーション層レート制限

認証エンドポイントへのレート制限は `ulule/limiter` を用いて実装済み。

- **ライブラリ**: `github.com/ulule/limiter/v3` (go.mod 参照)
- **適用対象**: 認証エンドポイント (ブルートフォース対策)
- **格納場所**: `backend/internal/` (server / middleware 層)
- **方式**: インメモリカウンタ (現在)。スケールアウト時は Redis バックエンドへの移行を検討。

**制限事項**: アプリ内レート制限はインスタンス単位の制限であり、複数インスタンスにまたがる集計は現時点では未対応。エッジレート制限 (§3) で補完する。

---

## 2. 防御レイヤーの全体像

```
Internet
    │
    ▼
[エッジ WAF / CDN] ← §3: クラウド選定後に実装
    │ IP制限・Bot対策・OWASP Top10ブロック
    ▼
[ロードバランサ] ← §4: クラウド選定後に実装
    │ エッジレート制限 (rps / IP)
    ▼
[アプリコンテナ] ← §1: 実装済み
    │ ulule/limiter によるエンドポイント単位レート制限
    ▼
[PostgreSQL] ← RLS + 接続制限 (infra/modules/database/ 参照)
```

---

## 3. エッジ WAF / DDoS 対策 (クラウド選定後)

| プロバイダ | WAF サービス | DDoS 対策 | CDN |
|-----------|------------|-----------|-----|
| AWS | AWS WAF v2 + ALB | Shield Standard (無料) / Advanced (有料) | CloudFront |
| GCP | Cloud Armor | Standard DDoS protection (Managed Protection) | Cloud CDN |
| Azure | Azure WAF (App Gateway / Front Door) | DDoS Protection Standard | Azure CDN / Front Door |
| Cloudflare (プロバイダ非依存) | Cloudflare WAF | DDoS Protection (全プラン) | Cloudflare CDN |

**推奨ルールセット** (WAF 実装時):
- OWASP Core Rule Set (CRS) 3.x ← SQLインジェクション・XSS 等
- レート制限ルール: 同一 IP から 100 req/分 を超えたらブロック (TBD: ビジネス要件で調整)
- Geo-blocking: 不要なリージョンからのトラフィックをブロック (任意)
- Bot 管理: 悪意のあるボットのフィンガープリントブロック

> **TODO(#17)**: WAF ルール設定は `infra/modules/network/` または専用モジュールとして GAP-01 解決後に実装。

---

## 4. エッジレート制限 (クラウド選定後)

アプリケーション層の `ulule/limiter` を補完するエッジレート制限。

**設計方針**:
- アクター: IP アドレス単位 (認証前) および テナント/ユーザー単位 (認証後)
- 閾値:
  - 認証エンドポイント: 10 req/分 (IP 単位) — ブルートフォース対策 (アプリ層と二重防御)
  - API 全体: TBD req/分 (IP 単位) — ビジネス要件で決定
  - バッチ/エクスポート系: TBD req/時間 (テナント単位) — 大量データ取得対策
- レスポンス: HTTP 429 Too Many Requests + `Retry-After` ヘッダ

**スケールアウト対応**:
- 複数インスタンス環境では Redis を `ulule/limiter` のバックエンドとして使用する
- TODO(#17): Redis 導入は infra/modules/container/ または専用モジュールで管理

---

## 5. IP 制限 / アクセス制御

| 対象 | 推奨制限 | 実装箇所 |
|------|---------|---------|
| DB ポート (5432) | アプリコンテナのプライベート IP のみ | infra/modules/network/ セキュリティグループ |
| 管理用 SSH / Bastion | 固定 IP / VPN 経由のみ | infra/modules/network/ |
| 管理 API エンドポイント | VPN / IP ホワイトリスト | WAF ルール or アプリミドルウェア |
| ヘルスチェックエンドポイント `/healthz` `/readyz` | LB からのみ (外部公開不要) | アプリまたは WAF ルール |

---

## 6. セキュリティヘッダ (実装済み)

`gin-contrib/secure` による以下のヘッダは実装済み (backend/internal/server/ 参照):
- `X-Frame-Options`
- `X-Content-Type-Options`
- `Strict-Transport-Security` (HSTS)
- `X-XSS-Protection`

CSRF 保護: `gorilla/csrf` で実装済み (issue #4 完了)。

---

## 7. 依存関係

- GAP-01 (クラウドプロバイダ選定) — Issue #9
- ロードバランサ / コンテナ基盤 — Issue #9 (IaC 実装時に WAF を統合)
- Redis (エッジレート制限スケールアウト) — 別途タスク
