# メール配信基盤 設計メモ — SPF / DKIM / DMARC / バウンス管理

本ドキュメントは issue #13「メール配信基盤の本番実装」の外部依存部分の設計メモ。
実装足場は `backend/internal/notification/mail_sender.go` と
`backend/internal/notification/bounce_webhook.go` に存在する。
**DNS 設定・送信ドメイン所有権の確認は運用チームが実施すること。**

---

## 1. 送信プロバイダ選定

| 項目 | Amazon SES | SendGrid |
|---|---|---|
| 実装ファイル | `SESSender` in `mail_sender.go` | `SendGridSender` in `mail_sender.go` |
| 差し替え方法 | `NewServiceWithMailer(tdb, ses)` | `NewServiceWithMailer(tdb, sg)` |
| 必要な環境変数 | 下記「SES 設定」参照 | 下記「SendGrid 設定」参照 |
| バウンス受信 | SNS → `/webhooks/ses/bounce` | Event Webhook → `/webhooks/sendgrid` |

---

## 2. 環境変数一覧

`.env.example` の「メール配信」セクションに記載されたプレースホルダを確認すること。

### Amazon SES

| 変数名 | 内容 | 必須 |
|---|---|---|
| `AWS_REGION` | SES エンドポイントのリージョン (例: `ap-northeast-1`) | 必須 |
| `NOTIFICATION_SES_FROM_ADDRESS` | 送信元アドレス (SES で検証済み) | 必須 |
| `NOTIFICATION_SES_ACCESS_KEY_ID` | AWS アクセスキー ID | 推奨: IAM ロールで代替 |
| `NOTIFICATION_SES_SECRET_ACCESS_KEY` | AWS シークレットアクセスキー | 推奨: IAM ロールで代替 |
| `NOTIFICATION_SES_SNS_TOPIC_ARN` | バウンス通知 SNS トピック ARN | バウンス受信時必須 |

### SendGrid

| 変数名 | 内容 | 必須 |
|---|---|---|
| `NOTIFICATION_SENDGRID_API_KEY` | SendGrid API キー (Mail Send 権限) | 必須 |
| `NOTIFICATION_SENDGRID_FROM_ADDRESS` | 送信元アドレス (SendGrid で検証済み) | 必須 |
| `NOTIFICATION_SENDGRID_WEBHOOK_SIGNING_KEY` | Event Webhook 署名検証キー | セキュリティ向上のため推奨 |

### 共通

| 変数名 | 内容 | 必須 |
|---|---|---|
| `NOTIFICATION_WEBHOOK_ACTOR_ID` | バウンス Webhook 監査レコードの actor_id (UUID) | 推奨 |

---

## 3. SPF 設定

SPF はメール送信者が許可した IP アドレスのリストを DNS TXT レコードで公開し、
受信 MTA がなりすましを検出できるようにする仕組み。

### Amazon SES の場合

```
送信ドメイン TXT レコード (例: example.co.jp)
v=spf1 include:amazonses.com ~all
```

- SES 送信ドメインの検証後、SES コンソールの「Email addresses / Domains」で
  表示される SPF レコードをそのまま DNS に追加する。
- `~all` (softfail) を使用し、全スコープが確認できたら `-all` (hardfail) に変更する。

### SendGrid の場合

```
送信ドメイン TXT レコード
v=spf1 include:sendgrid.net ~all
```

- 複数サービスから送信する場合は `include` を連結する (10 lookup 制限に注意)。

---

## 4. DKIM 設定

DKIM はメール本文・ヘッダの電子署名を DNS に公開した公開鍵で検証する。
受信側が改ざんを検出できる。

### Amazon SES の場合

1. SES コンソール → ドメイン → DKIM → 「DKIM 署名を有効にする」。
2. 表示された 3 つの CNAME レコード (`xxxxx._domainkey.example.co.jp`) を DNS に追加。
3. SES が自動的に Easy DKIM (RSA 2048-bit) で署名する。

### SendGrid の場合

1. SendGrid ダッシュボード → Sender Authentication → Authenticate Your Domain。
2. 表示された CNAME レコード 2 件 (`em1234.example.co.jp` + `s1._domainkey.example.co.jp`) を DNS に追加。
3. 検証完了後、Dedicated IP のリンク確認も行う。

---

## 5. DMARC 設定

DMARC は SPF または DKIM のアライメント (送信者ドメインとの一致) を要件として、
ポリシーを DNS で公開する。バウンス・なりすましレポートも受信できる。

### 推奨 DNS TXT レコード (段階的展開)

```
_dmarc.example.co.jp TXT "v=DMARC1; p=none; rua=mailto:dmarc-rua@example.co.jp; ruf=mailto:dmarc-ruf@example.co.jp; fo=1"
```

1. **フェーズ 1** (`p=none`): レポートのみ収集。配信に影響しない。
2. **フェーズ 2** (`p=quarantine`): SPF/DKIM ミスマッチメールをスパム扱い。
3. **フェーズ 3** (`p=reject`): 完全拒否 (最高強度)。

- `rua` / `ruf` には DMARC レポート受信用メールアドレスを設定する (例: `dmarc-reports@`...)。
- Google Postmaster Tools や Valimail Monitor でレポートを可視化することを推奨。

---

## 6. バウンス・苦情 処理フロー

```
送信          SES/SendGrid
                  |
          バウンス/苦情発生
                  |
     SNS Notification / Event Webhook
                  |
     POST /webhooks/ses/bounce
     または
     POST /webhooks/sendgrid
                  |
     BounceWebhookHandler.HandleSES / HandleSendGrid
                  |
     Service.MarkBounced (status: bounced / complained)
                  |
     email_deliveries.status 更新
```

### 実装済み (足場)

- `bounce_webhook.go`: `HandleSESBounce`, `HandleSendGridEvent` — SNS/SendGrid パース・MarkBounced 呼び出し。
- `Service.findDeliveriesByProviderAndHash`: スタブ (下記「本番ワイヤリング」参照)。
- `RegisterBounceWebhookRoutes`: 非認証ルータグループへの登録ヘルパー。

### 本番ワイヤリング 残作業

1. **DB 特権アクセサ**: `findDeliveriesByProviderAndHash` は RLS バイパスが必要 (テナントをまたぐクロスルックアップ)。
   `tenantdb.SupervisorDB` 相当のコネクションを `BounceWebhookHandler` に渡して実装する。
2. **SNS 署名検証**: `HandleSESBounce` は Topic ARN ヘッダーのみ検証。本番では
   `SigningCertURL` から証明書を取得し、SNS メッセージ署名を RSA-SHA1 で検証すること。
3. **SendGrid 署名検証タイムスタンプ**: `verifySignature` はリプレイ防止なし。
   `X-Twilio-Email-Event-Webhook-Timestamp` を HMAC 入力に含め、許容ウィンドウ (例: 5 分) を設けること。
4. **再試行抑制**: バウンス・苦情を記録した宛先への再送信を抑制するロジック (`Publish` 内の
   suppression list チェック) は未実装。SES/SendGrid の Suppression List 機能との連携を検討。
5. **コンテキスト伝播**: `processSESNotification` のシグネチャを `context.Context` に変更する
   (現在はスキャフォールドのため `interface{ Done() <-chan struct{} }` でコンパイルを通している)。

---

## 7. PII 最小化 (本番メール本文設計)

- メール本文には氏名・メールアドレス・マイナンバー・口座情報・健診結果等の
  実 PII を含めない。
- 詳細は「アプリ内のディープリンクを参照してください」のみ記載し、認証済みアプリ画面に誘導する。
- `TemplateData` に渡す値は非 PII のイベント識別子・深リンクのみとする。
- メールアドレス自体は `email_deliveries.to_email_enc` (AES-256-GCM) + `to_email_hash` のみで
  管理し、本文・監査レコード・ログに平文を出さない (既実装)。

---

*このドキュメントは設計メモであり、SPF/DKIM/DMARC の DNS 設定・送信ドメイン所有権確認・
送信量計画は運用チームと社内セキュリティ担当者が一次情報 (AWS/SendGrid 公式ドキュメント)
を参照して実施すること。法的義務に関する判断は社労士・弁護士に確認すること。*
