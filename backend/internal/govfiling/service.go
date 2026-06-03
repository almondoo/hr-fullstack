package govfiling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("govfiling: not found")
	ErrInvalidTransition = errors.New("govfiling: invalid filing status transition")
	ErrForbidden         = errors.New("govfiling: permission denied")
	ErrSettingsMissing   = errors.New("govfiling: insurance settings not configured")
)

// ---------------------------------------------------------------------------
// Status machine (申請ステータス機械)
//
//	draft → submitted → accepted → completed
//	                  ↘ returned → submitted (再申請)
//	                  ↘ error    → submitted (再送)
//
// 法令/運用上、accepted からの返戻 (accepted → returned) も許容する。
// ---------------------------------------------------------------------------

var allowedFilingTransitions = map[string]map[string]bool{
	StatusDraft: {
		StatusSubmitted: true,
	},
	StatusSubmitted: {
		StatusAccepted: true,
		StatusReturned: true,
		StatusError:    true,
	},
	StatusAccepted: {
		StatusCompleted: true,
		StatusReturned:  true,
	},
	StatusReturned: {
		// 返戻 → 再申請。
		StatusSubmitted: true,
	},
	StatusError: {
		// エラー保持後の再送。
		StatusSubmitted: true,
	},
	// completed は終端状態 (遷移なし)。
}

// isFilingTransitionAllowed reports whether moving a filing from current → next is valid.
func isFilingTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedFilingTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// ---------------------------------------------------------------------------
// Submitter — e-Gov / マイナポータル 送信の抽象化 (LM-012, INT-003)
// ---------------------------------------------------------------------------

// SubmitRequest is the input to a Submitter.
//
// IdempotencyKey は外部API二重送信防止に使用する。PayloadJSON は参照IDのみで構成し、
// マイナンバー等の復号値を含めてはならない。
type SubmitRequest struct {
	Channel        string
	FilingType     string
	IdempotencyKey string
	PayloadJSON    []byte
}

// SubmitResult is the result returned by a Submitter.
//
// ExternalRef は外部受付番号 (不透明ID)。
type SubmitResult struct {
	ExternalRef string
}

// Submitter abstracts the external electronic-filing channel (e-Gov / マイナポータル).
// The MVP uses mockSubmitter; the real e-Gov/マイナポータル adapter is implemented
// in P3 and swapped in without changing callers.
type Submitter interface {
	Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error)
}

// mockSubmitter is the MVP mock adapter. It does not perform any real network
// call; it deterministically derives an opaque external reference from the
// idempotency key so the same key always yields the same external_ref (冪等).
type mockSubmitter struct{}

// Submit returns a deterministic mock external reference derived from the
// idempotency key. No real e-Gov/マイナポータル call is made (実送信は P3)。
func (mockSubmitter) Submit(_ context.Context, req SubmitRequest) (SubmitResult, error) {
	// Deterministic opaque ref keyed by idempotency_key + channel (mock only).
	ref := "MOCK-" + req.Channel + "-" + req.IdempotencyKey
	return SubmitResult{ExternalRef: ref}, nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service provides business logic for the govfiling domain.
type Service struct {
	tdb       *tenantdb.TenantDB
	submitter Submitter
}

// NewService constructs a Service using the MVP mock submitter.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb, submitter: mockSubmitter{}}
}

// WithSubmitter returns a copy of the Service using the given Submitter.
// Used by tests and (future) production wiring to swap the e-Gov/マイナポータル adapter.
func (s *Service) WithSubmitter(sub Submitter) *Service {
	return &Service{tdb: s.tdb, submitter: sub}
}

// ---------------------------------------------------------------------------
// Insurance settings (法令値の設定化 LM-010/011/014)
// ---------------------------------------------------------------------------

// UpsertSettingsInput holds fields for creating/updating per-tenant insurance settings.
type UpsertSettingsInput struct {
	TenantID               uuid.UUID
	ActorID                uuid.UUID
	InsurerKind            string
	RateTableJSON          []byte
	GradeTableJSON         []byte
	JudgementThresholdJSON []byte
	FormVersionJSON        []byte
	IP                     *string
}

// jsonbOrEmpty returns b, or []byte("{}") when b is empty/nil, so that NOT NULL
// jsonb columns always receive a valid value (defence for direct service
// callers; the handler also normalises).
func jsonbOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte(`{}`)
	}
	return b
}

// UpsertSettings creates or updates the tenant's insurance settings (1 row per tenant).
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*InsuranceSettings, error) {
	rateTable := jsonbOrEmpty(in.RateTableJSON)
	gradeTable := jsonbOrEmpty(in.GradeTableJSON)
	judgementThreshold := jsonbOrEmpty(in.JudgementThresholdJSON)
	formVersion := jsonbOrEmpty(in.FormVersionJSON)

	var out InsuranceSettings
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO insurance_settings
			   (id, tenant_id, insurer_kind, rate_table_json, grade_table_json,
			    judgement_threshold_json, form_version_json)
			 VALUES (?, ?, ?, ?::jsonb, ?::jsonb, ?::jsonb, ?::jsonb)
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET insurer_kind             = EXCLUDED.insurer_kind,
			       rate_table_json          = EXCLUDED.rate_table_json,
			       grade_table_json         = EXCLUDED.grade_table_json,
			       judgement_threshold_json = EXCLUDED.judgement_threshold_json,
			       form_version_json        = EXCLUDED.form_version_json,
			       updated_at               = now()`,
			id, in.TenantID, in.InsurerKind, rateTable, gradeTable,
			judgementThreshold, formVersion,
		).Error; err != nil {
			return fmt.Errorf("govfiling: upsert settings: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, insurer_kind, rate_table_json, grade_table_json,
			        judgement_threshold_json, form_version_json, created_at, updated_at
			 FROM insurance_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("govfiling: upsert settings re-read: %w", err)
		}

		idStr := out.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "insurance_settings.upserted",
			ResourceType: "insurance_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSettings fetches the tenant's insurance settings.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*InsuranceSettings, error) {
	var out InsuranceSettings
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, insurer_kind, rate_table_json, grade_table_json,
			        judgement_threshold_json, form_version_json, created_at, updated_at
			 FROM insurance_settings WHERE tenant_id = ? LIMIT 1`,
			tenantID,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	if out.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Grade judgement (等級判定 — 設定駆動・ハードコード非依存 LM-010)
// ---------------------------------------------------------------------------

// gradeTable is the decoded shape of insurance_settings.grade_table_json.
//
// Format: {"grades":[{"grade":1,"lower":0,"upper":63000,"monthly":58000}, ...]}
type gradeTable struct {
	Grades []gradeEntry `json:"grades"`
}

type gradeEntry struct {
	Grade   int   `json:"grade"`
	Lower   int64 `json:"lower"`
	Upper   int64 `json:"upper"` // 0 (or absent) means open upper bound (上限なし)
	Monthly int64 `json:"monthly"`
}

// judgementThreshold is the decoded shape of insurance_settings.judgement_threshold_json.
//
// Format: {"monthly_change_grade_diff":2}
type judgementThreshold struct {
	MonthlyChangeGradeDiff int `json:"monthly_change_grade_diff"`
}

// JudgeGradeResult is the result of a standard-monthly-remuneration grade judgement.
type JudgeGradeResult struct {
	// CurrentGrade / NewGrade are the resolved 標準報酬月額等級 from the configured
	// grade table.
	CurrentGrade int
	NewGrade     int
	// GradeDiff is abs(NewGrade-CurrentGrade).
	GradeDiff int
	// MonthlyChangeRequired reports whether a 月額変更届 is required, i.e. the grade
	// difference meets or exceeds the configured threshold (2等級以上変動 等)。
	// The threshold itself comes from settings, never hardcoded.
	MonthlyChangeRequired bool
}

// resolveGrade maps a monthly remuneration amount to its 等級 using the configured grade table.
// Returns 0 when no grade matches (caller treats as not found).
func resolveGrade(gt gradeTable, monthlyRemuneration int64) int {
	for _, g := range gt.Grades {
		if monthlyRemuneration < g.Lower {
			continue
		}
		if g.Upper == 0 || monthlyRemuneration < g.Upper {
			return g.Grade
		}
	}
	return 0
}

// JudgeMonthlyChangeInput holds inputs for a 月額変更届 grade judgement.
type JudgeMonthlyChangeInput struct {
	TenantID       uuid.UUID
	CurrentMonthly int64
	NewMonthly     int64
}

// JudgeMonthlyChange resolves the 標準報酬月額等級 for the current and new monthly
// remuneration from the tenant's configured grade table and determines whether a
// 月額変更届 is required per the configured threshold.
//
// Statutory values (等級表・判定閾値) are read from insurance_settings — they are
// NEVER hardcoded, so a settings change is reflected in the result (改正追従)。
func (s *Service) JudgeMonthlyChange(ctx context.Context, in JudgeMonthlyChangeInput) (*JudgeGradeResult, error) {
	var res JudgeGradeResult
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var settings InsuranceSettings
		if err := tx.Raw(
			`SELECT grade_table_json, judgement_threshold_json
			 FROM insurance_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&settings).Error; err != nil {
			return fmt.Errorf("govfiling: judge load settings: %w", err)
		}
		if len(settings.GradeTableJSON) == 0 {
			return ErrSettingsMissing
		}

		var gt gradeTable
		if err := json.Unmarshal(settings.GradeTableJSON, &gt); err != nil {
			return fmt.Errorf("govfiling: judge parse grade table: %w", err)
		}
		if len(gt.Grades) == 0 {
			return ErrSettingsMissing
		}

		var th judgementThreshold
		if len(settings.JudgementThresholdJSON) > 0 {
			if err := json.Unmarshal(settings.JudgementThresholdJSON, &th); err != nil {
				return fmt.Errorf("govfiling: judge parse threshold: %w", err)
			}
		}

		res.CurrentGrade = resolveGrade(gt, in.CurrentMonthly)
		res.NewGrade = resolveGrade(gt, in.NewMonthly)
		diff := res.NewGrade - res.CurrentGrade
		if diff < 0 {
			diff = -diff
		}
		res.GradeDiff = diff
		// Threshold defaults to 0 only when unset; an unset threshold means no
		// change is ever required (fail-closed against accidental filings).
		res.MonthlyChangeRequired = th.MonthlyChangeGradeDiff > 0 && diff >= th.MonthlyChangeGradeDiff
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// ---------------------------------------------------------------------------
// Filings (電子申請ジョブ LM-010/011/012/013)
// ---------------------------------------------------------------------------

// CreateFilingInput holds fields for creating a filing job in draft status.
//
// PayloadJSON must contain only reference IDs / non-sensitive filing data;
// decrypted 機微情報 (マイナンバー等) must NOT be included.
type CreateFilingInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	EmployeeID     uuid.UUID
	FilingType     string
	Channel        string
	PayloadJSON    []byte
	IdempotencyKey string
	IP             *string
}

// CreateFiling creates a new filing job in 'draft' status.
// The employee must belong to the same tenant (composite FK + explicit check).
func (s *Service) CreateFiling(ctx context.Context, in CreateFilingInput) (*Filing, error) {
	f := Filing{
		ID:             uuid.New(),
		TenantID:       in.TenantID,
		EmployeeID:     in.EmployeeID,
		FilingType:     in.FilingType,
		Channel:        in.Channel,
		Status:         StatusDraft,
		PayloadJSON:    in.PayloadJSON,
		IdempotencyKey: in.IdempotencyKey,
		CreatedBy:      &in.ActorID,
	}
	if len(f.PayloadJSON) == 0 {
		f.PayloadJSON = []byte(`{}`)
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("govfiling: create filing verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO gov_filings
			   (id, tenant_id, employee_id, filing_type, channel, status,
			    payload_json, idempotency_key, created_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?)`,
			f.ID, f.TenantID, f.EmployeeID, f.FilingType, f.Channel, f.Status,
			f.PayloadJSON, f.IdempotencyKey, f.CreatedBy,
		).Error; err != nil {
			return fmt.Errorf("govfiling: create filing insert: %w", err)
		}

		idStr := f.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "gov_filing.created",
			ResourceType: "gov_filing",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// GetFiling fetches a filing by ID within the tenant.
func (s *Service) GetFiling(ctx context.Context, tenantID, id uuid.UUID) (*Filing, error) {
	var f Filing
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(filingSelectSQL+` WHERE id = ? AND tenant_id = ? LIMIT 1`, id, tenantID).Scan(&f).Error
	})
	if err != nil {
		return nil, err
	}
	if f.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &f, nil
}

// ListFilings returns filings for an employee within the tenant.
func (s *Service) ListFilings(ctx context.Context, tenantID, employeeID uuid.UUID) ([]Filing, error) {
	var fs []Filing
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			filingSelectSQL+` WHERE tenant_id = ? AND employee_id = ? ORDER BY created_at DESC`,
			tenantID, employeeID,
		).Scan(&fs).Error
	})
	if err != nil {
		return nil, err
	}
	return fs, nil
}

// filingSelectSQL is the column list shared by filing reads.
const filingSelectSQL = `SELECT id, tenant_id, employee_id, filing_type, channel, status,
	        payload_json, external_ref, idempotency_key, submitted_at, last_error,
	        created_by, created_at, updated_at
	 FROM gov_filings`

// SubmitFilingInput holds fields for submitting (or re-submitting) a filing.
type SubmitFilingInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	IP       *string
}

// SubmitFiling sends a filing to the external channel via the configured
// Submitter and transitions its status draft|returned|error → submitted.
//
// Idempotency: the external call is keyed by the filing's idempotency_key, so a
// re-submit with the same key never causes a duplicate external registration.
// The row is locked FOR UPDATE to avoid concurrent double-submission (TOCTOU).
func (s *Service) SubmitFiling(ctx context.Context, in SubmitFilingInput) (*Filing, error) {
	var f Filing
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock the row to serialise concurrent submits (TOCTOU avoidance).
		if err := tx.Raw(
			filingSelectSQL+` WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&f).Error; err != nil {
			return fmt.Errorf("govfiling: submit filing read: %w", err)
		}
		if f.ID == uuid.Nil {
			return ErrNotFound
		}
		if !isFilingTransitionAllowed(f.Status, StatusSubmitted) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, f.Status, StatusSubmitted)
		}
		fromStatus := f.Status

		// Call the external channel (idempotent via idempotency_key).
		// 機微情報の非格納: PayloadJSON は参照IDのみ。
		result, subErr := s.submitter.Submit(ctx, SubmitRequest{
			Channel:        f.Channel,
			FilingType:     f.FilingType,
			IdempotencyKey: f.IdempotencyKey,
			PayloadJSON:    f.PayloadJSON,
		})
		if subErr != nil {
			// On submit failure: hold the error and move to 'error' state so it
			// can be retried (再送)。
			errMsg := subErr.Error()
			if err := s.applyTransitionTx(tx, in.TenantID, in.ActorID, f.ID, fromStatus, StatusError, nil, &errMsg, in.IP); err != nil {
				return err
			}
			return tx.Raw(
				filingSelectSQL+` WHERE id = ? AND tenant_id = ? LIMIT 1`, in.ID, in.TenantID,
			).Scan(&f).Error
		}

		// On success: persist external_ref, submitted_at, clear last_error, move to submitted.
		ref := result.ExternalRef
		res := tx.Exec(
			`UPDATE gov_filings
			 SET status = ?, external_ref = ?, submitted_at = now(),
			     last_error = NULL, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			StatusSubmitted, ref, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("govfiling: submit filing update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := s.recordHistoryTx(tx, in.TenantID, in.ActorID, f.ID, fromStatus, StatusSubmitted, nil, nil); err != nil {
			return err
		}

		idStr := f.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "gov_filing.submitted",
			ResourceType: "gov_filing",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return err
		}

		return tx.Raw(
			filingSelectSQL+` WHERE id = ? AND tenant_id = ? LIMIT 1`, in.ID, in.TenantID,
		).Scan(&f).Error
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// UpdateStatusInput holds fields for an externally-driven status transition.
//
// Used when polling/webhook delivers an acceptance, return (返戻), completion, or
// error from the external channel.
type UpdateStatusInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	ID              uuid.UUID
	ToStatus        string
	Note            *string
	ExternalMessage *string // 返戻理由 等
	IP              *string
}

// UpdateStatus transitions a filing to ToStatus, recording the transition in the
// status history. Only allow-listed transitions are accepted.
func (s *Service) UpdateStatus(ctx context.Context, in UpdateStatusInput) (*Filing, error) {
	var f Filing
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT status FROM gov_filings WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&f).Error; err != nil {
			return fmt.Errorf("govfiling: update status read: %w", err)
		}
		if f.Status == "" {
			return ErrNotFound
		}
		if !isFilingTransitionAllowed(f.Status, in.ToStatus) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, f.Status, in.ToStatus)
		}
		fromStatus := f.Status

		if err := s.applyTransitionTx(tx, in.TenantID, in.ActorID, in.ID, fromStatus, in.ToStatus, in.Note, in.ExternalMessage, in.IP); err != nil {
			return err
		}

		return tx.Raw(
			filingSelectSQL+` WHERE id = ? AND tenant_id = ? LIMIT 1`, in.ID, in.TenantID,
		).Scan(&f).Error
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// applyTransitionTx updates the filing status, records the status-history row,
// and writes the audit entry — all within the caller's transaction.
//
// For the 'error' target, externalMessage carries the last_error string.
func (s *Service) applyTransitionTx(
	tx *gorm.DB,
	tenantID, actorID, filingID uuid.UUID,
	fromStatus, toStatus string,
	note, externalMessage, ip *string,
) error {
	if toStatus == StatusError {
		res := tx.Exec(
			`UPDATE gov_filings SET status = ?, last_error = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			toStatus, externalMessage, filingID, tenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("govfiling: apply transition update (error): %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
	} else {
		res := tx.Exec(
			`UPDATE gov_filings SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			toStatus, filingID, tenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("govfiling: apply transition update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
	}

	if err := s.recordHistoryTx(tx, tenantID, actorID, filingID, fromStatus, toStatus, note, externalMessage); err != nil {
		return err
	}

	idStr := filingID.String()
	return audit.Record(tx, audit.Entry{
		TenantID:     tenantID,
		UserID:       &actorID,
		Action:       "gov_filing.status_changed",
		ResourceType: "gov_filing",
		ResourceID:   &idStr,
		IP:           ip,
	})
}

// recordHistoryTx inserts a status-history row within the caller's transaction.
func (s *Service) recordHistoryTx(
	tx *gorm.DB,
	tenantID, actorID, filingID uuid.UUID,
	fromStatus, toStatus string,
	note, externalMessage *string,
) error {
	hid := uuid.New()
	if err := tx.Exec(
		`INSERT INTO gov_filing_status_history
		   (id, tenant_id, filing_id, from_status, to_status, note, external_message, changed_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hid, tenantID, filingID, fromStatus, toStatus, note, externalMessage, actorID,
	).Error; err != nil {
		return fmt.Errorf("govfiling: record status history: %w", err)
	}
	return nil
}

// ListStatusHistory returns the status transition history for a filing.
func (s *Service) ListStatusHistory(ctx context.Context, tenantID, filingID uuid.UUID) ([]StatusHistory, error) {
	var hs []StatusHistory
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Verify the filing belongs to the tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM gov_filings WHERE id = ? AND tenant_id = ?`,
			filingID, tenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("govfiling: list history verify filing: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}
		return tx.Raw(
			`SELECT id, tenant_id, filing_id, from_status, to_status, note,
			        external_message, changed_by, changed_at, created_at, updated_at
			 FROM gov_filing_status_history
			 WHERE filing_id = ? AND tenant_id = ?
			 ORDER BY changed_at ASC, created_at ASC`,
			filingID, tenantID,
		).Scan(&hs).Error
	})
	if err != nil {
		return nil, err
	}
	return hs, nil
}

// ---------------------------------------------------------------------------
// Documents (公文書/帳票 LM-013, CMP-006) — content_enc AES-256-GCM
// ---------------------------------------------------------------------------

// AttachDocumentInput holds fields for attaching an official document to a filing.
//
// ContentPlaintext is the document body in plaintext. It is encrypted with
// AES-256-GCM before storage; the plaintext is NEVER persisted, logged, or
// written to the audit record.
type AttachDocumentInput struct {
	TenantID         uuid.UUID
	ActorID          uuid.UUID
	FilingID         uuid.UUID
	DocKind          string
	ContentPlaintext []byte
	RetentionLabel   string
	IP               *string
}

// AttachDocument stores an official document encrypted under content_enc and
// links it to the filing. The plaintext is encrypted before the transaction
// opens (fail-fast) and never persisted as plaintext.
func (s *Service) AttachDocument(ctx context.Context, in AttachDocumentInput) (*FilingDocument, error) {
	// Encrypt BEFORE opening the transaction so crypto errors fail fast without
	// acquiring DB resources. The plaintext never appears in any error message.
	var contentEnc []byte
	if len(in.ContentPlaintext) > 0 {
		var err error
		contentEnc, err = crypto.Encrypt(in.ContentPlaintext)
		if err != nil {
			return nil, fmt.Errorf("govfiling: attach document encrypt: %w", err)
		}
	}

	retention := in.RetentionLabel
	if retention == "" {
		// Default retention label. The concrete legal value is configured per
		// tenant/regulation (法令値・社労士確認前提); this default is not a legal basis.
		retention = "4years"
	}

	doc := FilingDocument{
		ID:             uuid.New(),
		TenantID:       in.TenantID,
		FilingID:       in.FilingID,
		DocKind:        in.DocKind,
		ContentEnc:     contentEnc,
		RetentionLabel: retention,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the filing belongs to this tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM gov_filings WHERE id = ? AND tenant_id = ?`,
			in.FilingID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("govfiling: attach document verify filing: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO gov_filing_documents
			   (id, tenant_id, filing_id, doc_kind, content_enc, retention_label)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			doc.ID, doc.TenantID, doc.FilingID, doc.DocKind, doc.ContentEnc, doc.RetentionLabel,
		).Error; err != nil {
			return fmt.Errorf("govfiling: attach document insert: %w", err)
		}

		// Audit: opaque document ID only — never PII or decrypted content.
		idStr := doc.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "gov_filing_document.attached",
			ResourceType: "gov_filing_document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers of AttachDocument.
	doc.ContentEnc = nil
	return &doc, nil
}

// ListDocuments returns the document metadata for a filing (without ciphertext).
func (s *Service) ListDocuments(ctx context.Context, tenantID, filingID uuid.UUID) ([]FilingDocument, error) {
	var docs []FilingDocument
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Verify the filing belongs to the tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM gov_filings WHERE id = ? AND tenant_id = ?`,
			filingID, tenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("govfiling: list documents verify filing: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}
		// Deliberately omit content_enc from the metadata listing.
		return tx.Raw(
			`SELECT id, tenant_id, filing_id, doc_kind, retention_label, created_at, updated_at
			 FROM gov_filing_documents
			 WHERE filing_id = ? AND tenant_id = ?
			 ORDER BY created_at ASC`,
			filingID, tenantID,
		).Scan(&docs).Error
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// GetDocumentContentInput holds parameters for reading a document body.
//
// ReadSensitive must be true when the caller holds filing:read_sensitive; the
// service re-validates this permission within the transaction (defence in depth).
type GetDocumentContentInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	DocumentID    uuid.UUID
	ReadSensitive bool
	IP            *string
}

// GetDocumentContent fetches a document and, only when the caller holds
// filing:read_sensitive (re-validated at the service layer), returns the
// decrypted plaintext as a separate return value.
//
// Multi-layer permission enforcement:
//   - Layer 1 (HTTP): the route requires filing:read_sensitive via RequirePermission.
//   - Layer 2 (Service): when ReadSensitive is true, the service re-validates
//     filing:read_sensitive using LoadUserPermissions within the transaction.
//
// The decrypted content is NEVER written to logs or audit records.
func (s *Service) GetDocumentContent(ctx context.Context, in GetDocumentContentInput) (*FilingDocument, []byte, error) {
	var doc FilingDocument
	var permitted bool
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Service-layer permission re-check (defence in depth).
		if in.ReadSensitive {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("govfiling: get document load permissions: %w", err)
			}
			if !platformauth.HasPermission(perms, "filing:read_sensitive") {
				return ErrForbidden
			}
			permitted = true
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, filing_id, doc_kind, content_enc, retention_label,
			        created_at, updated_at
			 FROM gov_filing_documents WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.DocumentID, in.TenantID,
		).Scan(&doc).Error; err != nil {
			return fmt.Errorf("govfiling: get document: %w", err)
		}
		if doc.ID == uuid.Nil {
			return ErrNotFound
		}

		action := "gov_filing_document.read"
		if permitted {
			action = "gov_filing_document.read_sensitive"
		}
		idStr := doc.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "gov_filing_document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}

	// Decrypt only when the service-layer permission check passed. The decrypted
	// value is returned separately so callers cannot accidentally persist it.
	var plaintext []byte
	if permitted && len(doc.ContentEnc) > 0 {
		plain, derr := crypto.Decrypt(doc.ContentEnc)
		if derr != nil {
			return nil, nil, fmt.Errorf("govfiling: decrypt document: %w", derr)
		}
		plaintext = plain
	}
	// Clear ciphertext from the returned struct regardless.
	doc.ContentEnc = nil
	return &doc, plaintext, nil
}
