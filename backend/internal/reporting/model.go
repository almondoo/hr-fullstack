// Package reporting implements ST-FND-11: standard reports / CSV/xlsx export
// and the company-calendar / work-pattern (shift) masters.
//
// Two subsystems:
//
//   - Reporting / export: report_definitions activate and column-select the
//     code-defined standard reports (employee roster, attendance monthly, leave
//     status, billing/seat summary).  export_jobs model asynchronous CSV/xlsx
//     export jobs whose generated file is stored (encrypted) in the ST-FND-10
//     document store and fetched via an opaque document UUID.  Sensitive PII
//     columns (マイナンバー/口座/健診) are excluded by default and require the
//     export_sensitive elevated permission.
//
//   - Calendar / work-pattern masters: company_calendars + calendar_days drive
//     business-day calculation; work_patterns + shift_patterns +
//     employee_work_assignments model work systems (固定/フレックス/変形/裁量/
//     シフト) with effective periods resolved by application date.
//
// LEGAL / CONFIG NOTE: holidays, prescribed/statutory holidays, prescribed and
// statutory working hours, variable-labour settlement periods, flex core-time,
// and sensitive-PII export thresholds are all CONFIGURED (calendar_days /
// default_weekly_holidays_json / work_patterns.settings_json /
// report_definitions.columns_json), never hard-coded.  Statutory limits must be
// kept consistent with the attendance subsystem (LM-032/033).  Values depend on
// law (祝日法 etc.) and company work rules and require confirmation by a licensed
// 社労士/弁護士; this implementation is not legal advice and must be updated as
// the law changes.
package reporting

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Report key / format / status / day_type / pattern_type constants
// ---------------------------------------------------------------------------

const (
	// ReportEmployeeRoster lists employees (社員番号/氏名/部署/在籍状況/入社日).
	ReportEmployeeRoster = "employee_roster"
	// ReportAttendanceMonthly aggregates attendance per employee for a month.
	ReportAttendanceMonthly = "attendance_monthly"
	// ReportLeaveStatus summarises leave usage per employee (取得状況/5日義務).
	ReportLeaveStatus = "leave_status"
	// ReportBillingSummary reports seat count / plan (席数・課金サマリ).
	ReportBillingSummary = "billing_summary"
)

const (
	// FormatCSV is a UTF-8 (BOM-prefixed) comma-separated export.
	FormatCSV = "csv"
	// FormatXLSX is an Excel-compatible export.  Implemented in this slice as a
	// stdlib-only SpreadsheetML representation (no third-party xlsx library).
	FormatXLSX = "xlsx"
)

const (
	// JobPending is the initial export-job state.
	JobPending = "pending"
	// JobRunning indicates generation is in progress.
	JobRunning = "running"
	// JobCompleted indicates the file was generated and stored.
	JobCompleted = "completed"
	// JobFailed indicates generation failed (error_message holds a non-PII reason).
	JobFailed = "failed"
)

const (
	// DayTypeHoliday marks a holiday (祝日) overriding the weekday pattern.
	DayTypeHoliday = "holiday"
	// DayTypeBusinessDay marks a special business day overriding a weekend/holiday.
	DayTypeBusinessDay = "business_day"
	// DayTypeSpecialHoliday marks a special company closure (特別休業日).
	DayTypeSpecialHoliday = "special_holiday"
)

const (
	// PatternFixed is a fixed working-hours pattern.
	PatternFixed = "fixed"
	// PatternFlex is a flex-time pattern (with optional core time).
	PatternFlex = "flex"
	// PatternVariable is a variable working-hours pattern (変形労働).
	PatternVariable = "variable"
	// PatternDiscretionary is a discretionary-labour pattern (裁量労働).
	PatternDiscretionary = "discretionary"
	// PatternShift is a shift-based pattern (has shift_patterns).
	PatternShift = "shift"
)

// ---------------------------------------------------------------------------
// GORM models
// ---------------------------------------------------------------------------

// ReportDefinition is the GORM model for report_definitions.
type ReportDefinition struct {
	ID               uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID `gorm:"column:tenant_id"`
	ReportKey        string    `gorm:"column:report_key"`
	Name             string    `gorm:"column:name"`
	ParamsSchemaJSON []byte    `gorm:"column:params_schema_json;type:jsonb"`
	ColumnsJSON      []byte    `gorm:"column:columns_json;type:jsonb"`
	Active           bool      `gorm:"column:active"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// TableName maps ReportDefinition to report_definitions.
func (ReportDefinition) TableName() string { return "report_definitions" }

// ExportJob is the GORM model for export_jobs.
//
// ResultDocumentID is a logical reference to the ST-FND-10 documents store and
// is never an FK.  RequestedByUserID is a logical reference to users.id.
type ExportJob struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	ReportKey         string     `gorm:"column:report_key"`
	Format            string     `gorm:"column:format"`
	ParamsJSON        []byte     `gorm:"column:params_json;type:jsonb"`
	Status            string     `gorm:"column:status"`
	RequestedByUserID *uuid.UUID `gorm:"column:requested_by_user_id"`
	ResultDocumentID  *uuid.UUID `gorm:"column:result_document_id"`
	IncludeSensitive  bool       `gorm:"column:include_sensitive"`
	ErrorMessage      *string    `gorm:"column:error_message"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
	CompletedAt       *time.Time `gorm:"column:completed_at"`
}

// TableName maps ExportJob to export_jobs.
func (ExportJob) TableName() string { return "export_jobs" }

// CompanyCalendar is the GORM model for company_calendars.
type CompanyCalendar struct {
	ID                        uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID                  uuid.UUID  `gorm:"column:tenant_id"`
	Name                      string     `gorm:"column:name"`
	FiscalYear                int        `gorm:"column:fiscal_year"`
	DefaultWeeklyHolidaysJSON []byte     `gorm:"column:default_weekly_holidays_json;type:jsonb"`
	Active                    bool       `gorm:"column:active"`
	EffectiveFrom             time.Time  `gorm:"column:effective_from"`
	EffectiveTo               *time.Time `gorm:"column:effective_to"`
	CreatedAt                 time.Time  `gorm:"column:created_at"`
	UpdatedAt                 time.Time  `gorm:"column:updated_at"`
}

// TableName maps CompanyCalendar to company_calendars.
func (CompanyCalendar) TableName() string { return "company_calendars" }

// CalendarDay is the GORM model for calendar_days.
type CalendarDay struct {
	ID         uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID   uuid.UUID `gorm:"column:tenant_id"`
	CalendarID uuid.UUID `gorm:"column:calendar_id"`
	Date       time.Time `gorm:"column:date"`
	DayType    string    `gorm:"column:day_type"`
	Label      string    `gorm:"column:label"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName maps CalendarDay to calendar_days.
func (CalendarDay) TableName() string { return "calendar_days" }

// WorkPattern is the GORM model for work_patterns.
type WorkPattern struct {
	ID               uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id"`
	Name             string     `gorm:"column:name"`
	PatternType      string     `gorm:"column:pattern_type"`
	ScheduledMinutes int        `gorm:"column:scheduled_minutes"`
	BreakMinutes     int        `gorm:"column:break_minutes"`
	CoreTimeJSON     []byte     `gorm:"column:core_time_json;type:jsonb"`
	SettingsJSON     []byte     `gorm:"column:settings_json;type:jsonb"`
	EffectiveFrom    time.Time  `gorm:"column:effective_from"`
	EffectiveTo      *time.Time `gorm:"column:effective_to"`
	Active           bool       `gorm:"column:active"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName maps WorkPattern to work_patterns.
func (WorkPattern) TableName() string { return "work_patterns" }

// ShiftPattern is the GORM model for shift_patterns.
type ShiftPattern struct {
	ID               uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID `gorm:"column:tenant_id"`
	WorkPatternID    uuid.UUID `gorm:"column:work_pattern_id"`
	Name             string    `gorm:"column:name"`
	StartTime        string    `gorm:"column:start_time"`
	EndTime          string    `gorm:"column:end_time"`
	BreakMinutes     int       `gorm:"column:break_minutes"`
	ScheduledMinutes int       `gorm:"column:scheduled_minutes"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// TableName maps ShiftPattern to shift_patterns.
func (ShiftPattern) TableName() string { return "shift_patterns" }

// EmployeeWorkAssignment is the GORM model for employee_work_assignments.
type EmployeeWorkAssignment struct {
	ID            uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID      uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID    uuid.UUID  `gorm:"column:employee_id"`
	WorkPatternID uuid.UUID  `gorm:"column:work_pattern_id"`
	EffectiveFrom time.Time  `gorm:"column:effective_from"`
	EffectiveTo   *time.Time `gorm:"column:effective_to"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

// TableName maps EmployeeWorkAssignment to employee_work_assignments.
func (EmployeeWorkAssignment) TableName() string { return "employee_work_assignments" }

// ---------------------------------------------------------------------------
// Value objects
// ---------------------------------------------------------------------------

// ReportColumn describes a single column of a report definition.
// Columns flagged Sensitive (マイナンバー/口座/健診 等) are excluded by default
// and only emitted when the requester holds the reporting:export_sensitive
// permission.
type ReportColumn struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Sensitive bool   `json:"sensitive"`
}

// weeklyHolidays is the decoded shape of default_weekly_holidays_json.
// Weekday integers use time.Weekday semantics (0=Sunday .. 6=Saturday).
type weeklyHolidays struct {
	Weekdays []int `json:"weekdays"`
}

// ReportResult is a generic tabular report payload: a header row plus data rows.
// Values are stringified for stable CSV/xlsx serialisation.
type ReportResult struct {
	ReportKey string     `json:"report_key"`
	Columns   []string   `json:"columns"`
	Rows      [][]string `json:"rows"`
}
