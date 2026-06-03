// Package govfiling — this file implements the 36協定 e-Gov 電子届出 足場.
//
// # 36協定 e-Gov 接続点 (Issue #21)
//
// CreateArticle36Filing creates a gov_filings job of type 'article36_filing'
// linked to an agreement document row in the workrule package.
//
// 設計:
//   - govfiling パッケージは workrule パッケージを直接インポートしない
//     (循環依存回避)。呼出は workrule.EGovFilingBridge インタフェース経由で行う。
//   - Article36FilingInput には agreement document の不透明 ID のみを
//     格納し、機微情報は格納しない。
//   - 冪等キーは呼出側 (workrule.Service.InitiateEGovFiling) が生成し渡す。
//     重複呼出に対して DB の UNIQUE 制約 (uq_gov_filings_idempotency) で保護する。
//   - EGovSubmitter を設定したい場合は Service.WithEGovSubmitter で差し替える。
//     設定しない場合は eGovStubSubmitter (スタブ) が使われる。
//
// セキュリティ注記:
//   - PayloadJSON / ExternalRef / 監査 resource_id に機微情報を含めない。
//   - マイナンバー等の復号値は格納・ログ記録しない。
//
// 法令注記: 36協定様式・届出要件は社労士/弁護士との確認が前提。
// 本実装は法的助言ではない。
package govfiling

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/notification"
	"github.com/your-org/hr-saas/internal/platform/audit"
)

// ---------------------------------------------------------------------------
// EGovSubmitter injection on Service
// ---------------------------------------------------------------------------

// WithEGovSubmitter returns a copy of the Service that uses the given
// EGovSubmitter for 36協定 filings. Pass NewEGovStubSubmitter() to use the
// no-op stub; pass the real adapter once e-Gov credentials are available (P3).
func (s *Service) WithEGovSubmitter(sub EGovSubmitter) *Service {
	return &Service{
		tdb:              s.tdb,
		submitter:        s.submitter,
		mynumberProvider: s.mynumberProvider,
		egovSubmitter:    sub,
	}
}

// ---------------------------------------------------------------------------
// Article36FilingInput / Article36FilingRef
// ---------------------------------------------------------------------------

// Article36FilingInput holds the parameters for creating a 36協定 gov_filing job.
//
// PayloadJSON must contain only reference IDs and non-sensitive metadata.
// Decrypted 機微情報 (マイナンバー等) MUST NOT be included.
type Article36FilingInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// LaborAgreementDocID is the opaque ID of the agreement document row
	// (labour_agreement_documents) that this govfiling job represents.
	// Stored in PayloadJSON as a reference only; never sent to e-Gov in plaintext.
	LaborAgreementDocID uuid.UUID //nolint:misspell // field name matches DB schema contract
	// FilingRepresentativeID is the employee ID of the filing representative
	// (届出担当者). The gov_filings table requires (employee_id, tenant_id) ∈ employees;
	// for agreement-level 36協定 filings this is the representative who submits.
	// Required: must belong to the same tenant.
	FilingRepresentativeID uuid.UUID
	// IdempotencyKey prevents duplicate filing jobs.
	// Callers should derive this from a stable business key
	// (e.g. "<tenantID>:<laborAgreementDocID>:<validTo>").
	IdempotencyKey string
	IP             *string
}

// Article36FilingRef is the result of CreateArticle36Filing.
// It contains only opaque IDs — no machine-readable status is surfaced here;
// use GetFiling / ListFilings for status polling.
type Article36FilingRef struct {
	// FilingID is the newly created gov_filings row ID.
	FilingID uuid.UUID
	// ExternalRef is populated after a successful SubmitArticle36 call
	// (stub: deterministic mock; real: e-Gov 受付番号).
	ExternalRef string
}

// ---------------------------------------------------------------------------
// Service fields (extended by this file)
// ---------------------------------------------------------------------------
// NOTE: egovSubmitter is lazily referenced by the Service struct defined in
// service.go. To avoid duplicating the struct definition, we extend it via
// Go's package-level init pattern: Service already has tdb/submitter/
// mynumberProvider; we add egovSubmitter by extending service.go's struct.
// Since Go does not allow struct extension across files in the same package,
// the egovSubmitter field is declared in service.go; this file adds the
// method that uses it.

// ---------------------------------------------------------------------------
// CreateArticle36Filing
// ---------------------------------------------------------------------------

// CreateArticle36Filing creates a gov_filings job of type 'article36_filing'
// in draft status and, if an EGovSubmitter is configured, immediately submits
// it via the e-Gov stub (or real adapter in P3).
//
// The function is idempotent: if a job with the same idempotency_key already
// exists (UNIQUE constraint violation), the existing job ID is returned.
//
// Transaction boundaries:
//   - Draft INSERT + audit entry in one transaction.
//   - SubmitFiling (status transition + outbox) in a second transaction,
//     following the same pattern as the general SubmitFiling path.
//
// Caller responsibilities:
//   - After CreateArticle36Filing returns successfully, the caller (workrule)
//     should update the agreement document's govfiling_id column with FilingID.
func (s *Service) CreateArticle36Filing(ctx context.Context, in Article36FilingInput) (*Article36FilingRef, error) {
	if in.IdempotencyKey == "" {
		return nil, fmt.Errorf("govfiling: create article36: idempotency_key is required")
	}

	// Build payload: reference IDs only, no 機微情報.
	type article36Payload struct {
		LaborAgreementDocID string `json:"labor_agreement_doc_id"` //nolint:misspell // JSON key matches DB schema contract
	}
	payloadBytes, err := json.Marshal(article36Payload{
		LaborAgreementDocID: in.LaborAgreementDocID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("govfiling: create article36: marshal payload: %w", err)
	}

	filingID := uuid.New()

	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Check for existing job with this idempotency key (idempotent path).
		var existing struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Raw(
			`SELECT id FROM gov_filings WHERE tenant_id = ? AND idempotency_key = ? LIMIT 1`,
			in.TenantID, in.IdempotencyKey,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("govfiling: create article36 check existing: %w", err)
		}
		if existing.ID != uuid.Nil {
			// Already exists: return the existing ID via the outer filingID variable.
			filingID = existing.ID
			return nil
		}

		// Insert new draft filing.
		// govfiling has no direct FK to the agreement documents table
		// (cross-package reference; intentional DI inversion).
		// FilingRepresentativeID (届出担当者) satisfies the composite FK
		// (employee_id, tenant_id) → employees(id, tenant_id).
		// Verify it belongs to this tenant before INSERT (defence in depth).
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.FilingRepresentativeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("govfiling: create article36 verify representative: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO gov_filings
			   (id, tenant_id, employee_id, filing_type, channel, status,
			    payload_json, idempotency_key, created_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?)`,
			filingID, in.TenantID, in.FilingRepresentativeID,
			FilingArticle36, ChannelEgov, StatusDraft,
			payloadBytes, in.IdempotencyKey, in.ActorID,
		).Error; err != nil {
			return fmt.Errorf("govfiling: create article36 insert: %w", err)
		}

		idStr := filingID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "gov_filing.article36_created",
			ResourceType: "gov_filing",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}

	ref := &Article36FilingRef{FilingID: filingID}

	// If no EGovSubmitter is configured, return the draft ref and let the caller
	// trigger submission separately (e.g., via SubmitFiling API or a background job).
	if s.egovSubmitter == nil {
		return ref, nil
	}

	// Submit via the EGovSubmitter (stub by default; real adapter in P3).
	submitResult, err := s.egovSubmitter.SubmitArticle36(ctx, Article36SubmitRequest{
		TenantID:            in.TenantID.String(),
		IdempotencyKey:      in.IdempotencyKey,
		PayloadJSON:         payloadBytes,
		LaborAgreementDocID: in.LaborAgreementDocID.String(),
	})
	if err != nil {
		// Submission failure: leave in draft (caller can retry via SubmitFiling).
		// Do not surface raw e-Gov errors to callers; wrap for safe error handling.
		return ref, fmt.Errorf("govfiling: article36 egov submit: %w", err)
	}

	// Persist external_ref and transition draft → submitted.
	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE gov_filings
			 SET status = ?, external_ref = ?, submitted_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			StatusSubmitted, submitResult.ExternalRef, filingID, in.TenantID, StatusDraft,
		)
		if res.Error != nil {
			return fmt.Errorf("govfiling: article36 update submitted: %w", res.Error)
		}

		histID := uuid.New()
		if err := tx.Exec(
			`INSERT INTO gov_filing_status_history
			   (id, tenant_id, filing_id, from_status, to_status, note, changed_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			histID, in.TenantID, filingID, StatusDraft, StatusSubmitted,
			"auto-submitted via e-Gov adapter (article36)", in.ActorID,
		).Error; err != nil {
			return fmt.Errorf("govfiling: article36 record history: %w", err)
		}

		idStr := filingID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "gov_filing.article36_submitted",
			ResourceType: "gov_filing",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return err
		}

		return notification.InsertOutbox(tx, notification.InsertOutboxEntry{
			TenantID:        in.TenantID,
			EventType:       "govfiling.article36_submitted",
			ActorUserID:     &in.ActorID,
			RecipientUserID: in.ActorID,
			ResourceType:    "gov_filing",
			ResourceID:      &filingID,
		})
	})
	if err != nil {
		// Submission succeeded externally but DB update failed; return partial ref
		// so caller can retry the status sync via UpdateStatus.
		return ref, fmt.Errorf("govfiling: article36 persist submitted state: %w", err)
	}

	ref.ExternalRef = submitResult.ExternalRef
	return ref, nil
}
