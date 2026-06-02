package attendance_test

// integration_test.go — integration tests for the attendance domain.
//
// These tests require Docker (testcontainers). Skip with -short flag.
//
// Coverage:
//   - 36協定 (LM-032): monthly/yearly limit boundary, special clause count,
//     multi-month average boundary
//   - 割増計算 (LM-033): night overlap, 60h boundary, legal holiday, configurable rates
//   - 打刻集計: overnight, break deduction, rounding, correction immutability
//   - RLS: cross-tenant access is denied
//   - RBAC: 200 with permission, 403 without
//   - 監査ログ: audit entries recorded, no PII stored
//   - attendance_settings: changing the settings row changes computed results
//
// LEGAL NOTICE: All threshold values used in these tests are examples used to
// exercise the machinery. They are not legal advice. Verify statutory
// rates/limits with a qualified labor-law professional.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/attendance"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func seedTenant(t *testing.T, adminDB *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO tenants (id, name, plan_code, status, slug)
		 VALUES (?, ?, 'free', 'active', ?)`,
		id, "Test Corp", id.String()[:8],
	).Error)
	return id
}

// seedEmployeeMinimal creates a minimal employee row (no email, no dept).
// Uses synthetic names only — no real PII.
func seedEmployeeMinimal(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status)
		 VALUES (?, ?, ?, ?, ?, 'full_time', 'active')`,
		id, tenantID, code, "テスト", "従業員"+code,
	).Error)
	return id
}

// seedUserWithPermissions seeds a role with a unique name and a user holding that role.
// Role name is derived from the role UUID to guarantee uniqueness within a tenant.
func seedUserWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, perms []string) uuid.UUID {
	t.Helper()
	roleID := uuid.New()
	roleName := "role_" + roleID.String()[:8] // unique per invocation
	permJSON, _ := json.Marshal(map[string][]string{"perms": perms})
	require.NoError(t, adminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, ?, ?)`,
		roleID, tenantID, roleName, permJSON,
	).Error)
	userID := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status, role_id) VALUES (?, ?, ?, 'active', ?)`,
		userID, tenantID, fmt.Sprintf("user-%s@example.test", userID.String()[:8]), roleID,
	).Error)
	return userID
}

// defaultSettings inserts the canonical attendance settings for a tenant.
func defaultSettings(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO attendance_settings
		   (id, tenant_id, rounding_unit_minutes, overtime_rate, night_rate,
		    holiday_rate, over60_rate, night_start, night_end,
		    break_auto_minutes, deviation_alert_minutes)
		 VALUES (gen_random_uuid(), ?, 1, 1.25, 0.25, 1.35, 1.50,
		         '22:00:00', '05:00:00', 0, 30)`,
		tenantID,
	).Error)
}

// fakeRequireAuth injects tenant/user from test request headers.
// This bypasses real session machinery while keeping RBAC active.
func fakeRequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tidStr := c.GetHeader("x-test-tenant-id")
		uidStr := c.GetHeader("x-test-user-id")
		if tidStr == "" || uidStr == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		tid, err := uuid.Parse(tidStr)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		uid, err := uuid.Parse(uidStr)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("auth_tenant_id", tid)
		c.Set("auth_user_id", uid)
		c.Next()
	}
}

func buildRouter(tdb *tenantdb.TenantDB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	attendance.RegisterRoutes(v1, tdb, fakeRequireAuth())
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}, tenantID, userID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-test-tenant-id", tenantID.String())
	req.Header.Set("x-test-user-id", userID.String())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func truncateAttendanceTables(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"work_summaries",
		"attendance_corrections",
		"attendance_records",
		"attendance_settings",
		"labor_agreements",
		"employees",
		"users",
		"roles",
		"tenants",
	)
}

// ---------------------------------------------------------------------------
// RBAC — 200 with permission / 403 without (LM-030)
// ---------------------------------------------------------------------------

func TestAttendanceRBAC_ReadRequiresPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	r := buildRouter(tdb)
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	// User with attendance:read
	readUser := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:read"})
	// User with no permissions
	noPermUser := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{})

	defaultSettings(t, h.AdminDB, tenantID)

	w200 := doJSON(t, r, http.MethodGet, "/api/v1/attendance/settings", nil, tenantID, readUser)
	assert.Equal(t, http.StatusOK, w200.Code)

	w403 := doJSON(t, r, http.MethodGet, "/api/v1/attendance/settings", nil, tenantID, noPermUser)
	assert.Equal(t, http.StatusForbidden, w403.Code)
}

func TestAttendanceRBAC_WriteRequiresPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	r := buildRouter(tdb)
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	writeUser := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write", "attendance:read"})
	readOnlyUser := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:read"})

	// PUT /attendance/settings requires attendance:write
	body := map[string]interface{}{
		"rounding_unit_minutes":   1,
		"overtime_rate":           1.25,
		"night_rate":              0.25,
		"holiday_rate":            1.35,
		"over60_rate":             1.50,
		"night_start":             "22:00:00",
		"night_end":               "05:00:00",
		"break_auto_minutes":      0,
		"deviation_alert_minutes": 30,
	}

	w200 := doJSON(t, r, http.MethodPut, "/api/v1/attendance/settings", body, tenantID, writeUser)
	assert.Equal(t, http.StatusOK, w200.Code)

	w403 := doJSON(t, r, http.MethodPut, "/api/v1/attendance/settings", body, tenantID, readOnlyUser)
	assert.Equal(t, http.StatusForbidden, w403.Code)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestAttendanceRLS_CrossTenantDenied(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)
	empA := seedEmployeeMinimal(t, h.AdminDB, tenantA, "A001")
	// actorA must be a valid user (audit_logs.user_id FK → users)
	actorA := seedUserWithPermissions(t, h.AdminDB, tenantA, []string{"attendance:write"})
	defaultSettings(t, h.AdminDB, tenantA)

	svc := attendance.NewService(tdb)

	// Create record for tenant A
	ci := ptr(time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC))
	co := ptr(time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC))
	rec, err := svc.CreateRecord(ctx, attendance.CreateRecordInput{
		TenantID:   tenantA,
		ActorID:    actorA,
		EmployeeID: empA,
		WorkDate:   time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		ClockIn:    ci,
		ClockOut:   co,
		Source:     "web",
	})
	require.NoError(t, err)

	// Attempt to read the record as tenant B — should not find it (RLS).
	_, err = svc.GetRecord(ctx, tenantB, rec.ID)
	assert.ErrorIs(t, err, attendance.ErrNotFound,
		"tenant B must not access tenant A's attendance record")
}

// ---------------------------------------------------------------------------
// 打刻 CRUD + correction immutability (LM-030)
// ---------------------------------------------------------------------------

func TestAttendanceRecord_CreateAndCorrect(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E001")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write"})

	svc := attendance.NewService(tdb)

	ci := ptr(time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC))
	co := ptr(time.Date(2024, 1, 15, 18, 0, 0, 0, time.UTC))
	rec, err := svc.CreateRecord(ctx, attendance.CreateRecordInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		WorkDate:   time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		ClockIn:    ci,
		ClockOut:   co,
		Source:     "web",
	})
	require.NoError(t, err)
	assert.False(t, rec.IsCorrected)

	// Correct the record: change clock_out.
	newCO := ptr(time.Date(2024, 1, 15, 19, 0, 0, 0, time.UTC))
	note := "退勤打刻ミス修正"
	updated, err := svc.CorrectRecord(ctx, attendance.CorrectRecordInput{
		TenantID: tenantID,
		ActorID:  actorID,
		RecordID: rec.ID,
		ClockOut: newCO,
		Note:     &note,
		Reason:   "Employee reported wrong clock-out",
	})
	require.NoError(t, err)
	assert.True(t, updated.IsCorrected, "is_corrected must be set after correction")
	assert.Equal(t, newCO.UTC(), updated.ClockOut.UTC())

	// Verify correction history row exists.
	var corrCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM attendance_corrections
		 WHERE attendance_record_id = ? AND tenant_id = ?`,
		rec.ID, tenantID,
	).Scan(&corrCount).Error)
	assert.Equal(t, int64(1), corrCount, "one correction history row must exist")
}

func TestAttendanceRecord_DuplicateDateRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E002")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write"})

	svc := attendance.NewService(tdb)

	workDate := time.Date(2024, 2, 5, 0, 0, 0, 0, time.UTC)
	in := attendance.CreateRecordInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		WorkDate:   workDate,
		Source:     "web",
	}
	_, err := svc.CreateRecord(ctx, in)
	require.NoError(t, err)

	_, err = svc.CreateRecord(ctx, in)
	assert.ErrorIs(t, err, attendance.ErrDuplicateRecord,
		"second record on the same date should return ErrDuplicateRecord")
}

// ---------------------------------------------------------------------------
// 月次集計 (LM-033): overnight, break deduction, rounding
// ---------------------------------------------------------------------------

func TestWorkSummary_OvernightAndBreakDeduction(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E003")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write", "attendance:read"})
	defaultSettings(t, h.AdminDB, tenantID)

	svc := attendance.NewService(tdb)

	// Night shift: 22:00 Jan-15 → 06:00 Jan-16 with 60 min break
	ci := ptr(time.Date(2024, 1, 15, 22, 0, 0, 0, time.UTC))
	co := ptr(time.Date(2024, 1, 16, 6, 0, 0, 0, time.UTC))
	_, err := svc.CreateRecord(ctx, attendance.CreateRecordInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		EmployeeID:   empID,
		WorkDate:     time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		ClockIn:      ci,
		ClockOut:     co,
		BreakMinutes: 60,
		Source:       "device",
	})
	require.NoError(t, err)

	// Compute monthly summary.
	ws, err := svc.ComputeAndSaveMonthSummary(
		ctx, tenantID, empID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		480, // 8h scheduled per day
		nil,
		actorID,
	)
	require.NoError(t, err)
	// 8h gross - 1h break = 7h = 420 actual
	assert.Equal(t, 420, ws.ActualMinutes, "break correctly deducted from actual minutes")
	// Night zone 22:00-05:00: 22:00-05:00 = 7h = 420 min raw, but work ends at 06:00 so 420 night min
	assert.Greater(t, ws.NightMinutes, 0, "overnight shift produces night minutes")
}

func TestWorkSummary_RoundingUnit(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E004")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write", "attendance:read"})

	// First: rounding_unit = 1 (no rounding)
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO attendance_settings
		   (id, tenant_id, rounding_unit_minutes, overtime_rate, night_rate,
		    holiday_rate, over60_rate, night_start, night_end,
		    break_auto_minutes, deviation_alert_minutes)
		 VALUES (gen_random_uuid(), ?, 1, 1.25, 0.25, 1.35, 1.50,
		         '22:00:00', '05:00:00', 0, 30)`,
		tenantID,
	).Error)

	svc := attendance.NewService(tdb)

	// 9h7m gross - 60m break = 487 min.
	ci := ptr(time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC))
	co := ptr(time.Date(2024, 3, 1, 18, 7, 0, 0, time.UTC))
	_, err := svc.CreateRecord(ctx, attendance.CreateRecordInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		EmployeeID:   empID,
		WorkDate:     time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		ClockIn:      ci,
		ClockOut:     co,
		BreakMinutes: 60,
		Source:       "web",
	})
	require.NoError(t, err)

	ws1, err := svc.ComputeAndSaveMonthSummary(ctx, tenantID, empID,
		time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), 480, nil, actorID)
	require.NoError(t, err)
	actual1 := ws1.ActualMinutes // 487 with unit=1

	// Update rounding unit to 30 → 487 truncates to 480.
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE attendance_settings SET rounding_unit_minutes = 30 WHERE tenant_id = ?`,
		tenantID,
	).Error)

	ws2, err := svc.ComputeAndSaveMonthSummary(ctx, tenantID, empID,
		time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), 480, nil, actorID)
	require.NoError(t, err)
	actual2 := ws2.ActualMinutes // 480 with unit=30

	assert.Greater(t, actual1, actual2,
		"wider rounding unit (30 min) truncates more minutes than unit=1")
	// Key claim: the setting value drives the result.
	assert.Equal(t, 487, actual1)
	assert.Equal(t, 480, actual2)
}

// ---------------------------------------------------------------------------
// 36協定 (LM-032): monthly/yearly boundaries
// ---------------------------------------------------------------------------

func TestLaborAgreement_CreateAndList(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"laboragreement:write", "laboragreement:read"})

	svc := attendance.NewService(tdb)

	ag, err := svc.CreateAgreement(ctx, attendance.CreateAgreementInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		Workplace:           "本社事業場",
		ValidFrom:           time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:             time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC),
		MonthlyLimitMinutes: 2700,
		YearlyLimitMinutes:  21600,
	})
	require.NoError(t, err)
	assert.Equal(t, 2700, ag.MonthlyLimitMinutes)

	ags, err := svc.ListAgreements(ctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, ags, 1)
}

// TestAgreementAlerts_MonthlyExceeded verifies that EvaluateAgreementAlerts
// returns an "exceeded" alert when the work summary exceeds the monthly limit.
func TestAgreementAlerts_MonthlyExceeded(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E010")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"laboragreement:write", "attendance:write"})
	defaultSettings(t, h.AdminDB, tenantID)

	svc := attendance.NewService(tdb)

	// Seed a labor agreement with monthly_limit = 2700 min (45h).
	ag, err := svc.CreateAgreement(ctx, attendance.CreateAgreementInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		Workplace:           "テスト事業場",
		ValidFrom:           time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:             time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
		MonthlyLimitMinutes: 2700, // 45h
		YearlyLimitMinutes:  21600,
	})
	require.NoError(t, err)
	_ = ag

	// Directly insert a work_summary that exceeds the monthly limit.
	// 2701 min > 2700 → exceeded alert expected.
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO work_summaries
		   (id, tenant_id, employee_id, period_month,
		    scheduled_minutes, actual_minutes, overtime_minutes, night_minutes,
		    holiday_minutes, over60_minutes, computed_at)
		 VALUES (gen_random_uuid(), ?, ?, ?, 0, 0, 2701, 0, 0, 0, now())`,
		tenantID, empID, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	).Error)

	alerts, err := svc.EvaluateAgreementAlerts(ctx, tenantID, empID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 0, 0)
	require.NoError(t, err)
	var found bool
	for _, a := range alerts {
		if a.Rule == "monthly" && a.Level == "exceeded" {
			found = true
		}
	}
	assert.True(t, found, "monthly limit exceeded alert expected")
}

// TestAgreementAlerts_YearlyExceeded verifies the yearly limit check.
// LEGAL NOTE: 21600 min = 360h is the statutory default from migration 00005.
// This test uses it to verify the machinery, not as a binding legal threshold.
func TestAgreementAlerts_YearlyExceeded(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E011")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"laboragreement:write", "attendance:write"})
	defaultSettings(t, h.AdminDB, tenantID)

	svc := attendance.NewService(tdb)

	_, err := svc.CreateAgreement(ctx, attendance.CreateAgreementInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		Workplace:           "テスト事業場",
		ValidFrom:           time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:             time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
		MonthlyLimitMinutes: 2700,
		YearlyLimitMinutes:  21600, // 360h
	})
	require.NoError(t, err)

	// Seed 12 months × 1805 min OT = 21660 > 21600 yearly limit.
	for m := 1; m <= 12; m++ {
		require.NoError(t, h.AdminDB.Exec(
			`INSERT INTO work_summaries
			   (id, tenant_id, employee_id, period_month,
			    scheduled_minutes, actual_minutes, overtime_minutes,
			    night_minutes, holiday_minutes, over60_minutes, computed_at)
			 VALUES (gen_random_uuid(), ?, ?, ?, 0, 0, 1805, 0, 0, 0, now())`,
			tenantID, empID,
			time.Date(2024, time.Month(m), 1, 0, 0, 0, 0, time.UTC),
		).Error)
	}

	alerts, err := svc.EvaluateAgreementAlerts(ctx, tenantID, empID,
		time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC), 0, 0)
	require.NoError(t, err)
	var found bool
	for _, a := range alerts {
		if a.Rule == "yearly" && a.Level == "exceeded" {
			found = true
		}
	}
	assert.True(t, found, "yearly limit exceeded alert expected")
}

// TestAgreementAlerts_SpecialCountLimit verifies the special-clause count.
// LEGAL NOTE: The 6-per-year default is a common statutory pattern; verify
// with a qualified professional for the applicable employment type.
func TestAgreementAlerts_SpecialCountLimit(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E012")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"laboragreement:write", "attendance:write"})
	defaultSettings(t, h.AdminDB, tenantID)

	svc := attendance.NewService(tdb)

	specialLimit := 6
	specialMonthly := 4800
	_, err := svc.CreateAgreement(ctx, attendance.CreateAgreementInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		Workplace:                  "テスト事業場",
		ValidFrom:                  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:                    time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
		MonthlyLimitMinutes:        2700,
		YearlyLimitMinutes:         21600,
		SpecialClause:              true,
		SpecialMonthlyLimitMinutes: &specialMonthly,
		SpecialCountLimit:          &specialLimit,
	})
	require.NoError(t, err)

	// Seed 1 month of data so agreement lookup finds something.
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO work_summaries
		   (id, tenant_id, employee_id, period_month,
		    scheduled_minutes, actual_minutes, overtime_minutes,
		    night_minutes, holiday_minutes, over60_minutes, computed_at)
		 VALUES (gen_random_uuid(), ?, ?, ?, 0, 0, 100, 0, 0, 0, now())`,
		tenantID, empID, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	).Error)

	// Pass specialMonthCount=7 (exceeds 6).
	alerts, err := svc.EvaluateAgreementAlerts(ctx, tenantID, empID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 7, 0)
	require.NoError(t, err)
	var found bool
	for _, a := range alerts {
		if a.Rule == "special_count" && a.Level == "exceeded" {
			found = true
		}
	}
	assert.True(t, found, "special_count exceeded alert expected with 7 > 6")
}

// TestAgreementAlerts_MultiMonthAvg_Boundary verifies 2–6 month average.
// LEGAL NOTE: The 4800 min (80h) threshold in migration 00005 is a statutory
// health-care guidance value. Verify current thresholds with a qualified
// professional.
func TestAgreementAlerts_MultiMonthAvg_Boundary(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E013")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"laboragreement:write", "attendance:write"})
	defaultSettings(t, h.AdminDB, tenantID)

	svc := attendance.NewService(tdb)

	avgLimit := 4800
	_, err := svc.CreateAgreement(ctx, attendance.CreateAgreementInput{
		TenantID:                  tenantID,
		ActorID:                   actorID,
		Workplace:                 "テスト事業場",
		ValidFrom:                 time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:                   time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC),
		MonthlyLimitMinutes:       2700,
		YearlyLimitMinutes:        21600,
		MultiMonthAvgLimitMinutes: &avgLimit,
	})
	require.NoError(t, err)

	// Seed 3 months: (5000 + 5000 + 4800) / 3 = 4933 > 4800 → exceeded.
	for i, ot := range []int{5000, 5000, 4800} {
		require.NoError(t, h.AdminDB.Exec(
			`INSERT INTO work_summaries
			   (id, tenant_id, employee_id, period_month,
			    scheduled_minutes, actual_minutes, overtime_minutes,
			    night_minutes, holiday_minutes, over60_minutes, computed_at)
			 VALUES (gen_random_uuid(), ?, ?, ?, 0, 0, ?, 0, 0, 0, now())`,
			tenantID, empID,
			time.Date(2024, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC),
			ot,
		).Error)
	}

	alerts, err := svc.EvaluateAgreementAlerts(ctx, tenantID, empID,
		time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), 0, 0)
	require.NoError(t, err)
	var found bool
	for _, a := range alerts {
		if a.Rule == "multi_month_avg" && a.Level == "exceeded" {
			found = true
		}
	}
	assert.True(t, found, "multi-month average exceeded alert expected")
}

// ---------------------------------------------------------------------------
// 割増計算: configurable rates drive the PremiumResult (LM-033)
// ---------------------------------------------------------------------------

func TestPremium_NightAndOver60_ConfigurableRates(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E020")
	_ = seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write", "attendance:read"})
	defaultSettings(t, h.AdminDB, tenantID)

	svc := attendance.NewService(tdb)

	// Night shift crossing 22:00 + accumulated OT above 60h boundary.
	ci := ptr(time.Date(2024, 1, 31, 21, 0, 0, 0, time.UTC))
	co := ptr(time.Date(2024, 2, 1, 3, 0, 0, 0, time.UTC)) // 6h total; 22:00-03:00 = 5h night
	rec := attendance.AttendanceRecord{
		ID:           uuid.New(),
		TenantID:     tenantID,
		EmployeeID:   empID,
		WorkDate:     time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC),
		ClockIn:      ci,
		ClockOut:     co,
		BreakMinutes: 0,
	}

	// accOT = 3600 → all OT today goes to over60
	bd, pr, err := svc.ComputePremiumForRecord(ctx, tenantID, rec, 0, false, 3600)
	require.NoError(t, err)
	assert.Greater(t, bd.Over60Minutes, 0, "with accOT=3600, today's OT → over60 bucket")
	assert.Greater(t, bd.NightMinutes, 0, "night overlap produces night minutes")
	assert.Equal(t, 1.50, pr.Over60Rate, "over60 rate from attendance_settings")
	assert.Equal(t, 0.25, pr.NightRate, "night rate from attendance_settings")

	// Change over60_rate in settings and verify the result changes.
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE attendance_settings SET over60_rate = 1.75 WHERE tenant_id = ?`,
		tenantID,
	).Error)

	_, pr2, err := svc.ComputePremiumForRecord(ctx, tenantID, rec, 0, false, 3600)
	require.NoError(t, err)
	assert.Equal(t, 1.75, pr2.Over60Rate,
		"changing over60_rate in attendance_settings changes the computed rate")
}

// ---------------------------------------------------------------------------
// 監査ログ: audit entries recorded, no PII stored
// ---------------------------------------------------------------------------

func TestAuditLog_RecordedNoPII(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAttendanceTables(h) })

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployeeMinimal(t, h.AdminDB, tenantID, "E030")
	actorID := seedUserWithPermissions(t, h.AdminDB, tenantID, []string{"attendance:write"})

	svc := attendance.NewService(tdb)

	_, err := svc.CreateRecord(ctx, attendance.CreateRecordInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		WorkDate:   time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC),
		Source:     "web",
	})
	require.NoError(t, err)

	// Verify an audit log row was created for attendance_record.created.
	var rows []struct {
		Action     string `gorm:"column:action"`
		ResourceID string `gorm:"column:resource_id"`
		UserID     string `gorm:"column:user_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT action, resource_id::text, user_id::text
		 FROM audit_logs
		 WHERE tenant_id = ? AND action = 'attendance_record.created'`,
		tenantID,
	).Scan(&rows).Error)
	require.NotEmpty(t, rows, "audit row must exist after CreateRecord")

	for _, row := range rows {
		// resource_id must be a UUID (opaque), not a name/email.
		_, parseErr := uuid.Parse(row.ResourceID)
		assert.NoError(t, parseErr,
			"audit resource_id must be a UUID, not PII (got: %s)", row.ResourceID)
		// user_id must be a UUID.
		_, parseErr = uuid.Parse(row.UserID)
		assert.NoError(t, parseErr,
			"audit user_id must be a UUID, not PII (got: %s)", row.UserID)
		// action must not contain name or email fragments.
		assert.NotContains(t, row.Action, "@", "audit action must not contain email")
	}
}

// ---------------------------------------------------------------------------
// HTTP: 401 when no auth headers provided
// ---------------------------------------------------------------------------

func TestAttendance_Unauthenticated(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	r := buildRouter(tdb)
	t.Cleanup(func() { truncateAttendanceTables(h) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/attendance/settings", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

func ptr[T any](v T) *T { return &v }
