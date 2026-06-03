// Package leave implements the leave (休暇) domain covering annual paid leave
// (年次有給休暇, 労基法第39条), grant management, balance tracking, five-day
// obligation monitoring, and various leave-type request/approval flows.
//
// LEGAL NOTICE: All grant-table values, proportional-grant tables, five-day
// obligation thresholds, and expiry durations are read from per-tenant
// leave_settings and are NOT hard-coded.  Default values in migration 00007
// are based on Japanese Labour Standards Law as of 2026-06-02 and MUST be
// reviewed by a qualified labour-law professional (社会保険労務士 / 弁護士)
// and kept current with statutory amendments.  Nothing in this package
// constitutes legal advice.
package leave

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// leave_type constants
// ---------------------------------------------------------------------------

// Leave type values stored in the leave_type column.
const (
	LeaveTypeAnnual     = "annual"     // 年次有給休暇
	LeaveTypeSpecial    = "special"    // 特別休暇
	LeaveTypeCondolence = "condolence" // 慶弔休暇
	LeaveTypeMaternity  = "maternity"  // 産前産後休業 (参照枠組み; 詳細改正はFast-Follow)
	LeaveTypeChildcare  = "childcare"  // 育児休業
	LeaveTypeCare       = "care"       // 介護休業
	LeaveTypeAbsence    = "absence"    // 欠勤
)

// ---------------------------------------------------------------------------
// leave_request status constants
// ---------------------------------------------------------------------------

// Request status values stored in the status column.
const (
	RequestStatusPending   = "pending"
	RequestStatusApproved  = "approved"
	RequestStatusRejected  = "rejected"
	RequestStatusCancelled = "cancelled"
)

// ---------------------------------------------------------------------------
// leave_grant source constants
// ---------------------------------------------------------------------------

// Grant source values stored in the source column.
const (
	GrantSourceAnnual       = "annual_grant"
	GrantSourceProportional = "proportional_grant"
	GrantSourceCarryOver    = "carry_over"
	GrantSourceManual       = "manual"
)

// ---------------------------------------------------------------------------
// GORM models
// ---------------------------------------------------------------------------

// Setting is the GORM model for leave_settings.
// One row per tenant; all configurable leave-law thresholds live here.
//
// LEGAL NOTICE: Default values reflect statutory defaults as of 2026-06-02.
// They MUST be reviewed with each amendment by a qualified professional.
type Setting struct {
	ID                         uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID                   uuid.UUID `gorm:"column:tenant_id"`
	BaseDateRule               string    `gorm:"column:base_date_rule"`
	GrantTableJSON             []byte    `gorm:"column:grant_table_json;type:jsonb"`
	ProportionalTableJSON      []byte    `gorm:"column:proportional_table_json;type:jsonb"`
	FiveDayObligationThreshold int       `gorm:"column:five_day_obligation_threshold"`
	ExpiryMonths               int       `gorm:"column:expiry_months"`
	UpdatedAt                  time.Time `gorm:"column:updated_at"`
}

// TableName maps Setting to leave_settings.
func (Setting) TableName() string { return "leave_settings" }

// Grant is the GORM model for leave_grants.
// Each row represents one grant event (fresh, carry-over, or proportional).
type Grant struct {
	ID         uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID   uuid.UUID `gorm:"column:tenant_id"`
	EmployeeID uuid.UUID `gorm:"column:employee_id"`
	GrantDate  time.Time `gorm:"column:grant_date"`
	Days       float64   `gorm:"column:days"`
	Source     string    `gorm:"column:source"`
	ExpiresOn  time.Time `gorm:"column:expires_on"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

// TableName maps Grant to leave_grants.
func (Grant) TableName() string { return "leave_grants" }

// Request is the GORM model for leave_requests.
type Request struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID        uuid.UUID  `gorm:"column:employee_id"`
	LeaveType         string     `gorm:"column:leave_type"`
	StartDate         time.Time  `gorm:"column:start_date"`
	EndDate           time.Time  `gorm:"column:end_date"`
	Days              float64    `gorm:"column:days"`
	Status            string     `gorm:"column:status"`
	ApprovalRequestID *uuid.UUID `gorm:"column:approval_request_id"`
	Reason            *string    `gorm:"column:reason"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps Request to leave_requests.
func (Request) TableName() string { return "leave_requests" }

// Usage is the GORM model for leave_usages.
type Usage struct {
	ID             uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID `gorm:"column:tenant_id"`
	LeaveRequestID uuid.UUID `gorm:"column:leave_request_id"`
	LeaveGrantID   uuid.UUID `gorm:"column:leave_grant_id"`
	DaysUsed       float64   `gorm:"column:days_used"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

// TableName maps Usage to leave_usages.
func (Usage) TableName() string { return "leave_usages" }

// ---------------------------------------------------------------------------
// Decoded JSONB value types (not persisted directly)
// ---------------------------------------------------------------------------

// GrantEntry is one row in grant_table_json.
// TenureMonthsMax nil means "no upper bound" (≥ TenureMonthsMin).
//
// LEGAL NOTICE: values MUST be verified against current Labour Standards Law
// by a qualified professional before use.
type GrantEntry struct {
	TenureMonthsMin int     `json:"tenure_months_min"`
	TenureMonthsMax *int    `json:"tenure_months_max"`
	GrantDays       float64 `json:"grant_days"`
}

// ProportionalEntry is one row inside a ProportionalGroup.entries.
type ProportionalEntry struct {
	TenureMonthsMin int     `json:"tenure_months_min"`
	TenureMonthsMax *int    `json:"tenure_months_max"`
	GrantDays       float64 `json:"grant_days"`
}

// ProportionalGroup groups proportional grant entries by weekly_days.
type ProportionalGroup struct {
	WeeklyDays float64             `json:"weekly_days"`
	Entries    []ProportionalEntry `json:"entries"`
}

// ---------------------------------------------------------------------------
// Value objects (service layer)
// ---------------------------------------------------------------------------

// Balance holds the computed leave balance for one employee as of a reference date.
type Balance struct {
	EmployeeID uuid.UUID
	// TotalGranted is the sum of all non-expired grants as of AsOf.
	TotalGranted float64
	// TotalUsed is the sum of approved-request days against non-expired grants.
	TotalUsed float64
	// Remaining is TotalGranted - TotalUsed (non-negative).
	Remaining float64
	// UsedThisYear is approved days in the current grant year (for 5-day obligation).
	UsedThisYear float64
	// Grants lists all currently active (non-expired) grants.
	Grants []Grant
	// AsOf is the reference date for the balance computation.
	AsOf time.Time
}

// FiveDayObligation holds the 5-day obligation status for one employee.
//
// LEGAL NOTICE: The obligation (年5日取得義務) applies to employees granted
// 10 or more annual leave days in a year.  The threshold is read from
// leave_settings.five_day_obligation_threshold and MUST be verified by a
// qualified professional.  Nothing here constitutes legal advice.
type FiveDayObligation struct {
	EmployeeID uuid.UUID
	// GrantYearStart is the start of the current obligation year.
	GrantYearStart time.Time
	// GrantYearEnd is the end of the obligation year (GrantYearStart + 1y - 1d).
	GrantYearEnd time.Time
	// GrantDays is the number of days granted this year.
	GrantDays float64
	// UsedDays is the number of days taken this grant year.
	UsedDays float64
	// Obligated reports whether the employee is subject to the five-day rule.
	Obligated bool
	// Met reports whether the obligation is already satisfied.
	Met bool
	// ShortfallDays is the remaining days the employer must ensure are taken
	// (max(0, 5 - UsedDays)) when Obligated is true; 0 when not Obligated or Met.
	//
	// LEGAL NOTICE: The "5 days" minimum is from leave_settings; verify with amendments.
	ShortfallDays float64
}
