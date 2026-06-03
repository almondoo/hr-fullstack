package talent_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
	"github.com/your-org/hr-saas/internal/talent"
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

func seedDepartment(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code, name string, parent *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO departments (id, tenant_id, parent_id, name, code) VALUES (?, ?, ?, ?, ?)`,
		id, tenantID, parent, name, code,
	).Error)
	return id
}

func seedEmployee(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code, status string, dept *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status, department_id)
		 VALUES (?, ?, ?, '山田', '太郎', 'full_time', ?, ?)`,
		id, tenantID, code, status, dept,
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
		"pulse_survey_responses",
		"pulse_surveys",
		"placement_simulation_items",
		"placement_simulations",
		"employee_certifications",
		"employee_skills",
		"skills",
		"employee_assignments",
		"employees",
		"departments",
		"users",
		"roles",
		"tenants",
	)
}

func syntheticKey() []byte { return bytes.Repeat([]byte{0x42}, 32) }

func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

func ptrStr(s string) *string { return &s }

func dateUTC(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ---------------------------------------------------------------------------
// Skill master + employee skill tests (TM-020)
// ---------------------------------------------------------------------------

func TestCreateAndListSkills(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	sk, err := svc.CreateSkill(ctx, talent.CreateSkillInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		Category:   "プログラミング",
		Name:       "Go",
		LevelsJSON: []byte(`{"min":1,"max":5}`),
	})
	require.NoError(t, err)
	assert.True(t, sk.Active)

	list, err := svc.ListSkills(ctx, tenantID, "プログラミング")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, sk.ID, list[0].ID)
}

func TestAssignSkillLevelValidation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active", nil)
	t.Cleanup(func() { truncateAll(h) })

	sk, err := svc.CreateSkill(ctx, talent.CreateSkillInput{
		TenantID: tenantID, ActorID: actorID, Category: "言語", Name: "英語",
		LevelsJSON: []byte(`{"min":1,"max":5}`),
	})
	require.NoError(t, err)

	// Valid level — upsert succeeds.
	es, err := svc.AssignSkill(ctx, talent.AssignSkillInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, SkillID: sk.ID, Level: 3,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, es.Level)

	// Upsert: re-assign with a different level updates in place.
	es2, err := svc.AssignSkill(ctx, talent.AssignSkillInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, SkillID: sk.ID, Level: 4,
	})
	require.NoError(t, err)
	assert.Equal(t, es.ID, es2.ID, "upsert must keep the same row")
	assert.Equal(t, 4, es2.Level)

	// Out-of-range level — rejected.
	_, err = svc.AssignSkill(ctx, talent.AssignSkillInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, SkillID: sk.ID, Level: 9,
	})
	assert.ErrorIs(t, err, talent.ErrInvalidLevel, "level above max must be rejected")
}

func TestSearchSkillHoldersAndMatrix(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "D1", "開発部", nil)
	emp1 := seedEmployee(t, h.AdminDB, tenantID, "E1", "active", &deptID)
	emp2 := seedEmployee(t, h.AdminDB, tenantID, "E2", "active", &deptID)
	t.Cleanup(func() { truncateAll(h) })

	sk, err := svc.CreateSkill(ctx, talent.CreateSkillInput{
		TenantID: tenantID, ActorID: actorID, Category: "言語", Name: "Go",
		LevelsJSON: []byte(`{"min":1,"max":5}`),
	})
	require.NoError(t, err)

	_, err = svc.AssignSkill(ctx, talent.AssignSkillInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: emp1, SkillID: sk.ID, Level: 4,
	})
	require.NoError(t, err)
	_, err = svc.AssignSkill(ctx, talent.AssignSkillInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: emp2, SkillID: sk.ID, Level: 2,
	})
	require.NoError(t, err)

	// minLevel=3 returns only emp1.
	holders, err := svc.SearchSkillHolders(ctx, tenantID, sk.ID, 3)
	require.NoError(t, err)
	require.Len(t, holders, 1)
	assert.Equal(t, emp1, holders[0].EmployeeID)
	assert.Equal(t, 4, holders[0].Level)

	// minLevel=1 returns both.
	holders, err = svc.SearchSkillHolders(ctx, tenantID, sk.ID, 1)
	require.NoError(t, err)
	assert.Len(t, holders, 2)

	// Skill matrix: one (dept, skill) cell with 2 holders.
	matrix, err := svc.SkillMatrix(ctx, tenantID)
	require.NoError(t, err)
	require.Len(t, matrix, 1)
	assert.Equal(t, 2, matrix[0].HolderCount)
	assert.InDelta(t, 3.0, matrix[0].AvgLevel, 0.001) // (4+2)/2
}

// ---------------------------------------------------------------------------
// Certification + expiry alert (boundary) tests (TM-020)
// ---------------------------------------------------------------------------

func TestCertificationExpiryAlertBoundary(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP010", "active", nil)
	t.Cleanup(func() { truncateAll(h) })

	soon := time.Now().AddDate(0, 0, 10)   // within 30 days
	later := time.Now().AddDate(0, 0, 100) // outside 30 days

	_, err := svc.AddCertification(ctx, talent.AddCertificationInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		Name: "基本情報技術者", ExpiresOn: &soon, RenewalRequired: true,
	})
	require.NoError(t, err)
	_, err = svc.AddCertification(ctx, talent.AddCertificationInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		Name: "応用情報技術者", ExpiresOn: &later,
	})
	require.NoError(t, err)

	expiring, err := svc.ExpiringCertifications(ctx, tenantID, 30)
	require.NoError(t, err)
	require.Len(t, expiring, 1, "only the cert within 30 days must be returned")
	assert.Equal(t, "基本情報技術者", expiring[0].Name)
}

// ---------------------------------------------------------------------------
// Integrated profile masking tests (TM-020)
// ---------------------------------------------------------------------------

func TestIntegratedProfileMasksGradeWithoutPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	deptID := seedDepartment(t, h.AdminDB, tenantID, "D1", "営業部", nil)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP020", "active", &deptID)
	// Seed an assignment with a grade (compensation-related).
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO employee_assignments
		   (id, tenant_id, employee_id, department_id, position, grade, effective_from)
		 VALUES (?, ?, ?, ?, '課長', 'G5', '2026-01-01')`,
		uuid.New(), tenantID, empID, deptID,
	).Error)

	// Actor WITHOUT talent:read_sensitive.
	plainActor := seedUser(t, h.AdminDB, tenantID, "plain@example.com")
	// Actor WITH talent:read_sensitive.
	sensActor := seedUser(t, h.AdminDB, tenantID, "sensitive@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "hr_admin",
		`{"perms":["talent:read_sensitive"]}`)
	assignRole(t, h.AdminDB, sensActor, roleID)
	t.Cleanup(func() { truncateAll(h) })

	// Without permission → grade masked.
	p, err := svc.GetIntegratedProfile(ctx, talent.GetProfileInput{
		TenantID: tenantID, ActorID: plainActor, EmployeeID: empID,
	})
	require.NoError(t, err)
	assert.True(t, p.SensitiveMasked)
	require.Len(t, p.Assignments, 1)
	assert.Nil(t, p.Assignments[0].Grade, "grade must be masked without talent:read_sensitive")
	assert.NotNil(t, p.Assignments[0].Position, "position is not sensitive")

	// With permission → grade visible.
	p2, err := svc.GetIntegratedProfile(ctx, talent.GetProfileInput{
		TenantID: tenantID, ActorID: sensActor, EmployeeID: empID,
	})
	require.NoError(t, err)
	assert.False(t, p2.SensitiveMasked)
	require.Len(t, p2.Assignments, 1)
	require.NotNil(t, p2.Assignments[0].Grade)
	assert.Equal(t, "G5", *p2.Assignments[0].Grade)
}

// ---------------------------------------------------------------------------
// Org tree (TM-021)
// ---------------------------------------------------------------------------

func TestOrgTree(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	root := seedDepartment(t, h.AdminDB, tenantID, "ROOT", "本社", nil)
	child := seedDepartment(t, h.AdminDB, tenantID, "DEV", "開発部", &root)
	seedEmployee(t, h.AdminDB, tenantID, "E1", "active", &child)
	seedEmployee(t, h.AdminDB, tenantID, "E2", "left", &child) // excluded from head count
	t.Cleanup(func() { truncateAll(h) })

	roots, err := svc.GetOrgTree(ctx, tenantID)
	require.NoError(t, err)
	require.Len(t, roots, 1)
	assert.Equal(t, root, roots[0].DepartmentID)
	require.Len(t, roots[0].Children, 1)
	assert.Equal(t, child, roots[0].Children[0].DepartmentID)
	assert.Equal(t, 1, roots[0].Children[0].EmployeeCount, "left employee must be excluded")
}

// ---------------------------------------------------------------------------
// Placement simulation: non-destructive + apply atomicity + double-apply
// (TM-021)
// ---------------------------------------------------------------------------

func TestPlacementSimulationNonDestructiveAndApply(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	srcDept := seedDepartment(t, h.AdminDB, tenantID, "SRC", "現部署", nil)
	dstDept := seedDepartment(t, h.AdminDB, tenantID, "DST", "異動先", nil)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP030", "active", &srcDept)
	t.Cleanup(func() { truncateAll(h) })

	sim, err := svc.CreateSimulation(ctx, talent.CreateSimulationInput{
		TenantID: tenantID, ActorID: actorID, Name: "2026春異動案",
	})
	require.NoError(t, err)

	_, err = svc.AddSimulationItem(ctx, talent.AddSimulationItemInput{
		TenantID: tenantID, ActorID: actorID, SimulationID: sim.ID, EmployeeID: empID,
		TargetDepartmentID: &dstDept, TargetPosition: ptrStr("主任"), TargetGrade: ptrStr("G4"),
		EffectiveFrom: dateUTC(2026, 4, 1),
	})
	require.NoError(t, err)

	// Non-destructive: while draft, no employee_assignments row exists.
	var assignCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employee_assignments WHERE employee_id = ?`, empID,
	).Scan(&assignCount).Error)
	assert.Equal(t, int64(0), assignCount, "draft simulation must not write employee_assignments")

	// Employee's current department is unchanged.
	var empRow struct {
		DepartmentID *uuid.UUID `gorm:"column:department_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT department_id FROM employees WHERE id = ?`, empID,
	).Scan(&empRow).Error)
	require.NotNil(t, empRow.DepartmentID)
	assert.Equal(t, srcDept, *empRow.DepartmentID, "draft must not mutate the real org")

	// Apply → maps the item to employee_assignments.
	applied, err := svc.ApplySimulation(ctx, talent.ApplySimulationInput{
		TenantID: tenantID, ActorID: actorID, SimulationID: sim.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, applied)

	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employee_assignments WHERE employee_id = ? AND department_id = ?`,
		empID, dstDept,
	).Scan(&assignCount).Error)
	assert.Equal(t, int64(1), assignCount, "apply must write one assignment to the target dept")

	// Double-apply prevention.
	_, err = svc.ApplySimulation(ctx, talent.ApplySimulationInput{
		TenantID: tenantID, ActorID: actorID, SimulationID: sim.ID,
	})
	assert.ErrorIs(t, err, talent.ErrInvalidTransition, "re-applying an applied simulation must fail")
}

func TestDiscardSimulation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	sim, err := svc.CreateSimulation(ctx, talent.CreateSimulationInput{
		TenantID: tenantID, ActorID: actorID, Name: "破棄案",
	})
	require.NoError(t, err)
	require.NoError(t, svc.DiscardSimulation(ctx, talent.DiscardSimulationInput{
		TenantID: tenantID, ActorID: actorID, SimulationID: sim.ID,
	}))
	// Applying a discarded simulation must fail.
	_, err = svc.ApplySimulation(ctx, talent.ApplySimulationInput{
		TenantID: tenantID, ActorID: actorID, SimulationID: sim.ID,
	})
	assert.ErrorIs(t, err, talent.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Pulse survey: anonymity, min-disclosure threshold, free-text encryption
// (TM-022)
// ---------------------------------------------------------------------------

func openSurvey(t *testing.T, ctx context.Context, svc *talent.Service, tenantID, actorID uuid.UUID, anonymous bool, minResp int) uuid.UUID {
	t.Helper()
	sv, err := svc.CreateSurvey(ctx, talent.CreateSurveyInput{
		TenantID: tenantID, ActorID: actorID, Title: "パルス",
		QuestionsJSON: []byte(`[{"key":"q1","text":"満足度"}]`),
		Anonymous:     anonymous, MinResponsesToShow: minResp,
	})
	require.NoError(t, err)
	_, err = svc.SetSurveyStatus(ctx, talent.SetSurveyStatusInput{
		TenantID: tenantID, ActorID: actorID, SurveyID: sv.ID, Status: talent.SurveyStatusOpen,
	})
	require.NoError(t, err)
	return sv.ID
}

func TestAnonymousSurveyDoesNotStoreRespondent(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP040", "active", nil)
	t.Cleanup(func() { truncateAll(h) })

	surveyID := openSurvey(t, ctx, svc, tenantID, actorID, true, 1)

	// Even when an employee_id is supplied, an anonymous survey must store NULL.
	_, err := svc.SubmitResponse(ctx, talent.SubmitResponseInput{
		TenantID: tenantID, ActorID: actorID, SurveyID: surveyID,
		EmployeeID: &empID, AnswersJSON: []byte(`{"q1":"4"}`),
		FreeTextPlaintext: []byte("上司との関係に少し悩んでいます"),
	})
	require.NoError(t, err)

	var respRow struct {
		RespondentEmployeeID *uuid.UUID `gorm:"column:respondent_employee_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT respondent_employee_id FROM pulse_survey_responses WHERE survey_id = ? LIMIT 1`,
		surveyID,
	).Scan(&respRow).Error)
	assert.Nil(t, respRow.RespondentEmployeeID, "anonymous survey must NOT store the respondent (reverse-lookup prohibited)")
}

func TestSurveyMinDisclosureThreshold(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	surveyID := openSurvey(t, ctx, svc, tenantID, actorID, true, 3)

	// 2 responses < threshold(3) → suppressed.
	for i := 0; i < 2; i++ {
		_, err := svc.SubmitResponse(ctx, talent.SubmitResponseInput{
			TenantID: tenantID, ActorID: actorID, SurveyID: surveyID,
			AnswersJSON: []byte(`{"q1":"4"}`),
		})
		require.NoError(t, err)
	}
	agg, err := svc.AggregateSurvey(ctx, tenantID, surveyID)
	require.NoError(t, err)
	assert.True(t, agg.Suppressed, "below threshold must be suppressed")
	assert.Empty(t, agg.AnswerSummary)

	// 3rd response reaches threshold → summary revealed.
	_, err = svc.SubmitResponse(ctx, talent.SubmitResponseInput{
		TenantID: tenantID, ActorID: actorID, SurveyID: surveyID,
		AnswersJSON: []byte(`{"q1":"2"}`),
	})
	require.NoError(t, err)
	agg, err = svc.AggregateSurvey(ctx, tenantID, surveyID)
	require.NoError(t, err)
	assert.False(t, agg.Suppressed)
	require.Contains(t, agg.AnswerSummary, "q1")
	assert.InDelta(t, (4.0+4.0+2.0)/3.0, agg.AnswerSummary["q1"], 0.001)
}

func TestFreeTextEncryptionRoundTripAndPermission(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	// Reader WITH survey:read_freetext.
	reader := seedUser(t, h.AdminDB, tenantID, "reader@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "ft_reader",
		`{"perms":["survey:read_freetext"]}`)
	assignRole(t, h.AdminDB, reader, roleID)
	t.Cleanup(func() { truncateAll(h) })

	surveyID := openSurvey(t, ctx, svc, tenantID, actorID, true, 1)

	synthetic := "最近とても疲れていて眠れない日が続いています"
	resp, err := svc.SubmitResponse(ctx, talent.SubmitResponseInput{
		TenantID: tenantID, ActorID: actorID, SurveyID: surveyID,
		AnswersJSON: []byte(`{"q1":"3"}`), FreeTextPlaintext: []byte(synthetic),
	})
	require.NoError(t, err)
	assert.Nil(t, resp.FreeText, "ciphertext must not be returned from SubmitResponse")

	// DB stores ciphertext, NOT plaintext.
	var storedRow struct {
		FreeText []byte `gorm:"column:free_text"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT free_text FROM pulse_survey_responses WHERE id = ?`, resp.ID,
	).Scan(&storedRow).Error)
	require.NotEmpty(t, storedRow.FreeText)
	assert.NotEqual(t, []byte(synthetic), storedRow.FreeText, "plaintext must NOT be stored in DB")

	// Read with permission → decrypts to the original.
	plain, err := svc.ReadFreeText(ctx, talent.ReadFreeTextInput{
		TenantID: tenantID, ActorID: reader, ResponseID: resp.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, synthetic, string(plain))

	// Read WITHOUT permission → ErrForbidden, no plaintext.
	plain, err = svc.ReadFreeText(ctx, talent.ReadFreeTextInput{
		TenantID: tenantID, ActorID: actorID, ResponseID: resp.ID, // actor has no role
	})
	assert.ErrorIs(t, err, talent.ErrForbidden)
	assert.Nil(t, plain, "plaintext must not be returned without survey:read_freetext")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation (TM-020/021/022)
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "a@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EA1", "active", nil)

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "b@example.com")
	empB := seedEmployee(t, h.AdminDB, tenantB, "EB1", "active", nil)
	t.Cleanup(func() { truncateAll(h) })

	// Tenant A creates a skill.
	skA, err := svc.CreateSkill(ctx, talent.CreateSkillInput{
		TenantID: tenantA, ActorID: actorA, Category: "言語", Name: "Go",
		LevelsJSON: []byte(`{"min":1,"max":5}`),
	})
	require.NoError(t, err)

	// Tenant B cannot see tenant A's skills.
	listB, err := svc.ListSkills(ctx, tenantB, "")
	require.NoError(t, err)
	assert.Empty(t, listB, "tenant B must not see tenant A skills")

	// Tenant B cannot assign tenant A's skill to its own employee (skill not in B).
	_, err = svc.AssignSkill(ctx, talent.AssignSkillInput{
		TenantID: tenantB, ActorID: actorB, EmployeeID: empB, SkillID: skA.ID, Level: 1,
	})
	assert.Error(t, err, "cross-tenant skill assignment must fail")

	// Tenant B cannot read tenant A's employee profile.
	_, err = svc.GetIntegratedProfile(ctx, talent.GetProfileInput{
		TenantID: tenantB, ActorID: actorB, EmployeeID: empA,
	})
	assert.ErrorIs(t, err, talent.ErrNotFound, "tenant B must not read tenant A profile")

	// Tenant A creates a survey + response; tenant B cannot aggregate it.
	surveyID := openSurvey(t, ctx, svc, tenantA, actorA, true, 1)
	_, err = svc.SubmitResponse(ctx, talent.SubmitResponseInput{
		TenantID: tenantA, ActorID: actorA, SurveyID: surveyID,
		AnswersJSON: []byte(`{"q1":"5"}`), FreeTextPlaintext: []byte("合成自由記述"),
	})
	require.NoError(t, err)
	_, err = svc.AggregateSurvey(ctx, tenantB, surveyID)
	assert.ErrorIs(t, err, talent.ErrNotFound, "tenant B must not aggregate tenant A survey")
}

// ---------------------------------------------------------------------------
// Audit PII non-leak (free-text / respondent never in audit_logs)
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoFreeTextPII(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := talent.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	surveyID := openSurvey(t, ctx, svc, tenantID, actorID, true, 1)
	secret := "心身ともに限界です助けてください"
	_, err := svc.SubmitResponse(ctx, talent.SubmitResponseInput{
		TenantID: tenantID, ActorID: actorID, SurveyID: surveyID,
		AnswersJSON: []byte(`{"q1":"1"}`), FreeTextPlaintext: []byte(secret),
	})
	require.NoError(t, err)

	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%限界%", "%限界%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount, "audit_logs must not contain free-text PII")

	// resource_id must be a valid UUID (opaque), never PII.
	var resourceIDs []string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs WHERE tenant_id = ? AND resource_id IS NOT NULL`,
		tenantID,
	).Scan(&resourceIDs).Error)
	for _, rid := range resourceIDs {
		_, err := uuid.Parse(rid)
		assert.NoError(t, err, "audit resource_id must be an opaque UUID, got %q", rid)
	}
}
