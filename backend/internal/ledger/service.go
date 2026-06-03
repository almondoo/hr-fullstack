package ledger

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("ledger: not found")
	ErrInvalidTransition = errors.New("ledger: invalid transition")
	ErrForbidden         = errors.New("ledger: permission denied")
	ErrFinalised         = errors.New("ledger: record is finalised and immutable")
	ErrInvalidLedger     = errors.New("ledger: unknown ledger type")
)

// Ledger type identifiers used by finalise/export dispatch.
const (
	TypeWorkerRoster   = "worker_roster"
	TypeWageLedger     = "wage_ledger"
	TypeAttendanceBook = "attendance_book"
)

// ---------------------------------------------------------------------------
// Payroll-SaaS adapter abstraction (LM-050 / INT-002)
// ---------------------------------------------------------------------------

// PayrollData is the normalised wage data returned by a payroll-SaaS adapter.
// It contains only references and wage amounts — never card numbers, PANs, or
// raw provider tokens.  ProviderRef is an opaque provider-side reference.
type PayrollData struct {
	Provider    string `json:"provider"`
	Period      string `json:"period"`
	ProviderRef string `json:"provider_ref"`
	// WageJSON is the normalised statutory wage items
	// (基本給/手当/割増/控除 等) ready to store into wage_ledgers.wage_json.
	WageJSON json.RawMessage `json:"wage_json"`
}

// PayrollImporter abstracts a payroll-SaaS provider (moneyforward/freee/yayoi).
// MVP ships a mock implementation; real integrations are P3.
type PayrollImporter interface {
	// Provider returns the provider identifier (must match a payroll_links CHECK value).
	Provider() string
	// Fetch returns the normalised wage data for an employee and period.
	// Implementations must NOT return any card number / PAN / raw token.
	Fetch(ctx context.Context, tenantID, employeeID uuid.UUID, period string) (*PayrollData, error)
}

// MockPayrollImporter is a deterministic in-memory importer for MVP/testing.
// It synthesises normalised wage data; no external call is made.
type MockPayrollImporter struct {
	provider string
	// data optionally overrides the synthesised payload, keyed by period.
	data map[string]json.RawMessage
}

// NewMockPayrollImporter constructs a mock importer for the given provider.
// When provider is empty it defaults to ProviderMock.
func NewMockPayrollImporter(provider string) *MockPayrollImporter {
	if provider == "" {
		provider = ProviderMock
	}
	return &MockPayrollImporter{provider: provider, data: map[string]json.RawMessage{}}
}

// SetPayload registers a synthetic wage payload for a period (test seam).
func (m *MockPayrollImporter) SetPayload(period string, payload json.RawMessage) {
	m.data[period] = payload
}

// Provider returns the mock provider identifier.
func (m *MockPayrollImporter) Provider() string { return m.provider }

// Fetch returns a deterministic synthetic payload for the period.
func (m *MockPayrollImporter) Fetch(_ context.Context, _, _ uuid.UUID, period string) (*PayrollData, error) {
	payload, ok := m.data[period]
	if !ok {
		// Deterministic synthetic wage data (合成データ; not real wages).
		payload = json.RawMessage(`{"base_pay":0,"allowances":0,"overtime":0,"deductions":0}`)
	}
	return &PayrollData{
		Provider:    m.provider,
		Period:      period,
		ProviderRef: "mock-" + period,
		WageJSON:    payload,
	}, nil
}

// Service provides business logic for the statutory three-ledger domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// addYears returns basis + years, clamped to a valid calendar date.
// Used to compute 保存満了日 = 起算日 + retention_years.
func addYears(basis time.Time, years int) time.Time {
	return basis.AddDate(years, 0, 0)
}

// jsonbArg coerces an optional JSON byte slice to a non-empty default so the
// ?::jsonb cast never receives an empty string.
func jsonbArg(raw []byte, fallback string) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte(fallback)
	}
	return raw
}

// loadSettingsTx reads the tenant's ledger settings within an open tx, applying
// in-memory defaults when no row exists.  Retention values are NEVER hardcoded
// in business logic; they originate from this (configurable) settings row.
func (s *Service) loadSettingsTx(tx *gorm.DB, tenantID uuid.UUID) (Settings, error) {
	var st Settings
	if err := tx.Raw(
		`SELECT id, tenant_id, default_retention_years, default_retention_basis,
		        electronic_storage_json, created_at, updated_at
		 FROM ledger_settings
		 WHERE tenant_id = ? LIMIT 1`,
		tenantID,
	).Scan(&st).Error; err != nil {
		return Settings{}, fmt.Errorf("ledger: load settings: %w", err)
	}
	if st.ID == uuid.Nil {
		// No settings row yet: fall back to the schema defaults (原則5年 / 退職日).
		// These mirror the DB column defaults and remain configurable per tenant.
		st = Settings{
			TenantID:              tenantID,
			DefaultRetentionYears: 5,
			DefaultRetentionBasis: BasisResignation,
			ElectronicStorageJSON: []byte(`{}`),
		}
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// UpsertSettingsInput holds fields for configuring per-tenant ledger settings.
type UpsertSettingsInput struct {
	TenantID              uuid.UUID
	ActorID               uuid.UUID
	RetentionYears        int
	RetentionBasis        string
	ElectronicStorageJSON []byte
	IP                    *string
}

// UpsertSettings creates or updates the tenant's ledger settings.
// LEGAL: retention years / basis / electronic-storage policy are configurable
// here so the system follows amendments (改正・経過措置); they are not hardcoded.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*Settings, error) {
	var st Settings
	years := in.RetentionYears
	if years <= 0 {
		years = 5
	}
	basis := in.RetentionBasis
	if basis == "" {
		basis = BasisResignation
	}
	storage := jsonbArg(in.ElectronicStorageJSON, "{}")

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO ledger_settings
			   (id, tenant_id, default_retention_years, default_retention_basis, electronic_storage_json)
			 VALUES (?, ?, ?, ?, ?::jsonb)
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET default_retention_years = EXCLUDED.default_retention_years,
			       default_retention_basis = EXCLUDED.default_retention_basis,
			       electronic_storage_json = EXCLUDED.electronic_storage_json,
			       updated_at              = now()`,
			id, in.TenantID, years, basis, storage,
		).Error; err != nil {
			return fmt.Errorf("ledger: upsert settings: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, default_retention_years, default_retention_basis,
			        electronic_storage_json, created_at, updated_at
			 FROM ledger_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&st).Error; err != nil {
			return fmt.Errorf("ledger: upsert settings re-read: %w", err)
		}

		idStr := st.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "ledger_settings.updated",
			ResourceType: "ledger_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// GetSettings returns the tenant's ledger settings (or defaults when unset).
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*Settings, error) {
	var st Settings
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var loadErr error
		st, loadErr = s.loadSettingsTx(tx, tenantID)
		return loadErr
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ---------------------------------------------------------------------------
// Payroll-SaaS import (LM-050 / INT-002)
// ---------------------------------------------------------------------------

// ImportPayrollInput holds fields for importing wage data from a payroll SaaS.
type ImportPayrollInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	Period     string
	IP         *string
}

// ImportPayroll fetches normalised wage data via the importer and upserts a
// payroll_links row.  Idempotent on (employee, provider, period): re-importing
// the same period updates the existing link rather than duplicating it.
func (s *Service) ImportPayroll(ctx context.Context, importer PayrollImporter, in ImportPayrollInput) (*PayrollLink, error) {
	data, err := importer.Fetch(ctx, in.TenantID, in.EmployeeID, in.Period)
	if err != nil {
		return nil, fmt.Errorf("ledger: payroll fetch: %w", err)
	}

	var link PayrollLink
	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant (defence-in-depth).
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("ledger: import verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		id := uuid.New()
		payload := jsonbArg(data.WageJSON, "{}")
		if err := tx.Exec(
			`INSERT INTO payroll_links
			   (id, tenant_id, employee_id, provider, period, provider_ref, imported_payload_json, status, imported_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, 'imported', now())
			 ON CONFLICT (employee_id, tenant_id, provider, period) DO UPDATE
			   SET provider_ref          = EXCLUDED.provider_ref,
			       imported_payload_json = EXCLUDED.imported_payload_json,
			       imported_at           = now(),
			       updated_at            = now()`,
			id, in.TenantID, in.EmployeeID, data.Provider, in.Period, data.ProviderRef, payload,
		).Error; err != nil {
			return fmt.Errorf("ledger: import payroll insert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, provider, period, provider_ref,
			        imported_payload_json, status, imported_at, created_at, updated_at
			 FROM payroll_links
			 WHERE employee_id = ? AND tenant_id = ? AND provider = ? AND period = ? LIMIT 1`,
			in.EmployeeID, in.TenantID, data.Provider, in.Period,
		).Scan(&link).Error; err != nil {
			return fmt.Errorf("ledger: import payroll re-read: %w", err)
		}

		idStr := link.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "payroll_link.imported",
			ResourceType: "payroll_link",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &link, nil
}

// ---------------------------------------------------------------------------
// 労働者名簿 builder (LM-054)
// ---------------------------------------------------------------------------

// BuildRosterInput holds fields for building a worker roster.
type BuildRosterInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	// ResignationDate is the 退職日 used as the retention 起算日 when the
	// tenant's default basis is 'resignation'.  Optional (NULL until known).
	ResignationDate *time.Time
	IP              *string
}

// BuildWorkerRoster assembles a worker roster (労働者名簿) for an employee from
// employees / employee_assignments (発令履歴) / employment_contracts.  It computes
// retention_until from the tenant settings and the retention basis date.
// Upsert on (employee, tenant): rebuilding refreshes a non-finalised roster.
func (s *Service) BuildWorkerRoster(ctx context.Context, in BuildRosterInput) (*WorkerRoster, error) {
	var roster WorkerRoster
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Block rebuild of a finalised roster (真実性).
		var existing struct {
			Finalised *time.Time `gorm:"column:finalized_at"` //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
		}
		if err := tx.Raw(
			`SELECT finalized_at FROM worker_rosters WHERE employee_id = ? AND tenant_id = ? LIMIT 1`, //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
			in.EmployeeID, in.TenantID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("ledger: roster finalised check: %w", err)
		}
		if existing.Finalised != nil {
			return ErrFinalised
		}

		// Read the employee master.
		var emp struct {
			EmployeeCode   string     `gorm:"column:employee_code"`
			LastName       string     `gorm:"column:last_name"`
			FirstName      string     `gorm:"column:first_name"`
			Status         string     `gorm:"column:status"`
			EmploymentType string     `gorm:"column:employment_type"`
			HiredOn        *time.Time `gorm:"column:hired_on"`
		}
		if err := tx.Raw(
			`SELECT employee_code, last_name, first_name, status, employment_type, hired_on
			 FROM employees WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&emp).Error; err != nil {
			return fmt.Errorf("ledger: roster read employee: %w", err)
		}
		if emp.EmployeeCode == "" {
			return ErrNotFound
		}

		// Read 発令履歴 (assignments) — 従事業務/組織の履歴.
		var assignments []struct {
			DepartmentID  *uuid.UUID `gorm:"column:department_id"`
			Position      *string    `gorm:"column:position"`
			Grade         *string    `gorm:"column:grade"`
			EffectiveFrom time.Time  `gorm:"column:effective_from"`
			EffectiveTo   *time.Time `gorm:"column:effective_to"`
			Reason        *string    `gorm:"column:reason"`
		}
		if err := tx.Raw(
			`SELECT department_id, position, grade, effective_from, effective_to, reason
			 FROM employee_assignments
			 WHERE employee_id = ? AND tenant_id = ?
			 ORDER BY effective_from`,
			in.EmployeeID, in.TenantID,
		).Scan(&assignments).Error; err != nil {
			return fmt.Errorf("ledger: roster read assignments: %w", err)
		}

		// Read 雇用契約 (contracts) — 雇入/契約情報.
		var contracts []struct {
			ContractType string     `gorm:"column:contract_type"`
			StartDate    time.Time  `gorm:"column:start_date"`
			EndDate      *time.Time `gorm:"column:end_date"`
			Status       string     `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT contract_type, start_date, end_date, status
			 FROM employment_contracts
			 WHERE employee_id = ? AND tenant_id = ?
			 ORDER BY start_date`,
			in.EmployeeID, in.TenantID,
		).Scan(&contracts).Error; err != nil {
			return fmt.Errorf("ledger: roster read contracts: %w", err)
		}

		// Assemble the statutory roster_json (法定記載事項).
		rosterJSON, err := json.Marshal(map[string]any{
			"employee_code":   emp.EmployeeCode,
			"name":            emp.LastName + " " + emp.FirstName,
			"status":          emp.Status,
			"employment_type": emp.EmploymentType,
			"hired_on":        dateOrNil(emp.HiredOn),
			"assignments":     assignments, // 従事業務・組織の履歴
			"contracts":       contracts,   // 雇入/契約情報
		})
		if err != nil {
			return fmt.Errorf("ledger: roster marshal: %w", err)
		}

		// Compute retention from configurable settings (NOT hardcoded).
		st, err := s.loadSettingsTx(tx, in.TenantID)
		if err != nil {
			return err
		}
		basisDate := in.ResignationDate
		var retentionUntil *time.Time
		if basisDate != nil {
			u := addYears(*basisDate, st.DefaultRetentionYears)
			retentionUntil = &u
		}

		id := uuid.New()
		// The trailing `WHERE worker_rosters.finalized_at IS NULL` makes the
		// immutability guard part of the write itself (真実性), closing the
		// TOCTOU race between the SELECT above and this UPDATE: under READ
		// COMMITTED a concurrent Finalise that commits in between would now
		// leave this conflict path with RowsAffected==0 instead of silently
		// rewriting the finalised row.  A DB BEFORE UPDATE trigger enforces the
		// same invariant as a hard backstop.
		res := tx.Exec( //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
			`INSERT INTO worker_rosters
			   (id, tenant_id, employee_id, roster_json, retention_basis, retention_basis_date, retention_until)
			 VALUES (?, ?, ?, ?::jsonb, ?, ?, ?)
			 ON CONFLICT (employee_id, tenant_id) DO UPDATE
			   SET roster_json          = EXCLUDED.roster_json,
			       retention_basis      = EXCLUDED.retention_basis,
			       retention_basis_date = EXCLUDED.retention_basis_date,
			       retention_until      = EXCLUDED.retention_until,
			       updated_at           = now()
			 WHERE worker_rosters.finalized_at IS NULL`,
			id, in.TenantID, in.EmployeeID, rosterJSON, st.DefaultRetentionBasis, basisDate, retentionUntil,
		)
		if res.Error != nil {
			return fmt.Errorf("ledger: roster upsert: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// The only way an INSERT ... ON CONFLICT DO UPDATE affects zero rows
			// is when the conflict target exists but the DO UPDATE WHERE filtered
			// it out — i.e. the row was finalised by a racing tx.
			return ErrFinalised
		}

		if err := tx.Raw(
			"SELECT id, tenant_id, employee_id, roster_json, retention_basis,"+
				" retention_basis_date, retention_until, finalized_at, created_at, updated_at"+ //nolint:misspell // DB column contract
				" FROM worker_rosters WHERE employee_id = ? AND tenant_id = ? LIMIT 1",
			in.EmployeeID, in.TenantID,
		).Scan(&roster).Error; err != nil {
			return fmt.Errorf("ledger: roster re-read: %w", err)
		}

		idStr := roster.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "worker_roster.built",
			ResourceType: "worker_roster",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &roster, nil
}

// ---------------------------------------------------------------------------
// 出勤簿 builder (LM-054 / LM-031〜033)
// ---------------------------------------------------------------------------

// BuildAttendanceBookInput holds fields for building an attendance book.
type BuildAttendanceBookInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	// PeriodMonth is the target month, e.g. "2026-06".
	PeriodMonth string
	// LastAttendanceDate is the 最終出勤日 retention 起算日 (optional).
	LastAttendanceDate *time.Time
	IP                 *string
}

// BuildAttendanceBook assembles an attendance book (出勤簿) for an employee and
// month from work_summaries (monthly aggregate) and attendance_records (daily
// clock data: 労働日数/始業終業/休憩).  Computes retention_until from settings.
func (s *Service) BuildAttendanceBook(ctx context.Context, in BuildAttendanceBookInput) (*AttendanceBook, error) {
	var book AttendanceBook
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var existing struct {
			Finalised *time.Time `gorm:"column:finalized_at"` //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
		}
		if err := tx.Raw(
			`SELECT finalized_at FROM attendance_books WHERE employee_id = ? AND tenant_id = ? AND period_month = ? LIMIT 1`, //nolint:misspell // DB column contract
			in.EmployeeID, in.TenantID, in.PeriodMonth,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("ledger: book finalised check: %w", err)
		}
		if existing.Finalised != nil {
			return ErrFinalised
		}

		// Verify employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("ledger: book verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Monthly aggregate (work_summaries.period_month is the 1st of the month).
		monthStart := in.PeriodMonth + "-01"
		var summary struct {
			ScheduledMinutes int `gorm:"column:scheduled_minutes"`
			ActualMinutes    int `gorm:"column:actual_minutes"`
			OvertimeMinutes  int `gorm:"column:overtime_minutes"`
			NightMinutes     int `gorm:"column:night_minutes"`
			HolidayMinutes   int `gorm:"column:holiday_minutes"`
			Over60Minutes    int `gorm:"column:over60_minutes"`
		}
		if err := tx.Raw(
			`SELECT scheduled_minutes, actual_minutes, overtime_minutes,
			        night_minutes, holiday_minutes, over60_minutes
			 FROM work_summaries
			 WHERE employee_id = ? AND tenant_id = ? AND period_month = ?::date LIMIT 1`,
			in.EmployeeID, in.TenantID, monthStart,
		).Scan(&summary).Error; err != nil {
			return fmt.Errorf("ledger: book read summary: %w", err)
		}

		// Daily records for 労働日数 and 始業終業/休憩 detail.
		var days []struct {
			WorkDate     time.Time  `gorm:"column:work_date"`
			ClockIn      *time.Time `gorm:"column:clock_in"`
			ClockOut     *time.Time `gorm:"column:clock_out"`
			BreakMinutes int        `gorm:"column:break_minutes"`
		}
		if err := tx.Raw(
			`SELECT work_date, clock_in, clock_out, break_minutes
			 FROM attendance_records
			 WHERE employee_id = ? AND tenant_id = ?
			   AND to_char(work_date, 'YYYY-MM') = ?
			   AND clock_in IS NOT NULL
			 ORDER BY work_date`,
			in.EmployeeID, in.TenantID, in.PeriodMonth,
		).Scan(&days).Error; err != nil {
			return fmt.Errorf("ledger: book read records: %w", err)
		}

		bookJSON, err := json.Marshal(map[string]any{
			"period_month":      in.PeriodMonth,
			"work_days":         len(days), // 労働日数
			"scheduled_minutes": summary.ScheduledMinutes,
			"actual_minutes":    summary.ActualMinutes, // 労働時間数
			"overtime_minutes":  summary.OvertimeMinutes,
			"night_minutes":     summary.NightMinutes,
			"holiday_minutes":   summary.HolidayMinutes,
			"over60_minutes":    summary.Over60Minutes,
			"days":              days, // 始業終業・休憩 detail
		})
		if err != nil {
			return fmt.Errorf("ledger: book marshal: %w", err)
		}

		st, err := s.loadSettingsTx(tx, in.TenantID)
		if err != nil {
			return err
		}
		basisDate := in.LastAttendanceDate
		var retentionUntil *time.Time
		if basisDate != nil {
			u := addYears(*basisDate, st.DefaultRetentionYears)
			retentionUntil = &u
		}

		id := uuid.New()
		// Immutability guard on the write itself (真実性): the trailing WHERE
		// closes the TOCTOU race with a concurrent Finalise.  See BuildWorkerRoster.
		res := tx.Exec( //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
			`INSERT INTO attendance_books
			   (id, tenant_id, employee_id, period_month, book_json, retention_basis, retention_basis_date, retention_until)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?, ?, ?)
			 ON CONFLICT (employee_id, tenant_id, period_month) DO UPDATE
			   SET book_json            = EXCLUDED.book_json,
			       retention_basis      = EXCLUDED.retention_basis,
			       retention_basis_date = EXCLUDED.retention_basis_date,
			       retention_until      = EXCLUDED.retention_until,
			       updated_at           = now()
			 WHERE attendance_books.finalized_at IS NULL`,
			id, in.TenantID, in.EmployeeID, in.PeriodMonth, bookJSON, BasisLastAttendance, basisDate, retentionUntil,
		)
		if res.Error != nil {
			return fmt.Errorf("ledger: book upsert: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrFinalised
		}

		if err := tx.Raw(
			"SELECT id, tenant_id, employee_id, period_month, book_json, retention_basis,"+
				" retention_basis_date, retention_until, finalized_at, created_at, updated_at"+ //nolint:misspell // DB column contract
				" FROM attendance_books"+
				" WHERE employee_id = ? AND tenant_id = ? AND period_month = ? LIMIT 1",
			in.EmployeeID, in.TenantID, in.PeriodMonth,
		).Scan(&book).Error; err != nil {
			return fmt.Errorf("ledger: book re-read: %w", err)
		}

		idStr := book.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "attendance_book.built",
			ResourceType: "attendance_book",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &book, nil
}

// ---------------------------------------------------------------------------
// 賃金台帳 builder (LM-054 / LM-050)
// ---------------------------------------------------------------------------

// BuildWageLedgerInput holds fields for building a wage ledger from an import.
type BuildWageLedgerInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	Period     string
	// LastEntryDate is the 最終記入日 retention 起算日 (optional).
	LastEntryDate *time.Time
	IP            *string
}

// BuildWageLedger normalises a payroll_links import into a wage ledger (賃金台帳)
// for the given 賃金計算期間 (period).  The source link is marked consumed.
// Upsert on (employee, period); rebuild blocked once finalised.
func (s *Service) BuildWageLedger(ctx context.Context, in BuildWageLedgerInput) (*WageLedger, error) {
	var ledger WageLedger
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var existing struct {
			Finalised *time.Time `gorm:"column:finalized_at"` //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
		}
		if err := tx.Raw(
			`SELECT finalized_at FROM wage_ledgers WHERE employee_id = ? AND tenant_id = ? AND period = ? LIMIT 1`, //nolint:misspell // DB column contract
			in.EmployeeID, in.TenantID, in.Period,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("ledger: wage finalised check: %w", err)
		}
		if existing.Finalised != nil {
			return ErrFinalised
		}

		// Find the source payroll import for this period.
		var link PayrollLink
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, imported_payload_json
			 FROM payroll_links
			 WHERE employee_id = ? AND tenant_id = ? AND period = ?
			 ORDER BY imported_at DESC LIMIT 1`,
			in.EmployeeID, in.TenantID, in.Period,
		).Scan(&link).Error; err != nil {
			return fmt.Errorf("ledger: wage read payroll link: %w", err)
		}
		if link.ID == uuid.Nil {
			return ErrNotFound
		}

		// Normalise the imported payload into the statutory wage_json.
		wageJSON := jsonbArg(link.ImportedPayloadJSON, "{}")

		st, err := s.loadSettingsTx(tx, in.TenantID)
		if err != nil {
			return err
		}
		basisDate := in.LastEntryDate
		var retentionUntil *time.Time
		if basisDate != nil {
			u := addYears(*basisDate, st.DefaultRetentionYears)
			retentionUntil = &u
		}

		id := uuid.New()
		// Immutability guard on the write itself (真実性): the trailing WHERE
		// closes the TOCTOU race with a concurrent Finalise.  See BuildWorkerRoster.
		res := tx.Exec( //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
			`INSERT INTO wage_ledgers
			   (id, tenant_id, employee_id, period, wage_json, source_payroll_link_id,
			    retention_basis, retention_basis_date, retention_until)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?, ?, ?, ?)
			 ON CONFLICT (employee_id, tenant_id, period) DO UPDATE
			   SET wage_json              = EXCLUDED.wage_json,
			       source_payroll_link_id = EXCLUDED.source_payroll_link_id,
			       retention_basis        = EXCLUDED.retention_basis,
			       retention_basis_date   = EXCLUDED.retention_basis_date,
			       retention_until        = EXCLUDED.retention_until,
			       updated_at             = now()
			 WHERE wage_ledgers.finalized_at IS NULL`,
			id, in.TenantID, in.EmployeeID, in.Period, wageJSON, link.ID,
			BasisLastEntry, basisDate, retentionUntil,
		)
		if res.Error != nil {
			return fmt.Errorf("ledger: wage upsert: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Conflict row exists but is finalised: do not consume the source
			// link, and reject the rebuild.  Returning here rolls back the tx.
			return ErrFinalised
		}

		// Mark the source import as consumed.
		if err := tx.Exec(
			`UPDATE payroll_links SET status = 'consumed', updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			link.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("ledger: wage mark link consumed: %w", err)
		}

		if err := tx.Raw(
			"SELECT id, tenant_id, employee_id, period, wage_json, source_payroll_link_id,"+
				" retention_basis, retention_basis_date, retention_until, finalized_at,"+ //nolint:misspell // DB column contract
				" created_at, updated_at"+
				" FROM wage_ledgers WHERE employee_id = ? AND tenant_id = ? AND period = ? LIMIT 1",
			in.EmployeeID, in.TenantID, in.Period,
		).Scan(&ledger).Error; err != nil {
			return fmt.Errorf("ledger: wage re-read: %w", err)
		}

		idStr := ledger.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "wage_ledger.built",
			ResourceType: "wage_ledger",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &ledger, nil
}

// ---------------------------------------------------------------------------
// Finalise (真実性 / 電子帳簿保存法)
// ---------------------------------------------------------------------------

// FinaliseInput holds fields for finalising a ledger record.
type FinaliseInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	LedgerType string // worker_roster | wage_ledger | attendance_book
	ID         uuid.UUID
	IP         *string
}

// finaliseTable maps a ledger type to its table name for the finalise update.
var finaliseTable = map[string]string{
	TypeWorkerRoster:   "worker_rosters",
	TypeWageLedger:     "wage_ledgers",
	TypeAttendanceBook: "attendance_books",
}

// Finalise sets finalised_at on a ledger record, making it immutable (真実性).
// Re-finalising an already-finalised record is rejected as ErrInvalidTransition.
func (s *Service) Finalise(ctx context.Context, in FinaliseInput) error {
	table, ok := finaliseTable[in.LedgerType]
	if !ok {
		return ErrInvalidLedger
	}
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var existing struct {
			Finalised *time.Time `gorm:"column:finalized_at"` //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
		}
		// Table name is from a fixed allow-list map, never user input.
		if err := tx.Raw( //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
			"SELECT finalized_at FROM "+table+" WHERE id = ? AND tenant_id = ? LIMIT 1", //nolint:misspell // DB column contract
			in.ID, in.TenantID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("ledger: finalise read: %w", err)
		}
		// A NULL finalised_at on a non-existent row is indistinguishable from a
		// row that exists but is not finalised; disambiguate with a COUNT.
		var cnt int64
		if err := tx.Raw(
			"SELECT COUNT(1) FROM "+table+" WHERE id = ? AND tenant_id = ?",
			in.ID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("ledger: finalise count: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}
		if existing.Finalised != nil {
			return fmt.Errorf("%w: already finalised", ErrInvalidTransition)
		}

		res := tx.Exec( //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
			"UPDATE "+table+" SET finalized_at = now(), updated_at = now() WHERE id = ? AND tenant_id = ? AND finalized_at IS NULL", //nolint:misspell // DB column contract
			in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("ledger: finalise update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       in.LedgerType + ".finalised",
			ResourceType: in.LedgerType,
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// ---------------------------------------------------------------------------
// Retention precedence vs offboarding deletion policy (NFR-011)
// ---------------------------------------------------------------------------

// RetentionDecision reports whether a ledger record must be retained because
// its statutory retention period overrides the offboarding deletion policy.
type RetentionDecision struct {
	LedgerType         string
	LedgerID           uuid.UUID
	RetentionUntil     *time.Time
	OffboardingExpires *time.Time
	// MustRetain is true when the ledger must be kept past the offboarding
	// deletion date because legal retention has NOT yet expired.
	MustRetain bool
}

// EvaluateRetentionPrecedence checks, for one employee, whether each of the
// three ledgers must be retained past the offboarding deletion date.
// Statutory legal retention (retention_until) takes precedence over the
// offboarding deletion policy (offboarding_policies.expires_on): if a ledger's
// retention_until is after the policy's expires_on (or retention has no expiry),
// the ledger MUST be retained.  asOf lets callers evaluate at a specific date.
func (s *Service) EvaluateRetentionPrecedence(ctx context.Context, tenantID, employeeID uuid.UUID, asOf time.Time) ([]RetentionDecision, error) {
	var decisions []RetentionDecision
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Read the offboarding deletion policy (logical expiry date).
		// offboarding_policies is owned by the onboarding package; referenced
		// here only for reads (no FK — cross-package logical reference).
		var policy struct {
			ExpiresOn *time.Time
			Found     bool
		}
		var polCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM offboarding_policies WHERE employee_id = ? AND tenant_id = ?`,
			employeeID, tenantID,
		).Scan(&polCount).Error; err != nil {
			return fmt.Errorf("ledger: retention policy count: %w", err)
		}
		if polCount > 0 {
			var polRow struct {
				ExpiresOn *time.Time `gorm:"column:expires_on"`
			}
			if err := tx.Raw(
				`SELECT expires_on FROM offboarding_policies
				 WHERE employee_id = ? AND tenant_id = ? LIMIT 1`,
				employeeID, tenantID,
			).Scan(&polRow).Error; err != nil {
				return fmt.Errorf("ledger: retention policy read: %w", err)
			}
			policy.ExpiresOn = polRow.ExpiresOn
			policy.Found = true
		}

		// Gather each ledger's id + retention_until.
		type ledgerRow struct {
			ID             uuid.UUID  `gorm:"column:id"`
			RetentionUntil *time.Time `gorm:"column:retention_until"`
		}
		queries := []struct {
			typ   string
			table string
		}{
			{TypeWorkerRoster, "worker_rosters"},
			{TypeWageLedger, "wage_ledgers"},
			{TypeAttendanceBook, "attendance_books"},
		}
		for _, q := range queries {
			var rows []ledgerRow
			if err := tx.Raw(
				"SELECT id, retention_until FROM "+q.table+
					" WHERE employee_id = ? AND tenant_id = ?",
				employeeID, tenantID,
			).Scan(&rows).Error; err != nil {
				return fmt.Errorf("ledger: retention read %s: %w", q.table, err)
			}
			for _, r := range rows {
				d := RetentionDecision{
					LedgerType:     q.typ,
					LedgerID:       r.ID,
					RetentionUntil: r.RetentionUntil,
				}
				if policy.Found {
					d.OffboardingExpires = policy.ExpiresOn
				}
				d.MustRetain = mustRetain(r.RetentionUntil, policy.ExpiresOn, asOf)
				decisions = append(decisions, d)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return decisions, nil
}

// mustRetain decides whether legal retention overrides the offboarding deletion.
//   - retentionUntil == nil  → indefinite legal retention → MUST retain.
//   - asOf before retentionUntil → legal retention not expired → MUST retain
//     (this also covers retentionUntil after offboardingExpires).
//   - otherwise legal retention has expired → deletion policy may apply.
func mustRetain(retentionUntil, _ *time.Time, asOf time.Time) bool {
	if retentionUntil == nil {
		return true
	}
	return asOf.Before(*retentionUntil)
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// GetWorkerRoster fetches the roster for an employee.
func (s *Service) GetWorkerRoster(ctx context.Context, tenantID, employeeID uuid.UUID) (*WorkerRoster, error) {
	var roster WorkerRoster
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, employee_id, roster_json, retention_basis,"+
				" retention_basis_date, retention_until, finalized_at, created_at, updated_at"+ //nolint:misspell // DB column contract
				" FROM worker_rosters WHERE employee_id = ? AND tenant_id = ? LIMIT 1",
			employeeID, tenantID,
		).Scan(&roster).Error
	})
	if err != nil {
		return nil, err
	}
	if roster.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &roster, nil
}

// GetWageLedger fetches the wage ledger for an employee and period.
func (s *Service) GetWageLedger(ctx context.Context, tenantID, employeeID uuid.UUID, period string) (*WageLedger, error) {
	var ledger WageLedger
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, employee_id, period, wage_json, source_payroll_link_id,"+
				" retention_basis, retention_basis_date, retention_until, finalized_at,"+ //nolint:misspell // DB column contract
				" created_at, updated_at"+
				" FROM wage_ledgers WHERE employee_id = ? AND tenant_id = ? AND period = ? LIMIT 1",
			employeeID, tenantID, period,
		).Scan(&ledger).Error
	})
	if err != nil {
		return nil, err
	}
	if ledger.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &ledger, nil
}

// GetAttendanceBook fetches the attendance book for an employee and month.
func (s *Service) GetAttendanceBook(ctx context.Context, tenantID, employeeID uuid.UUID, periodMonth string) (*AttendanceBook, error) {
	var book AttendanceBook
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, employee_id, period_month, book_json, retention_basis,"+
				" retention_basis_date, retention_until, finalized_at, created_at, updated_at"+ //nolint:misspell // DB column contract
				" FROM attendance_books WHERE employee_id = ? AND tenant_id = ? AND period_month = ? LIMIT 1",
			employeeID, tenantID, periodMonth,
		).Scan(&book).Error
	})
	if err != nil {
		return nil, err
	}
	if book.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &book, nil
}

// ---------------------------------------------------------------------------
// CSV export (可視性 / 電子帳簿保存法)
// ---------------------------------------------------------------------------

// ExportWageLedgerCSV renders the tenant's wage ledgers for a period as CSV
// (可視性).  Uses encoding/csv only; no external library.  Amounts are taken
// from wage_json (no PAN/token); the writer never emits decrypted PII.
func (s *Service) ExportWageLedgerCSV(ctx context.Context, tenantID uuid.UUID, period string) (string, error) {
	type row struct {
		EmployeeID     uuid.UUID  `gorm:"column:employee_id"`
		Period         string     `gorm:"column:period"`
		WageJSON       []byte     `gorm:"column:wage_json"`
		RetentionUntil *time.Time `gorm:"column:retention_until"`
		FinalisedAt    *time.Time `gorm:"column:finalized_at"` //nolint:misspell // SQL/string refers to finalized_at DB column (schema contract)
	}
	var rows []row
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT employee_id, period, wage_json, retention_until, finalized_at FROM wage_ledgers WHERE tenant_id = ? AND period = ? ORDER BY employee_id`, //nolint:misspell // DB column contract
			tenantID, period,
		).Scan(&rows).Error
	})
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{"employee_id", "period", "wage_json", "retention_until", "finalised"})
	for _, r := range rows {
		wj := string(r.WageJSON)
		if wj == "" {
			wj = "{}"
		}
		retention := ""
		if r.RetentionUntil != nil {
			retention = r.RetentionUntil.Format("2006-01-02")
		}
		finalised := "false"
		if r.FinalisedAt != nil {
			finalised = "true"
		}
		_ = w.Write([]string{r.EmployeeID.String(), r.Period, wj, retention, finalised})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return "", fmt.Errorf("ledger: export csv: %w", err)
	}
	return sb.String(), nil
}

// dateOrNil formats an optional date as YYYY-MM-DD, or nil when absent.
func dateOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format("2006-01-02")
}
