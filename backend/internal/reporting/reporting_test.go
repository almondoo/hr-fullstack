package reporting_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
	"github.com/your-org/hr-saas/internal/reporting"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go pattern)
// ---------------------------------------------------------------------------

func seedTenant(t *testing.T, adminDB *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, ?, 'free', 'active', ?)`,
		id, "Test Tenant", id.String()[:8],
	).Error)
	return id
}

func seedUser(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, 'active')`,
		id, tenantID, email,
	).Error)
	return id
}

func seedEmployee(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status, hired_on)
		 VALUES (?, ?, ?, '合成', '太郎', 'full_time', ?, '2026-04-01')`,
		id, tenantID, code, status,
	).Error)
	return id
}

func seedRoleWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, name string, permsJSON string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, ?, ?::jsonb)`,
		id, tenantID, name, permsJSON,
	).Error)
	return id
}

func assignRole(t *testing.T, adminDB *gorm.DB, userID, roleID uuid.UUID) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`UPDATE users SET role_id = ? WHERE id = ?`, roleID, userID,
	).Error)
}

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"employee_work_assignments",
		"shift_patterns",
		"work_patterns",
		"calendar_days",
		"company_calendars",
		"export_jobs",
		"report_definitions",
		"leave_requests",
		"attendance_records",
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// ---------------------------------------------------------------------------
// Report definitions
// ---------------------------------------------------------------------------

func TestUpsertAndListReportDefinitions(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	def, err := svc.UpsertReportDefinition(ctx, reporting.UpsertReportDefinitionInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ReportKey:   reporting.ReportEmployeeRoster,
		Name:        "従業員台帳",
		ColumnsJSON: []byte(`[{"key":"employee_code","label":"社員番号","sensitive":false}]`),
		Active:      true,
	})
	require.NoError(t, err)
	assert.Equal(t, reporting.ReportEmployeeRoster, def.ReportKey)

	// Upsert again (same report_key) — must update, not duplicate.
	def2, err := svc.UpsertReportDefinition(ctx, reporting.UpsertReportDefinitionInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
		Name:      "従業員台帳v2",
		Active:    true,
	})
	require.NoError(t, err)
	assert.Equal(t, def.ID, def2.ID, "upsert must reuse the same row id for (tenant, report_key)")

	list, err := svc.ListReportDefinitions(ctx, tenantID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "従業員台帳v2", list[0].Name)
}

func TestUpsertReportDefinitionUnknownKey(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.UpsertReportDefinition(ctx, reporting.UpsertReportDefinitionInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: "no_such_report",
		Name:      "x",
		Active:    true,
	})
	assert.ErrorIs(t, err, reporting.ErrUnknownReport)
}

// ---------------------------------------------------------------------------
// Report execution
// ---------------------------------------------------------------------------

func TestRunEmployeeRosterReport(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	seedEmployee(t, h.AdminDB, tenantID, "EMP002", "active")
	t.Cleanup(func() { truncateAll(h) })

	result, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
	})
	require.NoError(t, err)
	assert.Equal(t, reporting.ReportEmployeeRoster, result.ReportKey)
	assert.Equal(t, []string{"社員番号", "姓", "名", "在籍状況", "入社日"}, result.Columns)
	require.Len(t, result.Rows, 2)
	assert.Equal(t, "EMP001", result.Rows[0][0])
	assert.Equal(t, "2026-04-01", result.Rows[0][4])
}

func TestRunBillingSummaryReport(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	seedEmployee(t, h.AdminDB, tenantID, "EMP002", "inactive")
	t.Cleanup(func() { truncateAll(h) })

	result, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportBillingSummary,
	})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	assert.Equal(t, "free", result.Rows[0][0])
	assert.Equal(t, "1", result.Rows[0][1], "only the active employee counts as a seat")
}

func TestRunAttendanceAndLeaveReports(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Seed two attendance records in June 2026.
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO attendance_records (id, tenant_id, employee_id, work_date, break_minutes, source)
		 VALUES (?, ?, ?, '2026-06-01', 60, 'web'), (?, ?, ?, '2026-06-02', 45, 'web')`,
		uuid.New(), tenantID, empID, uuid.New(), tenantID, empID,
	).Error)

	// Seed approved annual leave (6 days) so the 5-day obligation is satisfied.
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO leave_requests (id, tenant_id, employee_id, leave_type, start_date, end_date, days, status)
		 VALUES (?, ?, ?, 'annual', '2026-06-10', '2026-06-15', 6, 'approved')`,
		uuid.New(), tenantID, empID,
	).Error)

	att, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID: tenantID, ActorID: actorID,
		ReportKey: reporting.ReportAttendanceMonthly, Year: 2026, Month: 6,
	})
	require.NoError(t, err)
	require.Len(t, att.Rows, 1)
	assert.Equal(t, "2", att.Rows[0][1], "worked days")
	assert.Equal(t, "105", att.Rows[0][2], "break minutes sum")

	leave, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID: tenantID, ActorID: actorID,
		ReportKey: reporting.ReportLeaveStatus, Year: 2026,
	})
	require.NoError(t, err)
	require.Len(t, leave.Rows, 1)
	assert.Equal(t, "6", leave.Rows[0][1], "approved days")
	assert.Equal(t, "充足", leave.Rows[0][3], "5-day obligation satisfied (>=5 annual days)")
}

// ---------------------------------------------------------------------------
// Field-level permission: sensitive columns
// ---------------------------------------------------------------------------

func TestSensitiveColumnsExcludedByDefault(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Declare a sensitive column in the report definition.
	_, err := svc.UpsertReportDefinition(ctx, reporting.UpsertReportDefinitionInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
		Name:      "従業員台帳",
		ColumnsJSON: []byte(`[
			{"key":"employee_code","label":"社員番号","sensitive":false},
			{"key":"my_number","label":"マイナンバー","sensitive":true}
		]`),
		Active: true,
	})
	require.NoError(t, err)

	// Default run (no include_sensitive) — sensitive column label must be absent.
	result, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID: tenantID, ActorID: actorID, ReportKey: reporting.ReportEmployeeRoster,
	})
	require.NoError(t, err)
	assert.NotContains(t, result.Columns, "マイナンバー",
		"sensitive column must be excluded by default")
}

func TestSensitiveRunRequiresExportSensitivePermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	// Actor has no role → no reporting:export_sensitive.
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// include_sensitive=true without permission → ErrForbidden, no rows leaked.
	result, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID: tenantID, ActorID: actorID,
		ReportKey: reporting.ReportEmployeeRoster, IncludeSensitive: true,
	})
	assert.ErrorIs(t, err, reporting.ErrForbidden,
		"include_sensitive without reporting:export_sensitive must be rejected by the service layer")
	assert.Nil(t, result, "no report payload may be returned when permission is denied")
}

func TestSensitiveRunWithPermissionIncludesSensitiveColumn(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "exporter",
		`{"perms":["reporting:export_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.UpsertReportDefinition(ctx, reporting.UpsertReportDefinitionInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
		Name:      "従業員台帳",
		ColumnsJSON: []byte(`[
			{"key":"my_number","label":"マイナンバー","sensitive":true}
		]`),
		Active: true,
	})
	require.NoError(t, err)

	result, err := svc.RunReport(ctx, reporting.RunReportInput{
		TenantID: tenantID, ActorID: actorID,
		ReportKey: reporting.ReportEmployeeRoster, IncludeSensitive: true,
	})
	require.NoError(t, err)
	assert.Contains(t, result.Columns, "マイナンバー",
		"sensitive column must be included when actor holds reporting:export_sensitive")
}

// ---------------------------------------------------------------------------
// Export jobs (async) + CSV/xlsx encoding
// ---------------------------------------------------------------------------

func TestExportJobLifecycleCSV(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	job, err := svc.CreateExportJob(ctx, reporting.CreateExportJobInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
		Format:    reporting.FormatCSV,
	})
	require.NoError(t, err)
	assert.Equal(t, reporting.JobPending, job.Status)
	assert.Nil(t, job.ResultDocumentID)

	processed, fileBytes, err := svc.ProcessExportJob(ctx, reporting.ProcessExportJobInput{
		TenantID: tenantID, ActorID: actorID, JobID: job.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, reporting.JobCompleted, processed.Status)
	require.NotNil(t, processed.ResultDocumentID, "completed job must reference an opaque document id")
	assert.NotEqual(t, uuid.Nil, *processed.ResultDocumentID)

	// CSV must be UTF-8 BOM-prefixed (Excel mojibake avoidance) and have a header.
	require.True(t, bytes.HasPrefix(fileBytes, []byte{0xEF, 0xBB, 0xBF}), "CSV must start with UTF-8 BOM")
	assert.Contains(t, string(fileBytes), "社員番号", "CSV must contain the header row")
	assert.Contains(t, string(fileBytes), "EMP001", "CSV must contain the data row")

	// Re-processing a completed job must fail the transition.
	_, _, err = svc.ProcessExportJob(ctx, reporting.ProcessExportJobInput{
		TenantID: tenantID, ActorID: actorID, JobID: job.ID,
	})
	assert.ErrorIs(t, err, reporting.ErrInvalidTransition,
		"a completed job is terminal and cannot be re-processed")
}

func TestExportJobXLSXEncoding(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	job, err := svc.CreateExportJob(ctx, reporting.CreateExportJobInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
		Format:    reporting.FormatXLSX,
	})
	require.NoError(t, err)

	_, fileBytes, err := svc.ProcessExportJob(ctx, reporting.ProcessExportJobInput{
		TenantID: tenantID, ActorID: actorID, JobID: job.ID,
	})
	require.NoError(t, err)
	// SpreadsheetML xlsx-compatible payload: declares UTF-8 and Excel progid.
	assert.Contains(t, string(fileBytes), `encoding="UTF-8"`)
	assert.Contains(t, string(fileBytes), "Excel.Sheet")
	assert.Contains(t, string(fileBytes), "社員番号", "header text must be present without mojibake")
}

func TestCreateSensitiveExportJobRequiresPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.CreateExportJob(ctx, reporting.CreateExportJobInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		ReportKey:        reporting.ReportEmployeeRoster,
		Format:           reporting.FormatCSV,
		IncludeSensitive: true,
	})
	assert.ErrorIs(t, err, reporting.ErrForbidden,
		"include_sensitive export job without reporting:export_sensitive must be rejected")
}

// ---------------------------------------------------------------------------
// Audit log PII check (output values not stored)
// ---------------------------------------------------------------------------

func TestExportAuditContainsNoOutputValues(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	seedEmployee(t, h.AdminDB, tenantID, "EMP-SECRET-合成", "active")
	t.Cleanup(func() { truncateAll(h) })

	job, err := svc.CreateExportJob(ctx, reporting.CreateExportJobInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		ReportKey: reporting.ReportEmployeeRoster,
		Format:    reporting.FormatCSV,
	})
	require.NoError(t, err)
	_, _, err = svc.ProcessExportJob(ctx, reporting.ProcessExportJobInput{
		TenantID: tenantID, ActorID: actorID, JobID: job.ID,
	})
	require.NoError(t, err)

	// No audit row may contain the employee code (an output value) or '合成'.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ? OR resource_type LIKE ?`,
		"%合成%", "%合成%", "%合成%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount, "audit_logs must not contain report output values / PII")

	// The audit resource_id for the completed job must be the opaque job UUID.
	var resourceID string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs
		 WHERE action = 'export_job.completed' AND tenant_id = ? LIMIT 1`,
		tenantID,
	).Scan(&resourceID).Error)
	assert.Equal(t, job.ID.String(), resourceID, "audit resource_id must be the opaque job UUID")
}

// ---------------------------------------------------------------------------
// Calendar / business-day calculation
// ---------------------------------------------------------------------------

func TestBusinessDayCalculation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Sat(6) + Sun(0) are prescribed weekly holidays.
	cal, err := svc.CreateCalendar(ctx, reporting.CreateCalendarInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		Name:                  "2026年度カレンダー",
		FiscalYear:            2026,
		DefaultWeeklyHolidays: []byte(`{"weekdays":[0,6]}`),
		EffectiveFrom:         time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	// 2026-06-01 is a Monday → business day.
	monday := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	biz, err := svc.IsBusinessDay(ctx, tenantID, cal.ID, monday)
	require.NoError(t, err)
	assert.True(t, biz, "Monday must be a business day under the weekday pattern")

	// 2026-06-06 is a Saturday → not a business day (weekday pattern).
	saturday := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	biz, err = svc.IsBusinessDay(ctx, tenantID, cal.ID, saturday)
	require.NoError(t, err)
	assert.False(t, biz, "Saturday must not be a business day under the weekday pattern")

	// Override: mark the Monday as a holiday → not a business day.
	_, err = svc.AddCalendarDay(ctx, reporting.AddCalendarDayInput{
		TenantID: tenantID, ActorID: actorID, CalendarID: cal.ID,
		Date: monday, DayType: reporting.DayTypeHoliday, Label: "創立記念日",
	})
	require.NoError(t, err)
	biz, err = svc.IsBusinessDay(ctx, tenantID, cal.ID, monday)
	require.NoError(t, err)
	assert.False(t, biz, "holiday override must make the Monday a non-business day")

	// Override: mark the Saturday as a special business day → business day.
	_, err = svc.AddCalendarDay(ctx, reporting.AddCalendarDayInput{
		TenantID: tenantID, ActorID: actorID, CalendarID: cal.ID,
		Date: saturday, DayType: reporting.DayTypeBusinessDay, Label: "特別営業日",
	})
	require.NoError(t, err)
	biz, err = svc.IsBusinessDay(ctx, tenantID, cal.ID, saturday)
	require.NoError(t, err)
	assert.True(t, biz, "business_day override must make the Saturday a business day")
}

func TestAddCalendarDayUnknownCalendar(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.AddCalendarDay(ctx, reporting.AddCalendarDayInput{
		TenantID: tenantID, ActorID: actorID, CalendarID: uuid.New(),
		Date: time.Now(), DayType: reporting.DayTypeHoliday,
	})
	assert.ErrorIs(t, err, reporting.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Work patterns / shifts / effective-date resolution
// ---------------------------------------------------------------------------

func TestWorkPatternResolutionAndOverlap(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	fixed, err := svc.CreateWorkPattern(ctx, reporting.CreateWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, Name: "標準固定", PatternType: reporting.PatternFixed,
		ScheduledMinutes: 480, BreakMinutes: 60,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	flex, err := svc.CreateWorkPattern(ctx, reporting.CreateWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, Name: "フレックス", PatternType: reporting.PatternFlex,
		ScheduledMinutes: 480, BreakMinutes: 60,
		EffectiveFrom: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	// Assign fixed for Apr–Sep, flex from Oct (non-overlapping).
	_, err = svc.AssignWorkPattern(ctx, reporting.AssignWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, WorkPatternID: fixed.ID,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:   ptrTime(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)),
	})
	require.NoError(t, err)
	_, err = svc.AssignWorkPattern(ctx, reporting.AssignWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, WorkPatternID: flex.ID,
		EffectiveFrom: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	// Resolve on 2026-07-01 → fixed.
	wp, err := svc.ResolveWorkPattern(ctx, tenantID, empID, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, fixed.ID, wp.ID)

	// Resolve on 2026-11-01 → flex.
	wp, err = svc.ResolveWorkPattern(ctx, tenantID, empID, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, flex.ID, wp.ID)

	// Overlapping assignment (Aug, inside the fixed range) must be rejected.
	_, err = svc.AssignWorkPattern(ctx, reporting.AssignWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, WorkPatternID: flex.ID,
		EffectiveFrom: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		EffectiveTo:   ptrTime(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)),
	})
	assert.ErrorIs(t, err, reporting.ErrOverlap, "overlapping effective periods must be rejected")
}

func TestShiftPatternRequiresShiftType(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// A non-shift pattern must reject shift creation.
	fixed, err := svc.CreateWorkPattern(ctx, reporting.CreateWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, Name: "固定", PatternType: reporting.PatternFixed,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	_, err = svc.AddShiftPattern(ctx, reporting.AddShiftPatternInput{
		TenantID: tenantID, ActorID: actorID, WorkPatternID: fixed.ID,
		Name: "早番", StartTime: "08:00", EndTime: "17:00",
	})
	assert.ErrorIs(t, err, reporting.ErrInvalidTransition,
		"shift pattern may only attach to a pattern_type=shift work pattern")

	// A shift pattern accepts shifts, including an overnight one.
	shift, err := svc.CreateWorkPattern(ctx, reporting.CreateWorkPatternInput{
		TenantID: tenantID, ActorID: actorID, Name: "シフト", PatternType: reporting.PatternShift,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	sp, err := svc.AddShiftPattern(ctx, reporting.AddShiftPatternInput{
		TenantID: tenantID, ActorID: actorID, WorkPatternID: shift.ID,
		Name: "夜勤", StartTime: "22:00", EndTime: "06:00", BreakMinutes: 60, ScheduledMinutes: 420,
	})
	require.NoError(t, err)
	assert.Equal(t, "夜勤", sp.Name)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := reporting.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA01", "active")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Tenant A: definition + calendar + work pattern.
	_, err := svc.UpsertReportDefinition(ctx, reporting.UpsertReportDefinitionInput{
		TenantID: tenantA, ActorID: actorA, ReportKey: reporting.ReportEmployeeRoster,
		Name: "A台帳", Active: true,
	})
	require.NoError(t, err)
	calA, err := svc.CreateCalendar(ctx, reporting.CreateCalendarInput{
		TenantID: tenantA, ActorID: actorA, Name: "A暦", FiscalYear: 2026,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	wpA, err := svc.CreateWorkPattern(ctx, reporting.CreateWorkPatternInput{
		TenantID: tenantA, ActorID: actorA, Name: "A固定", PatternType: reporting.PatternFixed,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	// Tenant B must not see tenant A's report definitions.
	defsB, err := svc.ListReportDefinitions(ctx, tenantB)
	require.NoError(t, err)
	assert.Empty(t, defsB, "tenant B must not see tenant A report definitions")

	// Tenant B context cannot read tenant A's calendar (business-day → NotFound).
	_, err = svc.IsBusinessDay(ctx, tenantB, calA.ID, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	assert.ErrorIs(t, err, reporting.ErrNotFound,
		"tenant B must not resolve tenant A's calendar")

	// Tenant B context cannot add a day to tenant A's calendar.
	_, err = svc.AddCalendarDay(ctx, reporting.AddCalendarDayInput{
		TenantID: tenantB, ActorID: actorB, CalendarID: calA.ID,
		Date: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), DayType: reporting.DayTypeHoliday,
	})
	assert.ErrorIs(t, err, reporting.ErrNotFound,
		"tenant B must not add days to tenant A's calendar")

	// Tenant B context cannot assign tenant A's employee/work pattern.
	_, err = svc.AssignWorkPattern(ctx, reporting.AssignWorkPatternInput{
		TenantID: tenantB, ActorID: actorB, EmployeeID: empA, WorkPatternID: wpA.ID,
		EffectiveFrom: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	})
	assert.Error(t, err, "tenant B must not assign tenant A's employee/work pattern (RLS + explicit checks)")
}

func ptrTime(t time.Time) *time.Time { return &t }
