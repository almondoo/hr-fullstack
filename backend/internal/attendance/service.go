package attendance

// service.go — business logic for the attendance domain.
//
// LEGAL NOTICE: See model.go for the full legal notice. All compliance
// thresholds are read from per-tenant database configuration (AttendanceSetting,
// LaborAgreement). No statutory values are hard-coded in this file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound           = errors.New("attendance: not found")
	ErrDuplicateRecord    = errors.New("attendance: record already exists for this date")
	ErrDuplicateAgreement = errors.New("attendance: labor agreement already exists for this workplace and valid_from") //nolint:misspell // API contract: US spelling matches DB table and existing client expectations
	ErrSettingsNotFound   = errors.New("attendance: settings not configured for this tenant")
	ErrAgreementNotFound  = errors.New("attendance: no active labor agreement found") //nolint:misspell // API contract: US spelling matches DB table and existing client expectations
)

// maxCorrectionJSONBytes caps the size of before/after correction JSON payloads.
const maxCorrectionJSONBytes = 32 * 1024 // 32 KB

// laborAgreementsTable is the DB table for 36-agreement rows (migration 00005).
const laborAgreementsTable = "labor_agreements" //nolint:misspell // DB contract: table named in migration 00005_attendance.sql

// Service provides business logic for the attendance domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// AttendanceSetting
// ---------------------------------------------------------------------------

// GetSettings fetches the tenant's attendance settings, returning
// ErrSettingsNotFound if no row exists yet.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*AttendanceSetting, error) {
	var st AttendanceSetting
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, rounding_unit_minutes, overtime_rate, night_rate,
			        holiday_rate, over60_rate, night_start, night_end,
			        break_auto_minutes, deviation_alert_minutes,
			        over60_boundary_minutes, updated_at
			 FROM attendance_settings
			 WHERE tenant_id = ?
			 LIMIT 1`,
			tenantID,
		).Scan(&st).Error
	})
	if err != nil {
		return nil, err
	}
	if st.ID == uuid.Nil {
		return nil, ErrSettingsNotFound
	}
	return &st, nil
}

// UpsertSettingsInput holds fields for creating or updating attendance settings.
type UpsertSettingsInput struct {
	TenantID              uuid.UUID
	ActorID               uuid.UUID
	RoundingUnitMinutes   int
	OvertimeRate          float64
	NightRate             float64
	HolidayRate           float64
	Over60Rate            float64
	NightStart            string
	NightEnd              string
	BreakAutoMinutes      int
	DeviationAlertMinutes int
	// Over60BoundaryMinutes is the monthly overtime boundary (minutes) above which
	// the over60_rate applies. Statutory default: 3600 (60h × 60min).
	// LEGAL NOTICE: 要専門家確認・改正追従 (労働基準法第37条4項)
	Over60BoundaryMinutes int
	IP                    *string
}

// UpsertSettings inserts or updates the tenant's attendance settings.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*AttendanceSetting, error) {
	var st AttendanceSetting
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO attendance_settings
			   (id, tenant_id, rounding_unit_minutes, overtime_rate, night_rate,
			    holiday_rate, over60_rate, night_start, night_end,
			    break_auto_minutes, deviation_alert_minutes,
			    over60_boundary_minutes, updated_at)
			 VALUES (gen_random_uuid(), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, now())
			 ON CONFLICT (tenant_id) DO UPDATE SET
			   rounding_unit_minutes   = EXCLUDED.rounding_unit_minutes,
			   overtime_rate           = EXCLUDED.overtime_rate,
			   night_rate              = EXCLUDED.night_rate,
			   holiday_rate            = EXCLUDED.holiday_rate,
			   over60_rate             = EXCLUDED.over60_rate,
			   night_start             = EXCLUDED.night_start,
			   night_end               = EXCLUDED.night_end,
			   break_auto_minutes      = EXCLUDED.break_auto_minutes,
			   deviation_alert_minutes = EXCLUDED.deviation_alert_minutes,
			   over60_boundary_minutes = EXCLUDED.over60_boundary_minutes,
			   updated_at              = now()`,
			in.TenantID,
			in.RoundingUnitMinutes, in.OvertimeRate, in.NightRate,
			in.HolidayRate, in.Over60Rate,
			in.NightStart, in.NightEnd,
			in.BreakAutoMinutes, in.DeviationAlertMinutes,
			in.Over60BoundaryMinutes,
		).Error; err != nil {
			return fmt.Errorf("attendance: upsert settings: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, rounding_unit_minutes, overtime_rate, night_rate,
			        holiday_rate, over60_rate, night_start, night_end,
			        break_auto_minutes, deviation_alert_minutes,
			        over60_boundary_minutes, updated_at
			 FROM attendance_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&st).Error; err != nil {
			return fmt.Errorf("attendance: upsert settings re-read: %w", err)
		}

		idStr := st.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "attendance_settings.upserted",
			ResourceType: "attendance_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// ---------------------------------------------------------------------------
// AttendanceRecord (打刻 / 客観的把握 LM-030)
// ---------------------------------------------------------------------------

// CreateRecordInput holds validated fields for a new attendance record.
type CreateRecordInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	EmployeeID   uuid.UUID
	WorkDate     time.Time
	ClockIn      *time.Time
	ClockOut     *time.Time
	BreakMinutes int
	Source       string
	Note         *string
	IP           *string
}

// CreateRecord inserts a new attendance record and records an audit event.
// Returns ErrDuplicateRecord when a record already exists for the
// (tenant, employee, work_date) triple.
func (s *Service) CreateRecord(ctx context.Context, in CreateRecordInput) (*AttendanceRecord, error) {
	rec := AttendanceRecord{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		EmployeeID:   in.EmployeeID,
		WorkDate:     in.WorkDate,
		ClockIn:      in.ClockIn,
		ClockOut:     in.ClockOut,
		BreakMinutes: in.BreakMinutes,
		Source:       in.Source,
		Note:         in.Note,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("attendance: create record verify employee: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO attendance_records
			   (id, tenant_id, employee_id, work_date, clock_in, clock_out,
			    break_minutes, source, note)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.TenantID, rec.EmployeeID, rec.WorkDate,
			rec.ClockIn, rec.ClockOut,
			rec.BreakMinutes, rec.Source, rec.Note,
		).Error; err != nil {
			// Detect unique constraint violation for (tenant, employee, work_date).
			if isUniqueViolation(err) {
				return ErrDuplicateRecord
			}
			return fmt.Errorf("attendance: create record insert: %w", err)
		}

		idStr := rec.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "attendance_record.created",
			ResourceType: "attendance_record",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	}); err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetRecord fetches a single attendance record by ID.
func (s *Service) GetRecord(ctx context.Context, tenantID, id uuid.UUID) (*AttendanceRecord, error) {
	var rec AttendanceRecord
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, work_date, clock_in, clock_out,
			        break_minutes, source, is_corrected, note, created_at, updated_at
			 FROM attendance_records
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&rec).Error
	})
	if err != nil {
		return nil, err
	}
	if rec.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &rec, nil
}

// ListRecords returns attendance records for an employee within a date range.
func (s *Service) ListRecords(ctx context.Context, tenantID, employeeID uuid.UUID, from, to time.Time) ([]AttendanceRecord, error) {
	var recs []AttendanceRecord
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, work_date, clock_in, clock_out,
			        break_minutes, source, is_corrected, note, created_at, updated_at
			 FROM attendance_records
			 WHERE tenant_id = ? AND employee_id = ?
			   AND work_date >= ? AND work_date <= ?
			 ORDER BY work_date`,
			tenantID, employeeID, from, to,
		).Scan(&recs).Error
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// CorrectRecordInput holds validated fields for a correction operation.
type CorrectRecordInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	RecordID     uuid.UUID
	ClockIn      *time.Time
	ClockOut     *time.Time
	BreakMinutes *int
	Note         *string
	Reason       string
	IP           *string
}

// CorrectRecord updates an attendance record and appends a correction history
// row to attendance_corrections. is_corrected is set to true on the record.
// Both writes occur in the same transaction (LM-030 audit trail).
func (s *Service) CorrectRecord(ctx context.Context, in CorrectRecordInput) (*AttendanceRecord, error) {
	var updated AttendanceRecord
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Read current state for the before_json.
		var current AttendanceRecord
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, work_date, clock_in, clock_out,
			        break_minutes, source, is_corrected, note, created_at, updated_at
			 FROM attendance_records
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			in.RecordID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("attendance: correct record fetch: %w", err)
		}
		if current.ID == uuid.Nil {
			return ErrNotFound
		}

		beforeJSON, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("attendance: correct record marshal before: %w", err)
		}

		// Apply updates (only non-nil fields are changed).
		if in.ClockIn != nil {
			current.ClockIn = in.ClockIn
		}
		if in.ClockOut != nil {
			current.ClockOut = in.ClockOut
		}
		if in.BreakMinutes != nil {
			current.BreakMinutes = *in.BreakMinutes
		}
		if in.Note != nil {
			current.Note = in.Note
		}
		current.IsCorrected = true

		afterJSON, err := json.Marshal(current)
		if err != nil {
			return fmt.Errorf("attendance: correct record marshal after: %w", err)
		}

		// Guard jsonb sizes.
		if len(beforeJSON) > maxCorrectionJSONBytes || len(afterJSON) > maxCorrectionJSONBytes {
			return fmt.Errorf("attendance: correction JSON exceeds maximum size")
		}

		res := tx.Exec(
			`UPDATE attendance_records
			 SET clock_in = ?, clock_out = ?, break_minutes = ?, note = ?,
			     is_corrected = true, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			current.ClockIn, current.ClockOut, current.BreakMinutes, current.Note,
			in.RecordID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("attendance: correct record update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		// Append correction history.
		corrID := uuid.New()
		if err := tx.Exec(
			`INSERT INTO attendance_corrections
			   (id, tenant_id, attendance_record_id, before_json, after_json,
			    reason, corrected_by_user_id, corrected_at)
			 VALUES (?, ?, ?, ?::jsonb, ?::jsonb, ?, ?, now())`,
			corrID, in.TenantID, in.RecordID,
			string(beforeJSON), string(afterJSON),
			in.Reason, in.ActorID,
		).Error; err != nil {
			return fmt.Errorf("attendance: correct record insert correction: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, work_date, clock_in, clock_out,
			        break_minutes, source, is_corrected, note, created_at, updated_at
			 FROM attendance_records WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RecordID, in.TenantID,
		).Scan(&updated).Error; err != nil {
			return fmt.Errorf("attendance: correct record re-read: %w", err)
		}

		idStr := in.RecordID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "attendance_record.corrected",
			ResourceType: "attendance_record",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &updated, nil
}

// ---------------------------------------------------------------------------
// WorkSummary (月次集計 LM-033)
// ---------------------------------------------------------------------------

// ComputeAndSaveMonthSummary re-calculates the monthly work summary for an
// employee/month from individual attendance_records and upserts the result
// into work_summaries.
//
// scheduledMinutesPerDay is the employee's contracted daily work time (分).
// It is passed by the caller (e.g. from an employment contract) because the
// attendance package does not own the payroll/contract domain.
//
// holidayDates is the set of work_date values that are legal statutory holidays
// (法定休日) for this employee in the given month.
//
// LEGAL NOTICE: The calculation calls ComputeBreakdown for each record, which
// applies the statutory model as of 2026. See model.go for the full legal
// notice.
func (s *Service) ComputeAndSaveMonthSummary(
	ctx context.Context,
	tenantID, employeeID uuid.UUID,
	periodMonth time.Time,
	scheduledMinutesPerDay int,
	holidayDates map[time.Time]bool,
	actorID uuid.UUID,
) (*WorkSummary, error) {
	periodMonth = firstOfMonth(periodMonth)

	setting, err := s.GetSettings(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	from := periodMonth
	to := lastDayOfMonth(periodMonth)

	var summary WorkSummary
	err = s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var recs []AttendanceRecord
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, work_date, clock_in, clock_out,
			        break_minutes, source, is_corrected, note, created_at, updated_at
			 FROM attendance_records
			 WHERE tenant_id = ? AND employee_id = ?
			   AND work_date >= ? AND work_date <= ?
			 ORDER BY work_date`,
			tenantID, employeeID, from, to,
		).Scan(&recs).Error; err != nil {
			return fmt.Errorf("attendance: compute summary fetch records: %w", err)
		}

		// Aggregate across all records in the month.
		var totalActual, totalOvertime, totalNight, totalHoliday, totalOver60 int
		accOT := 0 // accumulated overtime so far this month (for 60h boundary)

		for _, r := range recs {
			if r.ClockIn == nil || r.ClockOut == nil {
				continue
			}
			isHoliday := holidayDates[r.WorkDate.Truncate(24*time.Hour)]
			bd, err := ComputeBreakdown(
				*r.ClockIn, *r.ClockOut,
				r.BreakMinutes, scheduledMinutesPerDay,
				isHoliday, accOT, *setting,
			)
			if err != nil {
				// Individual record error: skip (log in production)
				continue
			}
			totalActual += bd.RegularMinutes + bd.OvertimeMinutes + bd.Over60Minutes + bd.HolidayMinutes
			totalOvertime += bd.OvertimeMinutes
			totalNight += bd.NightMinutes
			totalHoliday += bd.HolidayMinutes
			totalOver60 += bd.Over60Minutes
			accOT += bd.OvertimeMinutes + bd.Over60Minutes
		}

		// Scheduled = business days × scheduled_per_day (simplified: caller passes total if desired)
		scheduledTotal := countBusinessDays(from, to, holidayDates) * scheduledMinutesPerDay

		// Upsert work_summaries.
		if err := tx.Exec(
			`INSERT INTO work_summaries
			   (id, tenant_id, employee_id, period_month,
			    scheduled_minutes, actual_minutes, overtime_minutes,
			    night_minutes, holiday_minutes, over60_minutes, computed_at)
			 VALUES (gen_random_uuid(), ?, ?, ?,  ?, ?, ?, ?, ?, ?, now())
			 ON CONFLICT (tenant_id, employee_id, period_month) DO UPDATE SET
			   scheduled_minutes = EXCLUDED.scheduled_minutes,
			   actual_minutes    = EXCLUDED.actual_minutes,
			   overtime_minutes  = EXCLUDED.overtime_minutes,
			   night_minutes     = EXCLUDED.night_minutes,
			   holiday_minutes   = EXCLUDED.holiday_minutes,
			   over60_minutes    = EXCLUDED.over60_minutes,
			   computed_at       = now()`,
			tenantID, employeeID, periodMonth,
			scheduledTotal, totalActual, totalOvertime,
			totalNight, totalHoliday, totalOver60,
		).Error; err != nil {
			return fmt.Errorf("attendance: compute summary upsert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, period_month,
			        scheduled_minutes, actual_minutes, overtime_minutes,
			        night_minutes, holiday_minutes, over60_minutes, computed_at
			 FROM work_summaries
			 WHERE tenant_id = ? AND employee_id = ? AND period_month = ?
			 LIMIT 1`,
			tenantID, employeeID, periodMonth,
		).Scan(&summary).Error; err != nil {
			return fmt.Errorf("attendance: compute summary re-read: %w", err)
		}

		idStr := summary.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &actorID,
			Action:       "work_summary.computed",
			ResourceType: "work_summary",
			ResourceID:   &idStr,
		})
	})
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

// GetWorkSummary returns the pre-computed monthly summary for an employee/month.
func (s *Service) GetWorkSummary(ctx context.Context, tenantID, employeeID uuid.UUID, periodMonth time.Time) (*WorkSummary, error) {
	periodMonth = firstOfMonth(periodMonth)
	var ws WorkSummary
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, period_month,
			        scheduled_minutes, actual_minutes, overtime_minutes,
			        night_minutes, holiday_minutes, over60_minutes, computed_at
			 FROM work_summaries
			 WHERE tenant_id = ? AND employee_id = ? AND period_month = ?
			 LIMIT 1`,
			tenantID, employeeID, periodMonth,
		).Scan(&ws).Error
	})
	if err != nil {
		return nil, err
	}
	if ws.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &ws, nil
}

// ---------------------------------------------------------------------------
// LaborAgreement (36協定 LM-032)
// ---------------------------------------------------------------------------

// CreateAgreementInput holds validated fields for a new labour agreement.
type CreateAgreementInput struct {
	TenantID                   uuid.UUID
	ActorID                    uuid.UUID
	Workplace                  string
	ValidFrom                  time.Time
	ValidTo                    time.Time
	MonthlyLimitMinutes        int
	YearlyLimitMinutes         int
	SpecialClause              bool
	SpecialMonthlyLimitMinutes *int
	SpecialCountLimit          *int
	MultiMonthAvgLimitMinutes  *int
	IP                         *string
}

// CreateAgreement inserts a new labour agreement.
func (s *Service) CreateAgreement(ctx context.Context, in CreateAgreementInput) (*LaborAgreement, error) {
	ag := LaborAgreement{
		ID:                         uuid.New(),
		TenantID:                   in.TenantID,
		Workplace:                  in.Workplace,
		ValidFrom:                  in.ValidFrom,
		ValidTo:                    in.ValidTo,
		MonthlyLimitMinutes:        in.MonthlyLimitMinutes,
		YearlyLimitMinutes:         in.YearlyLimitMinutes,
		SpecialClause:              in.SpecialClause,
		SpecialMonthlyLimitMinutes: in.SpecialMonthlyLimitMinutes,
		SpecialCountLimit:          in.SpecialCountLimit,
		MultiMonthAvgLimitMinutes:  in.MultiMonthAvgLimitMinutes,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			"INSERT INTO "+laborAgreementsTable+`
			   (id, tenant_id, workplace, valid_from, valid_to,
			    monthly_limit_minutes, yearly_limit_minutes,
			    special_clause, special_monthly_limit_minutes, special_count_limit,
			    multi_month_avg_limit_minutes)
			 VALUES (?, ?, ?, ?, ?,  ?, ?,  ?, ?, ?,  ?)`,
			ag.ID, ag.TenantID, ag.Workplace, ag.ValidFrom, ag.ValidTo,
			ag.MonthlyLimitMinutes, ag.YearlyLimitMinutes,
			ag.SpecialClause, ag.SpecialMonthlyLimitMinutes, ag.SpecialCountLimit,
			ag.MultiMonthAvgLimitMinutes,
		).Error; err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateAgreement
			}
			return fmt.Errorf("attendance: create agreement insert: %w", err)
		}
		idStr := ag.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "labor_agreement.created", //nolint:misspell // audit contract: US spelling matches existing audit log records
			ResourceType: "labor_agreement",         //nolint:misspell // audit contract: US spelling matches existing audit log records
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	}); err != nil {
		return nil, err
	}
	return &ag, nil
}

// ListAgreements returns all labour agreements for a tenant, ordered by valid_from DESC.
func (s *Service) ListAgreements(ctx context.Context, tenantID uuid.UUID) ([]LaborAgreement, error) {
	var ags []LaborAgreement
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, workplace, valid_from, valid_to,"+
				" monthly_limit_minutes, yearly_limit_minutes,"+
				" special_clause, special_monthly_limit_minutes, special_count_limit,"+
				" multi_month_avg_limit_minutes, created_at, updated_at"+
				" FROM "+laborAgreementsTable+
				" WHERE tenant_id = ?"+
				" ORDER BY valid_from DESC",
			tenantID,
		).Scan(&ags).Error
	})
	return ags, err
}

// ActiveAgreement returns the labour agreement whose valid_from..valid_to range
// covers asOf, or ErrAgreementNotFound if none matches.
func (s *Service) ActiveAgreement(ctx context.Context, tenantID uuid.UUID, asOf time.Time) (*LaborAgreement, error) {
	var ag LaborAgreement
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, workplace, valid_from, valid_to,"+
				" monthly_limit_minutes, yearly_limit_minutes,"+
				" special_clause, special_monthly_limit_minutes, special_count_limit,"+
				" multi_month_avg_limit_minutes, created_at, updated_at"+
				" FROM "+laborAgreementsTable+
				" WHERE tenant_id = ?"+
				" AND valid_from <= ? AND valid_to >= ?"+
				" ORDER BY valid_from DESC"+
				" LIMIT 1",
			tenantID, asOf, asOf,
		).Scan(&ag).Error
	})
	if err != nil {
		return nil, err
	}
	if ag.ID == uuid.Nil {
		return nil, ErrAgreementNotFound
	}
	return &ag, nil
}

// EvaluateAgreementAlerts loads the relevant work summaries and runs
// CheckAgreementAlerts against the active labour agreement for the given month.
//
// monthlySpecialCount is the number of months this fiscal year in which the
// caller has triggered the special clause (the caller must track this in the
// application layer or pass 0 if unknown).
func (s *Service) EvaluateAgreementAlerts(
	ctx context.Context,
	tenantID, employeeID uuid.UUID,
	targetMonth time.Time,
	monthlySpecialCount int,
	approachingPct float64,
) ([]AgreementAlert, error) {
	targetMonth = firstOfMonth(targetMonth)

	ag, err := s.ActiveAgreement(ctx, tenantID, targetMonth)
	if err != nil {
		return nil, err
	}

	// Load summaries for the agreement year to compute yearly overtime.
	yearStart := ag.ValidFrom
	yearEnd := ag.ValidTo
	var summaries []WorkSummary
	if err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, period_month,
			        overtime_minutes, over60_minutes
			 FROM work_summaries
			 WHERE tenant_id = ? AND employee_id = ?
			   AND period_month >= ? AND period_month <= ?
			 ORDER BY period_month`,
			tenantID, employeeID, yearStart, yearEnd,
		).Scan(&summaries).Error
	}); err != nil {
		return nil, err
	}

	// yearlyOT: cumulative overtime across ALL months within the agreement period
	// (ag.ValidFrom..ag.ValidTo), i.e. the agreement-year total, NOT a rolling
	// calendar-year total. This matches the 36-agreement (三六協定) reporting unit.
	var monthlyOT int
	var yearlyOT int
	// recentMonths collects overtime per month in chronological order for months
	// up to and including targetMonth (i.e. only past/current data, not future
	// months). The slice is capped at the most-recent 6 entries to match the
	// statutory 2–6-month average window.
	var recentMonths []int

	for _, ws := range summaries {
		monthOT := ws.OvertimeMinutes + ws.Over60Minutes
		yearlyOT += monthOT
		if ws.PeriodMonth.Equal(targetMonth) {
			monthlyOT = monthOT
		}
		// Only include months up to and including targetMonth in the average window
		// so future months (within the agreement year) don't dilute the average.
		if !ws.PeriodMonth.After(targetMonth) {
			recentMonths = append(recentMonths, monthOT)
		}
	}

	// Multi-month average: retain only the most-recent 6 months (末尾最大6件).
	// summaries are ordered by period_month ASC so the last N entries are the
	// most recent months prior to or including targetMonth.
	if len(recentMonths) > 6 {
		recentMonths = recentMonths[len(recentMonths)-6:]
	}

	alerts := CheckAgreementAlerts(
		monthlyOT, yearlyOT, monthlySpecialCount,
		recentMonths, *ag, approachingPct,
	)
	return alerts, nil
}

// ---------------------------------------------------------------------------
// Deviation alert (LM-031)
// ---------------------------------------------------------------------------

// CheckDeviationForRecord evaluates the deviation alert for a single record.
// actualMinutes is the computed net work minutes. scheduledMinutes is the
// employee's contracted daily work minutes.
//
// This method fetches the tenant's settings and delegates to CheckDeviationAlert
// for the pure calculation.
func (s *Service) CheckDeviationForRecord(
	ctx context.Context,
	tenantID, employeeID uuid.UUID,
	workDate time.Time,
	actualMinutes, scheduledMinutes int,
) (*DeviationAlert, error) {
	setting, err := s.GetSettings(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return CheckDeviationAlert(employeeID, workDate, actualMinutes, scheduledMinutes, *setting), nil
}

// ---------------------------------------------------------------------------
// Premium calculation (LM-033)
// ---------------------------------------------------------------------------

// ComputePremiumForRecord calculates the premium breakdown for a single
// attendance record. Returns both the OvertimeBreakdown and PremiumResult
// (rate metadata) so callers can apply base wages upstream.
func (s *Service) ComputePremiumForRecord(
	ctx context.Context,
	tenantID uuid.UUID,
	rec AttendanceRecord,
	scheduledMinutesPerDay int,
	isLegalHoliday bool,
	accOvertimeBeforeToday int,
) (*OvertimeBreakdown, *PremiumResult, error) {
	setting, err := s.GetSettings(ctx, tenantID)
	if err != nil {
		return nil, nil, err
	}
	if rec.ClockIn == nil || rec.ClockOut == nil {
		return nil, nil, fmt.Errorf("attendance: premium calc requires clock_in and clock_out")
	}
	bd, err := ComputeBreakdown(
		*rec.ClockIn, *rec.ClockOut,
		rec.BreakMinutes, scheduledMinutesPerDay,
		isLegalHoliday, accOvertimeBeforeToday, *setting,
	)
	if err != nil {
		return nil, nil, err
	}
	pr := ComputePremiumResult(bd, *setting)
	return &bd, &pr, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// isUniqueViolation returns true when err is a PostgreSQL unique-constraint
// violation (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// pgx wraps PgError; check by message pattern rather than importing pgconn.
	return contains(err.Error(), "23505") || contains(err.Error(), "duplicate key")
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || sub == "" ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// firstOfMonth truncates t to the first day of its month (UTC).
func firstOfMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// lastDayOfMonth returns the last day of the month containing t (UTC).
func lastDayOfMonth(t time.Time) time.Time {
	t = t.UTC()
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return first.AddDate(0, 1, -1)
}

// countBusinessDays counts days in [from, to] that are not in holidayDates.
// This is a simplistic approximation; a production system should account for
// the employee's scheduled work pattern (e.g. 4-day week).
func countBusinessDays(from, to time.Time, holidayDates map[time.Time]bool) int {
	count := 0
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		wd := d.Weekday()
		key := d.Truncate(24 * time.Hour)
		if wd != time.Saturday && wd != time.Sunday && !holidayDates[key] {
			count++
		}
	}
	return count
}

// multiMonthAvg computes the integer average of a slice (exported for tests).
func multiMonthAvg(mins []int) int {
	if len(mins) == 0 {
		return 0
	}
	sum := 0
	for _, v := range mins {
		sum += v
	}
	return int(math.Round(float64(sum) / float64(len(mins))))
}
