package selection_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
	"github.com/your-org/hr-saas/internal/selection"
)

// ---------------------------------------------------------------------------
// Shared test helpers (seed via AdminDB; truncate in t.Cleanup)
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
		`INSERT INTO departments (id, tenant_id, name, code) VALUES (?, ?, '採用部', ?)`,
		id, tenantID, code,
	).Error)
	return id
}

// seedJobPosting inserts a job_postings row (ST-ATS-01 / 00011 — other-story
// table referenced logically by selection). public_slug must be unique per tenant.
func seedJobPosting(t *testing.T, adminDB *gorm.DB, tenantID, deptID uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO job_postings
		   (id, tenant_id, title, status, employment_type, department_id, public_slug)
		 VALUES (?, ?, 'バックエンドエンジニア', 'open', 'full_time', ?, ?)`,
		id, tenantID, deptID, slug,
	).Error)
	return id
}

// seedApplicant inserts an applicants row (ST-ATS-02 / 00018 — other-story table
// referenced logically by selection). Synthetic names only.
func seedApplicant(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, last, first string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO applicants (id, tenant_id, last_name, first_name, status)
		 VALUES (?, ?, ?, ?, 'applied')`,
		id, tenantID, last, first,
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
		"application_stage_history",
		"applications",
		"selection_stages",
		"selection_stage_templates",
		"candidate_message_templates",
		"applicants",
		"job_postings",
		"employees",
		"users",
		"roles",
		"departments",
		"sessions",
		"tenants",
	)
}

// applicationIDByApplicant looks up an application id via AdminDB (test-only).
func applicationIDByApplicant(t *testing.T, h *testdb.Harness, tenantID, jobPostingID, applicantID uuid.UUID) uuid.UUID {
	t.Helper()
	var row struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT id FROM applications
		 WHERE tenant_id = ? AND job_posting_id = ? AND applicant_id = ? LIMIT 1`,
		tenantID, jobPostingID, applicantID,
	).Scan(&row).Error)
	require.NotEqual(t, uuid.Nil, row.ID)
	return row.ID
}

// runRequirePermission drives the real RequirePermission middleware with a gin
// context carrying the auth tenant/user keys and reports whether the request
// was allowed (Next called) or denied (aborted with 403).
func runRequirePermission(t *testing.T, tdb *tenantdb.TenantDB, tenantID, userID uuid.UUID, need string) bool {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	// Mirror what RequireAuth sets so TenantIDFrom/UserIDFrom resolve.
	c.Set("auth_tenant_id", tenantID)
	c.Set("auth_user_id", userID)

	allowed := false
	mw := platformauth.RequirePermission(tdb, need)
	mw(c)
	if !c.IsAborted() {
		allowed = true
	}
	return allowed
}

// standardStagesJSON returns a synthetic 5-stage pipeline template body.
func standardStagesJSON() json.RawMessage {
	return json.RawMessage(`[
		{"name":"書類選考","stage_type":"screening","position":0},
		{"name":"一次面接","stage_type":"interview","position":1},
		{"name":"最終面接","stage_type":"interview","position":2},
		{"name":"内定","stage_type":"offer","position":3},
		{"name":"採用","stage_type":"hired","position":4},
		{"name":"不採用","stage_type":"rejected","position":5}
	]`)
}

// initStandardPipeline creates a stage template, initialises a job posting's
// stages from it, and returns the ordered stages.
func initStandardPipeline(t *testing.T, svc *selection.Service, ctx context.Context, tenantID, actorID, jobPostingID uuid.UUID) []selection.Stage {
	t.Helper()
	tmpl, err := svc.CreateStageTemplate(ctx, selection.CreateStageTemplateInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		Name:       "標準選考フロー",
		StagesJSON: []byte(standardStagesJSON()),
	})
	require.NoError(t, err)

	stages, err := svc.InitStages(ctx, selection.InitStagesInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		JobPostingID: jobPostingID,
		TemplateID:   &tmpl.ID,
	})
	require.NoError(t, err)
	require.Len(t, stages, 6)
	return stages
}

// stageByType returns the first stage of the given type from a stage slice.
func stageByType(t *testing.T, stages []selection.Stage, stageType string) selection.Stage {
	t.Helper()
	for _, s := range stages {
		if s.StageType == stageType {
			return s
		}
	}
	t.Fatalf("no stage of type %q", stageType)
	return selection.Stage{}
}

// stageByPosition returns the stage at the given position.
func stageByPosition(t *testing.T, stages []selection.Stage, pos int) selection.Stage {
	t.Helper()
	for _, s := range stages {
		if s.Position == pos {
			return s
		}
	}
	t.Fatalf("no stage at position %d", pos)
	return selection.Stage{}
}

// newHarness builds a harness, tenantdb and service, returning all three.
func newHarness(t *testing.T) (*testdb.Harness, *selection.Service) {
	t.Helper()
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selection.NewService(tdb)
	return h, svc
}

// ---------------------------------------------------------------------------
// Stage init / template tests
// ---------------------------------------------------------------------------

func TestInitStagesFromTemplate(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-init-1")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	assert.Equal(t, 0, stages[0].Position)
	assert.Equal(t, selection.StageTypeScreening, stages[0].StageType)
	assert.Equal(t, selection.StageTypeRejected, stages[len(stages)-1].StageType)

	listed, err := svc.ListStages(ctx, tenantID, jpID)
	require.NoError(t, err)
	assert.Len(t, listed, 6)

	// Re-initialisation must be rejected (history stability).
	_, err = svc.InitStages(ctx, selection.InitStagesInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID,
		Stages: []selection.StageDef{{Name: "X", StageType: "screening", Position: 0}},
	})
	assert.ErrorIs(t, err, selection.ErrAlreadyExists)
}

func TestInitStagesUnknownJobPosting(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.InitStages(ctx, selection.InitStagesInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: uuid.New(),
		Stages: []selection.StageDef{{Name: "書類", StageType: "screening", Position: 0}},
	})
	assert.ErrorIs(t, err, selection.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Application creation + duplicate prevention
// ---------------------------------------------------------------------------

func TestCreateApplicationPlacesAtFirstStage(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-app-1")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "山田", "太郎")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	first := stageByPosition(t, stages, 0)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)
	assert.Equal(t, selection.AppStatusInProgress, app.Status)
	require.NotNil(t, app.CurrentStageID)
	assert.Equal(t, first.ID, *app.CurrentStageID)

	// Entry transition is recorded in history.
	hist, err := svc.ListHistory(ctx, tenantID, app.ID)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Nil(t, hist[0].FromStageID)
	assert.Equal(t, first.ID, hist[0].ToStageID)

	// Duplicate application (same job + applicant) is rejected.
	_, err = svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	assert.ErrorIs(t, err, selection.ErrAlreadyExists)
}

func TestCreateApplicationUnknownApplicant(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-app-2")
	t.Cleanup(func() { truncateAll(h) })

	initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)

	_, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: uuid.New(),
	})
	assert.ErrorIs(t, err, selection.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Stage transition state machine
// ---------------------------------------------------------------------------

func TestMoveStageAdvanceBackAndReject(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-move-1")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "山田", "花子")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	pos1 := stageByPosition(t, stages, 1)
	pos2 := stageByPosition(t, stages, 2)
	rejected := stageByType(t, stages, selection.StageTypeRejected)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	// advance 0 → 1
	moved, err := svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos1.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, pos1.ID, *moved.CurrentStageID)
	assert.Equal(t, selection.AppStatusInProgress, moved.Status)

	// advance 1 → 2
	moved, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos2.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, pos2.ID, *moved.CurrentStageID)

	// move-back 2 → 1 with reason
	reason := "追加面接が必要"
	moved, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos1.ID,
		Reason: &reason,
	})
	require.NoError(t, err)
	assert.Equal(t, pos1.ID, *moved.CurrentStageID)

	// reject from a non-terminal stage finalises status = rejected
	rejReason := "他候補者を優先(合成データ)"
	rejExpires := time.Date(2031, 6, 3, 0, 0, 0, 0, time.UTC)
	moved, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: rejected.ID,
		Reason:             &rejReason,
		RetentionLabel:     "1year",
		RetentionExpiresOn: &rejExpires,
	})
	require.NoError(t, err)
	assert.Equal(t, selection.AppStatusRejected, moved.Status)
	// Retention applied from settings-supplied values (ST-ATS-02).
	assert.Equal(t, "1year", moved.RetentionLabel)
	require.NotNil(t, moved.RetentionExpiresOn)
	assert.Equal(t, "2031-06-03", moved.RetentionExpiresOn.Format("2006-01-02"))

	// History records every transition: entry + 2 advances + 1 back + 1 reject = 5.
	hist, err := svc.ListHistory(ctx, tenantID, app.ID)
	require.NoError(t, err)
	assert.Len(t, hist, 5)
}

func TestMoveStageSkipAheadRejected(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-move-2")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "鈴木", "一郎")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	pos2 := stageByPosition(t, stages, 2) // skipping 0 → 2

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos2.ID,
	})
	assert.ErrorIs(t, err, selection.ErrInvalidTransition,
		"skipping ahead more than one position must be rejected")
}

func TestMoveStageHireOnlyFromPrecedingStage(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-move-3")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "佐藤", "次郎")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	hired := stageByType(t, stages, selection.StageTypeHired) // position 4

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	// Hire directly from position 0 must fail (offer stage is position 3).
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: hired.ID,
	})
	assert.ErrorIs(t, err, selection.ErrInvalidTransition)

	// Advance step-by-step to the offer stage, then hire succeeds.
	for pos := 1; pos <= 3; pos++ {
		st := stageByPosition(t, stages, pos)
		_, err = svc.MoveStage(ctx, selection.MoveStageInput{
			TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: st.ID,
		})
		require.NoError(t, err)
	}
	moved, err := svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: hired.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, selection.AppStatusHired, moved.Status)
}

func TestMoveStageFromTerminalRejected(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-move-4")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "高橋", "三郎")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	pos1 := stageByPosition(t, stages, 1)
	rejected := stageByType(t, stages, selection.StageTypeRejected)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	reason := "辞退(合成)"
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: rejected.ID,
		Reason: &reason,
	})
	require.NoError(t, err)

	// Application status is now rejected — any further move is rejected.
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos1.ID,
	})
	assert.ErrorIs(t, err, selection.ErrInvalidTransition,
		"a finalised application cannot transition further")
}

func TestMoveStageReasonRequired(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-move-5")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "田中", "四郎")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	rejected := stageByType(t, stages, selection.StageTypeRejected)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	// reject without reason while reason is required → ErrReasonRequired.
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: rejected.ID,
		ReasonRequiredForBackOrReject: true,
	})
	assert.ErrorIs(t, err, selection.ErrReasonRequired)
}

func TestMoveStageTargetWrongJobPosting(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpA := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-cross-A")
	jpB := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-cross-B")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "伊藤", "五郎")
	t.Cleanup(func() { truncateAll(h) })

	initStandardPipeline(t, svc, ctx, tenantID, actorID, jpA)
	stagesB := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpB)
	targetInB := stageByPosition(t, stagesB, 1)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpA, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	// Target stage belongs to job posting B — must not be reachable from A's app.
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: targetInB.ID,
	})
	assert.ErrorIs(t, err, selection.ErrNotFound,
		"a stage from a different job posting must not be a valid target")
}

// ---------------------------------------------------------------------------
// Kanban aggregation
// ---------------------------------------------------------------------------

func TestGetKanbanGroupsByStage(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-kanban-1")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	pos1 := stageByPosition(t, stages, 1)

	// Two applicants at stage 0, one advanced to stage 1.
	a1 := seedApplicant(t, h.AdminDB, tenantID, "応募", "A")
	a2 := seedApplicant(t, h.AdminDB, tenantID, "応募", "B")
	a3 := seedApplicant(t, h.AdminDB, tenantID, "応募", "C")
	for _, ap := range []uuid.UUID{a1, a2, a3} {
		_, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
			TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: ap,
		})
		require.NoError(t, err)
	}
	// Advance a3's application to stage 1.
	app3 := applicationIDByApplicant(t, h, tenantID, jpID, a3)
	_, err := svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app3, TargetStageID: pos1.ID,
	})
	require.NoError(t, err)

	cols, err := svc.GetKanban(ctx, tenantID, jpID)
	require.NoError(t, err)
	require.Len(t, cols, 6)

	// Stage 0 (screening) has 2, stage 1 (interview) has 1.
	assert.Equal(t, 2, len(cols[0].Applications))
	assert.Equal(t, 1, len(cols[1].Applications))
}

// ---------------------------------------------------------------------------
// Candidate notification hook (skip when no template; fire when configured)
// ---------------------------------------------------------------------------

// recordingNotifier captures notifications for assertions; never touches PII.
type recordingNotifier struct {
	mu     sync.Mutex
	events []selection.CandidateNotification
}

func (n *recordingNotifier) Notify(_ context.Context, ev selection.CandidateNotification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	return nil
}

func (n *recordingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.events)
}

func TestCandidateNotificationHook(t *testing.T) {
	h, baseSvc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-notify-1")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "通知", "対象")
	t.Cleanup(func() { truncateAll(h) })

	notifier := &recordingNotifier{}
	svc := baseSvc.WithNotifier(notifier)

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	pos1 := stageByPosition(t, stages, 1)
	pos2 := stageByPosition(t, stages, 2)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	// No interview template yet → advancing to stage 1 (interview) skips notify.
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos1.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, notifier.count(), "no template configured → notification skipped")

	// Configure an interview template, then advance to another interview stage.
	tmpl, err := svc.CreateMessageTemplate(ctx, selection.UpsertMessageTemplateInput{
		TenantID: tenantID, ActorID: actorID, StageType: selection.StageTypeInterview,
		Name: "面接案内", Subject: "面接のご案内", Body: "{{candidate_name}} 様",
	})
	require.NoError(t, err)

	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: pos2.ID,
	})
	require.NoError(t, err)
	require.Equal(t, 1, notifier.count(), "template configured → notification fired")

	notifier.mu.Lock()
	ev := notifier.events[0]
	notifier.mu.Unlock()
	assert.Equal(t, tmpl.ID, ev.TemplateID)
	assert.Equal(t, app.ID, ev.ApplicationID)
	assert.Equal(t, selection.StageTypeInterview, ev.StageType)
}

func TestCreateMessageTemplateDuplicateActiveRejected(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.CreateMessageTemplate(ctx, selection.UpsertMessageTemplateInput{
		TenantID: tenantID, ActorID: actorID, StageType: selection.StageTypeRejected,
		Name: "不採用通知", Subject: "選考結果", Body: "...",
	})
	require.NoError(t, err)

	_, err = svc.CreateMessageTemplate(ctx, selection.UpsertMessageTemplateInput{
		TenantID: tenantID, ActorID: actorID, StageType: selection.StageTypeRejected,
		Name: "不採用通知2", Subject: "選考結果", Body: "...",
	})
	assert.ErrorIs(t, err, selection.ErrAlreadyExists,
		"only one active template per (tenant, stage_type) is allowed")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	deptA := seedDepartment(t, h.AdminDB, tenantA, "DEPTA")
	jpA := seedJobPosting(t, h.AdminDB, tenantA, deptA, "jp-iso-A")
	applicantA := seedApplicant(t, h.AdminDB, tenantA, "越境", "太郎")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	stagesA := initStandardPipeline(t, svc, ctx, tenantA, actorA, jpA)
	pos1A := stageByPosition(t, stagesA, 1)

	appA, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantA, ActorID: actorA, JobPostingID: jpA, ApplicantID: applicantA,
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's application.
	_, err = svc.GetApplication(ctx, tenantB, appA.ID)
	assert.ErrorIs(t, err, selection.ErrNotFound,
		"tenant B must not read tenant A's application")

	// Tenant B cannot move tenant A's application (RLS + explicit tenant_id).
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantB, ActorID: actorB, ApplicationID: appA.ID, TargetStageID: pos1A.ID,
	})
	assert.ErrorIs(t, err, selection.ErrNotFound,
		"tenant B must not move tenant A's application")

	// Tenant B's kanban / history for tenant A's job posting are empty.
	cols, err := svc.GetKanban(ctx, tenantB, jpA)
	require.NoError(t, err)
	for _, col := range cols {
		assert.Empty(t, col.Applications, "tenant B sees no tenant A applications")
	}
	hist, err := svc.ListHistory(ctx, tenantB, appA.ID)
	require.NoError(t, err)
	assert.Empty(t, hist, "tenant B sees no tenant A history")

	// Tenant B's stages for tenant A's job posting are empty.
	stagesSeen, err := svc.ListStages(ctx, tenantB, jpA)
	require.NoError(t, err)
	assert.Empty(t, stagesSeen, "tenant B sees no tenant A stages")
}

// ---------------------------------------------------------------------------
// Permission enforcement (RBAC) — middleware via RequirePermission
// ---------------------------------------------------------------------------

func TestRBACMiddlewareEnforcesPipelinePermission(t *testing.T) {
	h, _ := newHarness(t)
	tdb := tenantdb.New(h.AppDB)

	tenantID := seedTenant(t, h.AdminDB)
	// User WITHOUT ats:pipeline_write.
	noPermUser := seedUser(t, h.AdminDB, tenantID, "noperm@example.com")
	// User WITH ats:pipeline_write.
	permUser := seedUser(t, h.AdminDB, tenantID, "perm@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "pipeline_writer",
		`{"perms":["ats:pipeline_write"]}`)
	assignRole(t, h.AdminDB, permUser, roleID)
	t.Cleanup(func() { truncateAll(h) })

	require.True(t, runRequirePermission(t, tdb, tenantID, permUser, "ats:pipeline_write"),
		"user with ats:pipeline_write must be allowed")
	require.False(t, runRequirePermission(t, tdb, tenantID, noPermUser, "ats:pipeline_write"),
		"user without ats:pipeline_write must be denied")
}

// ---------------------------------------------------------------------------
// Audit log contains no PII
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	h, svc := newHarness(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "DEPT01")
	jpID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "jp-audit-1")
	applicantID := seedApplicant(t, h.AdminDB, tenantID, "監査", "対象")
	t.Cleanup(func() { truncateAll(h) })

	stages := initStandardPipeline(t, svc, ctx, tenantID, actorID, jpID)
	rejected := stageByType(t, stages, selection.StageTypeRejected)

	app, err := svc.CreateApplication(ctx, selection.CreateApplicationInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: jpID, ApplicantID: applicantID,
	})
	require.NoError(t, err)

	// Rejection reason carries a synthetic phrase that must NOT leak into audit.
	secretReason := "不採用理由マーカーXYZ"
	_, err = svc.MoveStage(ctx, selection.MoveStageInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: app.ID, TargetStageID: rejected.ID,
		Reason: &secretReason,
	})
	require.NoError(t, err)

	// The reason text (and applicant name) must not appear in audit_logs.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR resource_id LIKE ? OR action LIKE ?`,
		"%マーカーXYZ%", "%監査%", "%マーカーXYZ%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain rejection reason text or applicant PII")

	// resource_id of stage-move audit must be the opaque application UUID.
	var resourceID string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs
		 WHERE action = 'application.stage_moved' AND tenant_id = ? LIMIT 1`,
		tenantID,
	).Scan(&resourceID).Error)
	assert.Equal(t, app.ID.String(), resourceID,
		"stage_moved audit must reference the opaque application id only")
}
