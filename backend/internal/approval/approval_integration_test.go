package approval_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// approvalTables is the ordered list of tables to truncate between sub-tests.
// Truncation is cascaded; order matters for FK constraints.
var approvalTables = []string{
	"audit_logs",
	"approval_steps",
	"approval_requests",
	"approval_routes",
}

// -----------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------

// seedTenant inserts a minimal tenant row and returns its ID.
func seedTenant(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(
		`INSERT INTO tenants (id, name, slug) VALUES (?, ?, ?)`,
		id, "Test Tenant "+id.String()[:8], "tenant-"+id.String()[:8],
	).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// seedUser inserts a minimal user row backed by the given tenant. The returned
// UUID can be safely used as an actor in audit.Record calls because the users
// FK constraint requires a real row.
func seedUser(t *testing.T, db *gorm.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(
		`INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, 'active')`,
		id, tenantID, "u-"+id.String()[:8]+"@example.test",
	).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// seedRoute creates a route and returns it.
func seedRoute(t *testing.T, svc *approval.Service, tenantID, actorID uuid.UUID, requestType string, steps []approval.RouteStep) *approval.ApprovalRoute {
	t.Helper()
	route, err := svc.CreateRoute(context.Background(), approval.CreateRouteInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		RequestType: requestType,
		Name:        "Test Route " + requestType,
		Steps:       steps,
	})
	require.NoError(t, err)
	return route
}

// seedDepartment inserts a minimal department row and returns its ID.
func seedDepartment(t *testing.T, db *gorm.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(
		`INSERT INTO departments (id, tenant_id, name, code) VALUES (?, ?, ?, ?)`,
		id, tenantID, "Department "+id.String()[:8], "D"+id.String()[:4],
	).Error; err != nil {
		t.Fatalf("seed department: %v", err)
	}
	return id
}

// -----------------------------------------------------------------------
// TestMultiStepApproval_Sequential
// -----------------------------------------------------------------------

// TestMultiStepApproval_Sequential verifies that a 2-step route progresses
// through each step and reaches status=approved after the final approval.
func TestMultiStepApproval_Sequential(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approver1ID := seedUser(t, h.AdminDB, tenantID)
	approver2ID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{
		{Step: 0, UserID: &approver1ID},
		{Step: 1, UserID: &approver2ID},
	}
	seedRoute(t, svc, tenantID, requesterID, "leave", steps)

	// Submit a request.
	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "leave",
		SubjectRef:  "leave-ref-001",
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusPending, ar.Status)
	assert.Equal(t, 0, ar.CurrentStep)

	// Step 0: approver1 approves.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approver1ID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusPending, ar.Status, "still pending after step 0")
	assert.Equal(t, 1, ar.CurrentStep, "advanced to step 1")

	// Step 1: approver2 approves — final step.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approver2ID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusApproved, ar.Status, "fully approved after all steps")
}

// -----------------------------------------------------------------------
// TestReject
// -----------------------------------------------------------------------

// TestReject verifies that a rejection at step 0 puts the request in status=rejected.
func TestReject(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "contract", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "contract",
		SubjectRef:  "contract-ref-002",
	})
	require.NoError(t, err)

	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approverID,
		Decision:  approval.DecisionRejected,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusRejected, ar.Status)
}

// -----------------------------------------------------------------------
// TestReturn_Diff
// -----------------------------------------------------------------------

// TestReturn_Diff verifies that returning a 2-step request from step 1 goes
// back to step 0 (status remains pending), and that returning from step 0
// puts the request into status=returned.
func TestReturn_Diff(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approver1ID := seedUser(t, h.AdminDB, tenantID)
	approver2ID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{
		{Step: 0, UserID: &approver1ID},
		{Step: 1, UserID: &approver2ID},
	}
	seedRoute(t, svc, tenantID, requesterID, "transfer", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "transfer",
		SubjectRef:  "transfer-ref-003",
	})
	require.NoError(t, err)

	// Step 0 approve → advance to step 1.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approver1ID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, ar.CurrentStep)

	// Step 1 return → back to step 0, still pending.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approver2ID,
		Decision:  approval.DecisionReturned,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusPending, ar.Status)
	assert.Equal(t, 0, ar.CurrentStep, "returned to step 0")

	// Step 0 return from step 0 → status=returned.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approver1ID,
		Decision:  approval.DecisionReturned,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusReturned, ar.Status, "returned to requester when returned from step 0")
}

// -----------------------------------------------------------------------
// TestDelegateApproval
// -----------------------------------------------------------------------

// TestDelegateApproval verifies that a delegate can submit a decision when set
// on the current step.
func TestDelegateApproval(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)
	delegateID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "leave", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "leave",
		SubjectRef:  "leave-delegate-004",
	})
	require.NoError(t, err)

	// Assign delegate.
	err = svc.SetDelegate(context.Background(), tenantID, ar.ID, 0, delegateID, approverID, nil)
	require.NoError(t, err)

	// Delegate approves.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   delegateID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusApproved, ar.Status, "delegate approval should finalise the single-step request")
}

// -----------------------------------------------------------------------
// TestUnauthorisedDecider_Forbidden
// -----------------------------------------------------------------------

// TestUnauthorisedDecider_Forbidden verifies that a random user cannot decide
// on a step to which they are not assigned (and no open approver slot exists).
func TestUnauthorisedDecider_Forbidden(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)
	randomUserID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "leave", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "leave",
		SubjectRef:  "leave-unauth-005",
	})
	require.NoError(t, err)

	// Random user tries to decide — must be rejected.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   randomUserID,
		Decision:  approval.DecisionApproved,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrForbidden, "non-assigned user must receive ErrForbidden")
}

// -----------------------------------------------------------------------
// TestInvalidTransition_TerminalRejection
// -----------------------------------------------------------------------

// TestInvalidTransition_TerminalRejection verifies that a second decision on
// an already-rejected request is refused.
func TestInvalidTransition_TerminalRejection(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "contract", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "contract",
		SubjectRef:  "contract-terminal-006",
	})
	require.NoError(t, err)

	// Reject.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approverID,
		Decision:  approval.DecisionRejected,
	})
	require.NoError(t, err)

	// Second decision must fail.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approverID,
		Decision:  approval.DecisionApproved,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrInvalidTransition, "decision after terminal state must return ErrInvalidTransition")
}

// -----------------------------------------------------------------------
// TestCancel
// -----------------------------------------------------------------------

// TestCancel verifies that the requester can cancel a pending request, and that
// a non-requester cannot.
func TestCancel(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)
	otherID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "leave", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "leave",
		SubjectRef:  "leave-cancel-007",
	})
	require.NoError(t, err)

	// Other user cannot cancel.
	_, err = svc.Cancel(context.Background(), approval.CancelInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   otherID,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrForbidden)

	// Requester can cancel.
	ar, err = svc.Cancel(context.Background(), approval.CancelInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   requesterID,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusCancelled, ar.Status)
}

// -----------------------------------------------------------------------
// TestRouteResolution_DepartmentScoped
// -----------------------------------------------------------------------

// TestRouteResolution_DepartmentScoped verifies that a department-scoped route
// takes priority over a tenant-wide fallback route when both exist.
func TestRouteResolution_DepartmentScoped(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	deptID := seedDepartment(t, h.AdminDB, tenantID)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	deptApproverID := seedUser(t, h.AdminDB, tenantID)
	globalApproverID := seedUser(t, h.AdminDB, tenantID)

	// Tenant-wide (fallback) route.
	globalSteps := []approval.RouteStep{{Step: 0, UserID: &globalApproverID}}
	seedRoute(t, svc, tenantID, requesterID, "leave", globalSteps)

	// Department-scoped route.
	deptSteps := []approval.RouteStep{{Step: 0, UserID: &deptApproverID}}
	_, err := svc.CreateRoute(context.Background(), approval.CreateRouteInput{
		TenantID:     tenantID,
		ActorID:      requesterID,
		RequestType:  "leave",
		DepartmentID: &deptID,
		Name:         "Dept Leave Route",
		Steps:        deptSteps,
	})
	require.NoError(t, err)

	// Submit with department — should use dept route.
	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:     tenantID,
		ActorID:      requesterID,
		RequestType:  "leave",
		SubjectRef:   "leave-dept-008",
		DepartmentID: &deptID,
	})
	require.NoError(t, err)

	// deptApproverID should be able to approve.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   deptApproverID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusApproved, ar.Status, "dept-scoped route should be used; deptApprover should approve successfully")
}

// -----------------------------------------------------------------------
// TestRLS_CrossTenantBlocked
// -----------------------------------------------------------------------

// TestRLS_CrossTenantBlocked verifies that tenant A cannot see or act on
// requests that belong to tenant B.
func TestRLS_CrossTenantBlocked(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	// Tenant A.
	tenantA := seedTenant(t, h.AdminDB)
	requesterA := seedUser(t, h.AdminDB, tenantA)
	approverA := seedUser(t, h.AdminDB, tenantA)
	stepsA := []approval.RouteStep{{Step: 0, UserID: &approverA}}
	seedRoute(t, svc, tenantA, requesterA, "leave", stepsA)

	arA, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantA,
		ActorID:     requesterA,
		RequestType: "leave",
		SubjectRef:  "leave-rls-A",
	})
	require.NoError(t, err)

	// Tenant B.
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB)

	// Tenant B actor cannot fetch tenant A request.
	_, err = svc.GetRequest(context.Background(), tenantB, arA.ID, actorB)
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrRequestNotFound, "cross-tenant GetRequest must return not-found")

	// Tenant B actor cannot decide on tenant A request.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantB,
		RequestID: arA.ID,
		ActorID:   actorB,
		Decision:  approval.DecisionApproved,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrRequestNotFound, "cross-tenant Decide must return not-found")
}

// -----------------------------------------------------------------------
// TestAudit_DecisionRecorded
// -----------------------------------------------------------------------

// TestAudit_DecisionRecorded verifies that each decision results in at least
// one audit_logs row with the expected action and resource_type.
func TestAudit_DecisionRecorded(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "leave", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "leave",
		SubjectRef:  "leave-audit-009",
	})
	require.NoError(t, err)

	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approverID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)

	// Check audit_logs as admin (bypasses RLS).
	var rows []struct {
		Action       string `gorm:"column:action"`
		ResourceType string `gorm:"column:resource_type"`
		ResourceID   string `gorm:"column:resource_id"`
	}
	err = h.AdminDB.Raw(
		`SELECT action, resource_type, resource_id
		 FROM audit_logs
		 WHERE resource_type = 'approval_request'
		   AND resource_id = ?
		 ORDER BY seq ASC`,
		ar.ID.String(),
	).Scan(&rows).Error
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 2, "expect at least submitted + approved audit rows")

	actions := make([]string, len(rows))
	for i, r := range rows {
		actions[i] = r.Action
		// Verify no PII: resource_id should be a UUID string, not an email or name.
		_, parseErr := uuid.Parse(r.ResourceID)
		assert.NoError(t, parseErr, "resource_id must be a UUID (no PII): got %q", r.ResourceID)
	}
	assert.Contains(t, actions, "approval.submitted")
	assert.Contains(t, actions, "approval.approved")
}

// -----------------------------------------------------------------------
// TestRoleBasedStep_Authorisation  [MUSTFIX 1]
// -----------------------------------------------------------------------

// seedRole inserts a role row with the given name and returns its ID.
func seedRole(t *testing.T, db *gorm.DB, tenantID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, ?, '{"perms":[]}')`,
		id, tenantID, name,
	).Error; err != nil {
		t.Fatalf("seed role %q: %v", name, err)
	}
	return id
}

// seedUserWithRole inserts a user belonging to tenantID and assigns roleID.
func seedUserWithRole(t *testing.T, db *gorm.DB, tenantID, roleID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := db.Exec(
		`INSERT INTO users (id, tenant_id, email, status, role_id) VALUES (?, ?, ?, 'active', ?)`,
		id, tenantID, "u-"+id.String()[:8]+"@example.test", roleID,
	).Error; err != nil {
		t.Fatalf("seed user with role: %v", err)
	}
	return id
}

// TestRoleBasedStep_Authorisation verifies that:
//   - A user whose role matches the step's required role may decide.
//   - A user whose role does NOT match is rejected with ErrForbidden.
//   - A user with no role is rejected with ErrForbidden.
func TestRoleBasedStep_Authorisation(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)

	// Create two roles: "manager" (required) and "employee" (not sufficient).
	managerRoleID := seedRole(t, h.AdminDB, tenantID, "manager")
	employeeRoleID := seedRole(t, h.AdminDB, tenantID, "employee")

	managerUserID := seedUserWithRole(t, h.AdminDB, tenantID, managerRoleID)
	employeeUserID := seedUserWithRole(t, h.AdminDB, tenantID, employeeRoleID)
	noRoleUserID := seedUser(t, h.AdminDB, tenantID) // no role_id

	// Route step: role="manager", no user_id assigned.
	steps := []approval.RouteStep{
		{Step: 0, Role: "manager"},
	}
	seedRoute(t, svc, tenantID, requesterID, "role_leave", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "role_leave",
		SubjectRef:  "role-leave-010",
	})
	require.NoError(t, err)

	// Employee-role user must be rejected.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   employeeUserID,
		Decision:  approval.DecisionApproved,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrForbidden, "employee-role user must be forbidden on manager step")

	// No-role user must also be rejected.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   noRoleUserID,
		Decision:  approval.DecisionApproved,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrForbidden, "user with no role must be forbidden on manager step")

	// Manager-role user must succeed.
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   managerUserID,
		Decision:  approval.DecisionApproved,
	})
	require.NoError(t, err)
	assert.Equal(t, approval.StatusApproved, ar.Status, "manager-role user should be able to approve")
}

// -----------------------------------------------------------------------
// TestDecide_ReturnedStatus_Rejected  [Imp 5]
// -----------------------------------------------------------------------

// TestDecide_ReturnedStatus_Rejected verifies that attempting to Decide on a
// request whose status is "returned" (not "pending") is refused with
// ErrInvalidTransition.
func TestDecide_ReturnedStatus_Rejected(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "returned_state_test", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "returned_state_test",
		SubjectRef:  "returned-state-011",
	})
	require.NoError(t, err)

	// Return from step 0 → status becomes "returned".
	ar, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approverID,
		Decision:  approval.DecisionReturned,
	})
	require.NoError(t, err)
	require.Equal(t, approval.StatusReturned, ar.Status, "request should be in returned state")

	// Attempting to decide again must fail with ErrInvalidTransition.
	_, err = svc.Decide(context.Background(), approval.DecideInput{
		TenantID:  tenantID,
		RequestID: ar.ID,
		ActorID:   approverID,
		Decision:  approval.DecisionApproved,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrInvalidTransition,
		"Decide on a returned request must return ErrInvalidTransition")
}

// -----------------------------------------------------------------------
// TestSetDelegate_CrossTenantDelegate_Rejected  [MUSTFIX 2]
// -----------------------------------------------------------------------

// TestSetDelegate_CrossTenantDelegate_Rejected verifies that assigning a user
// from a different tenant as a delegate is rejected.
func TestSetDelegate_CrossTenantDelegate_Rejected(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(approvalTables...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	// Tenant A setup.
	tenantA := seedTenant(t, h.AdminDB)
	requesterA := seedUser(t, h.AdminDB, tenantA)
	approverA := seedUser(t, h.AdminDB, tenantA)
	steps := []approval.RouteStep{{Step: 0, UserID: &approverA}}
	seedRoute(t, svc, tenantA, requesterA, "leave", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantA,
		ActorID:     requesterA,
		RequestType: "leave",
		SubjectRef:  "cross-tenant-delegate-012",
	})
	require.NoError(t, err)

	// Tenant B — delegate from a different tenant.
	tenantB := seedTenant(t, h.AdminDB)
	userB := seedUser(t, h.AdminDB, tenantB)

	// Attempting to set a tenant-B user as delegate on tenant-A request must fail.
	err = svc.SetDelegate(context.Background(), tenantA, ar.ID, 0, userB, approverA, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrForbidden,
		"cross-tenant delegate assignment must be rejected with ErrForbidden")
}

// -----------------------------------------------------------------------
// TestSetDelegate_ActorAuthorisation  [MUSTFIX 3]
// -----------------------------------------------------------------------

// TestSetDelegate_ActorAuthorisation verifies that only the requester, the
// assigned step approver, or an approval:admin user may set a delegate.
// A random user with approval:write should be refused.
func TestSetDelegate_ActorAuthorisation(t *testing.T) {
	h := testdb.NewHarness(t)
	h.TruncateTables(append(approvalTables, "roles")...)

	tdb := tenantdb.New(h.AppDB)
	svc := approval.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	requesterID := seedUser(t, h.AdminDB, tenantID)
	approverID := seedUser(t, h.AdminDB, tenantID)
	delegateID := seedUser(t, h.AdminDB, tenantID)
	randomUserID := seedUser(t, h.AdminDB, tenantID)

	// Admin user: give them an approval:admin role.
	adminRoleID := uuid.New()
	if err := h.AdminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, 'admin', '{"perms":["approval:admin"]}')`,
		adminRoleID, tenantID,
	).Error; err != nil {
		t.Fatalf("seed admin role: %v", err)
	}
	adminUserID := seedUserWithRole(t, h.AdminDB, tenantID, adminRoleID)

	steps := []approval.RouteStep{{Step: 0, UserID: &approverID}}
	seedRoute(t, svc, tenantID, requesterID, "delegate_auth_test", steps)

	ar, err := svc.Submit(context.Background(), approval.SubmitInput{
		TenantID:    tenantID,
		ActorID:     requesterID,
		RequestType: "delegate_auth_test",
		SubjectRef:  "delegate-auth-013",
	})
	require.NoError(t, err)

	// Random user (not requester, not approver, not admin) must be rejected.
	err = svc.SetDelegate(context.Background(), tenantID, ar.ID, 0, delegateID, randomUserID, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, approval.ErrForbidden,
		"random user must not be allowed to set delegate")

	// The requester may set the delegate.
	err = svc.SetDelegate(context.Background(), tenantID, ar.ID, 0, delegateID, requesterID, nil)
	require.NoError(t, err, "requester must be able to set delegate")

	// Reset the delegate to nil to allow re-testing with approver.
	if err := h.AdminDB.Exec(
		`UPDATE approval_steps SET delegate_user_id = NULL WHERE request_id = ? AND step_index = 0`,
		ar.ID,
	).Error; err != nil {
		t.Fatalf("reset delegate: %v", err)
	}

	// The assigned approver may also set the delegate.
	err = svc.SetDelegate(context.Background(), tenantID, ar.ID, 0, delegateID, approverID, nil)
	require.NoError(t, err, "assigned approver must be able to set delegate")

	// Reset again.
	if err := h.AdminDB.Exec(
		`UPDATE approval_steps SET delegate_user_id = NULL WHERE request_id = ? AND step_index = 0`,
		ar.ID,
	).Error; err != nil {
		t.Fatalf("reset delegate: %v", err)
	}

	// An approval:admin user may also set the delegate.
	err = svc.SetDelegate(context.Background(), tenantID, ar.ID, 0, delegateID, adminUserID, nil)
	require.NoError(t, err, "approval:admin user must be able to set delegate")
}
