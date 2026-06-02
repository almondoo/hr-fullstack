// Package attendance implements the attendance (勤怠) domain.
//
// LEGAL NOTICE: All time-calculation thresholds, overtime rates, 36-agreement
// limits, and night-work boundaries derive from per-tenant configuration stored
// in attendance_settings / labor_agreements tables. Hard-coded values are NOT
// used for any compliance-sensitive calculation. Default values in migration
// 00005 are based on Japanese Labor Standards Law as of 2026-06-02 and MUST be
// reviewed by a qualified labor-law professional (社会保険労務士/弁護士) and
// kept current with statutory amendments. Nothing in this package constitutes
// legal advice.
package attendance

import (
	"time"

	"github.com/google/uuid"
)

// AttendanceRecord is the GORM model for the attendance_records table.
// It captures the objective time record for one employee on one work date
// (客観的把握 LM-030).
type AttendanceRecord struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID   uuid.UUID  `gorm:"column:employee_id"`
	WorkDate     time.Time  `gorm:"column:work_date"`
	ClockIn      *time.Time `gorm:"column:clock_in"`
	ClockOut     *time.Time `gorm:"column:clock_out"`
	BreakMinutes int        `gorm:"column:break_minutes"`
	Source       string     `gorm:"column:source"`
	IsCorrected  bool       `gorm:"column:is_corrected"`
	Note         *string    `gorm:"column:note"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// TableName maps AttendanceRecord to the attendance_records table.
func (AttendanceRecord) TableName() string { return "attendance_records" }

// AttendanceCorrection is the GORM model for the attendance_corrections table.
// Every modification to an attendance record is recorded here to provide the
// objective-record audit trail required by LM-030.
type AttendanceCorrection struct {
	ID                 uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID `gorm:"column:tenant_id"`
	AttendanceRecordID uuid.UUID `gorm:"column:attendance_record_id"`
	BeforeJSON         []byte    `gorm:"column:before_json;type:jsonb"`
	AfterJSON          []byte    `gorm:"column:after_json;type:jsonb"`
	Reason             string    `gorm:"column:reason"`
	CorrectedByUserID  uuid.UUID `gorm:"column:corrected_by_user_id"`
	CorrectedAt        time.Time `gorm:"column:corrected_at"`
}

// TableName maps AttendanceCorrection to the attendance_corrections table.
func (AttendanceCorrection) TableName() string { return "attendance_corrections" }

// WorkSummary is the GORM model for the work_summaries table.
// It holds the monthly aggregation of actual/overtime/night/holiday minutes
// for one employee (LM-033).
type WorkSummary struct {
	ID               uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID `gorm:"column:tenant_id"`
	EmployeeID       uuid.UUID `gorm:"column:employee_id"`
	PeriodMonth      time.Time `gorm:"column:period_month"`
	ScheduledMinutes int       `gorm:"column:scheduled_minutes"`
	ActualMinutes    int       `gorm:"column:actual_minutes"`
	OvertimeMinutes  int       `gorm:"column:overtime_minutes"`
	NightMinutes     int       `gorm:"column:night_minutes"`
	HolidayMinutes   int       `gorm:"column:holiday_minutes"`
	Over60Minutes    int       `gorm:"column:over60_minutes"`
	ComputedAt       time.Time `gorm:"column:computed_at"`
}

// TableName maps WorkSummary to the work_summaries table.
func (WorkSummary) TableName() string { return "work_summaries" }

// LaborAgreement is the GORM model for the labor_agreements table.
// It holds the per-tenant 36-agreement (三六協定) configuration (LM-032).
//
// All limit columns derive from statutory requirements that may change with
// amendments. Values MUST be verified by a qualified professional.
type LaborAgreement struct {
	ID                         uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID                   uuid.UUID `gorm:"column:tenant_id"`
	Workplace                  string    `gorm:"column:workplace"`
	ValidFrom                  time.Time `gorm:"column:valid_from"`
	ValidTo                    time.Time `gorm:"column:valid_to"`
	MonthlyLimitMinutes        int       `gorm:"column:monthly_limit_minutes"`
	YearlyLimitMinutes         int       `gorm:"column:yearly_limit_minutes"`
	SpecialClause              bool      `gorm:"column:special_clause"`
	SpecialMonthlyLimitMinutes *int      `gorm:"column:special_monthly_limit_minutes"`
	SpecialCountLimit          *int      `gorm:"column:special_count_limit"`
	MultiMonthAvgLimitMinutes  *int      `gorm:"column:multi_month_avg_limit_minutes"`
	CreatedAt                  time.Time `gorm:"column:created_at"`
	UpdatedAt                  time.Time `gorm:"column:updated_at"`
}

// TableName maps LaborAgreement to the labor_agreements table.
func (LaborAgreement) TableName() string { return "labor_agreements" }

// AttendanceSetting is the GORM model for the attendance_settings table.
// One row per tenant; stores all configurable compliance thresholds.
//
// LEGAL NOTICE: Default values in this struct reflect statutory defaults as of
// 2026-06-02. They MUST be reviewed with each amendment by a qualified
// labor-law professional.
type AttendanceSetting struct {
	ID                    uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID              uuid.UUID `gorm:"column:tenant_id"`
	RoundingUnitMinutes   int       `gorm:"column:rounding_unit_minutes"`
	OvertimeRate          float64   `gorm:"column:overtime_rate"`
	NightRate             float64   `gorm:"column:night_rate"`
	HolidayRate           float64   `gorm:"column:holiday_rate"`
	Over60Rate            float64   `gorm:"column:over60_rate"`
	NightStart            string    `gorm:"column:night_start"` // "HH:MM:SS"
	NightEnd              string    `gorm:"column:night_end"`   // "HH:MM:SS"
	BreakAutoMinutes      int       `gorm:"column:break_auto_minutes"`
	DeviationAlertMinutes int       `gorm:"column:deviation_alert_minutes"`
	// Over60BoundaryMinutes is the monthly overtime threshold (in minutes) at which
	// the higher over60_rate kicks in. Statutory default is 3600 (60h × 60min).
	// LEGAL NOTICE: 要専門家確認・改正追従 (労働基準法第37条4項, 中小企業経過措置)
	Over60BoundaryMinutes int       `gorm:"column:over60_boundary_minutes"`
	UpdatedAt             time.Time `gorm:"column:updated_at"`
}

// TableName maps AttendanceSetting to the attendance_settings table.
func (AttendanceSetting) TableName() string { return "attendance_settings" }

// ---------------------------------------------------------------------------
// Value types (not persisted directly, used by service/calculation layers)
// ---------------------------------------------------------------------------

// OvertimeBreakdown holds the categorised overtime/premium minutes for one
// employee-month (LM-033). Used by the calculation layer; serialised to
// work_summaries.
type OvertimeBreakdown struct {
	// RegularMinutes is actual work within scheduled hours (no premium).
	RegularMinutes int
	// OvertimeMinutes is legal overtime up to 60h/month (25% premium).
	OvertimeMinutes int
	// Over60Minutes is monthly overtime exceeding 60h (50% premium for applicable employers).
	// LEGAL: The 50% threshold applies after the transitional provisions end.
	// Verify applicability with a qualified professional before relying on this value.
	Over60Minutes int
	// NightMinutes is the portion of working time within the statutory night zone
	// (night_start..night_end in attendance_settings; +25% in addition to any
	// overtime premium that may also apply).
	NightMinutes int
	// HolidayMinutes is statutory holiday work (+35% premium).
	// Distinguishing statutory (法定休日) from contractual (所定休日) holidays
	// requires the caller to supply is_legal_holiday on the record.
	HolidayMinutes int
}

// AgreementAlert represents one 36-agreement threshold violation or approach
// warning (LM-032).
type AgreementAlert struct {
	// Level is "exceeded" or "approaching".
	Level string
	// Rule names the threshold that triggered: "monthly", "yearly",
	// "special_monthly", "special_count", "multi_month_avg".
	Rule string
	// CurrentMinutes is the current accumulated value.
	CurrentMinutes int
	// LimitMinutes is the threshold against which CurrentMinutes is compared.
	LimitMinutes int
}

// DeviationAlert is raised when the difference between actual and scheduled
// work minutes exceeds the configured threshold (LM-031).
type DeviationAlert struct {
	EmployeeID       uuid.UUID
	WorkDate         time.Time
	ActualMinutes    int
	ScheduledMinutes int
	DeviationMinutes int
}

// PremiumResult contains the output of the overtime premium calculation
// (LM-033). Base wages are applied by the payroll layer; this struct carries
// only the minute counts and multiplier factors.
//
// LEGAL NOTICE: All rate fields originate from attendance_settings and MUST
// be verified against current statutory rates by a qualified professional.
type PremiumResult struct {
	OvertimeMinutes int
	OvertimeRate    float64 // e.g. 1.25
	Over60Minutes   int
	Over60Rate      float64 // e.g. 1.50
	NightMinutes    int
	NightRate       float64 // additive: e.g. 0.25 on top of other premia
	HolidayMinutes  int
	HolidayRate     float64 // e.g. 1.35
}
