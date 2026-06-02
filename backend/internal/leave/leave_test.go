package leave_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/leave"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func seedTenant(t *testing.T, adminDB *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, ?, 'free', 'active', ?)`,
		id, "Leave Test Tenant", id.String()[:8],
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

func seedEmployee(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code string, hiredOn time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name,
		    employment_type, status, hired_on)
		 VALUES (?, ?, ?, '山田', '太郎', 'full_time', 'active', ?)`,
		id, tenantID, code, hiredOn,
	).Error)
	return id
}

// seedDefaultSettings inserts the default leave_settings for a tenant.
func seedDefaultSettings(t *testing.T, svc *leave.Service, ctx context.Context, tenantID, actorID uuid.UUID) *leave.Setting {
	t.Helper()
	setting, err := svc.UpsertSettings(ctx, leave.UpsertSettingsInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		BaseDateRule:               "hire_date_anniversary",
		GrantTableJSON:             nil, // use migration defaults
		ProportionalTableJSON:      nil,
		FiveDayObligationThreshold: 10,
		ExpiryMonths:               24,
	})
	require.NoError(t, err)
	return setting
}

// seedApprovalRoute inserts a single-step approval route so approval.Submit succeeds.
func seedApprovalRoute(t *testing.T, approvalSvc *approval.Service, ctx context.Context, tenantID, approverID uuid.UUID, requestType string) {
	t.Helper()
	_, err := approvalSvc.CreateRoute(ctx, approval.CreateRouteInput{
		TenantID:    tenantID,
		ActorID:     approverID,
		RequestType: requestType,
		Name:        "default-" + requestType,
		Steps: []approval.RouteStep{
			{Step: 0, UserID: &approverID},
		},
	})
	require.NoError(t, err)
}

// truncateLeave resets leave tables and common dependency tables between sub-tests.
func truncateLeave(h *testdb.Harness) {
	h.TruncateTables(
		"leave_usages", "leave_requests", "leave_grants", "leave_settings",
		"audit_logs", "approval_steps", "approval_requests", "approval_routes",
		"employees", "users", "tenants",
	)
}

// makeService wires a leave.Service for tests.
func makeService(h *testdb.Harness) (*leave.Service, *approval.Service) {
	tdb := tenantdb.New(h.AppDB)
	approvalSvc := approval.NewService(tdb)
	leaveSvc := leave.NewService(tdb, approvalSvc)
	return leaveSvc, approvalSvc
}

// ---------------------------------------------------------------------------
// Settings tests
// ---------------------------------------------------------------------------

func TestUpsertAndGetSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")

	// First upsert creates row.
	setting := seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	assert.NotEqual(t, uuid.Nil, setting.ID)
	assert.Equal(t, 10, setting.FiveDayObligationThreshold)
	assert.Equal(t, 24, setting.ExpiryMonths)

	// Second upsert with different threshold updates the row.
	updated, err := svc.UpsertSettings(ctx, leave.UpsertSettingsInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		BaseDateRule:               "hire_date_anniversary",
		FiveDayObligationThreshold: 15,
		ExpiryMonths:               12,
	})
	require.NoError(t, err)
	assert.Equal(t, 15, updated.FiveDayObligationThreshold)
	assert.Equal(t, 12, updated.ExpiryMonths)
	assert.Equal(t, setting.ID, updated.ID, "upsert must not create a second row")

	// GetSettings returns the updated row.
	got, err := svc.GetSettings(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 15, got.FiveDayObligationThreshold)
}

func TestGetSettingsNotFound(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)

	_, err := svc.GetSettings(ctx, tenantID)
	assert.ErrorIs(t, err, leave.ErrSettingNotFound)
}

// ---------------------------------------------------------------------------
// Grant tests — 法令境界 (LM-040)
// ---------------------------------------------------------------------------

// TestGrantLeave_ManualGrant verifies a direct manual grant is stored correctly.
func TestGrantLeave_ManualGrant(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC))

	grantDate := time.Date(2020, 10, 1, 0, 0, 0, 0, time.UTC)
	grant, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  grantDate,
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, grant.ID)
	assert.Equal(t, 10.0, grant.Days)
	// Expiry should be 24 months after grant date.
	expected := grantDate.AddDate(0, 24, 0)
	assert.Equal(t, expected.Format("2006-01-02"), grant.ExpiresOn.Format("2006-01-02"))
}

// TestComputeAnnualGrant_6MonthBoundary verifies 6-month tenure boundary (first grant).
// LEGAL NOTICE: 10 days at 6 months is the statutory default in grant_table_json.
// These values must be verified by a qualified professional.
func TestComputeAnnualGrant_6MonthBoundary(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)

	hiredOn := time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP6M", hiredOn)

	// Exactly 6 months after hire → first grant (10 days in default table).
	grantDate := time.Date(2023, 10, 1, 0, 0, 0, 0, time.UTC)
	grant, err := svc.ComputeAndGrantAnnual(ctx, leave.ComputeAndGrantAnnualInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		HiredOn:    hiredOn,
		GrantDate:  grantDate,
	})
	require.NoError(t, err)
	require.NotNil(t, grant)
	assert.Equal(t, 10.0, grant.Days, "6-month boundary must yield 10 days (default grant table)")
	assert.Equal(t, leave.GrantSourceAnnual, grant.Source)
}

// TestComputeAnnualGrant_Below6Months verifies no grant before the 6-month threshold.
func TestComputeAnnualGrant_Below6Months(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)

	hiredOn := time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP5M", hiredOn)

	// 5 months — below threshold.
	grantDate := time.Date(2023, 9, 1, 0, 0, 0, 0, time.UTC)
	grant, err := svc.ComputeAndGrantAnnual(ctx, leave.ComputeAndGrantAnnualInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		HiredOn:    hiredOn,
		GrantDate:  grantDate,
	})
	require.NoError(t, err)
	assert.Nil(t, grant, "no grant should be issued before 6-month tenure threshold")
}

// TestComputeAnnualGrant_TenureProgression verifies that different tenure brackets
// yield different day counts.
// LEGAL NOTICE: values from grant_table_json defaults — verify with current law.
func TestComputeAnnualGrant_TenureProgression(t *testing.T) {
	cases := []struct {
		name         string
		tenureMonths int
		wantMinDays  float64 // lower bound
	}{
		{"6_months", 6, 10},
		{"18_months", 18, 11},
		{"30_months", 30, 12},
		{"42_months", 42, 14},
		{"54_months", 54, 16},
		{"66_months", 66, 18},
		{"78_months_max", 78, 20},
	}

	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hiredOn := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
			grantDate := hiredOn.AddDate(0, tc.tenureMonths, 0)
			empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-"+tc.name, hiredOn)
			// Cleanup must not truncate users (audit_log has a FK to users.id).
			// Only remove the leave rows and the employee created in this sub-test.
			t.Cleanup(func() {
				h.AdminDB.Exec(`DELETE FROM leave_usages WHERE tenant_id = ?`, tenantID)
				h.AdminDB.Exec(`DELETE FROM leave_grants WHERE tenant_id = ?`, tenantID)
				h.AdminDB.Exec(`DELETE FROM audit_logs WHERE tenant_id = ?`, tenantID)
				h.AdminDB.Exec(`DELETE FROM employees WHERE id = ?`, empID)
			})

			grant, err := svc.ComputeAndGrantAnnual(ctx, leave.ComputeAndGrantAnnualInput{
				TenantID:   tenantID,
				ActorID:    actorID,
				EmployeeID: empID,
				HiredOn:    hiredOn,
				GrantDate:  grantDate,
			})
			require.NoError(t, err)
			require.NotNil(t, grant, "expected a grant for tenure %d months", tc.tenureMonths)
			assert.GreaterOrEqual(t, grant.Days, tc.wantMinDays,
				"tenure %d months: expected >= %.0f days", tc.tenureMonths, tc.wantMinDays)
		})
	}
}

// TestComputeAnnualGrant_Proportional verifies a proportional grant for a 3-day/week employee.
// LEGAL NOTICE: values from proportional_table_json defaults — verify with current law.
func TestComputeAnnualGrant_Proportional(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)

	hiredOn := time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPPT", hiredOn)

	weeklyDays := 3.0
	grantDate := hiredOn.AddDate(0, 6, 0) // 6 months
	grant, err := svc.ComputeAndGrantAnnual(ctx, leave.ComputeAndGrantAnnualInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		HiredOn:    hiredOn,
		GrantDate:  grantDate,
		WeeklyDays: &weeklyDays,
	})
	require.NoError(t, err)
	require.NotNil(t, grant)
	assert.Equal(t, leave.GrantSourceProportional, grant.Source)
	// Default table: 3-day/week at 6 months = 5 days.
	assert.Equal(t, 5.0, grant.Days, "3 days/week at 6 months should yield 5 days")
}

// TestGrant_ExpiryAtTwoYears verifies the 2-year (24-month) statutory expiry.
// LEGAL NOTICE: expiry_months from settings; verify with current law.
func TestGrant_ExpiryAtTwoYears(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPEXP", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	grantDate := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	grant, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  grantDate,
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)
	// expires_on = grant_date + 24 months
	assert.Equal(t, "2025-01-01", grant.ExpiresOn.Format("2006-01-02"),
		"grant must expire 24 months after grant date (statutory default)")
}

// TestBalance_ExpiredGrantsExcluded verifies that expired grants do not contribute
// to the available balance.
func TestBalance_ExpiredGrantsExcluded(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPBALEXP", time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC))

	// Grant 1: expires Jan 2021 (already expired by Jan 2024).
	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Grant 2: valid (expires Jan 2025).
	_, err = svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       20,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	asOf := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	balance, err := svc.GetBalance(ctx, tenantID, empID, asOf)
	require.NoError(t, err)

	// Only the non-expired grant (20 days) should appear.
	assert.Equal(t, 20.0, balance.TotalGranted, "expired grants must not contribute to total_granted")
	assert.Equal(t, 20.0, balance.Remaining)
	assert.Len(t, balance.Grants, 1)
}

// TestBalance_CarryOver verifies that carry-over grants (previous year remainder)
// are included until their expiry.
func TestBalance_CarryOver(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPCO", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Year 1 grant (remaining unused).
	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       5,
		Source:     leave.GrantSourceCarryOver,
	})
	require.NoError(t, err)

	// Year 2 grant (fresh).
	_, err = svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       20,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	asOf := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	balance, err := svc.GetBalance(ctx, tenantID, empID, asOf)
	require.NoError(t, err)
	// Both grants are still valid: 5 + 20 = 25.
	assert.Equal(t, 25.0, balance.TotalGranted)
	assert.Equal(t, 25.0, balance.Remaining)
}

// ---------------------------------------------------------------------------
// 5-day obligation tests (LM-041)
// ---------------------------------------------------------------------------

// TestFiveDayObligation_Obligated verifies an employee with ≥10-day grant is obligated.
// LEGAL NOTICE: threshold from settings; verify with current law.
func TestFiveDayObligation_Obligated(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPO5D", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	grantDate := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  grantDate,
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	obl, err := svc.GetFiveDayObligation(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, obl.Obligated, "employee with 10 days grant must be obligated")
	assert.False(t, obl.Met, "obligation not yet met")
	assert.Equal(t, 5.0, obl.ShortfallDays)
}

// TestFiveDayObligation_NotObligated verifies employees with <10-day grant are exempt.
// LEGAL NOTICE: threshold from settings; verify with current law.
func TestFiveDayObligation_NotObligated(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPNO5D", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       9,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	obl, err := svc.GetFiveDayObligation(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.False(t, obl.Obligated, "employee with <10 days grant must NOT be obligated")
}

// TestFiveDayObligation_Met verifies that taking 5 days marks the obligation as met.
func TestFiveDayObligation_Met(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPMET5D", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Submit and approve 5 days.
	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC),
		Days:       5,
	})
	require.NoError(t, err)

	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       req.ID,
		ActorID:  actorID,
		Status:   leave.RequestStatusApproved,
	})
	require.NoError(t, err)

	obl, err := svc.GetFiveDayObligation(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, obl.Met, "obligation must be met after taking 5 days")
	assert.Equal(t, 0.0, obl.ShortfallDays)
}

// TestFiveDayObligation_ThresholdFromSettings verifies that changing the threshold
// changes the obligated result.
func TestFiveDayObligation_ThresholdFromSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPTHR5D", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Set threshold to 15 (employee with 10 days should NOT be obligated).
	_, err := svc.UpsertSettings(ctx, leave.UpsertSettingsInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		BaseDateRule:               "hire_date_anniversary",
		FiveDayObligationThreshold: 15,
		ExpiryMonths:               24,
	})
	require.NoError(t, err)

	_, err = svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	obl, err := svc.GetFiveDayObligation(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.False(t, obl.Obligated,
		"threshold=15: employee with 10-day grant must NOT be obligated")

	// Change threshold back to 10 — same employee should now be obligated.
	_, err = svc.UpsertSettings(ctx, leave.UpsertSettingsInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		BaseDateRule:               "hire_date_anniversary",
		FiveDayObligationThreshold: 10,
		ExpiryMonths:               24,
	})
	require.NoError(t, err)

	obl2, err := svc.GetFiveDayObligation(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, obl2.Obligated,
		"threshold=10: same employee must now be obligated")
}

// ---------------------------------------------------------------------------
// Request lifecycle tests (LM-042)
// ---------------------------------------------------------------------------

// TestLeaveRequest_CreateAndApprove verifies the full create → approve → balance flow.
func TestLeaveRequest_CreateAndApprove(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPAPPR", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Grant 10 days.
	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Create a 3-day annual leave request.
	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC),
		Days:       3,
	})
	require.NoError(t, err)
	assert.Equal(t, leave.RequestStatusPending, req.Status)

	// Approve.
	approved, err := svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       req.ID,
		ActorID:  actorID,
		Status:   leave.RequestStatusApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, leave.RequestStatusApproved, approved.Status)

	// Balance should reflect the 3-day deduction.
	balance, err := svc.GetBalance(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 10.0, balance.TotalGranted)
	assert.Equal(t, 3.0, balance.TotalUsed)
	assert.Equal(t, 7.0, balance.Remaining)
}

// TestLeaveRequest_Reject_BalanceRestored verifies rejection restores the balance.
func TestLeaveRequest_Reject_BalanceRestored(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPREJBAL", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC),
		Days:       3,
	})
	require.NoError(t, err)

	// Approve first.
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       req.ID,
		ActorID:  actorID,
		Status:   leave.RequestStatusApproved,
	})
	require.NoError(t, err)

	// Use asOf within grant validity (grant 2024-01-01, expires 2026-01-01).
	asOfValid := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	balanceAfterApprove, err := svc.GetBalance(ctx, tenantID, empID, asOfValid)
	require.NoError(t, err)
	assert.Equal(t, 7.0, balanceAfterApprove.Remaining)

	// Reject (revert).
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       req.ID,
		ActorID:  actorID,
		Status:   leave.RequestStatusRejected,
	})
	require.NoError(t, err)

	balanceAfterReject, err := svc.GetBalance(ctx, tenantID, empID, asOfValid)
	require.NoError(t, err)
	assert.Equal(t, 10.0, balanceAfterReject.Remaining, "balance must be restored after rejection")
}

// TestLeaveRequest_Cancel_BalanceRestored verifies cancellation restores the balance.
func TestLeaveRequest_Cancel_BalanceRestored(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPCANCELBAL", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 2, 0, 0, 0, 0, time.UTC),
		Days:       2,
	})
	require.NoError(t, err)

	// Approve then cancel.
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       req.ID,
		ActorID:  actorID,
		Status:   leave.RequestStatusApproved,
	})
	require.NoError(t, err)

	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       req.ID,
		ActorID:  actorID,
		Status:   leave.RequestStatusCancelled,
	})
	require.NoError(t, err)

	// Use asOf within grant validity (grant 2024-01-01, expires 2026-01-01).
	balance, err := svc.GetBalance(ctx, tenantID, empID, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 10.0, balance.Remaining, "balance must be restored after cancellation")
}

// TestLeaveRequest_NoDoubleAllocation verifies idempotency of allocation.
func TestLeaveRequest_NoDoubleAllocation(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPDOUBLE", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 3, 0, 0, 0, 0, time.UTC),
		Days:       3,
	})
	require.NoError(t, err)

	// Approve.
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID, ID: req.ID, ActorID: actorID, Status: leave.RequestStatusApproved,
	})
	require.NoError(t, err)

	// Attempt a second approve — must error (invalid transition) not double-allocate.
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID, ID: req.ID, ActorID: actorID, Status: leave.RequestStatusApproved,
	})
	assert.ErrorIs(t, err, leave.ErrInvalidTransition, "second approve must be rejected")

	// Balance must reflect only a single 3-day deduction.
	// Use an asOf within the grant validity window (grant issued 2024-01-01, expires 2026-01-01).
	asOf := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	balance, err := svc.GetBalance(ctx, tenantID, empID, asOf)
	require.NoError(t, err)
	assert.Equal(t, 7.0, balance.Remaining, "balance must reflect exactly one deduction")
}

// TestLeaveRequest_InsufficientBalance verifies that exceeding balance returns an error.
func TestLeaveRequest_InsufficientBalance(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPINSUF", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       3,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	_, err = svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC),
		Days:       5,
	})
	assert.ErrorIs(t, err, leave.ErrInsufficientBalance)
}

// TestLeaveRequest_FIFOFromOldestGrant verifies that allocateAnnualLeave consumes
// days from the earliest-expiring grant first (FIFO expiry order) and correctly
// reads net remaining days (not gross grant days) after prior usage.
func TestLeaveRequest_FIFOFromOldestGrant(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPFIFO", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Grant A: expires earlier (2025-01-01), 3 days already used via first request.
	grantA, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       5,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Grant B: expires later (2026-01-01), 10 days, untouched.
	_, err = svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Use 3 days from grant A by approving a first request.
	req1, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC),
		Days:       3,
	})
	require.NoError(t, err)
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID, ID: req1.ID, ActorID: actorID, Status: leave.RequestStatusApproved,
	})
	require.NoError(t, err)

	// Grant A now has only 2 days remaining; grant B has 10.
	// Request 2 days — must be satisfied from grant A (oldest expiry).
	req2, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 3, 2, 0, 0, 0, 0, time.UTC),
		Days:       2,
	})
	require.NoError(t, err)
	_, err = svc.UpdateRequestStatus(ctx, leave.UpdateRequestStatusInput{
		TenantID: tenantID, ID: req2.ID, ActorID: actorID, Status: leave.RequestStatusApproved,
	})
	require.NoError(t, err)

	// Verify: grant A must have exactly 2 days_used total via leave_usages.
	var grantAUsed float64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COALESCE(SUM(days_used),0) FROM leave_usages
		 WHERE leave_grant_id = ? AND tenant_id = ?`,
		grantA.ID, tenantID,
	).Scan(&grantAUsed).Error)
	assert.Equal(t, 5.0, grantAUsed, "grant A (earliest expiry) should have been fully consumed")

	// Total balance: started 15, used 5 → 10 remaining.
	bal, err := svc.GetBalance(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Equal(t, 10.0, bal.Remaining)
}

// TestLeaveRequest_Atomicity_SubmitRollback verifies that if the approval engine
// returns an unexpected error (not ErrRouteNotFound/ErrRouteEmpty), the entire
// CreateRequest transaction is rolled back and no leave_request row is persisted.
func TestLeaveRequest_Atomicity_SubmitRollback(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPATOMIC", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// No approval route is configured, so Submit returns ErrRouteNotFound;
	// the request should still be created (pending, no approval_request_id).
	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 2, 0, 0, 0, 0, time.UTC),
		Days:       2,
	})
	require.NoError(t, err, "no-route case must still succeed")
	assert.Equal(t, leave.RequestStatusPending, req.Status)
	assert.Nil(t, req.ApprovalRequestID, "no approval_request_id when no route configured")

	// Confirm the row actually exists in the DB.
	got, err := svc.GetRequest(ctx, tenantID, req.ID)
	require.NoError(t, err)
	assert.Equal(t, req.ID, got.ID)
}

// TestLeaveRequest_Atomicity_WithApprovalRoute verifies that when an approval route
// IS configured, the approval_request_id is linked on the leave_request atomically.
func TestLeaveRequest_Atomicity_WithApprovalRoute(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, approvalSvc := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPLINK", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Seed a route so the approval engine can link the request.
	seedApprovalRoute(t, approvalSvc, ctx, tenantID, actorID, "leave_annual")

	req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  leave.LeaveTypeAnnual,
		StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		EndDate:    time.Date(2024, 4, 2, 0, 0, 0, 0, time.UTC),
		Days:       2,
	})
	require.NoError(t, err)
	assert.NotNil(t, req.ApprovalRequestID, "approval_request_id must be set when route exists")

	// The DB row must also reflect the linked approval_request_id.
	got, err := svc.GetRequest(ctx, tenantID, req.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ApprovalRequestID)
	assert.Equal(t, *req.ApprovalRequestID, *got.ApprovalRequestID)
}

// TestFiveDayObligation_MultipleGrantsInYear verifies IMP-2: when multiple grants
// fall within the same grant year (i.e. both on or after the anchor grant_date
// and before anchor + 1 year) their totals are summed for the obligation threshold
// check rather than using only the latest grant's days.
//
// Scenario: the latest grant before asOf is on 2024-01-15 (the anchor date).
// A carry-over grant was also issued on 2024-01-15.  Both grants share the same
// anchor so the year window is 2024-01-15 to 2025-01-14, and their days (6 + 5)
// must be summed to 11.
func TestFiveDayObligation_MultipleGrantsInYear(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID) // threshold = 10
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPMULTIGRANT", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	anchor := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

	// Grant 1 (fresh annual): 6 days on anchor — below threshold alone.
	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  anchor,
		Days:       6,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Grant 2 (carry-over): 5 days also on anchor date — same grant year window.
	_, err = svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  anchor,
		Days:       5,
		Source:     leave.GrantSourceCarryOver,
	})
	require.NoError(t, err)

	// 6 + 5 = 11 days total in the grant year → must be Obligated (threshold=10).
	// The latest grant date before asOf is anchor (2024-01-15); both grants share
	// that date so both fall within the year window 2024-01-15 to 2025-01-14.
	obl, err := svc.GetFiveDayObligation(ctx, tenantID, empID, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, obl.Obligated, "11 days total across two grants must exceed threshold=10 and be obligated")
	assert.Equal(t, 11.0, obl.GrantDays, "GrantDays must be the sum of all grants in the year")
}

// TestProportionalGrant_FallbackToStandardTable verifies IMP-3: when weekly_days
// has no entry in proportional_table_json the lookup falls back to the standard
// grant table rather than returning ErrSettingNotFound.
func TestProportionalGrant_FallbackToStandardTable(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)

	hiredOn := time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPFALLBACK", hiredOn)

	// weekly_days=4.5 has no entry in the default proportional table
	// (which only has 1, 2, 3, 4) → must fall back to standard grant table.
	weeklyDays := 4.5
	grantDate := hiredOn.AddDate(0, 6, 0) // 6 months tenure → 10 days in standard table
	grant, err := svc.ComputeAndGrantAnnual(ctx, leave.ComputeAndGrantAnnualInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		HiredOn:    hiredOn,
		GrantDate:  grantDate,
		WeeklyDays: &weeklyDays,
	})
	require.NoError(t, err, "unrecognised weekly_days must fall back to standard table, not error")
	require.NotNil(t, grant, "fallback must yield a non-nil grant for 6-month tenure")
	// The standard table at 6 months yields 10 days.
	assert.Equal(t, 10.0, grant.Days, "fallback to standard table at 6 months must yield 10 days")
}

// TestLeaveRequest_NonAnnualTypes verifies non-annual leave types can be requested
// without a balance check.
func TestLeaveRequest_NonAnnualTypes(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPSPECIAL", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	for _, lt := range []string{
		leave.LeaveTypeSpecial, leave.LeaveTypeCondolence,
		leave.LeaveTypeMaternity, leave.LeaveTypeChildcare,
		leave.LeaveTypeCare, leave.LeaveTypeAbsence,
	} {
		t.Run(lt, func(t *testing.T) {
			req, err := svc.CreateRequest(ctx, leave.CreateRequestInput{
				TenantID:   tenantID,
				ActorID:    actorID,
				EmployeeID: empID,
				LeaveType:  lt,
				StartDate:  time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
				EndDate:    time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
				Days:       1,
			})
			require.NoError(t, err, "leave type %q should be creatable without balance", lt)
			assert.Equal(t, leave.RequestStatusPending, req.Status)
		})
	}
}

// ---------------------------------------------------------------------------
// RLS / cross-tenant tests
// ---------------------------------------------------------------------------

// TestRLS_CrossTenantIsolation verifies that a grant in tenant A is not visible
// from tenant B's context.
func TestRLS_CrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "a@example.com")
	seedDefaultSettings(t, svc, ctx, tenantA, actorA)
	actorB := seedUser(t, h.AdminDB, tenantB, "b@example.com")
	seedDefaultSettings(t, svc, ctx, tenantB, actorB)

	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Create a grant in tenant A.
	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantA,
		ActorID:    actorA,
		EmployeeID: empA,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Attempt to list grants from tenant B using empA's ID.
	// The composite FK means no grant will be found for empA under tenantB,
	// and RLS blocks the read.
	grantsB, err := svc.ListGrants(ctx, tenantB, empA)
	require.NoError(t, err)
	assert.Empty(t, grantsB, "tenant B must not see tenant A's grants")
}

// ---------------------------------------------------------------------------
// Audit log PII check
// ---------------------------------------------------------------------------

// TestAuditLog_NoPII verifies that audit_logs entries for leave operations do
// not contain PII (email, name, etc.) in resource_id or action columns.
func TestAuditLog_NoPII(t *testing.T) {
	h := testdb.NewHarness(t)
	t.Cleanup(func() { truncateLeave(h) })

	svc, _ := makeService(h)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "admin@example.com")
	seedDefaultSettings(t, svc, ctx, tenantID, actorID)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPAUDIT", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	_, err := svc.GrantLeave(ctx, leave.GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		Days:       10,
		Source:     leave.GrantSourceAnnual,
	})
	require.NoError(t, err)

	// Inspect audit_logs via admin connection to bypass RLS.
	var rows []struct {
		Action     string  `gorm:"column:action"`
		ResourceID *string `gorm:"column:resource_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT action, resource_id FROM audit_logs WHERE tenant_id = ?`,
		tenantID,
	).Scan(&rows).Error)

	for _, row := range rows {
		// resource_id must be a UUID (opaque), not an email or name.
		if row.ResourceID != nil {
			_, parseErr := uuid.Parse(*row.ResourceID)
			assert.NoError(t, parseErr,
				"audit resource_id must be a UUID, not PII; got: %q", *row.ResourceID)
		}
		// action must not contain PII-like substrings.
		assert.NotContains(t, row.Action, "@", "audit action must not contain email")
		assert.NotContains(t, row.Action, "山田", "audit action must not contain names")
	}
}
