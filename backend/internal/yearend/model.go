// Package yearend implements the year-end adjustment (年末調整) domain:
// LM-051 — deduction declaration collection (収集), tax calculation (計算),
// report generation (帳票), and payroll-SaaS push scaffold (給与SaaS連携足場).
//
// Security design:
//   - Employee declaration data (扶養親族, 保険料控除, 住宅借入金等) is stored
//     ONLY in yearend_submissions.declaration_enc as AES-256-GCM ciphertext.
//     The plaintext is NEVER persisted to non-encrypted columns, logs, audit
//     resource_ids, or API responses.
//   - declaration_hash is a SHA-256 hash of the plaintext for integrity
//     (改竄検知); it does NOT reveal the plaintext.
//   - The yearend:reveal permission is required to decrypt the declaration
//     (enforced at route layer AND in-service, defence-in-depth).
//   - yearend_calculations.result_json contains computed amounts only —
//     no decrypted PII from the submission.
//   - Payroll-SaaS integration is abstracted behind the PayrollPusher interface
//     with a stub (mock) implementation. Real integration requires external
//     provider credentials and is deferred (P3).
//
// Relation to ledger (法定三帳簿 / ledger package):
//   The finalised calculation feeds the 源泉徴収票 (withholding slip), which is
//   separate from the statutory three ledgers (worker_rosters / wage_ledgers /
//   attendance_books).  The yearend domain is kept in its own package to respect
//   distinct legal retention rules and workflows.
//
// LEGAL NOTE: rate tables, deduction ceilings, form definitions, and retention
// rules are legal-year configuration values (yearend_settings). They MUST be
// confirmed against the current 国税庁 guidance by a tax accountant / 社労士
// before production use.  This implementation is NOT legal advice.
package yearend

import (
	"time"

	"github.com/google/uuid"
)

// Submission status constants.
const (
	SubmissionDraft     = "draft"
	SubmissionSubmitted = "submitted"
	SubmissionLocked    = "locked"
)

// Calculation status constants.
const (
	CalcPending   = "pending"
	CalcCompleted = "completed"
	CalcFinalised = "finalised"
)

// Report type constants.
const (
	ReportWithholdingSlip = "withholding_slip" // 源泉徴収票 (per employee)
	ReportSummaryReturn   = "summary_return"   // 法定調書合計表 (per tenant)
)

// Report format constants.
const (
	ReportFormatCSV = "csv"
	ReportFormatPDF = "pdf"
)

// PayrollPush status constants.
const (
	PushPending = "pending"
	PushPushed  = "pushed"
	PushFailed  = "failed"
)

// Payroll provider identifiers (mirrors ledger.payroll_links).
const (
	ProviderMoneyForward = "moneyforward"
	ProviderFreee        = "freee"
	ProviderYayoi        = "yayoi"
	ProviderMock         = "mock"
)

// Settings is the GORM model for yearend_settings (per-tenant per-year config).
type Settings struct {
	ID                   uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID             uuid.UUID `gorm:"column:tenant_id"`
	TaxYear              int       `gorm:"column:tax_year"`
	RateTableJSON        []byte    `gorm:"column:rate_table_json;type:jsonb"`
	DeductionLimitsJSON  []byte    `gorm:"column:deduction_limits_json;type:jsonb"`
	CreatedAt            time.Time `gorm:"column:created_at"`
	UpdatedAt            time.Time `gorm:"column:updated_at"`
}

// TableName maps Settings to yearend_settings.
func (Settings) TableName() string { return "yearend_settings" }

// Submission is the GORM model for yearend_submissions (控除申告収集).
//
// Security note on DeclarationEnc:
//   - This field holds the AES-256-GCM ciphertext of the employee's deduction
//     declaration (扶養親族, 保険料控除, 住宅借入金等).
//   - The plaintext is NEVER stored or returned without the yearend:reveal
//     permission AND passing the service-layer decrypt gate.
//   - DeclarationHash is a SHA-256 of the plaintext for tamper detection;
//     it does NOT reveal the plaintext.
type Submission struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID      uuid.UUID  `gorm:"column:employee_id"`
	TaxYear         int        `gorm:"column:tax_year"`
	Status          string     `gorm:"column:status"`
	DeclarationEnc  []byte     `gorm:"column:declaration_enc;type:bytea"`
	DeclarationHash string     `gorm:"column:declaration_hash"`
	SubmittedAt     *time.Time `gorm:"column:submitted_at"`
	LockedAt        *time.Time `gorm:"column:locked_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps Submission to yearend_submissions.
func (Submission) TableName() string { return "yearend_submissions" }

// Calculation is the GORM model for yearend_calculations (計算結果).
//
// Security note: ResultJSON holds computed amounts (課税所得/年税額/過不足税額);
// it does NOT contain any decrypted PII from the source submission.
type Calculation struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID   uuid.UUID  `gorm:"column:employee_id"`
	TaxYear      int        `gorm:"column:tax_year"`
	SubmissionID uuid.UUID  `gorm:"column:submission_id"`
	Status       string     `gorm:"column:status"`
	ResultJSON   []byte     `gorm:"column:result_json;type:jsonb"`
	CalculatedAt *time.Time `gorm:"column:calculated_at"`
	FinalisedAt  *time.Time `gorm:"column:finalised_at"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// TableName maps Calculation to yearend_calculations.
func (Calculation) TableName() string { return "yearend_calculations" }

// Report is the GORM model for yearend_reports (帳票生成記録).
type Report struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID  *uuid.UUID `gorm:"column:employee_id"`
	TaxYear     int        `gorm:"column:tax_year"`
	ReportType  string     `gorm:"column:report_type"`
	CalcID      *uuid.UUID `gorm:"column:calc_id"`
	ContentRef  string     `gorm:"column:content_ref"`
	Format      string     `gorm:"column:format"`
	GeneratedAt time.Time  `gorm:"column:generated_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps Report to yearend_reports.
func (Report) TableName() string { return "yearend_reports" }

// SummaryReturn is an in-memory aggregate for generating the 法定調書合計表
// (summary return) per tenant/year.  It is computed from finalised calculations
// and is NOT stored as its own DB table — the resulting report row is recorded
// in yearend_reports with report_type='summary_return'.
//
// Security note: amounts are aggregated from result_json (computed values only);
// no decrypted PII from submissions is included here.
type SummaryReturn struct {
	TenantID       string `json:"tenant_id"`
	TaxYear        int    `json:"tax_year"`
	EmployeeCount  int    `json:"employee_count"`
	TotalGross     int64  `json:"total_gross_income"`
	TotalTax       int64  `json:"total_annual_tax"`
	TotalWithheld  int64  `json:"total_withheld_tax"`
	TotalDiff      int64  `json:"total_difference"`
}

// PayrollPush is the GORM model for yearend_payroll_pushes (給与SaaS連携足場).
//
// Security note: ProviderRef is an opaque provider-side reference only.
// No credentials, tokens, card numbers, or PII are stored here.
type PayrollPush struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID  uuid.UUID  `gorm:"column:employee_id"`
	TaxYear     int        `gorm:"column:tax_year"`
	CalcID      uuid.UUID  `gorm:"column:calc_id"`
	Provider    string     `gorm:"column:provider"`
	Status      string     `gorm:"column:status"`
	ProviderRef string     `gorm:"column:provider_ref"`
	PushedAt    *time.Time `gorm:"column:pushed_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps PayrollPush to yearend_payroll_pushes.
func (PayrollPush) TableName() string { return "yearend_payroll_pushes" }
