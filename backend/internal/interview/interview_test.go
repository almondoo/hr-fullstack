package interview_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/interview"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
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
		"interview_evaluations",
		"evaluation_sheets",
		"interview_panelists", //nolint:misspell // DB table uses US spelling "panelists"; schema contract
		"interview_slots",
		"interviews",
		"tenant_interview_settings",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// newSvc builds a fresh harness, tenantdb, and service.
func newSvc(t *testing.T) (*testdb.Harness, *interview.Service) {
	t.Helper()
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := interview.NewService(tdb)
	t.Cleanup(func() { truncateAll(h) })
	return h, svc
}

// ---------------------------------------------------------------------------
// Interview CRUD + state transitions + slot/scheduled_at consistency
// ---------------------------------------------------------------------------

func TestCreateInterviewAndAddSlots(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	appID := uuid.New() // logical reference to applications (no FK)

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID, Format: "online",
	})
	require.NoError(t, err)
	assert.Equal(t, interview.StatusProposed, iv.Status)
	assert.Equal(t, "online", iv.Format)
	assert.Nil(t, iv.ScheduledAt)

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	slot, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, CandidateStart: start,
	})
	require.NoError(t, err)
	assert.False(t, slot.Selected)

	slots, err := svc.ListSlots(ctx, tenantID, iv.ID)
	require.NoError(t, err)
	assert.Len(t, slots, 1)
}

func TestConfirmInterviewSyncsScheduledAtWithSelectedSlot(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID, Format: "onsite",
	})
	require.NoError(t, err)

	s1 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	s2 := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	slot1, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, CandidateStart: s1,
	})
	require.NoError(t, err)
	slot2, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, CandidateStart: s2,
	})
	require.NoError(t, err)
	_ = slot1

	confirmed, err := svc.ConfirmInterview(ctx, interview.ConfirmInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, SlotID: slot2.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, interview.StatusConfirmed, confirmed.Status)
	require.NotNil(t, confirmed.ScheduledAt)
	// scheduled_at must equal the SELECTED slot's start (consistency).
	assert.True(t, confirmed.ScheduledAt.Equal(s2),
		"scheduled_at must be synced with selected slot start")

	// Exactly one slot should be marked selected, and it must be slot2.
	slots, err := svc.ListSlots(ctx, tenantID, iv.ID)
	require.NoError(t, err)
	var selectedCount int
	var selectedID uuid.UUID
	for _, s := range slots {
		if s.Selected {
			selectedCount++
			selectedID = s.ID
		}
	}
	assert.Equal(t, 1, selectedCount, "exactly one slot selected")
	assert.Equal(t, slot2.ID, selectedID)
}

func TestInterviewTransitionBoundaries(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID,
	})
	require.NoError(t, err)

	// proposed → completed is NOT allowed (must confirm first).
	_, err = svc.TransitionInterview(ctx, interview.TransitionInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, Status: interview.StatusCompleted,
	})
	assert.ErrorIs(t, err, interview.ErrInvalidTransition,
		"proposed → completed must be rejected")

	// Confirm via a slot, then complete.
	slot, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		CandidateStart: time.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)
	_, err = svc.ConfirmInterview(ctx, interview.ConfirmInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, SlotID: slot.ID,
	})
	require.NoError(t, err)

	completed, err := svc.TransitionInterview(ctx, interview.TransitionInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, Status: interview.StatusCompleted,
	})
	require.NoError(t, err)
	assert.Equal(t, interview.StatusCompleted, completed.Status)

	// completed is terminal: completed → cancelled must fail.
	_, err = svc.TransitionInterview(ctx, interview.TransitionInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, Status: interview.StatusCancelled,
	})
	assert.ErrorIs(t, err, interview.ErrInvalidTransition, "completed is terminal")
}

func TestAddSlotRejectedAfterConfirm(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID,
	})
	require.NoError(t, err)
	slot, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		CandidateStart: time.Now().Add(48 * time.Hour),
	})
	require.NoError(t, err)
	_, err = svc.ConfirmInterview(ctx, interview.ConfirmInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, SlotID: slot.ID,
	})
	require.NoError(t, err)

	// Adding a slot to a confirmed interview must be rejected.
	_, err = svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		CandidateStart: time.Now().Add(72 * time.Hour),
	})
	assert.ErrorIs(t, err, interview.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Calendar linkage loose coupling (INT-005)
// ---------------------------------------------------------------------------

func TestSetExternalEventDoesNotAffectStatusOrSchedule(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID,
	})
	require.NoError(t, err)
	slot, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		CandidateStart: time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	confirmed, err := svc.ConfirmInterview(ctx, interview.ConfirmInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, SlotID: slot.ID,
	})
	require.NoError(t, err)

	ext := "gcal-evt-opaque-123"
	updated, err := svc.SetExternalEvent(ctx, interview.SetExternalEventInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, ExternalEventID: &ext,
	})
	require.NoError(t, err)
	require.NotNil(t, updated.ExternalEventID)
	assert.Equal(t, ext, *updated.ExternalEventID)
	// Status and schedule unchanged by the calendar side-effect.
	assert.Equal(t, confirmed.Status, updated.Status)
	require.NotNil(t, updated.ScheduledAt)
	assert.True(t, confirmed.ScheduledAt.Equal(*updated.ScheduledAt))

	// Clearing the external event (simulating a failed sync to retry) must also
	// leave interview data intact.
	cleared, err := svc.SetExternalEvent(ctx, interview.SetExternalEventInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, ExternalEventID: nil,
	})
	require.NoError(t, err)
	assert.Nil(t, cleared.ExternalEventID)
	assert.Equal(t, interview.StatusConfirmed, cleared.Status)
}

// failingReminder always errors; used to prove confirmation is not rolled back.
type failingReminder struct{}

func (failingReminder) Remind(_ context.Context, _ interview.ReminderEvent) error {
	return errors.New("synthetic remind failure")
}

func TestConfirmSucceedsEvenWhenReminderFails(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := interview.NewService(tdb).WithReminder(failingReminder{})
	t.Cleanup(func() { truncateAll(h) })
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID,
	})
	require.NoError(t, err)
	slot, err := svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		CandidateStart: time.Now().Add(24 * time.Hour),
	})
	require.NoError(t, err)

	// Reminder errors internally; confirmation must still succeed and persist.
	confirmed, err := svc.ConfirmInterview(ctx, interview.ConfirmInterviewInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID, SlotID: slot.ID,
	})
	require.NoError(t, err, "remind failure must not fail confirmation")
	assert.Equal(t, interview.StatusConfirmed, confirmed.Status)

	reread, err := svc.GetInterview(ctx, tenantID, iv.ID)
	require.NoError(t, err)
	assert.Equal(t, interview.StatusConfirmed, reread.Status,
		"confirmation must be committed despite remind failure")
}

// ---------------------------------------------------------------------------
// Panellists (multi-assign + tenant verification + unique)
// ---------------------------------------------------------------------------

func TestAddPanelistAndUniqueness(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	panelistUser := seedUser(t, h.AdminDB, tenantID, "panelist@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: appID,
	})
	require.NoError(t, err)

	p, err := svc.AddPanelist(ctx, interview.AddPanelistInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		UserID: panelistUser, Role: "interviewer",
	})
	require.NoError(t, err)
	assert.Equal(t, "interviewer", p.Role)

	// Re-adding the same user is rejected (one assignment per interview/user).
	_, err = svc.AddPanelist(ctx, interview.AddPanelistInput{
		TenantID: tenantID, ActorID: actorID, InterviewID: iv.ID,
		UserID: panelistUser, Role: "observer",
	})
	assert.ErrorIs(t, err, interview.ErrAlreadyExists)

	list, err := svc.ListPanelists(ctx, tenantID, iv.ID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestAddPanelistRejectsForeignTenantUser(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	foreignUser := seedUser(t, h.AdminDB, tenantB, "foreign@example.com")
	appID := uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantA, ActorID: actorA, ApplicationID: appID,
	})
	require.NoError(t, err)

	// Assigning a user from tenant B to tenant A's interview must fail.
	_, err = svc.AddPanelist(ctx, interview.AddPanelistInput{
		TenantID: tenantA, ActorID: actorA, InterviewID: iv.ID, UserID: foreignUser,
	})
	assert.ErrorIs(t, err, interview.ErrNotFound,
		"cross-tenant panellist assignment must be rejected")
}

// ---------------------------------------------------------------------------
// Evaluations: unique-per-panellist, masking, aggregation
// ---------------------------------------------------------------------------

// evalScenario holds the IDs created by setupEvalScenario.
type evalScenario struct {
	tenantID  uuid.UUID
	actorID   uuid.UUID
	appID     uuid.UUID
	ivID      uuid.UUID
	sheetID   uuid.UUID
	panelistA uuid.UUID
	panelistB uuid.UUID
}

// setupEvalScenario creates a tenant with an interview, sheet, and two panellist
// users, returning the relevant ids.
func setupEvalScenario(t *testing.T, h *testdb.Harness, svc *interview.Service) evalScenario {
	t.Helper()
	ctx := context.Background()
	sc := evalScenario{}
	sc.tenantID = seedTenant(t, h.AdminDB)
	sc.actorID = seedUser(t, h.AdminDB, sc.tenantID, "actor@example.com")
	sc.panelistA = seedUser(t, h.AdminDB, sc.tenantID, "panelA@example.com")
	sc.panelistB = seedUser(t, h.AdminDB, sc.tenantID, "panelB@example.com")
	sc.appID = uuid.New()

	iv, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: sc.tenantID, ActorID: sc.actorID, ApplicationID: sc.appID,
	})
	require.NoError(t, err)
	sc.ivID = iv.ID

	sheet, err := svc.CreateSheet(ctx, interview.CreateSheetInput{
		TenantID: sc.tenantID, ActorID: sc.actorID, Name: "標準評価シート",
		ItemsJSON: []byte(`[{"key":"tech","label":"技術力","scale":5,"weight":1.0}]`),
	})
	require.NoError(t, err)
	sc.sheetID = sheet.ID
	return sc
}

func TestSubmitEvaluationUniquePerPanelist(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, ivID, sheetID, panelistA := sc.tenantID, sc.ivID, sc.sheetID, sc.panelistA

	score := 4.0
	ev, err := svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID,
		EvaluatorUserID: panelistA, SheetID: sheetID,
		ScoresJSON: []byte(`{"tech":4}`), OverallScore: &score, Recommendation: "yes",
		Comment: "技術面は良好",
	})
	require.NoError(t, err)
	assert.Equal(t, "yes", ev.Recommendation)

	// Re-submission by the same evaluator updates (upsert), not duplicates.
	score2 := 5.0
	ev2, err := svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID,
		EvaluatorUserID: panelistA, SheetID: sheetID,
		ScoresJSON: []byte(`{"tech":5}`), OverallScore: &score2, Recommendation: "strong_yes",
		Comment: "更新後コメント",
	})
	require.NoError(t, err)
	assert.Equal(t, ev.ID, ev2.ID, "same evaluator must update same row")
	assert.Equal(t, "strong_yes", ev2.Recommendation)

	// Verify exactly one row exists for this (interview, evaluator).
	var cnt int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM interview_evaluations
		 WHERE interview_id = ? AND evaluator_user_id = ?`,
		ivID, panelistA,
	).Scan(&cnt).Error)
	assert.Equal(t, int64(1), cnt)
}

func TestPeerEvaluationMaskingHiddenByDefault(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, ivID, sheetID, panelistA, panelistB := sc.tenantID, sc.ivID, sc.sheetID, sc.panelistA, sc.panelistB

	// Grant panelistA the evaluation read permission.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "eval_reader",
		`{"perms":["ats:evaluation:read"]}`)
	assignRole(t, h.AdminDB, panelistA, roleID)

	// Both panellists submit evaluations.
	aScore := 3.0
	_, err := svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID,
		EvaluatorUserID: panelistA, SheetID: sheetID,
		ScoresJSON: []byte(`{"tech":3}`), OverallScore: &aScore, Recommendation: "neutral",
		Comment: "Aのコメント",
	})
	require.NoError(t, err)
	bScore := 5.0
	_, err = svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistB, InterviewID: ivID,
		EvaluatorUserID: panelistB, SheetID: sheetID,
		ScoresJSON: []byte(`{"tech":5}`), OverallScore: &bScore, Recommendation: "strong_yes",
		Comment: "Bの機微コメント",
	})
	require.NoError(t, err)

	// Default: peer_eval_visible is false → A sees own full, B's masked.
	list, err := svc.ListEvaluations(ctx, interview.ListEvaluationsInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID, CanReadEvaluations: true,
	})
	require.NoError(t, err)
	require.Len(t, list, 2)
	for _, e := range list {
		if e.EvaluatorUserID == panelistA {
			assert.Equal(t, "Aのコメント", e.Comment, "own evaluation must be visible in full")
		} else {
			assert.Empty(t, e.Comment, "other panellist comment must be masked by default")
			assert.Nil(t, e.OverallScore, "other panellist score must be masked")
		}
	}
}

func TestPeerEvaluationVisibleWhenTenantEnablesIt(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, actorID, ivID, sheetID, panelistA, panelistB := sc.tenantID, sc.actorID, sc.ivID, sc.sheetID, sc.panelistA, sc.panelistB

	// Enable peer visibility for the tenant.
	_, err := svc.SetPeerEvalVisibility(ctx, interview.SetPeerEvalVisibilityInput{
		TenantID: tenantID, ActorID: actorID, Visible: true,
	})
	require.NoError(t, err)

	// Grant panelistA ats:evaluation:read so the service-layer recheck passes.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "eval_reader",
		`{"perms":["ats:evaluation:read"]}`)
	assignRole(t, h.AdminDB, panelistA, roleID)

	_, err = svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistB, InterviewID: ivID,
		EvaluatorUserID: panelistB, SheetID: sheetID,
		ScoresJSON: []byte(`{"tech":5}`), Recommendation: "strong_yes",
		Comment: "Bの機微コメント",
	})
	require.NoError(t, err)

	list, err := svc.ListEvaluations(ctx, interview.ListEvaluationsInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID, CanReadEvaluations: true,
	})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "Bの機微コメント", list[0].Comment,
		"peer comment must be visible when tenant enables peer visibility and actor has permission")
}

func TestPeerEvaluationMaskedWithoutEvaluationReadPermission(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, actorID, ivID, sheetID, panelistA, panelistB := sc.tenantID, sc.actorID, sc.ivID, sc.sheetID, sc.panelistA, sc.panelistB

	// Tenant allows peer visibility, BUT panelistA has no ats:evaluation:read.
	_, err := svc.SetPeerEvalVisibility(ctx, interview.SetPeerEvalVisibilityInput{
		TenantID: tenantID, ActorID: actorID, Visible: true,
	})
	require.NoError(t, err)

	_, err = svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistB, InterviewID: ivID,
		EvaluatorUserID: panelistB, SheetID: sheetID,
		Recommendation: "no", Comment: "Bの機微コメント",
	})
	require.NoError(t, err)

	// Even with CanReadEvaluations=true from the (hypothetically bypassed) HTTP
	// layer, the service-layer permission recheck fails → other comment masked.
	list, err := svc.ListEvaluations(ctx, interview.ListEvaluationsInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID, CanReadEvaluations: true,
	})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Empty(t, list[0].Comment,
		"service-layer permission recheck must mask peer comment without ats:evaluation:read")
}

func TestSummarizeApplicationAggregates(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, appID, ivID, sheetID, panelistA, panelistB := sc.tenantID, sc.appID, sc.ivID, sc.sheetID, sc.panelistA, sc.panelistB

	// Two evaluations across the same application.
	s1 := 4.0
	_, err := svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID,
		EvaluatorUserID: panelistA, SheetID: sheetID, OverallScore: &s1, Recommendation: "yes",
	})
	require.NoError(t, err)
	s2 := 2.0
	_, err = svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistB, InterviewID: ivID,
		EvaluatorUserID: panelistB, SheetID: sheetID, OverallScore: &s2, Recommendation: "no",
	})
	require.NoError(t, err)

	summary, err := svc.SummarizeApplication(ctx, tenantID, appID)
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Count)
	require.NotNil(t, summary.AverageScore)
	assert.InDelta(t, 3.0, *summary.AverageScore, 0.001)
	assert.Equal(t, 1, summary.YesCount)
	assert.Equal(t, 1, summary.NoCount)
	assert.InDelta(t, 0.5, summary.RecommendRatioYes, 0.001)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	appID := uuid.New()

	ivA, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantA, ActorID: actorA, ApplicationID: appID,
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's interview.
	_, err = svc.GetInterview(ctx, tenantB, ivA.ID)
	assert.ErrorIs(t, err, interview.ErrNotFound, "tenantB must not read tenantA interview")

	// Tenant B cannot add a slot to tenant A's interview.
	_, err = svc.AddSlot(ctx, interview.AddSlotInput{
		TenantID: tenantB, ActorID: actorB, InterviewID: ivA.ID,
		CandidateStart: time.Now().Add(24 * time.Hour),
	})
	assert.ErrorIs(t, err, interview.ErrNotFound, "tenantB must not modify tenantA interview")

	// Tenant B cannot transition tenant A's interview.
	_, err = svc.TransitionInterview(ctx, interview.TransitionInterviewInput{
		TenantID: tenantB, ActorID: actorB, InterviewID: ivA.ID, Status: interview.StatusCancelled,
	})
	assert.ErrorIs(t, err, interview.ErrNotFound)

	// Listing interviews for the application in tenant B returns nothing.
	list, err := svc.ListInterviews(ctx, tenantB, appID)
	require.NoError(t, err)
	assert.Empty(t, list, "tenantB must not see tenantA interviews")
}

func TestEvaluationSheetCrossTenantRejected(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	appID := uuid.New()

	// Sheet in tenant A.
	sheetA, err := svc.CreateSheet(ctx, interview.CreateSheetInput{
		TenantID: tenantA, ActorID: actorA, Name: "A sheet",
	})
	require.NoError(t, err)

	// Interview in tenant B.
	ivB, err := svc.CreateInterview(ctx, interview.CreateInterviewInput{
		TenantID: tenantB, ActorID: actorB, ApplicationID: appID,
	})
	require.NoError(t, err)

	// Submitting an evaluation in tenant B referencing tenant A's sheet must fail.
	_, err = svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantB, ActorID: actorB, InterviewID: ivB.ID,
		EvaluatorUserID: actorB, SheetID: sheetA.ID, Recommendation: "yes",
	})
	assert.ErrorIs(t, err, interview.ErrNotFound,
		"evaluation must not reference another tenant's sheet")
}

// ---------------------------------------------------------------------------
// Audit log PII non-inclusion
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoEvaluationComment(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, ivID, sheetID, panelistA := sc.tenantID, sc.ivID, sc.sheetID, sc.panelistA

	secretComment := "機微評価コメント-SENTINEL-9f3a"
	_, err := svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID,
		EvaluatorUserID: panelistA, SheetID: sheetID, Recommendation: "yes",
		Comment: secretComment,
	})
	require.NoError(t, err)

	// Also exercise the read path which audits sensitive access.
	_, err = svc.ListEvaluations(ctx, interview.ListEvaluationsInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID, CanReadEvaluations: true,
	})
	require.NoError(t, err)

	// No audit_logs row may contain the comment text in resource_id or action.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%SENTINEL%", "%SENTINEL%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain evaluation comment text")

	// resource_id must be a parseable UUID (opaque), never free text.
	var resourceIDs []string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs WHERE tenant_id = ? AND resource_id IS NOT NULL`,
		tenantID,
	).Scan(&resourceIDs).Error)
	for _, rid := range resourceIDs {
		_, perr := uuid.Parse(rid)
		assert.NoError(t, perr, "audit resource_id must be an opaque UUID, got %q", rid)
	}
}

// ---------------------------------------------------------------------------
// Input validation guards
// ---------------------------------------------------------------------------

func TestSubmitEvaluationRejectsInvalidRecommendation(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	sc := setupEvalScenario(t, h, svc)
	tenantID, ivID, sheetID, panelistA := sc.tenantID, sc.ivID, sc.sheetID, sc.panelistA

	_, err := svc.SubmitEvaluation(ctx, interview.SubmitEvaluationInput{
		TenantID: tenantID, ActorID: panelistA, InterviewID: ivID,
		EvaluatorUserID: panelistA, SheetID: sheetID, Recommendation: "definitely_maybe",
	})
	assert.ErrorIs(t, err, interview.ErrInvalidInput)
}

func TestCreateSheetStoresItemsJSON(t *testing.T) {
	h, svc := newSvc(t)
	ctx := context.Background()
	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")

	items := json.RawMessage(`[{"key":"culture","label":"カルチャーフィット","scale":5,"weight":0.5}]`)
	sheet, err := svc.CreateSheet(ctx, interview.CreateSheetInput{
		TenantID: tenantID, ActorID: actorID, Name: "シート", ItemsJSON: []byte(items),
	})
	require.NoError(t, err)

	list, err := svc.ListSheets(ctx, tenantID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, sheet.ID, list[0].ID)
	assert.JSONEq(t, string(items), string(list[0].ItemsJSON))
}
