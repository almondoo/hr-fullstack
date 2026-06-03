package reporting

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("reporting: not found")
	ErrInvalidTransition = errors.New("reporting: invalid status transition")
	ErrForbidden         = errors.New("reporting: permission denied")
	ErrOverlap           = errors.New("reporting: overlapping effective period")
	ErrUnknownReport     = errors.New("reporting: unknown report key")
)

// permExportSensitive is the elevated permission required to include sensitive
// PII columns (マイナンバー/口座/健診) in a report or export.
const permExportSensitive = "reporting:export_sensitive"

// validReportKeys is the allow-list of code-defined standard reports.
var validReportKeys = map[string]bool{
	ReportEmployeeRoster:    true,
	ReportAttendanceMonthly: true,
	ReportLeaveStatus:       true,
	ReportBillingSummary:    true,
}

// allowedJobTransitions defines legal export-job status moves.
// Terminal states: completed, failed.
var allowedJobTransitions = map[string]map[string]bool{
	JobPending: {
		JobRunning: true,
		JobFailed:  true,
	},
	JobRunning: {
		JobCompleted: true,
		JobFailed:    true,
	},
}

func isJobTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedJobTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// Service provides business logic for the reporting / export and calendar /
// work-pattern subsystems.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// hasSensitivePermission loads the actor's permissions inside tx and reports
// whether they hold reporting:export_sensitive.  Multi-layer defence: even when
// an HTTP middleware already checked, the service re-verifies before exposing
// any sensitive column or accepting include_sensitive=true.
func hasSensitivePermission(tx *gorm.DB, tenantID, actorID uuid.UUID) (bool, error) {
	perms, err := platformauth.LoadUserPermissions(tx, tenantID, actorID)
	if err != nil {
		return false, fmt.Errorf("reporting: load permissions: %w", err)
	}
	return platformauth.HasPermission(perms, permExportSensitive), nil
}

// ---------------------------------------------------------------------------
// Report definitions
// ---------------------------------------------------------------------------

// UpsertReportDefinitionInput holds fields for activating / configuring a report.
type UpsertReportDefinitionInput struct {
	TenantID         uuid.UUID
	ActorID          uuid.UUID
	ReportKey        string
	Name             string
	ParamsSchemaJSON []byte
	ColumnsJSON      []byte
	Active           bool
	IP               *string
}

// UpsertReportDefinition creates or updates the per-tenant configuration for a
// standard report (one row per (tenant, report_key)).
func (s *Service) UpsertReportDefinition(ctx context.Context, in UpsertReportDefinitionInput) (*ReportDefinition, error) {
	if !validReportKeys[in.ReportKey] {
		return nil, ErrUnknownReport
	}
	def := ReportDefinition{
		ID:               uuid.New(),
		TenantID:         in.TenantID,
		ReportKey:        in.ReportKey,
		Name:             in.Name,
		ParamsSchemaJSON: in.ParamsSchemaJSON,
		ColumnsJSON:      in.ColumnsJSON,
		Active:           in.Active,
	}
	if len(def.ParamsSchemaJSON) == 0 {
		def.ParamsSchemaJSON = []byte(`{}`)
	}
	if len(def.ColumnsJSON) == 0 {
		def.ColumnsJSON = []byte(`[]`)
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO report_definitions
			   (id, tenant_id, report_key, name, params_schema_json, columns_json, active)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?::jsonb, ?)
			 ON CONFLICT (tenant_id, report_key) DO UPDATE
			   SET name               = EXCLUDED.name,
			       params_schema_json = EXCLUDED.params_schema_json,
			       columns_json       = EXCLUDED.columns_json,
			       active             = EXCLUDED.active,
			       updated_at         = now()`,
			def.ID, def.TenantID, def.ReportKey, def.Name,
			def.ParamsSchemaJSON, def.ColumnsJSON, def.Active,
		).Error; err != nil {
			return fmt.Errorf("reporting: upsert report definition: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, report_key, name, params_schema_json, columns_json,
			        active, created_at, updated_at
			 FROM report_definitions
			 WHERE tenant_id = ? AND report_key = ? LIMIT 1`,
			in.TenantID, in.ReportKey,
		).Scan(&def).Error; err != nil {
			return fmt.Errorf("reporting: re-read report definition: %w", err)
		}

		idStr := def.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "report_definition.upserted",
			ResourceType: "report_definition",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &def, nil
}

// ListReportDefinitions returns all active report definitions for a tenant.
func (s *Service) ListReportDefinitions(ctx context.Context, tenantID uuid.UUID) ([]ReportDefinition, error) {
	var defs []ReportDefinition
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, report_key, name, params_schema_json, columns_json,
			        active, created_at, updated_at
			 FROM report_definitions
			 WHERE tenant_id = ?
			 ORDER BY report_key`,
			tenantID,
		).Scan(&defs).Error
	})
	if err != nil {
		return nil, err
	}
	return defs, nil
}

// getReportDefinition fetches a definition by report_key within tx; returns a
// zero-value (ID == Nil) when absent (the report may run with defaults).
func getReportDefinition(tx *gorm.DB, tenantID uuid.UUID, reportKey string) (ReportDefinition, error) {
	var def ReportDefinition
	if err := tx.Raw(
		`SELECT id, tenant_id, report_key, name, params_schema_json, columns_json,
		        active, created_at, updated_at
		 FROM report_definitions
		 WHERE tenant_id = ? AND report_key = ? LIMIT 1`,
		tenantID, reportKey,
	).Scan(&def).Error; err != nil {
		return ReportDefinition{}, fmt.Errorf("reporting: fetch definition: %w", err)
	}
	return def, nil
}

// ---------------------------------------------------------------------------
// Report execution
// ---------------------------------------------------------------------------

// RunReportInput holds parameters for running a standard report.
type RunReportInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// ReportKey selects the code-defined report query.
	ReportKey string
	// IncludeSensitive requests sensitive columns; only honoured when the actor
	// holds reporting:export_sensitive (re-verified in the service layer).
	IncludeSensitive bool
	// Year / Month scope attendance_monthly and leave_status reports.
	Year  int
	Month int
	IP    *string
}

// RunReport resolves report_key to a query, applies field-level permission
// filtering (sensitive columns excluded unless the actor holds
// reporting:export_sensitive), executes the aggregate, and records an audit
// entry.  The audit row contains only the report_key as an opaque resource id —
// no output values or PII.
func (s *Service) RunReport(ctx context.Context, in RunReportInput) (*ReportResult, error) {
	if !validReportKeys[in.ReportKey] {
		return nil, ErrUnknownReport
	}
	var result ReportResult
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Field-level permission: only allow sensitive columns when the actor
		// actually holds reporting:export_sensitive (multi-layer defence).
		allowSensitive := false
		if in.IncludeSensitive {
			ok, err := hasSensitivePermission(tx, in.TenantID, in.ActorID)
			if err != nil {
				return err
			}
			if !ok {
				return ErrForbidden
			}
			allowSensitive = true
		}

		res, err := buildReport(tx, in, allowSensitive)
		if err != nil {
			return err
		}
		result = res

		action := "report.run"
		if allowSensitive {
			action = "report.run_sensitive"
		}
		// Audit: resource_id is the opaque report_key only — never output values.
		rk := in.ReportKey
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "report",
			ResourceID:   &rk,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// buildReport runs the actual aggregate query for report_key.  All SQL uses
// ? placeholders and explicit tenant_id conditions (multi-layer defence).
//
// allowSensitive gates whether sensitive columns (resolved from the report
// definition's columns_json) are appended; the base reports here expose only
// non-sensitive fields, and sensitive columns are surfaced from the definition
// to demonstrate the field-level filter without leaking PII into base queries.
func buildReport(tx *gorm.DB, in RunReportInput, allowSensitive bool) (ReportResult, error) {
	switch in.ReportKey {
	case ReportEmployeeRoster:
		return buildEmployeeRoster(tx, in.TenantID, allowSensitive)
	case ReportAttendanceMonthly:
		return buildAttendanceMonthly(tx, in.TenantID, in.Year, in.Month)
	case ReportLeaveStatus:
		return buildLeaveStatus(tx, in.TenantID, in.Year)
	case ReportBillingSummary:
		return buildBillingSummary(tx, in.TenantID)
	default:
		return ReportResult{}, ErrUnknownReport
	}
}

// sensitiveColumnLabels returns the labels of sensitive columns declared in the
// report definition for reportKey.  Used to demonstrate that, even when the
// definition declares sensitive columns, they are only included for callers
// holding reporting:export_sensitive.
func sensitiveColumnLabels(tx *gorm.DB, tenantID uuid.UUID, reportKey string) ([]string, error) {
	def, err := getReportDefinition(tx, tenantID, reportKey)
	if err != nil {
		return nil, err
	}
	if def.ID == uuid.Nil || len(def.ColumnsJSON) == 0 {
		return nil, nil
	}
	var cols []ReportColumn
	if err := json.Unmarshal(def.ColumnsJSON, &cols); err != nil {
		return nil, fmt.Errorf("reporting: parse columns_json: %w", err)
	}
	var out []string
	for _, c := range cols {
		if c.Sensitive {
			out = append(out, c.Label)
		}
	}
	return out, nil
}

func buildEmployeeRoster(tx *gorm.DB, tenantID uuid.UUID, allowSensitive bool) (ReportResult, error) {
	var rows []struct {
		EmployeeCode string     `gorm:"column:employee_code"`
		LastName     string     `gorm:"column:last_name"`
		FirstName    string     `gorm:"column:first_name"`
		Status       string     `gorm:"column:status"`
		HiredOn      *time.Time `gorm:"column:hired_on"`
	}
	if err := tx.Raw(
		`SELECT employee_code, last_name, first_name, status, hired_on
		 FROM employees
		 WHERE tenant_id = ?
		 ORDER BY employee_code`,
		tenantID,
	).Scan(&rows).Error; err != nil {
		return ReportResult{}, fmt.Errorf("reporting: employee roster query: %w", err)
	}

	columns := []string{"社員番号", "姓", "名", "在籍状況", "入社日"}
	// Sensitive columns (declared in the definition) are appended only with the
	// elevated permission; values remain empty here (base roster never carries
	// マイナンバー/口座 — those live in separate, encrypted stores).
	sensitiveLabels, err := sensitiveColumnLabels(tx, tenantID, ReportEmployeeRoster)
	if err != nil {
		return ReportResult{}, err
	}
	if allowSensitive {
		columns = append(columns, sensitiveLabels...)
	}

	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		hired := ""
		if r.HiredOn != nil {
			hired = r.HiredOn.Format("2006-01-02")
		}
		row := []string{r.EmployeeCode, r.LastName, r.FirstName, r.Status, hired}
		if allowSensitive {
			for range sensitiveLabels {
				row = append(row, "")
			}
		}
		out = append(out, row)
	}
	return ReportResult{ReportKey: ReportEmployeeRoster, Columns: columns, Rows: out}, nil
}

func buildAttendanceMonthly(tx *gorm.DB, tenantID uuid.UUID, year, month int) (ReportResult, error) {
	// Default to the current month when year/month are unset.
	if year == 0 || month == 0 {
		now := time.Now().UTC()
		year, month = now.Year(), int(now.Month())
	}
	start := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)

	var rows []struct {
		EmployeeCode string `gorm:"column:employee_code"`
		WorkedDays   int    `gorm:"column:worked_days"`
		BreakMinutes int    `gorm:"column:break_minutes"`
	}
	if err := tx.Raw(
		`SELECT e.employee_code AS employee_code,
		        COUNT(a.id)      AS worked_days,
		        COALESCE(SUM(a.break_minutes), 0) AS break_minutes
		 FROM employees e
		 LEFT JOIN attendance_records a
		        ON a.employee_id = e.id
		       AND a.tenant_id   = e.tenant_id
		       AND a.work_date  >= ?
		       AND a.work_date   < ?
		 WHERE e.tenant_id = ?
		 GROUP BY e.employee_code
		 ORDER BY e.employee_code`,
		start, end, tenantID,
	).Scan(&rows).Error; err != nil {
		return ReportResult{}, fmt.Errorf("reporting: attendance monthly query: %w", err)
	}

	columns := []string{"社員番号", "出勤日数", "休憩合計(分)"}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.EmployeeCode,
			fmt.Sprintf("%d", r.WorkedDays),
			fmt.Sprintf("%d", r.BreakMinutes),
		})
	}
	return ReportResult{ReportKey: ReportAttendanceMonthly, Columns: columns, Rows: out}, nil
}

func buildLeaveStatus(tx *gorm.DB, tenantID uuid.UUID, year int) (ReportResult, error) {
	if year == 0 {
		year = time.Now().UTC().Year()
	}
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)

	var rows []struct {
		EmployeeCode string  `gorm:"column:employee_code"`
		ApprovedDays float64 `gorm:"column:approved_days"`
		AnnualDays   float64 `gorm:"column:annual_days"`
	}
	// approved_days: total approved leave; annual_days: approved annual leave,
	// used to assess the 5-day-per-year obligation (5日取得義務).
	if err := tx.Raw(
		`SELECT e.employee_code AS employee_code,
		        COALESCE(SUM(lr.days) FILTER (WHERE lr.status = 'approved'), 0) AS approved_days,
		        COALESCE(SUM(lr.days) FILTER (WHERE lr.status = 'approved'
		                                        AND lr.leave_type = 'annual'), 0) AS annual_days
		 FROM employees e
		 LEFT JOIN leave_requests lr
		        ON lr.employee_id = e.id
		       AND lr.tenant_id   = e.tenant_id
		       AND lr.start_date >= ?
		       AND lr.start_date  < ?
		 WHERE e.tenant_id = ?
		 GROUP BY e.employee_code
		 ORDER BY e.employee_code`,
		start, end, tenantID,
	).Scan(&rows).Error; err != nil {
		return ReportResult{}, fmt.Errorf("reporting: leave status query: %w", err)
	}

	columns := []string{"社員番号", "承認済取得日数", "年休取得日数", "5日義務充足"}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		// 5-day obligation satisfied when annual leave taken >= 5 days.
		// NOTE: the 5-day threshold is a statutory value (労基法 改正); kept here
		// as a documented constant and surfaced as a derived flag only — the
		// authoritative threshold should be configured per LM-005/006.
		fulfilled := "未充足"
		if r.AnnualDays >= 5 {
			fulfilled = "充足"
		}
		out = append(out, []string{
			r.EmployeeCode,
			formatDays(r.ApprovedDays),
			formatDays(r.AnnualDays),
			fulfilled,
		})
	}
	return ReportResult{ReportKey: ReportLeaveStatus, Columns: columns, Rows: out}, nil
}

func buildBillingSummary(tx *gorm.DB, tenantID uuid.UUID) (ReportResult, error) {
	var row struct {
		PlanCode    string `gorm:"column:plan_code"`
		SeatCount   int    `gorm:"column:seat_count"`
		ActiveUsers int    `gorm:"column:active_users"`
	}
	// Seat / billing summary: plan code, active employee seats, active users.
	// Pricing is mocked; no card / PAN data is ever read or stored.
	if err := tx.Raw(
		`SELECT t.plan_code AS plan_code,
		        (SELECT COUNT(1) FROM employees e
		          WHERE e.tenant_id = t.id AND e.status = 'active') AS seat_count,
		        (SELECT COUNT(1) FROM users u
		          WHERE u.tenant_id = t.id AND u.status = 'active') AS active_users
		 FROM tenants t
		 WHERE t.id = ? LIMIT 1`,
		tenantID,
	).Scan(&row).Error; err != nil {
		return ReportResult{}, fmt.Errorf("reporting: billing summary query: %w", err)
	}

	columns := []string{"プラン", "アクティブ席数", "アクティブユーザ数"}
	out := [][]string{{
		row.PlanCode,
		fmt.Sprintf("%d", row.SeatCount),
		fmt.Sprintf("%d", row.ActiveUsers),
	}}
	return ReportResult{ReportKey: ReportBillingSummary, Columns: columns, Rows: out}, nil
}

// formatDays renders a fractional day count without a trailing ".0" for whole days.
func formatDays(d float64) string {
	if d == float64(int64(d)) {
		return fmt.Sprintf("%d", int64(d))
	}
	return fmt.Sprintf("%.1f", d)
}

// ---------------------------------------------------------------------------
// Export jobs
// ---------------------------------------------------------------------------

// CreateExportJobInput holds fields for queuing an asynchronous export.
type CreateExportJobInput struct {
	TenantID         uuid.UUID
	ActorID          uuid.UUID
	ReportKey        string
	Format           string
	ParamsJSON       []byte
	IncludeSensitive bool
	IP               *string
}

// CreateExportJob queues an export job in the pending state.  When
// IncludeSensitive is true, the actor must hold reporting:export_sensitive
// (re-verified in the service layer); otherwise ErrForbidden.
func (s *Service) CreateExportJob(ctx context.Context, in CreateExportJobInput) (*ExportJob, error) {
	if !validReportKeys[in.ReportKey] {
		return nil, ErrUnknownReport
	}
	if in.Format != FormatCSV && in.Format != FormatXLSX {
		return nil, fmt.Errorf("%w: unsupported format %q", ErrInvalidTransition, in.Format)
	}

	job := ExportJob{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		ReportKey:         in.ReportKey,
		Format:            in.Format,
		ParamsJSON:        in.ParamsJSON,
		Status:            JobPending,
		RequestedByUserID: &in.ActorID,
		IncludeSensitive:  in.IncludeSensitive,
	}
	if len(job.ParamsJSON) == 0 {
		job.ParamsJSON = []byte(`{}`)
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if in.IncludeSensitive {
			ok, err := hasSensitivePermission(tx, in.TenantID, in.ActorID)
			if err != nil {
				return err
			}
			if !ok {
				return ErrForbidden
			}
		}

		if err := tx.Exec(
			`INSERT INTO export_jobs
			   (id, tenant_id, report_key, format, params_json, status,
			    requested_by_user_id, include_sensitive)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?, ?, ?)`,
			job.ID, job.TenantID, job.ReportKey, job.Format, job.ParamsJSON,
			job.Status, job.RequestedByUserID, job.IncludeSensitive,
		).Error; err != nil {
			return fmt.Errorf("reporting: create export job insert: %w", err)
		}

		idStr := job.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "export_job.created",
			ResourceType: "export_job",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// GetExportJob fetches an export job by ID within the tenant.
func (s *Service) GetExportJob(ctx context.Context, tenantID, id uuid.UUID) (*ExportJob, error) {
	var job ExportJob
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, report_key, format, params_json, status,
			        requested_by_user_id, result_document_id, include_sensitive,
			        error_message, created_at, updated_at, completed_at
			 FROM export_jobs WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&job).Error
	})
	if err != nil {
		return nil, err
	}
	if job.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &job, nil
}

// ProcessExportJobInput holds fields for running a queued export job.
type ProcessExportJobInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	JobID    uuid.UUID
	Year     int
	Month    int
	IP       *string
}

// ProcessExportJob runs a pending export job: it generates the report bytes
// (CSV with UTF-8 BOM, or a stdlib SpreadsheetML xlsx-compatible payload),
// "stores" the file in the document store (modelled here by allocating an
// opaque document UUID and recording its size in params — the actual bytes are
// handed to the ST-FND-10 store via a logical reference), and marks the job
// completed.  The generated bytes are returned for the caller to persist; they
// are never written to logs or audit records.
//
// Permission is re-verified: a job flagged include_sensitive only proceeds when
// the actor still holds reporting:export_sensitive.
func (s *Service) ProcessExportJob(ctx context.Context, in ProcessExportJobInput) (*ExportJob, []byte, error) {
	var job ExportJob
	var fileBytes []byte

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, report_key, format, params_json, status,
			        requested_by_user_id, result_document_id, include_sensitive,
			        error_message, created_at, updated_at, completed_at
			 FROM export_jobs WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.JobID, in.TenantID,
		).Scan(&job).Error; err != nil {
			return fmt.Errorf("reporting: process export read job: %w", err)
		}
		if job.ID == uuid.Nil {
			return ErrNotFound
		}
		if !isJobTransitionAllowed(job.Status, JobRunning) {
			return fmt.Errorf("%w: %s → running", ErrInvalidTransition, job.Status)
		}

		allowSensitive := false
		if job.IncludeSensitive {
			ok, err := hasSensitivePermission(tx, in.TenantID, in.ActorID)
			if err != nil {
				return err
			}
			if !ok {
				return ErrForbidden
			}
			allowSensitive = true
		}

		// Build the report payload.
		result, err := buildReport(tx, RunReportInput{
			TenantID:         in.TenantID,
			ActorID:          in.ActorID,
			ReportKey:        job.ReportKey,
			IncludeSensitive: allowSensitive,
			Year:             in.Year,
			Month:            in.Month,
		}, allowSensitive)
		if err != nil {
			return err
		}

		switch job.Format {
		case FormatCSV:
			fileBytes, err = renderCSV(result)
		case FormatXLSX:
			fileBytes, err = renderXLSX(result)
		default:
			err = fmt.Errorf("%w: unsupported format %q", ErrInvalidTransition, job.Format)
		}
		if err != nil {
			return err
		}

		// Allocate an opaque document UUID (logical reference to the ST-FND-10
		// encrypted document store).  The actual encrypted bytes are persisted by
		// that store; here we only record the reference + completion.
		docID := uuid.New()
		res := tx.Exec(
			`UPDATE export_jobs
			 SET status = 'completed', result_document_id = ?,
			     completed_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = 'pending'`,
			docID, in.JobID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("reporting: process export complete: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Lost the race: another worker already advanced the job.
			return ErrInvalidTransition
		}

		job.Status = JobCompleted
		job.ResultDocumentID = &docID

		idStr := job.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "export_job.completed",
			ResourceType: "export_job",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return &job, fileBytes, nil
}

// utf8BOM is prefixed to CSV output so Microsoft Excel detects UTF-8 and avoids
// 文字化け (mojibake) for Japanese text.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// renderCSV serialises a ReportResult to a UTF-8 (BOM-prefixed) CSV with a
// header row, using the stdlib encoding/csv writer.
func renderCSV(r ReportResult) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(utf8BOM)
	w := csv.NewWriter(&buf)
	if err := w.Write(r.Columns); err != nil {
		return nil, fmt.Errorf("reporting: csv header: %w", err)
	}
	for _, row := range r.Rows {
		if err := w.Write(row); err != nil {
			return nil, fmt.Errorf("reporting: csv row: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("reporting: csv flush: %w", err)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Company calendars / business-day calculation
// ---------------------------------------------------------------------------

// CreateCalendarInput holds fields for creating a company calendar.
type CreateCalendarInput struct {
	TenantID              uuid.UUID
	ActorID               uuid.UUID
	Name                  string
	FiscalYear            int
	DefaultWeeklyHolidays []byte // JSON: {"weekdays":[0,6]}
	EffectiveFrom         time.Time
	EffectiveTo           *time.Time
	IP                    *string
}

// CreateCalendar creates a company calendar (year / effective-period unit).
func (s *Service) CreateCalendar(ctx context.Context, in CreateCalendarInput) (*CompanyCalendar, error) {
	cal := CompanyCalendar{
		ID:                        uuid.New(),
		TenantID:                  in.TenantID,
		Name:                      in.Name,
		FiscalYear:                in.FiscalYear,
		DefaultWeeklyHolidaysJSON: in.DefaultWeeklyHolidays,
		Active:                    true,
		EffectiveFrom:             in.EffectiveFrom,
		EffectiveTo:               in.EffectiveTo,
	}
	if len(cal.DefaultWeeklyHolidaysJSON) == 0 {
		cal.DefaultWeeklyHolidaysJSON = []byte(`{"weekdays":[0,6]}`)
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO company_calendars
			   (id, tenant_id, name, fiscal_year, default_weekly_holidays_json,
			    active, effective_from, effective_to)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?, ?, ?)`,
			cal.ID, cal.TenantID, cal.Name, cal.FiscalYear,
			cal.DefaultWeeklyHolidaysJSON, cal.Active, cal.EffectiveFrom, cal.EffectiveTo,
		).Error; err != nil {
			return fmt.Errorf("reporting: create calendar: %w", err)
		}
		idStr := cal.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "company_calendar.created",
			ResourceType: "company_calendar",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &cal, nil
}

// AddCalendarDayInput holds fields for adding a per-date override.
type AddCalendarDayInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	CalendarID uuid.UUID
	Date       time.Time
	DayType    string
	Label      string
	IP         *string
}

// AddCalendarDay adds (or replaces) a per-date override (祝日/特別休業/特別営業).
func (s *Service) AddCalendarDay(ctx context.Context, in AddCalendarDayInput) (*CalendarDay, error) {
	day := CalendarDay{
		ID:         uuid.New(),
		TenantID:   in.TenantID,
		CalendarID: in.CalendarID,
		Date:       in.Date,
		DayType:    in.DayType,
		Label:      in.Label,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the calendar belongs to this tenant (parent existence).
		var calCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM company_calendars WHERE id = ? AND tenant_id = ?`,
			in.CalendarID, in.TenantID,
		).Scan(&calCount).Error; err != nil {
			return fmt.Errorf("reporting: add calendar day verify calendar: %w", err)
		}
		if calCount == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO calendar_days
			   (id, tenant_id, calendar_id, date, day_type, label)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (calendar_id, date) DO UPDATE
			   SET day_type = EXCLUDED.day_type,
			       label    = EXCLUDED.label,
			       updated_at = now()`,
			day.ID, day.TenantID, day.CalendarID, day.Date, day.DayType, day.Label,
		).Error; err != nil {
			return fmt.Errorf("reporting: add calendar day insert: %w", err)
		}

		idStr := day.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "calendar_day.added",
			ResourceType: "calendar_day",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &day, nil
}

// IsBusinessDay reports whether the given date is a business day for the
// calendar.  Resolution order:
//  1. A calendar_days override for the exact date wins
//     (business_day → true; holiday/special_holiday → false).
//  2. Otherwise the weekday is checked against default_weekly_holidays_json
//     (a weekday in the list is a prescribed holiday → not a business day).
func (s *Service) IsBusinessDay(ctx context.Context, tenantID, calendarID uuid.UUID, date time.Time) (bool, error) {
	var business bool
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var cal CompanyCalendar
		if err := tx.Raw(
			`SELECT id, tenant_id, default_weekly_holidays_json
			 FROM company_calendars WHERE id = ? AND tenant_id = ? LIMIT 1`,
			calendarID, tenantID,
		).Scan(&cal).Error; err != nil {
			return fmt.Errorf("reporting: is business day read calendar: %w", err)
		}
		if cal.ID == uuid.Nil {
			return ErrNotFound
		}

		// Normalise to a date-only value for the override lookup.
		dateOnly := date.Format("2006-01-02")
		var override struct {
			DayType string `gorm:"column:day_type"`
		}
		if err := tx.Raw(
			`SELECT day_type FROM calendar_days
			 WHERE calendar_id = ? AND tenant_id = ? AND date = ? LIMIT 1`,
			calendarID, tenantID, dateOnly,
		).Scan(&override).Error; err != nil {
			return fmt.Errorf("reporting: is business day read override: %w", err)
		}

		if override.DayType != "" {
			business = override.DayType == DayTypeBusinessDay
			return nil
		}

		// Fall back to the weekday pattern.
		var wh weeklyHolidays
		if len(cal.DefaultWeeklyHolidaysJSON) > 0 {
			if err := json.Unmarshal(cal.DefaultWeeklyHolidaysJSON, &wh); err != nil {
				return fmt.Errorf("reporting: parse weekly holidays: %w", err)
			}
		}
		wd := int(date.Weekday())
		isHoliday := false
		for _, h := range wh.Weekdays {
			if h == wd {
				isHoliday = true
				break
			}
		}
		business = !isHoliday
		return nil
	})
	if err != nil {
		return false, err
	}
	return business, nil
}

// ---------------------------------------------------------------------------
// Work patterns / shift patterns / employee assignments
// ---------------------------------------------------------------------------

// CreateWorkPatternInput holds fields for creating a work pattern.
type CreateWorkPatternInput struct {
	TenantID         uuid.UUID
	ActorID          uuid.UUID
	Name             string
	PatternType      string
	ScheduledMinutes int
	BreakMinutes     int
	CoreTimeJSON     []byte
	SettingsJSON     []byte
	EffectiveFrom    time.Time
	EffectiveTo      *time.Time
	IP               *string
}

// CreateWorkPattern creates a work pattern (固定/フレックス/変形/裁量/シフト).
func (s *Service) CreateWorkPattern(ctx context.Context, in CreateWorkPatternInput) (*WorkPattern, error) {
	wp := WorkPattern{
		ID:               uuid.New(),
		TenantID:         in.TenantID,
		Name:             in.Name,
		PatternType:      in.PatternType,
		ScheduledMinutes: in.ScheduledMinutes,
		BreakMinutes:     in.BreakMinutes,
		CoreTimeJSON:     in.CoreTimeJSON,
		SettingsJSON:     in.SettingsJSON,
		EffectiveFrom:    in.EffectiveFrom,
		EffectiveTo:      in.EffectiveTo,
		Active:           true,
	}
	if len(wp.CoreTimeJSON) == 0 {
		wp.CoreTimeJSON = []byte(`{}`)
	}
	if len(wp.SettingsJSON) == 0 {
		wp.SettingsJSON = []byte(`{}`)
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO work_patterns
			   (id, tenant_id, name, pattern_type, scheduled_minutes, break_minutes,
			    core_time_json, settings_json, effective_from, effective_to, active)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?::jsonb, ?, ?, ?)`,
			wp.ID, wp.TenantID, wp.Name, wp.PatternType, wp.ScheduledMinutes,
			wp.BreakMinutes, wp.CoreTimeJSON, wp.SettingsJSON,
			wp.EffectiveFrom, wp.EffectiveTo, wp.Active,
		).Error; err != nil {
			return fmt.Errorf("reporting: create work pattern: %w", err)
		}
		idStr := wp.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "work_pattern.created",
			ResourceType: "work_pattern",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &wp, nil
}

// AddShiftPatternInput holds fields for adding a shift pattern to a work pattern.
type AddShiftPatternInput struct {
	TenantID         uuid.UUID
	ActorID          uuid.UUID
	WorkPatternID    uuid.UUID
	Name             string
	StartTime        string
	EndTime          string
	BreakMinutes     int
	ScheduledMinutes int
	IP               *string
}

// AddShiftPattern adds a shift pattern, requiring the parent work pattern to be
// of type "shift".
func (s *Service) AddShiftPattern(ctx context.Context, in AddShiftPatternInput) (*ShiftPattern, error) {
	sp := ShiftPattern{
		ID:               uuid.New(),
		TenantID:         in.TenantID,
		WorkPatternID:    in.WorkPatternID,
		Name:             in.Name,
		StartTime:        in.StartTime,
		EndTime:          in.EndTime,
		BreakMinutes:     in.BreakMinutes,
		ScheduledMinutes: in.ScheduledMinutes,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var wp struct {
			PatternType string `gorm:"column:pattern_type"`
		}
		if err := tx.Raw(
			`SELECT pattern_type FROM work_patterns
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.WorkPatternID, in.TenantID,
		).Scan(&wp).Error; err != nil {
			return fmt.Errorf("reporting: add shift verify work pattern: %w", err)
		}
		if wp.PatternType == "" {
			return ErrNotFound
		}
		if wp.PatternType != PatternShift {
			return fmt.Errorf("%w: work pattern is %q, not shift", ErrInvalidTransition, wp.PatternType)
		}

		if err := tx.Exec(
			`INSERT INTO shift_patterns
			   (id, tenant_id, work_pattern_id, name, start_time, end_time,
			    break_minutes, scheduled_minutes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sp.ID, sp.TenantID, sp.WorkPatternID, sp.Name, sp.StartTime,
			sp.EndTime, sp.BreakMinutes, sp.ScheduledMinutes,
		).Error; err != nil {
			return fmt.Errorf("reporting: add shift insert: %w", err)
		}

		idStr := sp.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "shift_pattern.added",
			ResourceType: "shift_pattern",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// AssignWorkPatternInput holds fields for assigning a work pattern to an employee.
type AssignWorkPatternInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	EmployeeID    uuid.UUID
	WorkPatternID uuid.UUID
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	IP            *string
}

// AssignWorkPattern assigns a work pattern to an employee for an effective
// period.  Overlapping periods for the same employee are rejected (ErrOverlap).
// A FOR UPDATE lock on existing assignments avoids a TOCTOU race.
func (s *Service) AssignWorkPattern(ctx context.Context, in AssignWorkPatternInput) (*EmployeeWorkAssignment, error) {
	a := EmployeeWorkAssignment{
		ID:            uuid.New(),
		TenantID:      in.TenantID,
		EmployeeID:    in.EmployeeID,
		WorkPatternID: in.WorkPatternID,
		EffectiveFrom: in.EffectiveFrom,
		EffectiveTo:   in.EffectiveTo,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee + work pattern belong to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("reporting: assign verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}
		var wpCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM work_patterns WHERE id = ? AND tenant_id = ?`,
			in.WorkPatternID, in.TenantID,
		).Scan(&wpCount).Error; err != nil {
			return fmt.Errorf("reporting: assign verify work pattern: %w", err)
		}
		if wpCount == 0 {
			return ErrNotFound
		}

		// Lock existing assignments for this employee and check overlap.
		// Two periods [from, to] overlap when start_a <= end_b AND start_b <= end_a,
		// treating NULL effective_to as +infinity.
		//
		// FOR UPDATE cannot be combined with aggregate functions, so we SELECT the
		// overlapping rows' ids (locking them) and count in Go rather than using
		// SELECT COUNT(1) ... FOR UPDATE.
		var overlapIDs []uuid.UUID
		if err := tx.Raw(
			`SELECT id FROM employee_work_assignments
			 WHERE employee_id = ? AND tenant_id = ?
			   AND effective_from <= COALESCE(?::date, 'infinity'::date)
			   AND COALESCE(effective_to, 'infinity'::date) >= ?::date
			 FOR UPDATE`,
			in.EmployeeID, in.TenantID, in.EffectiveTo, in.EffectiveFrom,
		).Scan(&overlapIDs).Error; err != nil {
			return fmt.Errorf("reporting: assign overlap check: %w", err)
		}
		if len(overlapIDs) > 0 {
			return ErrOverlap
		}

		if err := tx.Exec(
			`INSERT INTO employee_work_assignments
			   (id, tenant_id, employee_id, work_pattern_id, effective_from, effective_to)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			a.ID, a.TenantID, a.EmployeeID, a.WorkPatternID, a.EffectiveFrom, a.EffectiveTo,
		).Error; err != nil {
			return fmt.Errorf("reporting: assign insert: %w", err)
		}

		idStr := a.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "work_assignment.created",
			ResourceType: "employee_work_assignment",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ResolveWorkPattern resolves the work pattern in effect for an employee on the
// given application date (effective_from <= date <= effective_to/∞).
func (s *Service) ResolveWorkPattern(ctx context.Context, tenantID, employeeID uuid.UUID, date time.Time) (*WorkPattern, error) {
	var wp WorkPattern
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT wp.id, wp.tenant_id, wp.name, wp.pattern_type, wp.scheduled_minutes,
			        wp.break_minutes, wp.core_time_json, wp.settings_json,
			        wp.effective_from, wp.effective_to, wp.active,
			        wp.created_at, wp.updated_at
			 FROM employee_work_assignments a
			 JOIN work_patterns wp
			   ON wp.id = a.work_pattern_id AND wp.tenant_id = a.tenant_id
			 WHERE a.employee_id = ? AND a.tenant_id = ?
			   AND a.effective_from <= ?::date
			   AND COALESCE(a.effective_to, 'infinity'::date) >= ?::date
			 ORDER BY a.effective_from DESC
			 LIMIT 1`,
			employeeID, tenantID, date.Format("2006-01-02"), date.Format("2006-01-02"),
		).Scan(&wp).Error
	})
	if err != nil {
		return nil, err
	}
	if wp.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &wp, nil
}
