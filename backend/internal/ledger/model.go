// Package ledger implements the statutory three-ledger (法定三帳簿) domain:
// 労働者名簿 (worker roster), 賃金台帳 (wage ledger), and 出勤簿 (attendance book).
//
// Features (ST-LM-10 / LM-054 / LM-050 / LM-053 / NFR-011 / INT-002):
//   - Builds each ledger from existing masters/aggregates:
//     worker roster from employees / employee_assignments / employment_contracts;
//     attendance book from attendance_records / work_summaries;
//     wage ledger from payroll-SaaS imports (payroll_links).
//   - Payroll-SaaS integration is abstracted behind the PayrollImporter interface
//     with a mock implementation (adapter abstraction; real integration is P3).
//   - Retention management: retention_until is computed from a tenant-configured
//     retention period (ledger_settings) and a retention-basis date (起算日), so
//     the statutory period (原則5年 / 経過措置3年) is NEVER hardcoded.
//   - 真実性 (truthfulness): finalise sets finalised_at; finalised records are
//     immutable.  可視性 (visibility): CSV export of each ledger.
//
// LEGAL NOTE: retention years, retention-basis rules, statutory-item templates,
// and the electronic-storage policy are configurable in ledger_settings and
// require confirmation by a 社労士 / lawyer.  This implementation is not legal
// advice; settings must be kept current to follow amendments (改正・経過措置).
package ledger

import (
	"time"

	"github.com/google/uuid"
)

// Retention-basis kinds (起算日種別).
const (
	BasisResignation    = "resignation"     // 退職日
	BasisLastEntry      = "last_entry"      // 最終記入日
	BasisLastAttendance = "last_attendance" // 最終出勤日
)

// Payroll provider identifiers (adapter abstraction).
const (
	ProviderMoneyForward = "moneyforward"
	ProviderFreee        = "freee"
	ProviderYayoi        = "yayoi"
	ProviderMock         = "mock"
)

// Payroll-link statuses.
const (
	PayrollStatusImported = "imported"
	PayrollStatusConsumed = "consumed"
)

// Settings is the GORM model for ledger_settings (per-tenant legal config).
type Settings struct {
	ID                    uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID              uuid.UUID `gorm:"column:tenant_id"`
	DefaultRetentionYears int       `gorm:"column:default_retention_years"`
	DefaultRetentionBasis string    `gorm:"column:default_retention_basis"`
	ElectronicStorageJSON []byte    `gorm:"column:electronic_storage_json;type:jsonb"`
	CreatedAt             time.Time `gorm:"column:created_at"`
	UpdatedAt             time.Time `gorm:"column:updated_at"`
}

// TableName maps Settings to ledger_settings.
func (Settings) TableName() string { return "ledger_settings" }

// PayrollLink is the GORM model for payroll_links (payroll-SaaS import record).
//
// Security: provider_ref is an opaque provider-side reference only.  No card
// numbers / PANs / raw tokens are ever received or stored here.
type PayrollLink struct {
	ID                  uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID `gorm:"column:tenant_id"`
	EmployeeID          uuid.UUID `gorm:"column:employee_id"`
	Provider            string    `gorm:"column:provider"`
	Period              string    `gorm:"column:period"`
	ProviderRef         string    `gorm:"column:provider_ref"`
	ImportedPayloadJSON []byte    `gorm:"column:imported_payload_json;type:jsonb"`
	Status              string    `gorm:"column:status"`
	ImportedAt          time.Time `gorm:"column:imported_at"`
	CreatedAt           time.Time `gorm:"column:created_at"`
	UpdatedAt           time.Time `gorm:"column:updated_at"`
}

// TableName maps PayrollLink to payroll_links.
func (PayrollLink) TableName() string { return "payroll_links" }

// WorkerRoster is the GORM model for worker_rosters (労働者名簿).
type WorkerRoster struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID         uuid.UUID  `gorm:"column:employee_id"`
	RosterJSON         []byte     `gorm:"column:roster_json;type:jsonb"`
	RetentionBasis     string     `gorm:"column:retention_basis"`
	RetentionBasisDate *time.Time `gorm:"column:retention_basis_date"`
	RetentionUntil     *time.Time `gorm:"column:retention_until"`
	FinalisedAt        *time.Time `gorm:"column:finalized_at"` //nolint:misspell // DB column name finalized_at is the established schema contract
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps WorkerRoster to worker_rosters.
func (WorkerRoster) TableName() string { return "worker_rosters" }

// WageLedger is the GORM model for wage_ledgers (賃金台帳).
type WageLedger struct {
	ID                  uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID          uuid.UUID  `gorm:"column:employee_id"`
	Period              string     `gorm:"column:period"`
	WageJSON            []byte     `gorm:"column:wage_json;type:jsonb"`
	SourcePayrollLinkID *uuid.UUID `gorm:"column:source_payroll_link_id"`
	RetentionBasis      string     `gorm:"column:retention_basis"`
	RetentionBasisDate  *time.Time `gorm:"column:retention_basis_date"`
	RetentionUntil      *time.Time `gorm:"column:retention_until"`
	FinalisedAt         *time.Time `gorm:"column:finalized_at"` //nolint:misspell // DB column name finalized_at is the established schema contract
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

// TableName maps WageLedger to wage_ledgers.
func (WageLedger) TableName() string { return "wage_ledgers" }

// AttendanceBook is the GORM model for attendance_books (出勤簿).
type AttendanceBook struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID         uuid.UUID  `gorm:"column:employee_id"`
	PeriodMonth        string     `gorm:"column:period_month"`
	BookJSON           []byte     `gorm:"column:book_json;type:jsonb"`
	RetentionBasis     string     `gorm:"column:retention_basis"`
	RetentionBasisDate *time.Time `gorm:"column:retention_basis_date"`
	RetentionUntil     *time.Time `gorm:"column:retention_until"`
	FinalisedAt        *time.Time `gorm:"column:finalized_at"` //nolint:misspell // DB column name finalized_at is the established schema contract
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps AttendanceBook to attendance_books.
func (AttendanceBook) TableName() string { return "attendance_books" }
