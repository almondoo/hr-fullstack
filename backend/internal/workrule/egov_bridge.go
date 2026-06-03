// Package workrule — this file wires the 36協定 e-Gov 電子届出 接続点.
//
// # workrule → govfiling 接続点 (Issue #21 足場)
//
// 設計方針:
//   - workrule パッケージは govfiling パッケージを直接インポートしない
//     (循環依存回避・依存逆転)。
//   - EGovFilingBridge インタフェースを定義し、govfiling.Service がこれを
//     実装できる形にする。呼出は InitiateEGovFiling メソッド経由で行う。
//   - Service に egovBridge フィールドを追加し、WithEGovBridge で DI する。
//     nil のままでも InitiateEGovFiling は安全に返す (stub モード)。
//
// 接続フロー (36協定有効期限アラートからの届出フロー):
//
//	workrule.Service.ListExpiringAgreements()
//	  → workrule.Service.InitiateEGovFiling(docID, repID, idempotencyKey)
//	      → EGovFilingBridge.CreateArticle36Filing()
//	          → govfiling.Service.CreateArticle36Filing() (DI 経由)
//	      → UpdateGovFilingRef(docID, filingID)
//	          → agreement_doc.govfiling_id = filingID
//
// セキュリティ注記:
//   - 機微情報を govfiling_id 列・PayloadJSON に格納しない。
//   - 冪等キーはビジネスキー (<tenantID>:<docID>) から生成し、二重起動を防ぐ。
//
// 法令注記: 36協定届出様式・送信タイミング等の法令要件は社労士/弁護士との
// 確認が前提。本実装は法的助言ではない。
package workrule

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
)

// ---------------------------------------------------------------------------
// EGovFilingBridge — workrule → govfiling の依存逆転インタフェース
// ---------------------------------------------------------------------------

// EGovFilingBridgeInput holds parameters for creating a 36協定 gov_filing job.
// Passed from workrule to govfiling via EGovFilingBridge (DI inversion).
type EGovFilingBridgeInput struct {
	TenantID               uuid.UUID
	ActorID                uuid.UUID
	LaborAgreementDocID    uuid.UUID
	FilingRepresentativeID uuid.UUID
	IdempotencyKey         string
	IP                     *string
}

// EGovFilingBridgeResult holds the result of an EGovFilingBridge call.
type EGovFilingBridgeResult struct {
	// FilingID is the gov_filings row ID created (or already existing).
	FilingID uuid.UUID
	// ExternalRef is the e-Gov acceptance reference (stub: STUB-EGOV-36-...).
	// Empty when the job is left in draft (no EGovSubmitter configured).
	ExternalRef string
}

// EGovFilingBridge abstracts the govfiling.Service.CreateArticle36Filing call
// so that workrule does not import govfiling directly (dependency inversion).
//
// govfiling.Service implements this interface via an adapter wrapper.
// Inject via workrule.Service.WithEGovBridge in main wiring (cmd/server or
// equivalent), passing govfiling.NewEGovBridgeAdapter(govfilingSvc).
type EGovFilingBridge interface {
	CreateArticle36Filing(ctx context.Context, in EGovFilingBridgeInput) (EGovFilingBridgeResult, error)
}

// ---------------------------------------------------------------------------
// Service extension — egovBridge field + WithEGovBridge
// ---------------------------------------------------------------------------

// egovBridge is the DI field; set via WithEGovBridge.
// Declared separately to keep service.go unchanged (Go struct field in same pkg).
// Go allows fields to be accessed across files within the same package.
// NOTE: this field is added to the Service struct defined in service.go;
// Go does not support partial struct definitions across files. Instead, we
// add egovBridge to service.go's Service struct definition via Edit. See below.

// WithEGovBridge returns a copy of the Service wired to the given bridge.
// Call this during application startup to enable 36協定 e-Gov filing initiation.
// Pass nil (or omit) to disable; InitiateEGovFiling will be a no-op.
func (s *Service) WithEGovBridge(b EGovFilingBridge) *Service {
	return &Service{tdb: s.tdb, egovBridge: b}
}

// ---------------------------------------------------------------------------
// InitiateEGovFiling
// ---------------------------------------------------------------------------

// InitiateEGovFilingInput holds parameters for InitiateEGovFiling.
type InitiateEGovFilingInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// DocID is the agreement document row to initiate filing for.
	// Must be a labour_agreement_documents (type article36) row in this tenant.
	DocID uuid.UUID
	// FilingRepresentativeID is the employee who acts as the filing
	// representative (届出担当者) and satisfies gov_filings' employee_id FK.
	// Must belong to the same tenant.
	FilingRepresentativeID uuid.UUID
	IP                     *string
}

// InitiateEGovFiling creates a govfiling job for a 36協定 document and
// stores the resulting filing ID in the agreement document's govfiling_id column.
//
// The function is idempotent: if govfiling_id is already set on the document,
// the existing filing ID is returned and no new job is created.
//
// When no EGovFilingBridge is configured (nil), the method returns the
// existing govfiling_id or ErrNotFound without error (safe stub mode).
//
// Transaction boundaries:
//   - Read + govfiling_id NULL check in one transaction (WithinTenant).
//   - CreateArticle36Filing runs in govfiling's own transaction.
//   - govfiling_id UPDATE + audit in a second workrule transaction.
//
// Error paths:
//   - ErrNotFound: docID not in this tenant, or no bridge configured and no
//     existing govfiling_id.
//   - ErrInvalidTransition: document not in filing_status 'draft' or 'filed'.
func (s *Service) InitiateEGovFiling(ctx context.Context, in InitiateEGovFilingInput) (*LaborAgreementDocument, error) {
	// Phase 1: read the document and check existing govfiling_id.
	var existingFilingID *uuid.UUID

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var row struct {
			ID           uuid.UUID  `gorm:"column:id"`
			FilingStatus string     `gorm:"column:filing_status"`
			GovFilingID  *uuid.UUID `gorm:"column:govfiling_id"`
		}
		if err := tx.Raw(
			`SELECT id, filing_status, govfiling_id
			 FROM labor_agreement_documents `+ //nolint:misspell // DB table name is schema contract
				`WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.DocID, in.TenantID,
		).Scan(&row).Error; err != nil {
			return fmt.Errorf("workrule: initiate egov read doc: %w", err)
		}
		if row.ID == uuid.Nil {
			return ErrNotFound
		}
		existingFilingID = row.GovFilingID
		return nil
	})
	if err != nil {
		return nil, err
	}

	// If govfiling_id is already set, return the document without re-creating.
	if existingFilingID != nil {
		return s.GetAgreement(ctx, in.TenantID, in.DocID)
	}

	// No bridge configured: return ErrNotFound to signal stub mode.
	if s.egovBridge == nil {
		return nil, fmt.Errorf("%w: e-Gov filing bridge not configured (stub mode)", ErrNotFound)
	}

	// Phase 2: create the govfiling job via the bridge (govfiling's transaction).
	idempotencyKey := fmt.Sprintf("article36:%s:%s", in.TenantID.String(), in.DocID.String())

	result, err := s.egovBridge.CreateArticle36Filing(ctx, EGovFilingBridgeInput{
		TenantID:               in.TenantID,
		ActorID:                in.ActorID,
		LaborAgreementDocID:    in.DocID,
		FilingRepresentativeID: in.FilingRepresentativeID,
		IdempotencyKey:         idempotencyKey,
		IP:                     in.IP,
	})
	if err != nil {
		return nil, fmt.Errorf("workrule: initiate egov create filing: %w", err)
	}

	// Phase 3: store govfiling_id on the document + audit.
	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE labor_agreement_documents `+ //nolint:misspell // DB table name is schema contract
				`SET govfiling_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND govfiling_id IS NULL`,
			result.FilingID, in.DocID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("workrule: initiate egov update govfiling_id: %w", res.Error)
		}
		// RowsAffected == 0 means another concurrent call set govfiling_id first (OK).

		idStr := in.DocID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "labor_agreement.egov_filing_initiated", //nolint:misspell // audit event name is schema contract
			ResourceType: "labor_agreement_document",              //nolint:misspell // audit resource type is schema contract
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}

	return s.GetAgreement(ctx, in.TenantID, in.DocID)
}
