package hiring_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/hiring"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (synthetic data only — no real PII / keys / tokens)
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

func seedDepartment(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO departments (id, tenant_id, name, code) VALUES (?, ?, ?, ?)`,
		id, tenantID, "合成部署", code,
	).Error)
	return id
}

// seedTemplate inserts an onboarding_checklist_templates row (reused asset).
func seedTemplate(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, itemsJSON string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO onboarding_checklist_templates (id, tenant_id, name, kind, items_json, active)
		 VALUES (?, ?, '入社チェックリスト', 'onboarding', ?::jsonb, true)`,
		id, tenantID, itemsJSON,
	).Error)
	return id
}

func seedRoleWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, name, permsJSON string) uuid.UUID {
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
		"onboarding_surveys",
		"preboarding_requests",
		"new_hire_onboardings",
		"applicant_employee_links",
		"onboarding_tasks",
		"onboarding_checklist_templates",
		"employees",
		"departments",
		"roles",
		"users",
		"sessions",
		"tenants",
	)
}

// employeeStatus reads an employee's status directly (admin, bypasses RLS).
func employeeStatus(t *testing.T, adminDB *gorm.DB, empID uuid.UUID) string {
	t.Helper()
	var row struct {
		Status string `gorm:"column:status"`
	}
	require.NoError(t, adminDB.Raw(
		`SELECT status FROM employees WHERE id = ? LIMIT 1`, empID,
	).Scan(&row).Error)
	return row.Status
}

// ---------------------------------------------------------------------------
// Candidate → employee conversion (ATS-020 / ATS-021)
// ---------------------------------------------------------------------------

func TestConvertApplicantCreatesEmployeeAndTasks(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	tmplID := seedTemplate(t, h.AdminDB, tenantID,
		`[{"title":"社員証発行","category":"総務","due_offset_days":0},
		  {"title":"PC設定","category":"IT","due_offset_days":3}]`)
	t.Cleanup(func() { truncateAll(h) })

	applicantID := uuid.New()
	offerID := uuid.New()
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	res, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		ApplicantID:       applicantID,
		OfferID:           &offerID,
		EmployeeCode:      "EMP-NEW-001",
		LastName:          "山田",
		FirstName:         "太郎",
		EmploymentType:    "full_time",
		DepartmentID:      &deptID,
		TemplateID:        &tmplID,
		ExpectedStartDate: &start,
	})
	require.NoError(t, err)
	require.NotNil(t, res.Link)
	require.NotNil(t, res.Onboarding)
	assert.Equal(t, applicantID, res.Link.ApplicantID)
	assert.Equal(t, hiring.OnboardingStatusOfferAccepted, res.Onboarding.Status)
	assert.Len(t, res.Tasks, 2, "onboarding_tasks must be generated from the template")

	// Employee is created in pre-start 'inactive' status (activated at completion).
	assert.Equal(t, "inactive", employeeStatus(t, h.AdminDB, res.EmployeeID))

	// onboarding_tasks rows really exist for the new employee.
	var taskCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM onboarding_tasks WHERE employee_id = ? AND tenant_id = ?`,
		res.EmployeeID, tenantID,
	).Scan(&taskCount).Error)
	assert.Equal(t, int64(2), taskCount)

	// Link provenance row exists.
	var linkCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM applicant_employee_links WHERE applicant_id = ? AND tenant_id = ?`,
		applicantID, tenantID,
	).Scan(&linkCount).Error)
	assert.Equal(t, int64(1), linkCount)
}

// TestConvertApplicantIdempotent verifies the same candidate cannot generate a
// second employee — a re-fired trigger returns the existing conversion.
func TestConvertApplicantIdempotent(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	applicantID := uuid.New()
	in := hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    applicantID,
		EmployeeCode:   "EMP-IDEMP-001",
		LastName:       "山田",
		FirstName:      "花子",
		EmploymentType: "full_time",
	}

	first, err := svc.ConvertApplicant(ctx, in)
	require.NoError(t, err)

	// Second call with a different employee_code must NOT create a new employee.
	in2 := in
	in2.EmployeeCode = "EMP-IDEMP-002"
	second, err := svc.ConvertApplicant(ctx, in2)
	require.NoError(t, err)
	assert.Equal(t, first.EmployeeID, second.EmployeeID,
		"re-conversion must return the existing employee, not create a new one")

	// Exactly one employee and one link exist for this applicant.
	var empCount, linkCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE tenant_id = ?`, tenantID,
	).Scan(&empCount).Error)
	assert.Equal(t, int64(1), empCount, "exactly one employee must exist")
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM applicant_employee_links WHERE applicant_id = ? AND tenant_id = ?`,
		applicantID, tenantID,
	).Scan(&linkCount).Error)
	assert.Equal(t, int64(1), linkCount, "exactly one link must exist (idempotency)")
}

// TestConvertTemplateExpansionAtomic verifies that when task generation fails
// (template missing/invalid), the whole conversion is rolled back — no orphan
// employee/link/onboarding rows are left behind.
func TestConvertTemplateExpansionAtomic(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Reference a template id that does not exist → conversion must fail wholesale.
	missingTemplate := uuid.New()
	applicantID := uuid.New()

	_, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    applicantID,
		EmployeeCode:   "EMP-ATOMIC-001",
		LastName:       "山田",
		FirstName:      "次郎",
		EmploymentType: "full_time",
		TemplateID:     &missingTemplate,
	})
	require.ErrorIs(t, err, hiring.ErrNotFound)

	// No partial state: no employee, link, or onboarding rows.
	for _, tbl := range []string{"employees", "applicant_employee_links", "new_hire_onboardings", "onboarding_tasks"} {
		var cnt int64
		require.NoError(t, h.AdminDB.Raw(
			`SELECT COUNT(1) FROM `+tbl+` WHERE tenant_id = ?`, tenantID,
		).Scan(&cnt).Error)
		assert.Equal(t, int64(0), cnt, "no rows must remain in %s after a rolled-back conversion", tbl)
	}
}

// ---------------------------------------------------------------------------
// Status transitions: onboarding lifecycle + employee activation
// ---------------------------------------------------------------------------

func TestOnboardingLifecycleAndEmployeeActivation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-LC-001",
		LastName:       "山田",
		FirstName:      "三郎",
		EmploymentType: "full_time",
	})
	require.NoError(t, err)
	obID := res.Onboarding.ID

	// offer_accepted → preboarding
	ob, err := svc.AdvanceOnboarding(ctx, hiring.AdvanceOnboardingInput{
		TenantID: tenantID, ActorID: actorID, ID: obID, Status: hiring.OnboardingStatusPreboarding,
	})
	require.NoError(t, err)
	assert.Equal(t, hiring.OnboardingStatusPreboarding, ob.Status)

	// preboarding → onboarding
	ob, err = svc.AdvanceOnboarding(ctx, hiring.AdvanceOnboardingInput{
		TenantID: tenantID, ActorID: actorID, ID: obID, Status: hiring.OnboardingStatusOnboarding,
	})
	require.NoError(t, err)
	assert.Equal(t, hiring.OnboardingStatusOnboarding, ob.Status)

	// Employee still inactive until completion.
	assert.Equal(t, "inactive", employeeStatus(t, h.AdminDB, res.EmployeeID))

	// Complete → employee activated.
	hiredOn := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	ob, err = svc.CompleteOnboarding(ctx, hiring.CompleteOnboardingInput{
		TenantID: tenantID, ActorID: actorID, ID: obID, HiredOn: &hiredOn,
	})
	require.NoError(t, err)
	assert.Equal(t, hiring.OnboardingStatusCompleted, ob.Status)
	assert.Equal(t, "active", employeeStatus(t, h.AdminDB, res.EmployeeID),
		"employee must be activated on onboarding completion")
}

// TestInvalidOnboardingTransitions verifies boundary/illegal moves.
func TestInvalidOnboardingTransitions(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-INV-001",
		LastName:       "山田",
		FirstName:      "四郎",
		EmploymentType: "full_time",
	})
	require.NoError(t, err)
	obID := res.Onboarding.ID

	// offer_accepted → onboarding (skipping preboarding) must fail.
	_, err = svc.AdvanceOnboarding(ctx, hiring.AdvanceOnboardingInput{
		TenantID: tenantID, ActorID: actorID, ID: obID, Status: hiring.OnboardingStatusOnboarding,
	})
	assert.ErrorIs(t, err, hiring.ErrInvalidTransition, "skipping preboarding must be rejected")

	// Completing from offer_accepted (not onboarding) must fail.
	_, err = svc.CompleteOnboarding(ctx, hiring.CompleteOnboardingInput{
		TenantID: tenantID, ActorID: actorID, ID: obID,
	})
	assert.ErrorIs(t, err, hiring.ErrInvalidTransition,
		"completion is only allowed from onboarding status")

	// Employee must NOT be activated by the failed completion.
	assert.Equal(t, "inactive", employeeStatus(t, h.AdminDB, res.EmployeeID))
}

// ---------------------------------------------------------------------------
// Preboarding requests (ATS-022)
// ---------------------------------------------------------------------------

func TestPreboardingRequestLifecycle(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-PRE-001",
		LastName:       "山田",
		FirstName:      "五郎",
		EmploymentType: "full_time",
	})
	require.NoError(t, err)
	obID := res.Onboarding.ID

	req, err := svc.CreatePreboardingRequest(ctx, hiring.CreatePreboardingRequestInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		NewHireOnboardingID: obID,
		RequestType:         hiring.RequestTypeAccount,
		AssigneeUserID:      &actorID,
	})
	require.NoError(t, err)
	assert.Equal(t, hiring.RequestStatusRequested, req.Status)

	updated, err := svc.UpdatePreboardingRequestStatus(ctx, hiring.UpdatePreboardingRequestStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: req.ID, Status: hiring.RequestStatusCompleted,
	})
	require.NoError(t, err)
	assert.Equal(t, hiring.RequestStatusCompleted, updated.Status)

	// completed is terminal — further transition must fail.
	_, err = svc.UpdatePreboardingRequestStatus(ctx, hiring.UpdatePreboardingRequestStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: req.ID, Status: hiring.RequestStatusInProgress,
	})
	assert.ErrorIs(t, err, hiring.ErrInvalidTransition)

	list, err := svc.ListPreboardingRequests(ctx, tenantID, obID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestPreboardingRequestRejectsMissingOnboarding(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.CreatePreboardingRequest(ctx, hiring.CreatePreboardingRequestInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		NewHireOnboardingID: uuid.New(), // nonexistent
		RequestType:         hiring.RequestTypeEquipment,
	})
	assert.ErrorIs(t, err, hiring.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Surveys (ATS-023 stub)
// ---------------------------------------------------------------------------

func TestScheduleSurvey(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-SVY-001",
		LastName:       "山田",
		FirstName:      "六郎",
		EmploymentType: "full_time",
	})
	require.NoError(t, err)

	scheduledOn := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	survey, err := svc.ScheduleSurvey(ctx, hiring.ScheduleSurveyInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		NewHireOnboardingID: res.Onboarding.ID,
		SurveyType:          hiring.SurveyTypeOnboarding30d,
		ScheduledOn:         &scheduledOn,
	})
	require.NoError(t, err)
	assert.Equal(t, hiring.SurveyStatusScheduled, survey.Status)
	// employee_id is resolved from the parent onboarding header.
	assert.Equal(t, res.EmployeeID, survey.EmployeeID)

	list, err := svc.ListSurveys(ctx, tenantID, res.Onboarding.ID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation + composite FK cross-tenant rejection
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Conversion in tenant A.
	resA, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantA,
		ActorID:        actorA,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-RLS-A01",
		LastName:       "山田",
		FirstName:      "七郎",
		EmploymentType: "full_time",
	})
	require.NoError(t, err)
	obA := resA.Onboarding.ID

	// Tenant B cannot read tenant A's onboarding header.
	_, err = svc.GetOnboarding(ctx, tenantB, obA)
	assert.ErrorIs(t, err, hiring.ErrNotFound, "tenantB must not read tenantA onboarding")

	// Tenant B cannot advance tenant A's onboarding header.
	_, err = svc.AdvanceOnboarding(ctx, hiring.AdvanceOnboardingInput{
		TenantID: tenantB, ActorID: actorB, ID: obA, Status: hiring.OnboardingStatusPreboarding,
	})
	assert.ErrorIs(t, err, hiring.ErrNotFound, "tenantB must not mutate tenantA onboarding")

	// Tenant B cannot attach a preboarding request to tenant A's onboarding.
	_, err = svc.CreatePreboardingRequest(ctx, hiring.CreatePreboardingRequestInput{
		TenantID:            tenantB,
		ActorID:             actorB,
		NewHireOnboardingID: obA,
		RequestType:         hiring.RequestTypeAccount,
	})
	assert.ErrorIs(t, err, hiring.ErrNotFound,
		"cross-tenant preboarding request creation must be rejected")

	// Tenant B's onboarding list is empty.
	listB, err := svc.ListOnboardings(ctx, tenantB, "")
	require.NoError(t, err)
	assert.Empty(t, listB, "tenantB must not see tenantA onboardings")

	// Cross-tenant composite FK: a template owned by tenant A cannot be used in
	// a tenant B conversion (template lookup is tenant-scoped → ErrNotFound).
	tmplA := seedTemplate(t, h.AdminDB, tenantA, `[{"title":"T","category":"","due_offset_days":0}]`)
	_, err = svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantB,
		ActorID:        actorB,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-RLS-B01",
		LastName:       "山田",
		FirstName:      "八郎",
		EmploymentType: "full_time",
		TemplateID:     &tmplA, // belongs to tenant A
	})
	assert.ErrorIs(t, err, hiring.ErrNotFound,
		"using tenant A's template from tenant B must be rejected")
}

// ---------------------------------------------------------------------------
// RBAC permission enforcement (route-layer middleware)
// ---------------------------------------------------------------------------

// The hiring routes are guarded by ats:onboarding_read / ats:onboarding_write.
// RequirePermission resolves the actor's role via WithinTenant; this test
// exercises that path through the platform RBAC middleware behaviour by
// asserting permission resolution against seeded roles.
func TestPermissionResolution(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	writer := seedUser(t, h.AdminDB, tenantID, "writer@example.com")
	reader := seedUser(t, h.AdminDB, tenantID, "reader@example.com")
	noperm := seedUser(t, h.AdminDB, tenantID, "noperm@example.com")
	t.Cleanup(func() { truncateAll(h) })

	writeRole := seedRoleWithPermissions(t, h.AdminDB, tenantID, "hiring_writer",
		`{"perms":["ats:onboarding_write","ats:onboarding_read"]}`)
	readRole := seedRoleWithPermissions(t, h.AdminDB, tenantID, "hiring_reader",
		`{"perms":["ats:onboarding_read"]}`)
	assignRole(t, h.AdminDB, writer, writeRole)
	assignRole(t, h.AdminDB, reader, readRole)

	tdb := tenantdb.New(h.AppDB)

	check := func(userID uuid.UUID, need string) bool {
		var allowed bool
		require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			perms, err := platformauth.LoadUserPermissions(tx, tenantID, userID)
			if err != nil {
				return err
			}
			allowed = platformauth.HasPermission(perms, need)
			return nil
		}))
		return allowed
	}

	assert.True(t, check(writer, "ats:onboarding_write"), "writer must hold write")
	assert.True(t, check(reader, "ats:onboarding_read"), "reader must hold read")
	assert.False(t, check(reader, "ats:onboarding_write"), "reader must NOT hold write")
	assert.False(t, check(noperm, "ats:onboarding_read"), "no-role user must hold nothing")
}

// ---------------------------------------------------------------------------
// Audit: no PII leakage
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := hiring.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Distinctive synthetic name/email to scan for in audit rows.
	email := "山田太郎-pii@example.com"
	_, err := svc.ConvertApplicant(ctx, hiring.ConvertApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ApplicantID:    uuid.New(),
		EmployeeCode:   "EMP-PII-001",
		LastName:       "山田太郎PII",
		FirstName:      "個人情報",
		Email:          &email,
		EmploymentType: "full_time",
	})
	require.NoError(t, err)

	// No audit row may contain the synthetic name or email fragments.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR resource_id LIKE ? OR resource_id LIKE ?
		    OR resource_type LIKE ? OR action LIKE ?`,
		"%山田太郎PII%", "%個人情報%", "%pii@example.com%",
		"%山田太郎PII%", "%山田太郎PII%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain name/email PII — only opaque UUIDs")

	// And there is at least one audit row recorded (the conversion).
	var auditCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs WHERE tenant_id = ? AND action = 'hiring.applicant_converted'`,
		tenantID,
	).Scan(&auditCount).Error)
	assert.Equal(t, int64(1), auditCount)
}
