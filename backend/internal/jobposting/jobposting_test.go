package jobposting_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/jobposting"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go conventions)
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
		id, tenantID, "開発部", code,
	).Error)
	return id
}

// seedRoleWithPermissions inserts a role with a JSON perms array, e.g.
// `{"perms":["ats:read_budget"]}`, and returns its ID.
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
		"job_posting_interviewers",
		"job_postings",
		"departments",
		"users",
		"roles",
		"employees",
		"sessions",
		"tenants",
	)
}

// newInt64 returns a pointer to the given int64 (test helper).
func newInt64(v int64) *int64 { return &v }

// ---------------------------------------------------------------------------
// Create / Get / List (正常系 CRUD)
// ---------------------------------------------------------------------------

func TestCreateAndGetJobPosting(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	recruiterID := seedUser(t, h.AdminDB, tenantID, "recruiter@example.com")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		Title:           "バックエンドエンジニア",
		EmploymentType:  "full_time",
		DepartmentID:    deptID,
		RecruiterUserID: &recruiterID,
		RequirementsJSON: []byte(
			`{"description":"Go経験","location":"東京"}`),
		SalaryRangeMin: newInt64(6000000),
		SalaryRangeMax: newInt64(9000000),
		HiringBudget:   newInt64(12000000),
		RetentionLabel: "3years",
	})
	require.NoError(t, err)
	assert.Equal(t, jobposting.StatusDraft, jp.Status)
	assert.False(t, jp.PublicPublished)
	assert.NotEmpty(t, jp.PublicSlug)
	assert.NotContains(t, jp.PublicSlug, jp.ID.String(), "slug must be opaque, not derived from row id")

	// Get with budget permission granted to the actor.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "manager",
		`{"perms":["ats:read_budget"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)

	got, err := svc.GetJobPosting(ctx, jobposting.GetJobPostingInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		ID:         jp.ID,
		ReadBudget: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "バックエンドエンジニア", got.Title)
	require.NotNil(t, got.SalaryRangeMin, "budget reader must see salary range")
	assert.Equal(t, int64(6000000), *got.SalaryRangeMin)
	require.NotNil(t, got.HiringBudget)
	assert.Equal(t, int64(12000000), *got.HiringBudget)
}

func TestListJobPostingsFilters(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptA := seedDepartment(t, h.AdminDB, tenantID, "DEVA")
	deptB := seedDepartment(t, h.AdminDB, tenantID, "DEVB")
	t.Cleanup(func() { truncateAll(h) })

	for i, dept := range []uuid.UUID{deptA, deptA, deptB} {
		_, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
			TenantID: tenantID, ActorID: actorID,
			Title: "求人" + string(rune('A'+i)), EmploymentType: "full_time",
			DepartmentID: dept,
		})
		require.NoError(t, err)
	}

	all, err := svc.ListJobPostings(ctx, jobposting.ListJobPostingsInput{
		TenantID: tenantID, ActorID: actorID,
	})
	require.NoError(t, err)
	assert.Len(t, all, 3)

	byDept, err := svc.ListJobPostings(ctx, jobposting.ListJobPostingsInput{
		TenantID: tenantID, ActorID: actorID, DepartmentID: &deptA,
	})
	require.NoError(t, err)
	assert.Len(t, byDept, 2, "department filter must narrow results")

	byStatus, err := svc.ListJobPostings(ctx, jobposting.ListJobPostingsInput{
		TenantID: tenantID, ActorID: actorID, Status: jobposting.StatusOpen,
	})
	require.NoError(t, err)
	assert.Empty(t, byStatus, "no postings are open yet")
}

// ---------------------------------------------------------------------------
// Validation: required fields (acceptanceCriteria #1)
// ---------------------------------------------------------------------------

func TestCreateJobPostingRequiredFields(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	t.Cleanup(func() { truncateAll(h) })

	// Missing title.
	_, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "  ", EmploymentType: "full_time", DepartmentID: deptID,
	})
	assert.ErrorIs(t, err, jobposting.ErrValidation, "blank title must be rejected")

	// Missing employment_type.
	_, err = svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "", DepartmentID: deptID,
	})
	assert.ErrorIs(t, err, jobposting.ErrValidation, "blank employment_type must be rejected")

	// Missing department.
	_, err = svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: uuid.Nil,
	})
	assert.ErrorIs(t, err, jobposting.ErrValidation, "nil department_id must be rejected")
}

// ---------------------------------------------------------------------------
// Status transitions (acceptanceCriteria #2 / testFocus 境界)
// ---------------------------------------------------------------------------

func TestStatusTransitionsHappyPath(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptID,
	})
	require.NoError(t, err)

	// draft → open (publishes).
	opened, _, err := svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusOpen,
	})
	require.NoError(t, err)
	assert.Equal(t, jobposting.StatusOpen, opened.Status)
	assert.True(t, opened.PublicPublished, "open must publish the listing")

	// open → on_hold.
	held, _, err := svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusOnHold,
	})
	require.NoError(t, err)
	assert.Equal(t, jobposting.StatusOnHold, held.Status)

	// on_hold → open.
	reopened, _, err := svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusOpen,
	})
	require.NoError(t, err)
	assert.Equal(t, jobposting.StatusOpen, reopened.Status)

	// open → closed (withdraws listing).
	closed, _, err := svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusClosed,
	})
	require.NoError(t, err)
	assert.Equal(t, jobposting.StatusClosed, closed.Status)
	assert.False(t, closed.PublicPublished, "close must withdraw the public listing")
}

func TestStatusTransitionInvalidIsRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptID,
	})
	require.NoError(t, err)

	// Move to closed via open.
	_, _, err = svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusOpen,
	})
	require.NoError(t, err)
	_, _, err = svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusClosed,
	})
	require.NoError(t, err)

	// closed → open must be rejected (closed is terminal).
	_, _, err = svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusOpen,
	})
	assert.ErrorIs(t, err, jobposting.ErrInvalidTransition,
		"closed → open must be rejected (closed is terminal)")
}

func TestStatusTransitionNotFound(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, _, err := svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: uuid.New(), Status: jobposting.StatusOpen,
	})
	assert.ErrorIs(t, err, jobposting.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Item-level budget permission (acceptanceCriteria #7 / testFocus 項目レベル権限)
// ---------------------------------------------------------------------------

func TestBudgetFieldsHiddenWithoutPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	// Actor has NO role → no ats:read_budget.
	actorID := seedUser(t, h.AdminDB, tenantID, "noperm@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptID,
		SalaryRangeMin: newInt64(5000000),
		SalaryRangeMax: newInt64(8000000),
		HiringBudget:   newInt64(10000000),
	})
	require.NoError(t, err)

	// Read requesting budget but without the permission → budget fields cleared.
	got, err := svc.GetJobPosting(ctx, jobposting.GetJobPostingInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, ReadBudget: true,
	})
	require.NoError(t, err)
	assert.Nil(t, got.SalaryRangeMin, "salary range must be hidden without ats:read_budget")
	assert.Nil(t, got.SalaryRangeMax, "salary range must be hidden without ats:read_budget")
	assert.Nil(t, got.HiringBudget, "hiring budget must be hidden without ats:read_budget")

	// List must also hide budget fields.
	list, err := svc.ListJobPostings(ctx, jobposting.ListJobPostingsInput{
		TenantID: tenantID, ActorID: actorID, ReadBudget: true,
	})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Nil(t, list[0].HiringBudget, "list must hide budget without permission")
}

func TestBudgetFieldsVisibleWithPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "mgr@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "manager",
		`{"perms":["ats:read_budget"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptID,
		HiringBudget: newInt64(10000000),
	})
	require.NoError(t, err)

	got, err := svc.GetJobPosting(ctx, jobposting.GetJobPostingInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, ReadBudget: true,
	})
	require.NoError(t, err)
	require.NotNil(t, got.HiringBudget)
	assert.Equal(t, int64(10000000), *got.HiringBudget)
}

// ---------------------------------------------------------------------------
// Composite FK cross-tenant prevention (acceptanceCriteria #4 / testFocus)
// ---------------------------------------------------------------------------

func TestCreateRejectsCrossTenantDepartment(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	deptA := seedDepartment(t, h.AdminDB, tenantA, "DEVA") // belongs to tenant A
	t.Cleanup(func() { truncateAll(h) })

	// tenant B context cannot reference tenant A's department.
	_, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantB, ActorID: actorB,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptA,
	})
	assert.Error(t, err, "cross-tenant department reference must fail")
}

func TestCreateRejectsCrossTenantRecruiter(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	deptB := seedDepartment(t, h.AdminDB, tenantB, "DEVB")
	recruiterA := seedUser(t, h.AdminDB, tenantA, "recruitera@example.com") // tenant A user
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantB, ActorID: actorB,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptB,
		RecruiterUserID: &recruiterA, // cross-tenant user
	})
	assert.ErrorIs(t, err, jobposting.ErrNotFound,
		"cross-tenant recruiter assignment must be rejected by service-layer check")
}

func TestAssignInterviewerRejectsCrossTenantUser(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	deptB := seedDepartment(t, h.AdminDB, tenantB, "DEVB")
	interviewerA := seedUser(t, h.AdminDB, tenantA, "interviewera@example.com")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantB, ActorID: actorB,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptB,
	})
	require.NoError(t, err)

	_, err = svc.AssignInterviewer(ctx, jobposting.AssignInterviewerInput{
		TenantID: tenantB, ActorID: actorB, JobPostingID: jp.ID, UserID: interviewerA,
	})
	assert.ErrorIs(t, err, jobposting.ErrNotFound,
		"cross-tenant interviewer assignment must be rejected")
}

// ---------------------------------------------------------------------------
// Interviewer assignment happy path + dedup
// ---------------------------------------------------------------------------

func TestAssignAndListInterviewers(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	iv1 := seedUser(t, h.AdminDB, tenantID, "iv1@example.com")
	iv2 := seedUser(t, h.AdminDB, tenantID, "iv2@example.com")
	t.Cleanup(func() { truncateAll(h) })

	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: "Engineer", EmploymentType: "full_time", DepartmentID: deptID,
	})
	require.NoError(t, err)

	_, err = svc.AssignInterviewer(ctx, jobposting.AssignInterviewerInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jp.ID, UserID: iv1,
	})
	require.NoError(t, err)
	_, err = svc.AssignInterviewer(ctx, jobposting.AssignInterviewerInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jp.ID, UserID: iv2,
	})
	require.NoError(t, err)
	// Duplicate assignment is idempotent (ON CONFLICT DO NOTHING).
	_, err = svc.AssignInterviewer(ctx, jobposting.AssignInterviewerInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jp.ID, UserID: iv1,
	})
	require.NoError(t, err)

	list, err := svc.ListInterviewers(ctx, tenantID, jp.ID)
	require.NoError(t, err)
	assert.Len(t, list, 2, "duplicate assignment must not create a second row")

	// Remove one.
	require.NoError(t, svc.RemoveInterviewer(ctx, jobposting.RemoveInterviewerInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jp.ID, UserID: iv1,
	}))
	list, err = svc.ListInterviewers(ctx, tenantID, jp.ID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation (acceptanceCriteria #3 / testFocus 越境)
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	deptA := seedDepartment(t, h.AdminDB, tenantA, "DEVA")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Create a posting in tenant A.
	jpA, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantA, ActorID: actorA,
		Title: "A社の求人", EmploymentType: "full_time", DepartmentID: deptA,
	})
	require.NoError(t, err)

	// tenant B cannot SELECT tenant A's posting → ErrNotFound (RLS + explicit WHERE).
	_, err = svc.GetJobPosting(ctx, jobposting.GetJobPostingInput{
		TenantID: tenantB, ActorID: actorB, ID: jpA.ID,
	})
	assert.ErrorIs(t, err, jobposting.ErrNotFound,
		"tenant B must not read tenant A's posting")

	// tenant B cannot UPDATE status of tenant A's posting.
	_, _, err = svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantB, ActorID: actorB, ID: jpA.ID, Status: jobposting.StatusOpen,
	})
	assert.ErrorIs(t, err, jobposting.ErrNotFound,
		"tenant B must not update tenant A's posting")

	// tenant B's List must be empty (does not see tenant A's posting).
	list, err := svc.ListJobPostings(ctx, jobposting.ListJobPostingsInput{
		TenantID: tenantB, ActorID: actorB,
	})
	require.NoError(t, err)
	assert.Empty(t, list, "tenant B List must not include tenant A postings")

	// Confirm tenant A still sees its own posting.
	listA, err := svc.ListJobPostings(ctx, jobposting.ListJobPostingsInput{
		TenantID: tenantA, ActorID: actorA,
	})
	require.NoError(t, err)
	assert.Len(t, listA, 1)
}

// ---------------------------------------------------------------------------
// Audit log: PII non-storage + opaque resource IDs (acceptanceCriteria #5)
// ---------------------------------------------------------------------------

func TestAuditLogRecordsOpaqueIDsOnly(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := jobposting.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEV")
	t.Cleanup(func() { truncateAll(h) })

	const sensitiveTitle = "極秘プロジェクト募集_社外秘"
	jp, err := svc.CreateJobPosting(ctx, jobposting.CreateJobPostingInput{
		TenantID: tenantID, ActorID: actorID,
		Title: sensitiveTitle, EmploymentType: "full_time", DepartmentID: deptID,
	})
	require.NoError(t, err)

	// Trigger an open (audit: job_posting.opened).
	_, _, err = svc.UpdateStatus(ctx, jobposting.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: jp.ID, Status: jobposting.StatusOpen,
	})
	require.NoError(t, err)

	// audit_logs must not contain the posting title (PII / business-sensitive)
	// in any field; resource_id must be the opaque UUID.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%極秘%", "%極秘%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain the job posting title or other sensitive text")

	// Confirm the resource_id stored equals the opaque posting UUID.
	var resourceID string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs
		 WHERE tenant_id = ? AND action = 'job_posting.created' LIMIT 1`,
		tenantID,
	).Scan(&resourceID).Error)
	assert.Equal(t, jp.ID.String(), resourceID,
		"resource_id must be the opaque posting UUID")
}
