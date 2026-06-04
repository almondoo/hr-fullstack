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
	// GrossIncome: 年収 (給与収入金額) in yen.
	GrossIncome int64
	// EmploymentDeduction: 給与所得控除 in yen (from rate table).
	EmploymentDeduction int64
	// BasicDeduction: 基礎控除 in yen (from deduction limits).
	BasicDeduction int64
	// DependentDeduction: 扶養控除合計 in yen.
	DependentDeduction int64
	// SpouseDeduction: 配偶者控除 / 配偶者特別控除 in yen.
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
//  1. 給与所得 = GrossIncome - EmploymentDeduction (給与所得控除)
//  2. 課税所得 = 給与所得 - 所得控除合計 (千円未満切り捨て)
//  3. 年税額 = tax(課税所得) × 102.1% (復興特別所得税)
//  4. 過不足税額 = 年税額 - WithheldTax
//
// LEGAL: rate table and deduction limits MUST be loaded from yearend_settings and
// confirmed per 国税庁 annual revision.  The constants below are placeholders
// (the actual values are in yearend_settings.deduction_limits_json /
// rate_table_json).  This function is NOT legal advice.
func CalculateTax(in TaxInput) TaxResult {
	// 1. 給与所得 = 年収 - 給与所得控除 (non-negative)
	employmentIncome := in.GrossIncome - in.EmploymentDeduction
	if employmentIncome < 0 {
		employmentIncome = 0
	}

	// 2. 所得控除合計
	totalDeductions := in.BasicDeduction +
		in.DependentDeduction +
		in.SpouseDeduction +
		in.LifeInsuranceDeduction +
		in.EarthquakeInsuranceDeduction +
		in.SocialInsuranceDeduction

	// 3. 課税所得 = 給与所得 - 所得控除合計 (千円未満切り捨て)
	taxableIncome := employmentIncome - totalDeductions
	if taxableIncome < 0 {
		taxableIncome = 0
	}
	// 千円未満切り捨て (truncate to nearest 1000 yen)
	taxableIncome = (taxableIncome / 1000) * 1000

	// 4. 所得税額 (速算表 — placeholder rates; load from rate_table_json in production)
	incomeTax := computeIncomeTax(taxableIncome)

	// 住宅借入金等特別控除 (applied after income tax computation)
	incomeTax -= in.HousingLoanDeduction
	if incomeTax < 0 {
		incomeTax = 0
	}

	// 5. 復興特別所得税 = 所得税額 × 2.1%
	// Annual tax = 所得税額 × 102.1% (rounded down)
	annualTax := incomeTax + (incomeTax * 21 / 1000)

	// 6. 過不足税額
	difference := annualTax - in.WithheldTax

	return TaxResult{
		GrossIncome:         in.GrossIncome,
		TaxableIncome:       taxableIncome,
		AnnualTax:           annualTax,
		Difference:          difference,
		EmploymentDeduction: in.EmploymentDeduction,
		TotalDeductions:     totalDeductions,
		WithheldTax:         in.WithheldTax,
	}
}

// computeIncomeTax applies the 所得税速算表 brackets.
// PLACEHOLDER: these brackets are approximate 2024-tax-year values and MUST be
// confirmed/updated from yearend_settings.rate_table_json per annual revision.
// This function is NOT legal advice.
func computeIncomeTax(taxableIncome int64) int64 {
	// 速算表 (超過累進税率)
	// 課税所得      税率  控除額
	// ~1,950,000    5%       0
	// ~3,300,000   10%   97,500
	// ~6,950,000   20%  427,500
	// ~9,000,000   23%  636,000
	// ~18,000,000  33% 1,536,000
	// ~40,000,000  40% 2,796,000
	// >40,000,000  45% 4,796,000
	switch {
	case taxableIncome <= 1_950_000:
		return taxableIncome * 5 / 100
	case taxableIncome <= 3_300_000:
		return taxableIncome*10/100 - 97_500
	case taxableIncome <= 6_950_000:
		return taxableIncome*20/100 - 427_500
	case taxableIncome <= 9_000_000:
		return taxableIncome*23/100 - 636_000
	case taxableIncome <= 18_000_000:
		return taxableIncome*33/100 - 1_536_000
	case taxableIncome <= 40_000_000:
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
			// PDF generation scaffold: real PDF generation (pdfcpu / external renderer)
			// is deferred (P3).  Return a placeholder content_ref.
			content = nil
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
