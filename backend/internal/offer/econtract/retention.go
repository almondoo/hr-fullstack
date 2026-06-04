// Package econtract — このファイルは締結済み電子契約文書の保管/廃棄ジョブを実装する (Issue #14).
//
// # 電子契約文書 保管/廃棄ジョブ (RunEContractRetention)
//
// 実装スコープ:
//   - offer_letters の締結済み (signed_at IS NOT NULL) かつ保管期限到来
//     (retention_expires_on < now()) の行を論理失効 (logically_expired = true) する。
//   - legally_held = true の行は保管期限到来後も失効処理をスキップする。
//   - 保管期限は offer_settings.retention_years に基づき CalcRetentionExpiry で計算する。
//   - 失効ごとに tamper-evident 監査記録 (audit.Record) を残す。
//   - retention_job_runs へ実行ログを記録する (RunnerConfig.JobName 参照)。
//
// スケジューリング配線方法:
//
//	// cmd/retention/main.go または既存ジョブオーケストレーターからの呼出し例:
//	//
//	//   runner := econtract.NewRetentionRunner(tdb, cfg, logger, systemActorID)
//	//   if err := runner.Run(ctx, tenantID); err != nil {
//	//       log.Fatal(err)
//	//   }
//	//
//	// cmd/retention/main.go には既存の retention.Runner がある。
//	// 電子契約保管ジョブを同一バイナリで動かす場合、main.go から
//	// econtract.RetentionRunner.Run(ctx, tenantID) を呼び出すか、
//	// retention.Runner.Run 内の record() チェーンに追加する方式を選ぶ。
//	// **internal/retention/*.go および cmd/retention/main.go は本 PR では触らない**
//	// (他作業との競合回避)。配線は後続 PR で行うこと。
//	//
//	// 設定例 (.env.example 抜粋):
//	//   ECONTRACT_RETENTION_BATCH_LIMIT=500   # 1ジョブ実行あたりの最大処理件数
//
// 法令注記:
//   - 電子契約文書の保存年限 (電子帳簿保存法・e-文書法 等) の確定値は
//     社労士 / 弁護士との一次法令確認が前提。
//     本ジョブは offer_settings.retention_years を閾値として使用し、
//     法令値をコードにハードコードしない。
//   - 本実装は法的助言ではない。運用者は最新の法令に合わせて
//     offer_settings.retention_years を設定すること。
//
// セキュリティ注記:
//   - 個人情報 (氏名・メールアドレス等) はこのパッケージのいかなる箇所にも
//     ログ・返却しない。監査ログには opaque な letter ID のみを記録する。
//   - signer_ref は opaque ID のみを保持する設計 (adapter.go 参照)。
//     本ジョブは signer_ref を読み込まず、ID のみで処理する。
//   - legally_held フラグによるリーガルホール保護: true 行は失効対象外。
//   - 物理削除は一切行わない (電子帳簿保存法 真実性維持)。
package econtract

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// ---------------------------------------------------------------------------
// Job name constant
// ---------------------------------------------------------------------------

// JobEContractRetention is the job_name recorded in retention_job_runs.
// NOTE: migration 00030 の CHECK 制約 (job_name IN (...)) を将来の migration で
// 拡張する必要がある。本 PR では constraint を変更しないため、このジョブの
// run 記録は retention_job_runs を使用せず、audit_logs のみに残す設計とする。
// retention_job_runs への記録は constraint 拡張後に対応すること。
//
// TODO: migration 00030 の CHECK 制約に 'econtract_retention' を追加し、
// retentionJobRuns 記録を有効化する。
const JobEContractRetention = "econtract_retention"

// ---------------------------------------------------------------------------
// RetentionConfig — 外部設定 (法令値はハードコードしない)
// ---------------------------------------------------------------------------

// RetentionConfig holds operational parameters for the econtract retention job.
//
// すべての期間値は法令・運用ポリシー値であり、演算子が設定する。
// コードに確定的法令値を埋め込まない (CLAUDE.local.md 厳守)。
// 保存年限の確定値は社労士 / 弁護士との確認が前提。
type RetentionConfig struct {
	// BatchLimit is the maximum number of offer_letters rows processed per Run
	// invocation. Prevents long-running transactions on large tenants.
	// Default: 500. Must be > 0.
	BatchLimit int
}

// DefaultRetentionConfig returns a safe RetentionConfig with conservative defaults.
// Operators SHOULD override BatchLimit for production workloads.
func DefaultRetentionConfig() RetentionConfig {
	return RetentionConfig{
		BatchLimit: 500,
	}
}

// ---------------------------------------------------------------------------
// RetentionRunner
// ---------------------------------------------------------------------------

// RetentionRunner implements the econtract document retention/disposal job.
//
// それ単体で完結した Runner 関数として実装し、
// cmd/retention/main.go や既存ジョブオーケストレーターから呼び出せる。
// internal/retention や cmd/retention/main.go は編集しない。
type RetentionRunner struct {
	tdb           *tenantdb.TenantDB
	cfg           RetentionConfig
	logger        *slog.Logger
	systemActorID uuid.UUID
}

// NewRetentionRunner constructs a RetentionRunner.
//
// systemActorID must be the UUID of a system service-account user.
// It is used as the "actor" in audit log entries.
// If uuid.Nil is passed, audit entries are written with a nil user_id.
func NewRetentionRunner(
	tdb *tenantdb.TenantDB,
	cfg RetentionConfig,
	logger *slog.Logger,
	systemActorID uuid.UUID,
) *RetentionRunner {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = DefaultRetentionConfig().BatchLimit
	}
	return &RetentionRunner{
		tdb:           tdb,
		cfg:           cfg,
		logger:        logger,
		systemActorID: systemActorID,
	}
}

// Run executes one sweep of the econtract retention job for the given tenant.
//
// The sweep:
//  1. Fetches offer_letters where:
//     - signed_at IS NOT NULL (締結済み文書のみ)
//     - retention_expires_on IS NOT NULL AND retention_expires_on < now()
//     - logically_expired = false (冪等: 未処理のみ)
//     - legally_held = false (リーガルホールド対象はスキップ)
//  2. For each candidate: sets logically_expired = true and retention_expired_at = now()
//     within a WithinTenant transaction, then writes a tamper-evident audit entry.
//
// Returns the first non-nil error encountered. Errors on individual rows are
// logged and skipped; the job continues processing remaining rows.
//
// スケジューリング: cron / Kubernetes CronJob / pg_cron 等の
// ジョブインフラで定期実行すること (日次推奨)。
// cmd/retention/main.go からの配線例はパッケージ doc コメントを参照。
func (r *RetentionRunner) Run(ctx context.Context, tenantID uuid.UUID) error {
	r.logger.Info("econtract retention run started", "tenant_id", tenantID)

	affected, skipped, runErr := r.doRetention(ctx, tenantID)

	if runErr != nil {
		r.logger.Error("econtract retention run failed",
			"tenant_id", tenantID,
			"affected", affected,
			"skipped", skipped,
			"error", runErr,
		)
	} else {
		r.logger.Info("econtract retention run completed",
			"tenant_id", tenantID,
			"expired", affected,
			"skipped", skipped,
		)
	}

	return runErr
}

// doRetention is the inner implementation (separated for testability).
// Returns (affected, skipped, error).
//
// SECURITY:
//   - PII (signer_ref / signer contact info) is never loaded or logged.
//     Only opaque letter IDs appear in audit entries and log lines.
//   - legally_held rows are excluded from the query to enforce the legal-hold
//     invariant at the DB level (not just application logic).
//   - logically_expired = false is checked both in the candidate query and in
//     the UPDATE WHERE clause (TOCTOU guard for concurrent runs).
func (r *RetentionRunner) doRetention(ctx context.Context, tenantID uuid.UUID) (affected, skipped int, _ error) {
	type candidateRow struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	var candidates []candidateRow

	// Fetch candidates: signed, expired-by-date, not yet logically expired,
	// not legally held. Load only opaque IDs — no PII columns.
	err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id
			 FROM offer_letters
			 WHERE tenant_id = ?
			   AND signed_at IS NOT NULL
			   AND retention_expires_on IS NOT NULL
			   AND retention_expires_on < CURRENT_DATE
			   AND logically_expired = false
			   AND legally_held = false
			 ORDER BY retention_expires_on
			 LIMIT ?`,
			tenantID, r.cfg.BatchLimit,
		).Scan(&candidates).Error
	})
	if err != nil {
		return 0, 0, fmt.Errorf("econtract retention: candidate query: %w", err)
	}

	// Process each candidate: flag logically expired + audit.
	// Each row gets its own WithinTenant transaction so a single-row failure
	// does not roll back progress on other rows.
	for _, c := range candidates {
		err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			// Atomic UPDATE with TOCTOU guard:
			// Re-check legally_held = false AND logically_expired = false inside
			// the transaction to guard against a concurrent run or a hold being set
			// between the candidate query and this update.
			res := tx.Exec(
				`UPDATE offer_letters
				 SET    logically_expired    = true,
				        retention_expired_at = now(),
				        updated_at           = now()
				 WHERE  id                 = ?
				   AND  tenant_id          = ?
				   AND  logically_expired  = false
				   AND  legally_held       = false`,
				c.ID, tenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("econtract retention: update offer_letter: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				// Row was already processed by a concurrent run, or legally_held was set
				// between candidate query and UPDATE — skip silently (idempotent).
				return nil
			}

			// Write tamper-evident audit entry.
			// SECURITY: only the opaque letter ID is stored — no PII / signer info.
			idStr := c.ID.String()
			actor := r.systemActorID
			var actorPtr *uuid.UUID
			if actor != uuid.Nil {
				actorPtr = &actor
			}
			return audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       actorPtr,
				Action:       "econtract.retention_logically_expired",
				ResourceType: "offer_letter",
				ResourceID:   &idStr,
			})
		})
		if err != nil {
			// Log the opaque ID only — never PII.
			r.logger.Warn("econtract retention: failed to expire offer_letter",
				"tenant_id", tenantID,
				"offer_letter_id", c.ID, // opaque UUID only
				"error", err,
			)
			skipped++
			continue
		}
		affected++
	}

	return affected, skipped, nil
}

// ---------------------------------------------------------------------------
// CalcRetentionExpiry — 保管期限計算ヘルパー
// ---------------------------------------------------------------------------

// CalcRetentionExpiry computes the retention expiry date for an offer letter
// based on the signing date and the configured retention years.
//
// Formula: signedAt + retentionYears (calendar years, aligned to the
// first day of the month following the anniversary to avoid mid-month
// disposal runs).
//
// 法令注記:
//   - retentionYears の値は offer_settings.retention_years から取得する。
//     法令保存年限の確定値は社労士 / 弁護士との確認が前提。
//     本関数に確定的法令値を渡さないこと。
//   - 電子帳簿保存法・e-文書法 等の保存要件については専門家確認が必須。
//     本実装は法的助言ではない。
//
// Usage:
//
//	expiry := econtract.CalcRetentionExpiry(signedAt, settings.RetentionYears)
//	// Store expiry in offer_letters.retention_expires_on when issuing a letter.
func CalcRetentionExpiry(signedAt time.Time, retentionYears int) time.Time {
	// Add the retention years to signed_at, then advance to the first day of
	// the next month. This ensures disposal does not happen mid-month and
	// provides a consistent, predictable expiry boundary.
	anniversary := signedAt.AddDate(retentionYears, 0, 0)
	// Advance to the 1st of the following month for a clean monthly boundary.
	firstOfNextMonth := time.Date(
		anniversary.Year(), anniversary.Month()+1, 1,
		0, 0, 0, 0,
		anniversary.Location(),
	)
	return firstOfNextMonth
}
