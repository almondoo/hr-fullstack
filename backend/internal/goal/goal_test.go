package goal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/goal"
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

func seedEmployee(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status)
		 VALUES (?, ?, ?, '合成', '太郎', 'full_time', ?)`,
		id, tenantID, code, status,
	).Error)
	return id
}

// seedRoleWithPermissions inserts a role with the given JSON-encoded perms array
// (e.g. `{"perms":["goal:read_sensitive"]}`) and returns its ID.
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

// seedApprovalRoute inserts an active approval route with a single step assigned
// to approverUserID for the given request_type (tenant-wide).
func seedApprovalRoute(t *testing.T, adminDB *gorm.DB, tenantID, approverUserID uuid.UUID, requestType string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	steps := `[{"step":0,"user_id":"` + approverUserID.String() + `"}]`
	require.NoError(t, adminDB.Exec(
		`INSERT INTO approval_routes (id, tenant_id, request_type, department_id, name, steps_json, active)
		 VALUES (?, ?, ?, NULL, ?, ?::jsonb, true)`,
		id, tenantID, requestType, "goal-approval-route", steps,
	).Error)
	return id
}

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"goal_progress_logs",
		"key_results",
		"goals",
		"review_cycles",
		"approval_steps",
		"approval_requests",
		"approval_routes",
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// activeCycle creates an active review cycle and returns its ID.
func activeCycle(t *testing.T, ctx context.Context, svc *goal.Service, tenantID, actorID uuid.UUID, requireWeight100 bool) uuid.UUID {
	t.Helper()
	cycle, err := svc.CreateCycle(ctx, goal.CreateCycleInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		Name:             "2026上期",
		StartsOn:         "2026-04-01",
		EndsOn:           "2026-09-30",
		RequireWeight100: requireWeight100,
	})
	require.NoError(t, err)
	_, err = svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, Status: goal.CycleStatusActive,
	})
	require.NoError(t, err)
	return cycle.ID
}

// ---------------------------------------------------------------------------
// Review cycle lifecycle
// ---------------------------------------------------------------------------

func TestCycleLifecycle(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	cycle, err := svc.CreateCycle(ctx, goal.CreateCycleInput{
		TenantID: tenantID, ActorID: actorID, Name: "2026上期",
		StartsOn: "2026-04-01", EndsOn: "2026-09-30",
	})
	require.NoError(t, err)
	assert.Equal(t, goal.CycleStatusDraft, cycle.Status)
	assert.Equal(t, goal.ProgressMethodAverage, cycle.ProgressMethod)
	assert.Equal(t, 10, cycle.MaxCascadeDepth)

	// draft → active
	c2, err := svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, Status: goal.CycleStatusActive,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.CycleStatusActive, c2.Status)

	// active → draft is not allowed
	_, err = svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, Status: goal.CycleStatusDraft,
	})
	assert.ErrorIs(t, err, goal.ErrInvalidTransition)

	// active → closed
	c3, err := svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, Status: goal.CycleStatusClosed,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.CycleStatusClosed, c3.Status)
}

// ---------------------------------------------------------------------------
// MBO goal CRUD + weight handling
// ---------------------------------------------------------------------------

func TestCreateMBOGoalAndWeightTotal(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)

	w60 := 60.0
	g1, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "売上目標", Weight: &w60,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusDraft, g1.Status)
	assert.Equal(t, goal.MethodMBO, g1.Method)
	require.NotNil(t, g1.Weight)
	assert.InDelta(t, 60.0, *g1.Weight, 0.001)

	w50 := 50.0
	_, err = svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "品質目標", Weight: &w50,
	})
	require.NoError(t, err)

	// Over 100% allowed at create time (warning, not hard error).
	total, err := svc.MBOWeightTotal(ctx, tenantID, cycleID, empID)
	require.NoError(t, err)
	assert.InDelta(t, 110.0, total, 0.001)
}

func TestCreateGoalRejectsClosedCycle(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	_, err := svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, Status: goal.CycleStatusClosed,
	})
	require.NoError(t, err)

	_, err = svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "遅すぎる目標",
	})
	assert.ErrorIs(t, err, goal.ErrCycleClosed, "closed cycles are read-only for goal creation")
}

// ---------------------------------------------------------------------------
// OKR progress boundary values
// ---------------------------------------------------------------------------

func TestOKRKeyResultProgressBoundaries(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)

	okr, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodOKR, Title: "Objective: NPS向上",
	})
	require.NoError(t, err)

	// Normal mid-progress: start=0, target=100, current=40 → 40%.
	kr, err := svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "NPS", StartValue: 0, TargetValue: 100, CurrentValue: 40,
	})
	require.NoError(t, err)
	assert.InDelta(t, 40.0, kr.ProgressPct, 0.001)

	// current < start → clamp to 0%.
	krBelow, err := svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "解約率", StartValue: 10, TargetValue: 0, CurrentValue: 20, // current above start, target below
	})
	require.NoError(t, err)
	// start=10, target=0 → denom=-10; current=20 → ratio=(20-10)/-10=-1 → clamp 0.
	assert.InDelta(t, 0.0, krBelow.ProgressPct, 0.001)

	// current > target → clamp to 100%.
	krOver, err := svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "問い合わせ削減", StartValue: 0, TargetValue: 50, CurrentValue: 80,
	})
	require.NoError(t, err)
	assert.InDelta(t, 100.0, krOver.ProgressPct, 0.001)

	// Degenerate target == start: current below target → 0; current at/above → 100.
	krEqBelow, err := svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "退化(未達)", StartValue: 5, TargetValue: 5, CurrentValue: 4,
	})
	require.NoError(t, err)
	assert.InDelta(t, 0.0, krEqBelow.ProgressPct, 0.001)

	krEqReached, err := svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "退化(達成)", StartValue: 5, TargetValue: 5, CurrentValue: 5,
	})
	require.NoError(t, err)
	assert.InDelta(t, 100.0, krEqReached.ProgressPct, 0.001)
}

func TestUpdateKeyResultRecomputesObjective(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	okr, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodOKR, Title: "Objective",
	})
	require.NoError(t, err)

	kr1, err := svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "KR1", StartValue: 0, TargetValue: 100, CurrentValue: 0,
	})
	require.NoError(t, err)
	_, err = svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "KR2", StartValue: 0, TargetValue: 100, CurrentValue: 100,
	})
	require.NoError(t, err)

	// After KR1=0%, KR2=100% → objective average = 50%.
	g, err := svc.GetGoal(ctx, tenantID, okr.ID)
	require.NoError(t, err)
	assert.InDelta(t, 50.0, g.ProgressPct, 0.001)

	// Update KR1 to 50% → objective average = 75%.
	_, err = svc.UpdateKeyResultProgress(ctx, goal.UpdateKeyResultProgressInput{
		TenantID: tenantID, ActorID: actorID, KeyResultID: kr1.ID, CurrentValue: 50, Comment: "中間",
	})
	require.NoError(t, err)
	g, err = svc.GetGoal(ctx, tenantID, okr.ID)
	require.NoError(t, err)
	assert.InDelta(t, 75.0, g.ProgressPct, 0.001)

	// A progress log was appended referencing the KR.
	logs, err := svc.ListProgressLogs(ctx, tenantID, okr.ID)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.NotNil(t, logs[0].KeyResultID)
	assert.Equal(t, kr1.ID, *logs[0].KeyResultID)
}

func TestAddKeyResultRejectsMBOGoal(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	mbo, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "MBO目標",
	})
	require.NoError(t, err)

	_, err = svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: mbo.ID,
		Title: "KR", StartValue: 0, TargetValue: 1, CurrentValue: 0,
	})
	assert.ErrorIs(t, err, goal.ErrInvalidInput, "key results require an OKR goal")
}

// ---------------------------------------------------------------------------
// MBO whole-goal progress logging
// ---------------------------------------------------------------------------

func TestUpdateGoalProgressLogs(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	mbo, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "MBO目標",
	})
	require.NoError(t, err)

	g, err := svc.UpdateGoalProgress(ctx, goal.UpdateGoalProgressInput{
		TenantID: tenantID, ActorID: actorID, GoalID: mbo.ID, ProgressPct: 30, Comment: "進捗30%",
	})
	require.NoError(t, err)
	assert.InDelta(t, 30.0, g.ProgressPct, 0.001)

	logs, err := svc.ListProgressLogs(ctx, tenantID, mbo.ID)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Nil(t, logs[0].KeyResultID, "MBO whole-goal progress log has no key_result_id")
	assert.InDelta(t, 30.0, logs[0].ProgressPct, 0.001)

	// Out-of-range rejected.
	_, err = svc.UpdateGoalProgress(ctx, goal.UpdateGoalProgressInput{
		TenantID: tenantID, ActorID: actorID, GoalID: mbo.ID, ProgressPct: 150,
	})
	assert.ErrorIs(t, err, goal.ErrInvalidInput)
}

// ---------------------------------------------------------------------------
// Cascade — cycle detection (direct + multi-level), tree retrieval
// ---------------------------------------------------------------------------

func TestCascadeCycleDetection(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)

	// org (root) ← dept ← personal
	org, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "組織目標",
	})
	require.NoError(t, err)
	dept, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "部署目標", ParentGoalID: &org.ID,
	})
	require.NoError(t, err)
	personal, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "個人目標", ParentGoalID: &dept.ID,
	})
	require.NoError(t, err)

	// Direct self-parent rejected.
	_, err = svc.UpdateGoal(ctx, goal.UpdateGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: org.ID,
		Title: "組織目標", ParentGoalID: &org.ID,
	})
	assert.ErrorIs(t, err, goal.ErrCascadeCycle, "a goal cannot be its own parent")

	// Multi-level ancestor cycle: set org.parent = personal (personal is org's
	// grandchild) → would create org→personal→dept→org loop. Rejected.
	_, err = svc.UpdateGoal(ctx, goal.UpdateGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: org.ID,
		Title: "組織目標", ParentGoalID: &personal.ID,
	})
	assert.ErrorIs(t, err, goal.ErrCascadeCycle, "multi-level cascade cycle must be rejected")

	// Cascade tree from org has the full chain.
	tree, err := svc.GetCascadeTree(ctx, tenantID, org.ID)
	require.NoError(t, err)
	require.Equal(t, org.ID, tree.Goal.ID)
	require.Len(t, tree.Children, 1)
	assert.Equal(t, dept.ID, tree.Children[0].Goal.ID)
	require.Len(t, tree.Children[0].Children, 1)
	assert.Equal(t, personal.ID, tree.Children[0].Children[0].Goal.ID)
}

func TestCascadeDepthLimit(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Cycle with max_cascade_depth = 2.
	cycle, err := svc.CreateCycle(ctx, goal.CreateCycleInput{
		TenantID: tenantID, ActorID: actorID, Name: "浅いカスケード",
		StartsOn: "2026-04-01", EndsOn: "2026-09-30", MaxCascadeDepth: 2,
	})
	require.NoError(t, err)
	_, err = svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, Status: goal.CycleStatusActive,
	})
	require.NoError(t, err)

	g1, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "L1",
	})
	require.NoError(t, err)
	g2, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "L2", ParentGoalID: &g1.ID,
	})
	require.NoError(t, err)
	// Depth 2 (g3→g2→g1) is allowed (== max).
	g3, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "L3", ParentGoalID: &g2.ID,
	})
	require.NoError(t, err)

	// Depth 3 (g4→g3→g2→g1) exceeds max=2 → rejected.
	_, err = svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycle.ID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "L4", ParentGoalID: &g3.ID,
	})
	assert.ErrorIs(t, err, goal.ErrCascadeTooDeep)
}

// ---------------------------------------------------------------------------
// State transition FSM
// ---------------------------------------------------------------------------

func TestGoalStateMachineRejectsInvalidTransitions(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	g, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標",
	})
	require.NoError(t, err)

	// draft → in_progress directly is invalid (must go via submitted/approved).
	_, err = svc.TransitionGoal(ctx, goal.TransitionGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID, Status: goal.GoalStatusInProgress,
	})
	assert.ErrorIs(t, err, goal.ErrInvalidTransition)

	// TransitionGoal rejects driving to "submitted" (must use SubmitGoal).
	_, err = svc.TransitionGoal(ctx, goal.TransitionGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID, Status: goal.GoalStatusSubmitted,
	})
	assert.ErrorIs(t, err, goal.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Approval-engine integration (submit/approve atomicity + manual fallback)
// ---------------------------------------------------------------------------

func TestSubmitGoalWithApprovalRoute(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	approverID := seedUser(t, h.AdminDB, tenantID, "approver@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	seedApprovalRoute(t, h.AdminDB, tenantID, approverID, "goal_approval")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	g, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標",
	})
	require.NoError(t, err)

	submitted, err := svc.SubmitGoal(ctx, goal.SubmitGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusSubmitted, submitted.Status)
	require.NotNil(t, submitted.ApprovalRequestID, "submit must link an approval request when a route exists")

	// The approval_request row exists and is linked atomically.
	var cnt int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM approval_requests WHERE id = ? AND tenant_id = ?`,
		*submitted.ApprovalRequestID, tenantID,
	).Scan(&cnt).Error)
	assert.Equal(t, int64(1), cnt)
}

func TestSubmitGoalNoRouteFallback(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	g, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標",
	})
	require.NoError(t, err)

	// No approval route configured — goal still moves to submitted, no link.
	submitted, err := svc.SubmitGoal(ctx, goal.SubmitGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusSubmitted, submitted.Status)
	assert.Nil(t, submitted.ApprovalRequestID, "no route → no linked approval request")

	// Approver then approves via the goal FSM (上司承認).
	approved, err := svc.TransitionGoal(ctx, goal.TransitionGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID, Status: goal.GoalStatusApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusApproved, approved.Status)

	// approved → in_progress → achieved.
	_, err = svc.TransitionGoal(ctx, goal.TransitionGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID, Status: goal.GoalStatusInProgress,
	})
	require.NoError(t, err)
	achieved, err := svc.TransitionGoal(ctx, goal.TransitionGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID, Status: goal.GoalStatusAchieved,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusAchieved, achieved.Status)
}

func TestSubmitGoalWeight100Gate(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// require_weight_100 = true.
	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, true)

	w60 := 60.0
	g1, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標1", Weight: &w60,
	})
	require.NoError(t, err)

	// Total weight 60 ≠ 100 → submit blocked.
	_, err = svc.SubmitGoal(ctx, goal.SubmitGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g1.ID,
	})
	assert.ErrorIs(t, err, goal.ErrInvalidInput, "weight total must be 100 when require_weight_100=true")

	// Add a 40% goal → total 100; now submit succeeds.
	w40 := 40.0
	_, err = svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標2", Weight: &w40,
	})
	require.NoError(t, err)
	submitted, err := svc.SubmitGoal(ctx, goal.SubmitGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g1.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusSubmitted, submitted.Status)
}

func TestSubmitGoalWeight100GateDisabled(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// require_weight_100 = false → any total may submit.
	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	w60 := 60.0
	g1, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標1", Weight: &w60,
	})
	require.NoError(t, err)
	submitted, err := svc.SubmitGoal(ctx, goal.SubmitGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g1.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, goal.GoalStatusSubmitted, submitted.Status)
}

// ---------------------------------------------------------------------------
// Cross-cycle copy (期跨ぎコピー) + closed read-only
// ---------------------------------------------------------------------------

func TestCrossCycleCopy(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Source cycle with a parent/child MBO pair and an OKR with KRs.
	fromCycle := activeCycle(t, ctx, svc, tenantID, actorID, false)
	parent, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: fromCycle, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "親",
	})
	require.NoError(t, err)
	_, err = svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: fromCycle, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "子", ParentGoalID: &parent.ID,
	})
	require.NoError(t, err)
	okr, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: fromCycle, EmployeeID: empID,
		Method: goal.MethodOKR, Title: "OKR",
	})
	require.NoError(t, err)
	_, err = svc.AddKeyResult(ctx, goal.AddKeyResultInput{
		TenantID: tenantID, ActorID: actorID, GoalID: okr.ID,
		Title: "KR", StartValue: 0, TargetValue: 100, CurrentValue: 80,
	})
	require.NoError(t, err)

	// Close the source cycle → it becomes read-only.
	_, err = svc.UpdateCycleStatus(ctx, goal.UpdateCycleStatusInput{
		TenantID: tenantID, ActorID: actorID, CycleID: fromCycle, Status: goal.CycleStatusClosed,
	})
	require.NoError(t, err)

	// Closed cycle is read-only: updating a goal in it fails.
	_, err = svc.UpdateGoal(ctx, goal.UpdateGoalInput{
		TenantID: tenantID, ActorID: actorID, GoalID: parent.ID, Title: "改名",
	})
	assert.ErrorIs(t, err, goal.ErrCycleClosed, "closed cycle goals are read-only")

	// New active destination cycle.
	toCycle := activeCycle(t, ctx, svc, tenantID, actorID, false)

	copied, err := svc.CopyGoals(ctx, goal.CopyGoalsInput{
		TenantID: tenantID, ActorID: actorID, FromCycleID: fromCycle, ToCycleID: toCycle, EmployeeID: empID,
	})
	require.NoError(t, err)
	assert.Len(t, copied, 3, "all three goals copied")

	// All copies are draft with reset progress; parent links remapped within
	// the new cycle (no cross-cycle references).
	var parentCopy *goal.Goal
	for i := range copied {
		assert.Equal(t, goal.GoalStatusDraft, copied[i].Status)
		assert.Equal(t, toCycle, copied[i].CycleID)
		assert.InDelta(t, 0.0, copied[i].ProgressPct, 0.001)
		if copied[i].Title == "親" {
			g := copied[i]
			parentCopy = &g
		}
	}
	require.NotNil(t, parentCopy)
	// The copied child references the copied parent (same cycle).
	var childParent *uuid.UUID
	for i := range copied {
		if copied[i].Title == "子" {
			childParent = copied[i].ParentGoalID
		}
	}
	require.NotNil(t, childParent)
	assert.Equal(t, parentCopy.ID, *childParent, "child parent link remapped to copied parent")

	// Copied OKR KeyResults have reset progress (current = start).
	var okrCopyID uuid.UUID
	for i := range copied {
		if copied[i].Method == goal.MethodOKR {
			okrCopyID = copied[i].ID
		}
	}
	require.NotEqual(t, uuid.Nil, okrCopyID)
	krs, err := svc.ListKeyResults(ctx, tenantID, okrCopyID)
	require.NoError(t, err)
	require.Len(t, krs, 1)
	assert.InDelta(t, 0.0, krs[0].ProgressPct, 0.001, "copied KR progress reset")
	assert.InDelta(t, krs[0].StartValue, krs[0].CurrentValue, 0.001, "copied KR current reset to start")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA01", "active")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	empB := seedEmployee(t, h.AdminDB, tenantB, "EMPB01", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleA := activeCycle(t, ctx, svc, tenantA, actorA, false)
	goalA, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantA, ActorID: actorA, CycleID: cycleA, EmployeeID: empA,
		Method: goal.MethodMBO, Title: "テナントA目標",
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's goal.
	_, err = svc.GetGoal(ctx, tenantB, goalA.ID)
	assert.ErrorIs(t, err, goal.ErrNotFound, "tenant B must not see tenant A's goal")

	// Tenant B cannot read tenant A's cycle.
	_, err = svc.GetCycle(ctx, tenantB, cycleA)
	assert.ErrorIs(t, err, goal.ErrNotFound)

	// Tenant B context listing tenant A's cycle goals returns empty.
	listed, err := svc.ListGoals(ctx, tenantB, cycleA, nil)
	require.NoError(t, err)
	assert.Empty(t, listed, "tenant B must not see tenant A goals via list")

	// Tenant B cannot create a goal referencing tenant A's cycle (composite FK
	// to review_cycles + RLS). The cycle lookup in tenant B context yields
	// not-found before the insert.
	cycleB := activeCycle(t, ctx, svc, tenantB, actorB, false)
	_, err = svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantB, ActorID: actorB, CycleID: cycleA, EmployeeID: empB,
		Method: goal.MethodMBO, Title: "越境",
	})
	assert.Error(t, err, "cannot create a goal against another tenant's cycle")

	// Tenant B cannot set parent_goal_id to tenant A's goal (composite self-FK +
	// tenant-scoped parent validation).
	goalBOwn, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantB, ActorID: actorB, CycleID: cycleB, EmployeeID: empB,
		Method: goal.MethodMBO, Title: "テナントB目標",
	})
	require.NoError(t, err)
	_, err = svc.UpdateGoal(ctx, goal.UpdateGoalInput{
		TenantID: tenantB, ActorID: actorB, GoalID: goalBOwn.ID,
		Title: "テナントB目標", ParentGoalID: &goalA.ID, // tenant A's goal
	})
	assert.Error(t, err, "parent_goal_id cannot point at another tenant's goal")
}

// ---------------------------------------------------------------------------
// RBAC — HTTP route layer 403 enforcement (RegisterRoutes + httptest)
// ---------------------------------------------------------------------------

// rbacRouter builds a gin router with RegisterRoutes wired and a stub
// requireAuth middleware that injects tenantID and userID into the context
// without requiring a real session cookie.
func rbacRouter(tdb *tenantdb.TenantDB, tenantID, userID uuid.UUID) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Stub requireAuth: injects tenant_id and user_id into the gin.Context.
	// The key strings must match those used by platformauth.TenantIDFrom /
	// UserIDFrom (defined as "auth_tenant_id" / "auth_user_id" in the auth
	// package).
	stubAuth := func(c *gin.Context) {
		c.Set("auth_tenant_id", tenantID)
		c.Set("auth_user_id", userID)
		c.Next()
	}
	api := r.Group("/api")
	goal.RegisterRoutes(api, tdb, stubAuth)
	return r
}

// seedRoleUser creates a user with the given permission set and returns its ID.
// Permission JSON format: {"perms":["goal:read","goal:write"]}
func seedRoleUser(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, email string, perms []string) uuid.UUID {
	t.Helper()
	roleID := uuid.New()
	permsJSON := fmt.Sprintf(`{"perms":[%s]}`, func() string {
		b, _ := json.Marshal(perms)
		s := string(b)
		return s[1 : len(s)-1] // strip outer []
	}())
	require.NoError(t, adminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, ?, ?::jsonb)`,
		roleID, tenantID, email+"-role", permsJSON,
	).Error)
	userID := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status, role_id) VALUES (?, ?, ?, 'active', ?)`,
		userID, tenantID, email, roleID,
	).Error)
	return userID
}

// TestRBACGoalWrite verifies that POST /goals requires goal:write and returns
// 403 when the actor lacks that permission.
func TestRBACGoalWrite(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	tenantID := seedTenant(t, h.AdminDB)

	// User with only goal:read — must be denied POST /goals (requires goal:write).
	readOnlyUserID := seedRoleUser(t, h.AdminDB, tenantID, "readonly@example.com", []string{"goal:read"})

	// Seed an active cycle so the handler would succeed if RBAC allowed it.
	actorID := seedRoleUser(t, h.AdminDB, tenantID, "admin@example.com", []string{"goal:write", "goal:read_all"})
	svc := goal.NewService(tdb)
	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")

	r := rbacRouter(tdb, tenantID, readOnlyUserID)

	body, _ := json.Marshal(map[string]any{
		"cycle_id":    cycleID.String(),
		"employee_id": empID.String(),
		"method":      "mbo",
		"title":       "テスト目標",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/goals", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"actor without goal:write must receive 403 on POST /goals")
}

// TestRBACGoalApprove verifies that PATCH /goals/:id/status requires
// goal:approve and returns 403 for a user with only goal:write.
func TestRBACGoalApprove(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedRoleUser(t, h.AdminDB, tenantID, "writer@example.com", []string{"goal:write", "goal:read_all"})
	svc := goal.NewService(tdb)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	g, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: "目標",
	})
	require.NoError(t, err)

	// writer has goal:write but not goal:approve — must be denied.
	r := rbacRouter(tdb, tenantID, actorID)

	body, _ := json.Marshal(map[string]any{"status": "approved"})
	req := httptest.NewRequest(http.MethodPatch, "/api/goals/"+g.ID.String()+"/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"actor without goal:approve must receive 403 on PATCH /goals/:id/status")
}

// TestRBACGoalReadAll verifies that GET /goal-cycles requires goal:read_all
// and returns 403 for a user with only goal:read.
func TestRBACGoalReadAll(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	t.Cleanup(func() { truncateAll(h) })

	tenantID := seedTenant(t, h.AdminDB)
	readUserID := seedRoleUser(t, h.AdminDB, tenantID, "reader@example.com", []string{"goal:read"})

	r := rbacRouter(tdb, tenantID, readUserID)

	req := httptest.NewRequest(http.MethodGet, "/api/goal-cycles", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"actor without goal:read_all must receive 403 on GET /goal-cycles")
}

// TestRBACOwnershipForbidden verifies that a user cannot update another
// employee's goal (ownership enforcement at the service layer).
func TestRBACOwnershipForbidden(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	tenantID := seedTenant(t, h.AdminDB)

	// Two employees with separate user accounts, both have goal:write.
	empA := seedEmployee(t, h.AdminDB, tenantID, "EMPA01", "active")
	empB := seedEmployee(t, h.AdminDB, tenantID, "EMPB01", "active")

	userA := seedRoleUser(t, h.AdminDB, tenantID, "usera@example.com", []string{"goal:write", "goal:read"})
	userB := seedRoleUser(t, h.AdminDB, tenantID, "userb@example.com", []string{"goal:write", "goal:read"})

	// Link userA → empA, userB → empB via users.employee_id.
	require.NoError(t, h.AdminDB.Exec(`UPDATE users SET employee_id = ? WHERE id = ?`, empA, userA).Error)
	require.NoError(t, h.AdminDB.Exec(`UPDATE users SET employee_id = ? WHERE id = ?`, empB, userB).Error)

	// Create a goal owned by empA using the admin actor (actorEmpID=nil → bypass ownership check).
	adminUser := seedRoleUser(t, h.AdminDB, tenantID, "admin@example.com", []string{"goal:write", "goal:read_all"})
	svc := goal.NewService(tdb)
	cycleID := activeCycle(t, ctx, svc, tenantID, adminUser, false)
	gA, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: adminUser, CycleID: cycleID, EmployeeID: empA,
		Method: goal.MethodMBO, Title: "EmpA目標",
	})
	require.NoError(t, err)

	// UserB (linked to empB) tries to update empA's goal via the HTTP layer.
	r := rbacRouter(tdb, tenantID, userB)
	body, _ := json.Marshal(map[string]any{"title": "乗っ取り", "description": ""})
	req := httptest.NewRequest(http.MethodPut, "/api/goals/"+gA.ID.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"user must not update another employee's goal")
}

// ---------------------------------------------------------------------------
// Audit PII non-inclusion
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := goal.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := activeCycle(t, ctx, svc, tenantID, actorID, false)
	// Use a recognisable synthetic title; it must NOT leak into audit rows.
	const secretTitle = "機密タイトル合成PII"
	g, err := svc.CreateGoal(ctx, goal.CreateGoalInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, EmployeeID: empID,
		Method: goal.MethodMBO, Title: secretTitle, Description: "扶養家族の機微情報合成",
	})
	require.NoError(t, err)
	_, err = svc.UpdateGoalProgress(ctx, goal.UpdateGoalProgressInput{
		TenantID: tenantID, ActorID: actorID, GoalID: g.ID, ProgressPct: 25, Comment: "コメント合成PII",
	})
	require.NoError(t, err)

	// audit_logs must reference only opaque UUIDs — no title/description/comment.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR resource_id LIKE ? OR action LIKE ?`,
		"%機密タイトル%", "%機微%", "%合成PII%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount, "audit_logs must not contain goal title/description/comment PII")

	// resource_id values must all be parseable UUIDs (opaque references).
	var resourceIDs []string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs WHERE tenant_id = ? AND resource_id IS NOT NULL`,
		tenantID,
	).Scan(&resourceIDs).Error)
	require.NotEmpty(t, resourceIDs)
	for _, rid := range resourceIDs {
		_, perr := uuid.Parse(rid)
		assert.NoErrorf(t, perr, "audit resource_id %q must be an opaque UUID", rid)
	}
}
