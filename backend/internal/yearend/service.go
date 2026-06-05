package yearend

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("yearend: not found")
	ErrInvalidTransition = errors.New("yearend: invalid status transition")
	ErrForbidden         = errors.New("yearend: permission denied")
	ErrLocked            = errors.New("yearend: submission is locked and immutable")
	ErrFinalised         = errors.New("yearend: calculation is finalised and immutable")
	ErrInvalidInput      = errors.New("yearend: invalid input")
)

// Permission constants (RBAC / least privilege).
//
//	yearend:read   — read submission metadata, calculation results, reports (no plaintext declaration)
//	yearend:write  — create / update submissions, trigger calculations, generate reports
//	yearend:reveal — DECRYPT the declaration (defence-in-depth: enforced at route AND in service)
const (
	permRead   = "yearend:read"
	permWrite  = "yearend:write"
	permReveal = "yearend:reveal"
)

// requirePermission re-validates actor permission inside an open tenant tx.
// Returns ErrForbidden when the permission is absent.
func requirePermission(tx *gorm.DB, tenantID, actorID uuid.UUID, need string) error {
	perms, err := platformauth.LoadUserPermissions(tx, tenantID, actorID)
	if err != nil {
		return fmt.Errorf("yearend: load permissions: %w", err)
	}
	if !platformauth.HasPermission(perms, need) {
		return ErrForbidden
	}
	return nil
}

// ---------------------------------------------------------------------------
// PayrollPusher adapter abstraction (給与SaaS連携足場)
// ---------------------------------------------------------------------------

// PushRequest holds the data required to push a year-end adjustment result to
// a payroll SaaS provider.  It contains amounts and identifiers only — never
// decrypted PII from the submission declaration.
type PushRequest struct {
	TenantID   uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	CalcID     uuid.UUID
	// ResultJSON is the calculated result payload (amounts only; no PII).
	ResultJSON json.RawMessage
}

// PushResult holds the response from a payroll SaaS push.
// ProviderRef is an opaque provider-side reference; no credentials are returned.
type PushResult struct {
	// ProviderRef is an opaque provider-side reference (NOT a credential / token).
	ProviderRef string
}

// PayrollPusher abstracts the payroll-SaaS provider (moneyforward/freee/yayoi).
//
// Design: this interface is the adapter boundary for the payroll-SaaS integration
// scaffold.  The stub implementation (StubPayrollPusher) is wired in MVP.  Real
// provider implementations require external credentials (OAuth2 / API keys) that
// are NOT available in this repository and are deferred to P3.
//
// Security constraints on implementations:
//   - Push MUST NOT transmit credentials, tokens, card numbers, or PII beyond
//     what is listed in PushRequest.
//   - provider_ref in PushResult MUST be an opaque reference only (no PAN /
//     plaintext PII).
//   - Errors MUST NOT include credential values in their text.
type PayrollPusher interface {
	// Provider returns the provider identifier (must match a DB CHECK constraint).
	Provider() string
	// Push transmits the year-end adjustment result to the payroll SaaS.
	// Implementations MUST return a stable provider_ref on success.
	Push(ctx context.Context, req PushRequest) (PushResult, error)
}

// StubPayrollPusher is a deterministic in-memory stub for MVP and testing.
// No external call is made; the stub returns a deterministic provider_ref.
//
// Design note: the stub exists so the rest of the domain (service, handler,
// routes, DB tables) can be fully implemented and tested without waiting for
// real provider credentials.  Swap for a real adapter once credentials are
// available (P3 / see remaining_tasks.md for the follow-up item).
type StubPayrollPusher struct {
	provider string
}

// NewStubPayrollPusher constructs a stub pusher for the given provider.
// When provider is empty it defaults to ProviderMock.
func NewStubPayrollPusher(provider string) *StubPayrollPusher {
	if provider == "" {
		provider = ProviderMock
	}
	return &StubPayrollPusher{provider: provider}
}

// Provider returns the stub provider identifier.
func (s *StubPayrollPusher) Provider() string { return s.provider }

// Push returns a deterministic stub result; no external call is made.
func (s *StubPayrollPusher) Push(_ context.Context, req PushRequest) (PushResult, error) {
	ref := fmt.Sprintf("stub-%s-%d-%s", s.provider, req.TaxYear, req.EmployeeID.String()[:8])
	return PushResult{ProviderRef: ref}, nil
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service provides business logic for the year-end adjustment domain.
type Service struct {
	tdb    *tenantdb.TenantDB
	pusher PayrollPusher
}

// NewService constructs a Service with the given PayrollPusher.
// When pusher is nil a StubPayrollPusher (ProviderMock) is used.
func NewService(tdb *tenantdb.TenantDB, pusher PayrollPusher) *Service {
	if pusher == nil {
		pusher = NewStubPayrollPusher(ProviderMock)
	}
	return &Service{tdb: tdb, pusher: pusher}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// jsonbArg coerces an optional JSON byte slice to a non-empty default so the
// ?::jsonb cast never receives an empty string.
func jsonbArg(raw []byte, fallback string) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte(fallback)
	}
	return raw
}

// hashDeclaration returns a hex-encoded SHA-256 of the plaintext declaration.
// Used for integrity verification (改竄検知); does NOT reveal the plaintext.
func hashDeclaration(plaintext []byte) string {
	h := sha256.Sum256(plaintext)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// UpsertSettingsInput holds fields for configuring per-tenant per-year settings.
type UpsertSettingsInput struct {
	TenantID            uuid.UUID
	ActorID             uuid.UUID
	TaxYear             int
	RateTableJSON       []byte
	DeductionLimitsJSON []byte
	IP                  *string
}

// UpsertSettings creates or updates the tenant's year-end settings for a tax year.
//
// LEGAL: rate tables and deduction ceilings MUST be updated each year per the
// current 国税庁 guidance; they are stored here so the application never
// hardcodes them.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*Settings, error) {
	if in.TaxYear < 2000 || in.TaxYear > 2100 {
		return nil, fmt.Errorf("%w: tax_year must be between 2000 and 2100", ErrInvalidInput)
	}
	rateTable := jsonbArg(in.RateTableJSON, "{}")
	deductLimits := jsonbArg(in.DeductionLimitsJSON, "{}")

	var st Settings
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO yearend_settings
			   (id, tenant_id, tax_year, rate_table_json, deduction_limits_json)
			 VALUES (?, ?, ?, ?::jsonb, ?::jsonb)
			 ON CONFLICT (tenant_id, tax_year) DO UPDATE
			   SET rate_table_json      = EXCLUDED.rate_table_json,
			       deduction_limits_json = EXCLUDED.deduction_limits_json,
			       updated_at           = now()`,
			id, in.TenantID, in.TaxYear, rateTable, deductLimits,
		).Error; err != nil {
			return fmt.Errorf("yearend: upsert settings: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, tax_year, rate_table_json, deduction_limits_json,
			        created_at, updated_at
			 FROM yearend_settings WHERE tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.TenantID, in.TaxYear,
		).Scan(&st).Error; err != nil {
			return fmt.Errorf("yearend: upsert settings re-read: %w", err)
		}

		idStr := st.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_settings.updated",
			ResourceType: "yearend_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetSettings returns the tenant's year-end settings for the given tax year.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID, taxYear int) (*Settings, error) {
	var st Settings
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, tax_year, rate_table_json, deduction_limits_json,
			        created_at, updated_at
			 FROM yearend_settings WHERE tenant_id = ? AND tax_year = ? LIMIT 1`,
			tenantID, taxYear,
		).Scan(&st).Error
	})
	if err != nil {
		return nil, err
	}
	if st.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &st, nil
}

// ---------------------------------------------------------------------------
// Submission (控除申告収集)
// ---------------------------------------------------------------------------

// UpsertSubmissionInput holds fields for creating or updating a submission.
//
// Security: DeclarationJSON is the plaintext declaration payload. It MUST be
// encrypted before storage and never logged or returned to callers.
type UpsertSubmissionInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	EmployeeID     uuid.UUID
	TaxYear        int
	// DeclarationJSON is the plaintext declaration (扶養親族/保険料控除/住宅借入金等).
	// It is AES-256-GCM encrypted before storage; never persisted as plaintext.
	DeclarationJSON []byte
	IP              *string
}

// UpsertSubmission creates or updates a draft submission for (employee, tax_year).
// Locked submissions are immutable (both DB trigger and service-layer guard).
// DeclarationJSON is encrypted with AES-256-GCM before storage.
//
// Security: DeclarationJSON MUST NOT appear in logs, audit resource_id, or
// API responses.  Only the hash (改竄検知) is persisted alongside the ciphertext.
func (s *Service) UpsertSubmission(ctx context.Context, in UpsertSubmissionInput) (*Submission, error) {
	if in.TaxYear < 2000 || in.TaxYear > 2100 {
		return nil, fmt.Errorf("%w: tax_year must be between 2000 and 2100", ErrInvalidInput)
	}
	if len(in.DeclarationJSON) == 0 {
		return nil, fmt.Errorf("%w: declaration_json is required", ErrInvalidInput)
	}
	if !json.Valid(in.DeclarationJSON) {
		return nil, fmt.Errorf("%w: declaration_json is not valid JSON", ErrInvalidInput)
	}

	// Encrypt the declaration before entering the tenant transaction.
	// Security: plaintext is never stored; only the ciphertext and hash are persisted.
	enc, err := crypto.Encrypt(in.DeclarationJSON)
	if err != nil {
		return nil, fmt.Errorf("yearend: encrypt declaration: %w", err)
	}
	hash := hashDeclaration(in.DeclarationJSON)

	var sub Submission
	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Block update of a locked submission.
		var existing struct {
			LockedAt *time.Time `gorm:"column:locked_at"`
		}
		if err := tx.Raw(
			`SELECT locked_at FROM yearend_submissions
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("yearend: submission locked check: %w", err)
		}
		if existing.LockedAt != nil {
			return ErrLocked
		}

		// Verify employee belongs to this tenant (defence-in-depth).
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("yearend: verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		id := uuid.New()
		res := tx.Exec(
			`INSERT INTO yearend_submissions
			   (id, tenant_id, employee_id, tax_year, status, declaration_enc, declaration_hash)
			 VALUES (?, ?, ?, ?, 'draft', ?, ?)
			 ON CONFLICT (employee_id, tenant_id, tax_year) DO UPDATE
			   SET declaration_enc  = EXCLUDED.declaration_enc,
			       declaration_hash = EXCLUDED.declaration_hash,
			       status           = CASE
			                           WHEN yearend_submissions.locked_at IS NOT NULL
			                             THEN yearend_submissions.status
			                           ELSE 'draft'
			                         END,
			       updated_at       = now()
			 WHERE yearend_submissions.locked_at IS NULL`,
			id, in.TenantID, in.EmployeeID, in.TaxYear, enc, hash,
		)
		if res.Error != nil {
			return fmt.Errorf("yearend: submission upsert: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrLocked
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, status,
			        declaration_enc, declaration_hash,
			        submitted_at, locked_at, created_at, updated_at
			 FROM yearend_submissions
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&sub).Error; err != nil {
			return fmt.Errorf("yearend: submission re-read: %w", err)
		}

		// SECURITY: ResourceID uses the opaque row UUID — never the employee's
		// decrypted declaration or hash.
		idStr := sub.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_submission.upserted",
			ResourceType: "yearend_submission",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// SubmitSubmissionInput holds fields for advancing a submission to 'submitted'.
type SubmitSubmissionInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	IP         *string
}

// SubmitSubmission advances a draft submission to 'submitted'.
// Only draft submissions can be submitted.
func (s *Service) SubmitSubmission(ctx context.Context, in SubmitSubmissionInput) (*Submission, error) {
	var sub Submission
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var existing struct {
			ID       uuid.UUID  `gorm:"column:id"`
			Status   string     `gorm:"column:status"`
			LockedAt *time.Time `gorm:"column:locked_at"`
		}
		if err := tx.Raw(
			`SELECT id, status, locked_at FROM yearend_submissions
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("yearend: submit submission read: %w", err)
		}
		if existing.ID == uuid.Nil {
			return ErrNotFound
		}
		if existing.LockedAt != nil {
			return ErrLocked
		}
		if existing.Status != SubmissionDraft {
			return fmt.Errorf("%w: submission is already in status %q", ErrInvalidTransition, existing.Status)
		}

		now := time.Now().UTC()
		res := tx.Exec(
			`UPDATE yearend_submissions
			 SET status = 'submitted', submitted_at = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = 'draft' AND locked_at IS NULL`,
			now, existing.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("yearend: submit submission update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrInvalidTransition
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, status,
			        declaration_enc, declaration_hash,
			        submitted_at, locked_at, created_at, updated_at
			 FROM yearend_submissions WHERE id = ? AND tenant_id = ? LIMIT 1`,
			existing.ID, in.TenantID,
		).Scan(&sub).Error; err != nil {
			return fmt.Errorf("yearend: submit submission re-read: %w", err)
		}

		idStr := sub.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_submission.submitted",
			ResourceType: "yearend_submission",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// GetSubmissionInput holds fields for fetching a submission.
type GetSubmissionInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	// Reveal requests decryption of the declaration.
	// Requires yearend:reveal permission (enforced in-service, defence-in-depth).
	Reveal bool
	IP     *string
}

// SubmissionResult wraps a Submission and optionally the decrypted declaration.
type SubmissionResult struct {
	*Submission
	// DeclarationJSON is the decrypted declaration, set only when Reveal=true
	// and the actor holds yearend:reveal permission.
	// Security: this field MUST NOT be logged or written to non-encrypted storage.
	DeclarationJSON []byte
}

// GetSubmission fetches a submission.  When Reveal is true and the actor holds
// yearend:reveal permission, the declaration is decrypted and returned in
// DeclarationJSON.  Otherwise DeclarationJSON is nil.
func (s *Service) GetSubmission(ctx context.Context, in GetSubmissionInput) (*SubmissionResult, error) {
	var sub Submission
	var canReveal bool

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Check reveal permission in-service (defence-in-depth).
		if in.Reveal {
			if err := requirePermission(tx, in.TenantID, in.ActorID, permReveal); err != nil {
				return err
			}
			canReveal = true
		}

		return tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, status,
			        declaration_enc, declaration_hash,
			        submitted_at, locked_at, created_at, updated_at
			 FROM yearend_submissions
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&sub).Error
	})
	if err != nil {
		return nil, err
	}
	if sub.ID == uuid.Nil {
		return nil, ErrNotFound
	}

	result := &SubmissionResult{Submission: &sub}
	if canReveal && len(sub.DeclarationEnc) > 0 {
		plain, err := crypto.Decrypt(sub.DeclarationEnc)
		if err != nil {
			return nil, fmt.Errorf("yearend: decrypt declaration: %w", err)
		}
		result.DeclarationJSON = plain
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Calculation (計算)
// ---------------------------------------------------------------------------

// TaxInput holds the normalised inputs used by the calculation engine.
// All amounts are in yen (円単位, integer).
//
// LEGAL: the calculation formula and parameters (給与所得控除/基礎控除/税率 etc.)
// are determined by annual tax-law revisions and MUST be validated by a tax
// accountant / 社労士.  The calculator uses per-tenant yearend_settings
// (rate_table_json / deduction_limits_json) so the formulas are configurable.
// This implementation is NOT legal advice.
type TaxInput struct {
	// TaxYear is the target tax year (対象年) for the year-end adjustment.
	// It drives year-dependent deduction rules (令和7年度税制改正対応).
	// When zero, pre-R7 rules (令和6以前) are applied as a safe default.
	TaxYear int
	// GrossIncome: 年収 (給与収入金額) in yen.
	GrossIncome int64
	// EmploymentDeduction: 給与所得控除 in yen (caller-supplied, derived from rate table).
	// CalculateTax enforces the statutory minimum (最低保障額) for the given TaxYear:
	// 55万 for taxYear ≤ 2024, 65万 for taxYear ≥ 2025.
	// If the caller supplies a value below the minimum it is raised automatically.
	EmploymentDeduction int64
	// DependentDeduction: 扶養控除合計 in yen.
	// NOTE: the income threshold used to qualify dependents (合計所得要件) is
	// year-dependent (48万 ≤2024 / 58万 ≥2025).  The threshold check is the
	// caller's responsibility when building DependentDeduction.
	// TODO(legal/reiwa7-dependent-wiring): 扶養親族の所得要件判定ロジック
	// (DependentIncomeLimit を使った合計所得チェック)は本パッケージに未実装。
	// 扶養申告書の解析・資格判定を実装する際に DependentIncomeLimit(TaxYear)
	// を使って各扶養親族の合計所得が閾値以下であることを検証すること。
	// 出典: 国税庁 No.1191 https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1191.htm
	DependentDeduction int64
	// SpouseDeduction: 配偶者控除 / 配偶者特別控除 in yen.
	// NOTE: same year-dependent income threshold caveat as DependentDeduction.
	// TODO(legal/reiwa7-spouse-wiring): 配偶者の所得要件判定に DependentIncomeLimit
	// を使うこと。現在の実装は呼び出し側が計算済みの控除額を渡す設計のため、
	// 配偶者資格判定ロジックを実装する際に配線すること。
	SpouseDeduction int64
	// LifeInsuranceDeduction: 生命保険料控除 in yen.
	LifeInsuranceDeduction int64
	// EarthquakeInsuranceDeduction: 地震保険料控除 in yen.
	EarthquakeInsuranceDeduction int64
	// SocialInsuranceDeduction: 社会保険料控除 in yen.
	SocialInsuranceDeduction int64
	// HousingLoanDeduction: 住宅借入金等特別控除 in yen.
	HousingLoanDeduction int64
	// WithheldTax: 源泉徴収済み税額 in yen.
	WithheldTax int64
}

// TaxResult holds the calculation output.
// All amounts are in yen (円単位, integer).
type TaxResult struct {
	// GrossIncome: 年収 (給与収入金額) in yen.
	GrossIncome int64 `json:"gross_income"`
	// TaxableIncome: 課税所得金額 in yen (千円未満切り捨て).
	TaxableIncome int64 `json:"taxable_income"`
	// AnnualTax: 年税額 (所得税 + 復興特別所得税) in yen.
	AnnualTax int64 `json:"annual_tax"`
	// Difference: 過不足税額 in yen.
	// Positive = 徴収不足 (employee owes); negative = 過剰徴収 (employee gets refund).
	Difference int64 `json:"difference"`
	// EmploymentDeduction used in yen.
	EmploymentDeduction int64 `json:"employment_deduction"`
	// TotalDeductions: 所得控除の合計 in yen.
	TotalDeductions int64 `json:"total_deductions"`
	// WithheldTax used in yen.
	WithheldTax int64 `json:"withheld_tax"`
}

// CalculateTax computes the year-end adjustment tax result from TaxInput.
//
// Formula (simplified statutory flow — requires annual update per 国税庁):
//  1. 給与所得控除の最低保障額を TaxYear に基づき強制適用
//     (令和6以前=55万、令和7以後=65万 — EmploymentDeductionMinimum)
//  2. 給与所得 = GrossIncome - EmploymentDeduction (最低保障床適用後)
//  3. 基礎控除 = BasicDeductionForYear(TaxYear, 給与所得) で自動決定
//     (令和7・8分は5段階制、令和6以前は従来の逓減制)
//  4. 課税所得 = 給与所得 - 所得控除合計 (千円未満切り捨て)
//  5. 年税額 = tax(課税所得) × 102.1% (復興特別所得税)
//  6. 過不足税額 = 年税額 - WithheldTax
//
// 年度依存配線 (令和7年度税制改正対応):
//   - 基礎控除は TaxInput.BasicDeduction を無視し BasicDeductionForYear で上書き。
//     年収・扶養の多寡による合計所得の代理値として給与所得(GrossIncome-Employment
//     Deduction)を使用。給与所得者の場合、給与所得 ≒ 合計所得金額。
//   - 給与所得控除は最低保障額をサービス層で保証。
//   - 扶養親族・配偶者の所得要件判定は現状未実装 (TaxInput フィールドの TODO 参照)。
//
// LEGAL: rate table and deduction limits MUST be loaded from yearend_settings and
// confirmed per 国税庁 annual revision.  This function is NOT legal advice.
// 前提: この実装は法的助言ではありません。社会保険労務士・税理士・弁護士による
// 一次法令源との確認が必要です。
func CalculateTax(in TaxInput) TaxResult {
	// 1. 給与所得控除の最低保障額を年度に基づき強制適用。
	// 令和6以前=55万 / 令和7以後=65万 (EmploymentDeductionMinimum 参照)。
	// 出典: 国税庁「令和7年分の基礎控除等の改正について」
	// https://www.nta.go.jp/users/gensen/2025kiso/index.htm
	//
	// LEGAL(免責): この実装は法的助言ではありません。
	empDeduction := in.EmploymentDeduction
	if minEmp := EmploymentDeductionMinimum(in.TaxYear); empDeduction < minEmp {
		empDeduction = minEmp
	}

	// 2. 給与所得 = 年収 - 給与所得控除 (non-negative)
	employmentIncome := in.GrossIncome - empDeduction
	if employmentIncome < 0 {
		employmentIncome = 0
	}

	// 3. 基礎控除を年度・合計所得依存で自動決定。
	// 給与所得者の場合、合計所得金額 ≒ 給与所得 (GrossIncome - EmploymentDeduction)。
	// 副業所得・一時所得等を考慮する場合は合計所得金額を別途算定して渡すこと。
	// TaxInput.BasicDeduction は BasicDeductionForYear の結果で上書きされる。
	// 出典: 国税庁 No.1199 / https://www.nta.go.jp/users/gensen/2025kiso/index.htm
	//
	// LEGAL(免責): この実装は法的助言ではありません。
	basicDeduction := BasicDeductionForYear(in.TaxYear, employmentIncome)

	// 4. 所得控除合計
	totalDeductions := basicDeduction +
		in.DependentDeduction +
		in.SpouseDeduction +
		in.LifeInsuranceDeduction +
		in.EarthquakeInsuranceDeduction +
		in.SocialInsuranceDeduction

	// 5. 課税所得 = 給与所得 - 所得控除合計 (千円未満切り捨て)
	taxableIncome := employmentIncome - totalDeductions
	if taxableIncome < 0 {
		taxableIncome = 0
	}
	// 千円未満切り捨て (truncate to nearest 1000 yen)
	taxableIncome = (taxableIncome / 1000) * 1000

	// 6. 所得税額 (速算表 — 国税庁 No.2260; 令和6・7年分で不変・確認済み)
	incomeTax := computeIncomeTax(taxableIncome)

	// 住宅借入金等特別控除 (applied after income tax computation)
	incomeTax -= in.HousingLoanDeduction
	if incomeTax < 0 {
		incomeTax = 0
	}

	// 7. 復興特別所得税 = 所得税額 × 2.1%
	// Annual tax = 所得税額 × 102.1% (rounded down)
	annualTax := incomeTax + (incomeTax * 21 / 1000)

	// 8. 過不足税額
	difference := annualTax - in.WithheldTax

	return TaxResult{
		GrossIncome:         in.GrossIncome,
		TaxableIncome:       taxableIncome,
		AnnualTax:           annualTax,
		Difference:          difference,
		EmploymentDeduction: empDeduction,
		TotalDeductions:     totalDeductions,
		WithheldTax:         in.WithheldTax,
	}
}

// ---------------------------------------------------------------------------
// 年度依存控除ロジック (令和7年度税制改正対応)
//
// LEGAL(免責): 以下の関数は法的助言ではありません。実運用前に社会保険労務士・税理士・
// 弁護士による一次法令源との確認が前提です。
//
// 出典:
//   - 国税庁「令和7年分の基礎控除等の改正について」
//     https://www.nta.go.jp/users/gensen/2025kiso/index.htm
//   - 国税庁 No.1199 基礎控除
//     https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1199.htm
//   - 国税庁 No.1191 配偶者控除
//     https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1191.htm
// ---------------------------------------------------------------------------

// BasicDeductionForYear returns the 基礎控除額 (yen) for the given tax year and
// the employee's 合計所得金額 (totalIncome, yen).
//
// The step-function introduced by the 令和7年度税制改正 applies to tax years 2025
// and 2026 (令和7・8年分); the transitional mid-income supplement lapses for 2027
// onwards (令和9年分以後).
//
//	令和6年分以前 (taxYear ≤ 2024):
//	  ≤ 24,000,000  → 480,000
//	  ≤ 24,500,000  → 320,000
//	  ≤ 25,000,000  → 160,000
//	  > 25,000,000  →       0
//
//	令和7・8年分 (taxYear = 2025 or 2026):
//	  ≤  1,320,000  → 950,000
//	  ≤  3,360,000  → 880,000
//	  ≤  4,890,000  → 680,000
//	  ≤  6,550,000  → 630,000
//	  ≤ 23,500,000  → 580,000
//	  > 23,500,000  → 高所得逓減を適用 (下記参照)
//
//	令和9年分以後 (taxYear ≥ 2027):
//	  ≤  1,320,000  → 950,000
//	  ≤ 23,500,000  → 580,000
//	  > 23,500,000  → 高所得逓減を適用 (下記参照)
//
// 高所得逓減 (令和7以後・2,350万超部分):
//
//	TODO(legal/reiwa7-highearner): 令和7分の2,350万超の基礎控除逓減テーブルは
//	国税庁一次情報での詳細未確認のため、既存の令和6基準(24百万超で320,000/160,000/0)
//	を暫定適用。正確な令和7の逓減テーブルは一次情報確認後に更新すること。
//	出典要確認: https://www.nta.go.jp/users/gensen/2025kiso/index.htm
//
// LEGAL(免責): この実装は法的助言ではありません。社会保険労務士・税理士・弁護士による
// 一次法令源との確認が前提です。
func BasicDeductionForYear(taxYear int, totalIncome int64) int64 {
	switch {
	case taxYear <= 2024:
		// 令和6年分以前: 国税庁 No.1199 準拠(確認済み)
		switch {
		case totalIncome <= 24_000_000:
			return 480_000
		case totalIncome <= 24_500_000:
			return 320_000
		case totalIncome <= 25_000_000:
			return 160_000
		default:
			return 0
		}

	case taxYear <= 2026:
		// 令和7・8年分: 国税庁「令和7年分の基礎控除等の改正について」準拠(確認済み)
		// 中間層上乗せは時限措置(令和7・8年分のみ)。
		switch {
		case totalIncome <= 1_320_000:
			return 950_000
		case totalIncome <= 3_360_000:
			return 880_000
		case totalIncome <= 4_890_000:
			return 680_000
		case totalIncome <= 6_550_000:
			return 630_000
		case totalIncome <= 23_500_000:
			return 580_000
		default:
			// TODO(legal/reiwa7-highearner): 2,350万超の令和7基礎控除逓減テーブルは
			// 国税庁一次情報での詳細未確認。既存令和6の高所得逓減を暫定適用。
			// 出典要確認: https://www.nta.go.jp/users/gensen/2025kiso/index.htm
			return basicDeductionHighIncomeR6Compat(totalIncome)
		}

	default:
		// 令和9年分以後 (taxYear >= 2027):
		// 中間層上乗せ解消。132万以下=95万、132万超23,500,000以下=58万。
		// TODO(legal/reiwa9): 令和9年分の詳細は国税庁一次情報での確認が必要。
		// 現時点では令和7改正の「最終形」として実装(中間層上乗せ解消の方向性のみ確認)。
		// 出典要確認: https://www.nta.go.jp/users/gensen/2025kiso/index.htm
		switch {
		case totalIncome <= 1_320_000:
			return 950_000
		case totalIncome <= 23_500_000:
			return 580_000
		default:
			// TODO(legal/reiwa7-highearner): 同上 — 2,350万超の逓減テーブルは未確認。
			return basicDeductionHighIncomeR6Compat(totalIncome)
		}
	}
}

// basicDeductionHighIncomeR6Compat applies the pre-R7 high-income taper for
// total income exceeding 23,500,000 yen as a temporary fallback.
//
// TODO(legal/reiwa7-highearner): 令和7年分の2,350万超の基礎控除逓減テーブルは
// 国税庁一次情報での詳細未確認のため暫定適用。令和6基準(24百万超での段階)を流用。
func basicDeductionHighIncomeR6Compat(totalIncome int64) int64 {
	// Reuse pre-R7 taper brackets: 24M → 320k, 24.5M → 160k, 25M+ → 0.
	// These thresholds originated from No.1199 (令和6以前) and are applied here
	// ONLY as a temporary stand-in until the authoritative R7 taper is confirmed.
	switch {
	case totalIncome <= 24_000_000:
		return 320_000
	case totalIncome <= 24_500_000:
		return 160_000
	default:
		return 0
	}
}

// EmploymentDeductionMinimum returns the 給与所得控除の最低保障額 (yen) for the
// given tax year.
//
//	令和6年分以前 (taxYear ≤ 2024): 550,000
//	令和7年分以後 (taxYear ≥ 2025): 650,000
//
// NOTE: この関数は最低保障額のみを返す。給与所得控除全体の上限・各区分の令和7改正詳細は
// 国税庁一次情報での確認が必要なため、最低保障額の切替のみ実装。
// TODO(legal/reiwa7-employment-deduction): 給与所得控除表全体(各区分・上限)の
// 令和7改正詳細は一次情報確認後に実装すること。
// 出典: 国税庁「令和7年分の基礎控除等の改正について」
// https://www.nta.go.jp/users/gensen/2025kiso/index.htm
//
// LEGAL(免責): この実装は法的助言ではありません。社会保険労務士・税理士・弁護士による
// 一次法令源との確認が前提です。
func EmploymentDeductionMinimum(taxYear int) int64 {
	if taxYear >= 2025 {
		return 650_000 // 令和7年分以後: 65万円
	}
	return 550_000 // 令和6年分以前: 55万円
}

// DependentIncomeLimit returns the 合計所得金額の上限 (yen) used to determine
// whether a person qualifies as a 控除対象配偶者 or 控除対象扶養親族.
//
//	令和元年分以前                (taxYear ≤ 2019): 380,000
//	令和2〜6年分 (2020 ≤ taxYear ≤ 2024): 480,000
//	令和7年分以後               (taxYear ≥ 2025): 580,000
//
// 出典: 国税庁 No.1191 配偶者控除
// https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1191.htm
//
// LEGAL(免責): この実装は法的助言ではありません。社会保険労務士・税理士・弁護士による
// 一次法令源との確認が前提です。
func DependentIncomeLimit(taxYear int) int64 {
	switch {
	case taxYear >= 2025:
		return 580_000 // 令和7年分以後: 58万円
	case taxYear >= 2020:
		return 480_000 // 令和2〜6年分: 48万円
	default:
		return 380_000 // 令和元年分以前: 38万円
	}
}

// computeIncomeTax applies the 所得税速算表 (超過累進税率) brackets.
//
// 出典: 国税庁 No.2260 所得税の税率
// https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/2260.htm
//
// 令和6年分・令和7年分ともに速算表の境界値・税率・控除額は不変(確認済み)。
// 注: 速算表だけでは最終所得税額にならない。以下を別途加算すること:
//   - 復興特別所得税 = 基準所得税額 × 2.1%(2013–2037年適用、CalculateTax で計上済み)
//   - 令和7年以後の超高所得者サーチャージ(課税所得330百万円超部分)は別建て未実装。
//
// TODO(legal/reiwa7-surcharge): 令和7年以後の超高所得者サーチャージ(課税所得3億3千万円
// 超部分への追加課税)は速算表外の別建て計算が必要。本タスクのスコープ外。
// 対象年は令和7年分以後(taxYear >= 2025)。一次情報確認後に CalculateTax 内で
// computeIncomeTax の戻り値に上乗せして実装すること。
// 出典要確認: 国税庁 No.2260 関連・令和7年度税制改正法令
//
// TODO(legal/reiwa9): 速算表そのものは令和7・令和9ともに不変(確認済み)。
// 令和9年以後の制度変更があれば taxYear パラメータを追加して対応すること。
//
// LEGAL(免責): この実装は法的助言ではありません。社会保険労務士・税理士・弁護士による
// 一次法令源との確認が前提です。
func computeIncomeTax(taxableIncome int64) int64 {
	// 所得税速算表 (令和6・令和7年分 — 国税庁 No.2260 より)
	// 課税所得(円)             税率  控除額(円)
	//      1,000 〜  1,949,000   5%          0
	//  1,950,000 〜  3,299,000  10%     97,500
	//  3,300,000 〜  6,949,000  20%    427,500
	//  6,950,000 〜  8,999,000  23%    636,000
	//  9,000,000 〜 17,999,000  33%  1,536,000
	// 18,000,000 〜 39,999,000  40%  2,796,000
	// 40,000,000 以上           45%  4,796,000
	switch {
	case taxableIncome <= 1_949_000:
		return taxableIncome * 5 / 100
	case taxableIncome <= 3_299_000:
		return taxableIncome*10/100 - 97_500
	case taxableIncome <= 6_949_000:
		return taxableIncome*20/100 - 427_500
	case taxableIncome <= 8_999_000:
		return taxableIncome*23/100 - 636_000
	case taxableIncome <= 17_999_000:
		return taxableIncome*33/100 - 1_536_000
	case taxableIncome <= 39_999_000:
		return taxableIncome*40/100 - 2_796_000
	default:
		return taxableIncome*45/100 - 4_796_000
	}
}

// RunCalculationInput holds fields for triggering a calculation.
type RunCalculationInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	// TaxInput provides the normalised calculation inputs.
	// Callers are responsible for deriving TaxInput from the decrypted submission
	// (yearend:reveal permission required) and payroll data.
	TaxIn TaxInput
	IP    *string
}

// RunCalculation computes the year-end tax result for an employee and upserts
// a yearend_calculations row.  The source submission must be in 'submitted' status.
// Finalised calculations are immutable.
//
// Security: result_json contains amounts only — no decrypted PII.
func (s *Service) RunCalculation(ctx context.Context, in RunCalculationInput) (*Calculation, error) {
	result := CalculateTax(in.TaxIn)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("yearend: marshal result: %w", err)
	}

	var calc Calculation
	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Fetch the source submission (must be 'submitted').
		var sub struct {
			ID     uuid.UUID `gorm:"column:id"`
			Status string    `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT id, status FROM yearend_submissions
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&sub).Error; err != nil {
			return fmt.Errorf("yearend: calc read submission: %w", err)
		}
		if sub.ID == uuid.Nil {
			return ErrNotFound
		}
		if sub.Status != SubmissionSubmitted {
			return fmt.Errorf("%w: submission must be in 'submitted' status, got %q",
				ErrInvalidTransition, sub.Status)
		}

		// Check existing calculation for finalised guard.
		var existing struct {
			FinalisedAt *time.Time `gorm:"column:finalised_at"`
		}
		if err := tx.Raw(
			`SELECT finalised_at FROM yearend_calculations
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("yearend: calc finalised check: %w", err)
		}
		if existing.FinalisedAt != nil {
			return ErrFinalised
		}

		now := time.Now().UTC()
		id := uuid.New()
		res := tx.Exec(
			`INSERT INTO yearend_calculations
			   (id, tenant_id, employee_id, tax_year, submission_id, status, result_json, calculated_at)
			 VALUES (?, ?, ?, ?, ?, 'completed', ?::jsonb, ?)
			 ON CONFLICT (employee_id, tenant_id, tax_year) DO UPDATE
			   SET submission_id  = EXCLUDED.submission_id,
			       status         = 'completed',
			       result_json    = EXCLUDED.result_json,
			       calculated_at  = EXCLUDED.calculated_at,
			       updated_at     = now()
			 WHERE yearend_calculations.finalised_at IS NULL`,
			id, in.TenantID, in.EmployeeID, in.TaxYear, sub.ID, resultJSON, now,
		)
		if res.Error != nil {
			return fmt.Errorf("yearend: calc upsert: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrFinalised
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, submission_id, status,
			        result_json, calculated_at, finalised_at, created_at, updated_at
			 FROM yearend_calculations
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&calc).Error; err != nil {
			return fmt.Errorf("yearend: calc re-read: %w", err)
		}

		idStr := calc.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_calculation.completed",
			ResourceType: "yearend_calculation",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &calc, nil
}

// FinaliseCalculationInput holds fields for finalising a calculation.
type FinaliseCalculationInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	IP         *string
}

// FinaliseCalculation marks a completed calculation as finalised (immutable) and
// locks the source submission.  Both records become immutable after this call.
func (s *Service) FinaliseCalculation(ctx context.Context, in FinaliseCalculationInput) (*Calculation, error) {
	var calc Calculation
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var existing struct {
			ID          uuid.UUID  `gorm:"column:id"`
			Status      string     `gorm:"column:status"`
			SubmissionID uuid.UUID `gorm:"column:submission_id"`
			FinalisedAt *time.Time `gorm:"column:finalised_at"`
		}
		if err := tx.Raw(
			`SELECT id, status, submission_id, finalised_at FROM yearend_calculations
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("yearend: finalise calc read: %w", err)
		}
		if existing.ID == uuid.Nil {
			return ErrNotFound
		}
		if existing.FinalisedAt != nil {
			return fmt.Errorf("%w: calculation already finalised", ErrInvalidTransition)
		}
		if existing.Status != CalcCompleted {
			return fmt.Errorf("%w: calculation must be in 'completed' status, got %q",
				ErrInvalidTransition, existing.Status)
		}

		now := time.Now().UTC()
		// Finalise the calculation.
		if err := tx.Exec(
			`UPDATE yearend_calculations
			 SET status = 'finalised', finalised_at = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND finalised_at IS NULL`,
			now, existing.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("yearend: finalise calc update: %w", err)
		}

		// Lock the source submission.
		if err := tx.Exec(
			`UPDATE yearend_submissions
			 SET status = 'locked', locked_at = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			now, existing.SubmissionID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("yearend: finalise lock submission: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, submission_id, status,
			        result_json, calculated_at, finalised_at, created_at, updated_at
			 FROM yearend_calculations WHERE id = ? AND tenant_id = ? LIMIT 1`,
			existing.ID, in.TenantID,
		).Scan(&calc).Error; err != nil {
			return fmt.Errorf("yearend: finalise calc re-read: %w", err)
		}

		idStr := calc.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_calculation.finalised",
			ResourceType: "yearend_calculation",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &calc, nil
}

// GetCalculation fetches the calculation for an employee and tax year.
func (s *Service) GetCalculation(ctx context.Context, tenantID, employeeID uuid.UUID, taxYear int) (*Calculation, error) {
	var calc Calculation
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, submission_id, status,
			        result_json, calculated_at, finalised_at, created_at, updated_at
			 FROM yearend_calculations
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			employeeID, tenantID, taxYear,
		).Scan(&calc).Error
	})
	if err != nil {
		return nil, err
	}
	if calc.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &calc, nil
}

// ---------------------------------------------------------------------------
// Report generation (帳票)
// ---------------------------------------------------------------------------

// GenerateWithholdingSlipInput holds fields for generating a 源泉徴収票.
type GenerateWithholdingSlipInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	// Format: "csv" or "pdf".  PDF scaffold returns a placeholder.
	Format string
	IP     *string
}

// GenerateWithholdingSlip generates a withholding slip (源泉徴収票) for an employee.
// The calculation must be in 'finalised' status.
// CSV format: rendered inline.  PDF format: scaffold (content_ref placeholder).
//
// Security: the generated content contains amounts from result_json only;
// no decrypted PII from the submission declaration.
func (s *Service) GenerateWithholdingSlip(ctx context.Context, in GenerateWithholdingSlipInput) (*Report, []byte, error) {
	if in.Format != ReportFormatCSV && in.Format != ReportFormatPDF {
		return nil, nil, fmt.Errorf("%w: format must be 'csv' or 'pdf'", ErrInvalidInput)
	}

	var report Report
	var content []byte

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var calc Calculation
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, status, result_json
			 FROM yearend_calculations
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&calc).Error; err != nil {
			return fmt.Errorf("yearend: report read calc: %w", err)
		}
		if calc.ID == uuid.Nil {
			return ErrNotFound
		}
		if calc.Status != CalcFinalised {
			return fmt.Errorf("%w: calculation must be finalised before generating report, got %q",
				ErrInvalidTransition, calc.Status)
		}

		var resultJSON TaxResult
		if err := json.Unmarshal(jsonbArg(calc.ResultJSON, "{}"), &resultJSON); err != nil {
			return fmt.Errorf("yearend: report unmarshal result: %w", err)
		}

		var contentRef string
		switch in.Format {
		case ReportFormatCSV:
			csv, err := renderWithholdingSlipCSV(in.EmployeeID, in.TaxYear, resultJSON)
			if err != nil {
				return err
			}
			content = csv
			contentRef = ""
		case ReportFormatPDF:
			// Render a real PDF using go-pdf/fpdf.
			// Security: result contains amounts only — no decrypted PII.
			pdfBytes, pdfErr := renderWithholdingSlipPDF(in.EmployeeID, in.TaxYear, resultJSON)
			if pdfErr != nil {
				return fmt.Errorf("yearend: generate withholding slip PDF: %w", pdfErr)
			}
			content = pdfBytes
			contentRef = fmt.Sprintf("yearend/%d/%s/withholding_slip.pdf", in.TaxYear, in.EmployeeID.String())
		}

		id := uuid.New()
		calcID := calc.ID
		empID := in.EmployeeID
		if err := tx.Exec(
			`INSERT INTO yearend_reports
			   (id, tenant_id, employee_id, tax_year, report_type, calc_id, content_ref, format, generated_at)
			 VALUES (?, ?, ?, ?, 'withholding_slip', ?, ?, ?, now())`,
			id, in.TenantID, empID, in.TaxYear, calcID, contentRef, in.Format,
		).Error; err != nil {
			return fmt.Errorf("yearend: report insert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, report_type, calc_id,
			        content_ref, format, generated_at, created_at, updated_at
			 FROM yearend_reports WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, in.TenantID,
		).Scan(&report).Error; err != nil {
			return fmt.Errorf("yearend: report re-read: %w", err)
		}

		idStr := report.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_report.generated",
			ResourceType: "yearend_report",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return &report, content, nil
}

// renderWithholdingSlipCSV renders a 源泉徴収票 as CSV.
// Output contains amounts from the calculation result only — no PII.
func renderWithholdingSlipCSV(employeeID uuid.UUID, taxYear int, r TaxResult) ([]byte, error) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{
		"employee_id", "tax_year",
		"gross_income", "employment_deduction", "total_deductions",
		"taxable_income", "annual_tax", "withheld_tax", "difference",
	})
	_ = w.Write([]string{
		employeeID.String(),
		fmt.Sprintf("%d", taxYear),
		fmt.Sprintf("%d", r.GrossIncome),
		fmt.Sprintf("%d", r.EmploymentDeduction),
		fmt.Sprintf("%d", r.TotalDeductions),
		fmt.Sprintf("%d", r.TaxableIncome),
		fmt.Sprintf("%d", r.AnnualTax),
		fmt.Sprintf("%d", r.WithheldTax),
		fmt.Sprintf("%d", r.Difference),
	})
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("yearend: render csv: %w", err)
	}
	return []byte(sb.String()), nil
}

// ---------------------------------------------------------------------------
// Payroll SaaS push (給与SaaS連携足場)
// ---------------------------------------------------------------------------

// PushToPayrollInput holds fields for pushing results to a payroll SaaS.
type PushToPayrollInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TaxYear    int
	IP         *string
}

// PushToPayroll pushes a finalised year-end adjustment result to the payroll
// SaaS via the configured PayrollPusher adapter.  Idempotent on
// (employee, tax_year, provider): re-pushing updates the existing push record.
//
// Security: only amounts from result_json are forwarded — no decrypted PII.
// ProviderRef is an opaque reference; credentials are never stored.
func (s *Service) PushToPayroll(ctx context.Context, in PushToPayrollInput) (*PayrollPush, error) {
	var push PayrollPush

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var calc Calculation
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, status, result_json
			 FROM yearend_calculations
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear,
		).Scan(&calc).Error; err != nil {
			return fmt.Errorf("yearend: push read calc: %w", err)
		}
		if calc.ID == uuid.Nil {
			return ErrNotFound
		}
		if calc.Status != CalcFinalised {
			return fmt.Errorf("%w: calculation must be finalised before pushing, got %q",
				ErrInvalidTransition, calc.Status)
		}

		// Perform the push via the adapter (stub in MVP).
		pushResult, err := s.pusher.Push(ctx, PushRequest{
			TenantID:   in.TenantID,
			EmployeeID: in.EmployeeID,
			TaxYear:    in.TaxYear,
			CalcID:     calc.ID,
			ResultJSON: json.RawMessage(jsonbArg(calc.ResultJSON, "{}")),
		})
		if err != nil {
			// Record a 'failed' push row on error.
			_ = tx.Exec(
				`INSERT INTO yearend_payroll_pushes
				   (id, tenant_id, employee_id, tax_year, calc_id, provider, status, provider_ref)
				 VALUES (?, ?, ?, ?, ?, ?, 'failed', '')
				 ON CONFLICT (employee_id, tenant_id, tax_year, provider) DO UPDATE
				   SET status = 'failed', updated_at = now()`,
				uuid.New(), in.TenantID, in.EmployeeID, in.TaxYear, calc.ID, s.pusher.Provider(),
			)
			return fmt.Errorf("yearend: push to payroll: %w", err)
		}

		now := time.Now().UTC()
		id := uuid.New()
		res := tx.Exec(
			`INSERT INTO yearend_payroll_pushes
			   (id, tenant_id, employee_id, tax_year, calc_id, provider, status, provider_ref, pushed_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'pushed', ?, ?)
			 ON CONFLICT (employee_id, tenant_id, tax_year, provider) DO UPDATE
			   SET calc_id      = EXCLUDED.calc_id,
			       status       = 'pushed',
			       provider_ref = EXCLUDED.provider_ref,
			       pushed_at    = EXCLUDED.pushed_at,
			       updated_at   = now()`,
			id, in.TenantID, in.EmployeeID, in.TaxYear, calc.ID,
			s.pusher.Provider(), pushResult.ProviderRef, now,
		)
		if res.Error != nil {
			return fmt.Errorf("yearend: push upsert: %w", res.Error)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, calc_id, provider, status,
			        provider_ref, pushed_at, created_at, updated_at
			 FROM yearend_payroll_pushes
			 WHERE employee_id = ? AND tenant_id = ? AND tax_year = ? AND provider = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, in.TaxYear, s.pusher.Provider(),
		).Scan(&push).Error; err != nil {
			return fmt.Errorf("yearend: push re-read: %w", err)
		}

		idStr := push.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_payroll_push.pushed",
			ResourceType: "yearend_payroll_push",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &push, nil
}

// ---------------------------------------------------------------------------
// Summary return (法定調書合計表)
// ---------------------------------------------------------------------------

// GenerateSummaryReturnInput holds fields for generating a 法定調書合計表.
type GenerateSummaryReturnInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	TaxYear  int
	// Format: "csv" or "pdf".
	Format string
	IP     *string
}

// GenerateSummaryReturn aggregates all finalised calculations for the tenant/year
// and generates the 法定調書合計表.  At least one finalised calculation must exist.
//
// Security: aggregates amounts from result_json only — no decrypted PII.
func (s *Service) GenerateSummaryReturn(ctx context.Context, in GenerateSummaryReturnInput) (*Report, []byte, error) {
	if in.Format != ReportFormatCSV && in.Format != ReportFormatPDF {
		return nil, nil, fmt.Errorf("%w: format must be 'csv' or 'pdf'", ErrInvalidInput)
	}

	var report Report
	var content []byte

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Aggregate finalised calculations.
		type aggRow struct {
			EmployeeCount int   `gorm:"column:employee_count"`
			TotalGross    int64 `gorm:"column:total_gross"`
			TotalTax      int64 `gorm:"column:total_tax"`
			TotalWithheld int64 `gorm:"column:total_withheld"`
			TotalDiff     int64 `gorm:"column:total_diff"`
		}
		var agg aggRow
		if err := tx.Raw(`
			SELECT
			    COUNT(*)                                                    AS employee_count,
			    COALESCE(SUM((result_json->>'gross_income')::bigint),0)    AS total_gross,
			    COALESCE(SUM((result_json->>'annual_tax')::bigint),0)      AS total_tax,
			    COALESCE(SUM((result_json->>'withheld_tax')::bigint),0)    AS total_withheld,
			    COALESCE(SUM((result_json->>'difference')::bigint),0)      AS total_diff
			FROM yearend_calculations
			WHERE tenant_id = ? AND tax_year = ? AND status = 'finalised'`,
			in.TenantID, in.TaxYear,
		).Scan(&agg).Error; err != nil {
			return fmt.Errorf("yearend: summary_return aggregate: %w", err)
		}
		if agg.EmployeeCount == 0 {
			return fmt.Errorf("%w: no finalised calculations found for tax year %d",
				ErrNotFound, in.TaxYear)
		}

		sr := SummaryReturn{
			TenantID:      in.TenantID.String(),
			TaxYear:       in.TaxYear,
			EmployeeCount: agg.EmployeeCount,
			TotalGross:    agg.TotalGross,
			TotalTax:      agg.TotalTax,
			TotalWithheld: agg.TotalWithheld,
			TotalDiff:     agg.TotalDiff,
		}

		var contentRef string
		switch in.Format {
		case ReportFormatCSV:
			csvBytes, err := renderSummaryReturnCSV(sr)
			if err != nil {
				return err
			}
			content = csvBytes
			contentRef = ""
		case ReportFormatPDF:
			pdfBytes, err := renderSummaryReturnPDF(sr)
			if err != nil {
				return fmt.Errorf("yearend: generate summary return PDF: %w", err)
			}
			content = pdfBytes
			contentRef = fmt.Sprintf("yearend/%d/summary_return.pdf", in.TaxYear)
		}

		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO yearend_reports
			   (id, tenant_id, employee_id, tax_year, report_type, calc_id, content_ref, format, generated_at)
			 VALUES (?, ?, NULL, ?, 'summary_return', NULL, ?, ?, now())`,
			id, in.TenantID, in.TaxYear, contentRef, in.Format,
		).Error; err != nil {
			return fmt.Errorf("yearend: summary_return report insert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, report_type, calc_id,
			        content_ref, format, generated_at, created_at, updated_at
			 FROM yearend_reports WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, in.TenantID,
		).Scan(&report).Error; err != nil {
			return fmt.Errorf("yearend: summary_return report re-read: %w", err)
		}

		idStr := report.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "yearend_report.summary_return.generated",
			ResourceType: "yearend_report",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return &report, content, nil
}

// renderSummaryReturnCSV renders a 法定調書合計表 as CSV.
// Output contains aggregate amounts only — no per-employee PII.
func renderSummaryReturnCSV(sr SummaryReturn) ([]byte, error) {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{
		"tenant_id", "tax_year", "employee_count",
		"total_gross_income", "total_annual_tax", "total_withheld_tax", "total_difference",
	})
	_ = w.Write([]string{
		sr.TenantID,
		fmt.Sprintf("%d", sr.TaxYear),
		fmt.Sprintf("%d", sr.EmployeeCount),
		fmt.Sprintf("%d", sr.TotalGross),
		fmt.Sprintf("%d", sr.TotalTax),
		fmt.Sprintf("%d", sr.TotalWithheld),
		fmt.Sprintf("%d", sr.TotalDiff),
	})
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("yearend: render summary_return csv: %w", err)
	}
	return []byte(sb.String()), nil
}

// GetSummaryReturnReports returns report records for summary_return for the
// given tenant/year (most recent first).
func (s *Service) GetSummaryReturnReports(ctx context.Context, tenantID uuid.UUID, taxYear int) ([]Report, error) {
	var reports []Report
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, tax_year, report_type, calc_id,
			        content_ref, format, generated_at, created_at, updated_at
			 FROM yearend_reports
			 WHERE tenant_id = ? AND tax_year = ? AND report_type = 'summary_return'
			 ORDER BY generated_at DESC`,
			tenantID, taxYear,
		).Scan(&reports).Error
	})
	if err != nil {
		return nil, err
	}
	return reports, nil
}
