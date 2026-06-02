package employee_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/employee"
	"github.com/your-org/hr-saas/internal/platform/audit"
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

// truncateAll resets all tables between sub-tests.
func truncateAll(h *testdb.Harness) {
	h.TruncateTables("audit_logs", "employment_contracts", "employee_assignments",
		"employees", "departments", "users", "sessions", "tenants")
}

// ---------------------------------------------------------------------------
// Employee CRUD
// ---------------------------------------------------------------------------

func TestEmployeeCRUD(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := employee.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")

	t.Cleanup(func() { truncateAll(h) })

	ip := "127.0.0.1"
	hiredOn := "2020-04-01"

	// Create
	emp, err := svc.CreateEmployee(ctx, employee.CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeCode:   "EMP001",
		LastName:       "山田",
		FirstName:      "太郎",
		EmploymentType: "full_time",
		Status:         "active",
		HiredOn:        parseDate(&hiredOn),
		IP:             &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "山田", emp.LastName)
	assert.Equal(t, "太郎", emp.FirstName)
	assert.Equal(t, "EMP001", emp.EmployeeCode)
	assert.Equal(t, tenantID, emp.TenantID)

	// Get
	got, err := svc.GetEmployee(ctx, tenantID, emp.ID)
	require.NoError(t, err)
	assert.Equal(t, emp.ID, got.ID)
	assert.Equal(t, "山田", got.LastName)

	// List
	emps, err := svc.ListEmployees(ctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, emps, 1)

	// Update
	email := "yamada@example.com"
	updated, err := svc.UpdateEmployee(ctx, employee.UpdateEmployeeInput{
		TenantID:       tenantID,
		ID:             emp.ID,
		ActorID:        actorID,
		EmployeeCode:   "EMP001",
		LastName:       "山田",
		FirstName:      "花子",
		Email:          &email,
		EmploymentType: "part_time",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "花子", updated.FirstName)
	assert.Equal(t, "part_time", updated.EmploymentType)
	assert.Equal(t, &email, updated.Email)

	// Delete
	err = svc.DeleteEmployee(ctx, tenantID, emp.ID, actorID, &ip)
	require.NoError(t, err)

	_, err = svc.GetEmployee(ctx, tenantID, emp.ID)
	assert.ErrorIs(t, err, employee.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Assignment (発令履歴)
// ---------------------------------------------------------------------------

func TestAssignmentHistory(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := employee.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")

	t.Cleanup(func() { truncateAll(h) })

	ip := "127.0.0.1"

	// Seed employee.
	emp, err := svc.CreateEmployee(ctx, employee.CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeCode:   "EMP010",
		LastName:       "鈴木",
		FirstName:      "一郎",
		EmploymentType: "full_time",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)

	pos1 := "Junior Engineer"
	asgn1, err := svc.CreateAssignment(ctx, employee.CreateAssignmentInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    emp.ID,
		Position:      &pos1,
		EffectiveFrom: time.Date(2021, 4, 1, 0, 0, 0, 0, time.UTC),
		IP:            &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "Junior Engineer", *asgn1.Position)

	pos2 := "Senior Engineer"
	asgn2, err := svc.CreateAssignment(ctx, employee.CreateAssignmentInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    emp.ID,
		Position:      &pos2,
		EffectiveFrom: time.Date(2023, 4, 1, 0, 0, 0, 0, time.UTC),
		IP:            &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "Senior Engineer", *asgn2.Position)

	// List — should be ordered by effective_from DESC (most recent first).
	asgns, err := svc.ListAssignments(ctx, tenantID, emp.ID)
	require.NoError(t, err)
	require.Len(t, asgns, 2)
	assert.Equal(t, "Senior Engineer", *asgns[0].Position, "most recent assignment first")
	assert.Equal(t, "Junior Engineer", *asgns[1].Position)
	// Verify strict ordering.
	assert.True(t, asgns[0].EffectiveFrom.After(asgns[1].EffectiveFrom),
		"assignments must be ordered by effective_from DESC")
}

// ---------------------------------------------------------------------------
// Contract lifecycle
// ---------------------------------------------------------------------------

func TestContractLifecycle(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := employee.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")

	t.Cleanup(func() { truncateAll(h) })

	ip := "127.0.0.1"

	emp, err := svc.CreateEmployee(ctx, employee.CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeCode:   "EMP020",
		LastName:       "佐藤",
		FirstName:      "二郎",
		EmploymentType: "contract",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)

	// Create contract — starts as draft.
	ctr, err := svc.CreateContract(ctx, employee.CreateContractInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		EmployeeID:        emp.ID,
		ContractType:      "fixed_term",
		StartDate:         time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		WorkingConditions: []byte(`{"hours_per_week":40}`),
		IP:                &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "draft", ctr.Status)
	assert.Nil(t, ctr.SignedAt)

	// Activate — signed_at should be set.
	active, err := svc.UpdateContractStatus(ctx, employee.UpdateContractStatusInput{
		TenantID: tenantID,
		ID:       ctr.ID,
		ActorID:  actorID,
		Status:   "active",
		IP:       &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "active", active.Status)
	assert.NotNil(t, active.SignedAt, "signed_at must be set when status transitions to active")

	// Expire.
	expired, err := svc.UpdateContractStatus(ctx, employee.UpdateContractStatusInput{
		TenantID: tenantID,
		ID:       ctr.ID,
		ActorID:  actorID,
		Status:   "expired",
		IP:       &ip,
	})
	require.NoError(t, err)
	assert.Equal(t, "expired", expired.Status)

	// Get contract directly.
	fetched, err := svc.GetContract(ctx, tenantID, ctr.ID)
	require.NoError(t, err)
	assert.Equal(t, "expired", fetched.Status)

	// List contracts for the employee.
	ctrs, err := svc.ListContracts(ctx, tenantID, emp.ID)
	require.NoError(t, err)
	assert.Len(t, ctrs, 1)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestEmployeeRLSCrossTenant(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := employee.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	tenantB := seedTenant(t, h.AdminDB)

	actorA := seedUser(t, h.AdminDB, tenantA, "a@example.com")
	actorB := seedUser(t, h.AdminDB, tenantB, "b@example.com")

	t.Cleanup(func() { truncateAll(h) })

	ip := "127.0.0.1"

	// Create an employee under tenant A.
	empA, err := svc.CreateEmployee(ctx, employee.CreateEmployeeInput{
		TenantID:       tenantA,
		ActorID:        actorA,
		EmployeeCode:   "A001",
		LastName:       "田中",
		FirstName:      "三郎",
		EmploymentType: "full_time",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)

	// Tenant B cannot see it via Get.
	_, err = svc.GetEmployee(ctx, tenantB, empA.ID)
	assert.ErrorIs(t, err, employee.ErrNotFound, "cross-tenant GET must be denied")

	// Tenant B list is empty.
	emps, err := svc.ListEmployees(ctx, tenantB)
	require.NoError(t, err)
	assert.Empty(t, emps, "cross-tenant List must return no rows")

	// Tenant B cannot delete tenant A's employee.
	err = svc.DeleteEmployee(ctx, tenantB, empA.ID, actorB, &ip)
	assert.ErrorIs(t, err, employee.ErrNotFound, "cross-tenant DELETE must be denied")

	// Create a contract under tenant A.
	ctr, err := svc.CreateContract(ctx, employee.CreateContractInput{
		TenantID:     tenantA,
		ActorID:      actorA,
		EmployeeID:   empA.ID,
		ContractType: "permanent",
		StartDate:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		IP:           &ip,
	})
	require.NoError(t, err)

	// Tenant B cannot read the contract.
	_, err = svc.GetContract(ctx, tenantB, ctr.ID)
	assert.ErrorIs(t, err, employee.ErrContractNotFound, "cross-tenant contract GET must be denied")

	// Tenant B cannot update the contract status.
	_, err = svc.UpdateContractStatus(ctx, employee.UpdateContractStatusInput{
		TenantID: tenantB,
		ID:       ctr.ID,
		ActorID:  actorB,
		Status:   "active",
		IP:       &ip,
	})
	assert.ErrorIs(t, err, employee.ErrContractNotFound, "cross-tenant contract status update must be denied")
}

// ---------------------------------------------------------------------------
// Audit chain integrity + PII check
// ---------------------------------------------------------------------------

func TestEmployeeAudit(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := employee.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")

	t.Cleanup(func() { truncateAll(h) })

	ip := "127.0.0.1"

	emp, err := svc.CreateEmployee(ctx, employee.CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeCode:   "AUD001",
		LastName:       "高橋",
		FirstName:      "四郎",
		EmploymentType: "full_time",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)

	_, err = svc.CreateAssignment(ctx, employee.CreateAssignmentInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    emp.ID,
		EffectiveFrom: time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		IP:            &ip,
	})
	require.NoError(t, err)

	ctr, err := svc.CreateContract(ctx, employee.CreateContractInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		EmployeeID:   emp.ID,
		ContractType: "permanent",
		StartDate:    time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC),
		IP:           &ip,
	})
	require.NoError(t, err)

	_, err = svc.UpdateContractStatus(ctx, employee.UpdateContractStatusInput{
		TenantID: tenantID,
		ID:       ctr.ID,
		ActorID:  actorID,
		Status:   "active",
		IP:       &ip,
	})
	require.NoError(t, err)

	// Verify audit chain.
	err = tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ok, err := audit.VerifyChain(tx, tenantID)
		require.NoError(t, err)
		assert.True(t, ok, "audit chain must be intact after employee/assignment/contract mutations")

		// Confirm no PII in resource_id column.
		var rows []struct {
			ResourceID *string `gorm:"column:resource_id"`
			Action     string  `gorm:"column:action"`
		}
		if err := tx.Raw(
			`SELECT resource_id, action FROM audit_logs WHERE tenant_id = ? ORDER BY seq`,
			tenantID,
		).Scan(&rows).Error; err != nil {
			return err
		}
		// Expect: employee.created, assignment.created, contract.created, contract.status_updated
		// (plus the user that was seeded via admin, which uses adminDB bypassing audit recording)
		assert.GreaterOrEqual(t, len(rows), 4, "expected at least 4 audit rows")

		for _, r := range rows {
			if r.ResourceID != nil {
				_, parseErr := uuid.Parse(*r.ResourceID)
				assert.NoError(t, parseErr,
					"resource_id must be a UUID, not PII (action=%s, resource_id=%s)",
					r.Action, *r.ResourceID)
			}
		}
		return nil
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// [Security: MUSTFIX 2] Contract status transition validation
// ---------------------------------------------------------------------------

func TestContractStatusTransitions(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := employee.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	ip := "127.0.0.1"

	emp, err := svc.CreateEmployee(ctx, employee.CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeCode:   "TR001",
		LastName:       "遷移",
		FirstName:      "テスト",
		EmploymentType: "full_time",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)

	newContract := func(t *testing.T) *employee.Contract {
		t.Helper()
		ctr, err := svc.CreateContract(ctx, employee.CreateContractInput{
			TenantID:     tenantID,
			ActorID:      actorID,
			EmployeeID:   emp.ID,
			ContractType: "fixed_term",
			StartDate:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			IP:           &ip,
		})
		require.NoError(t, err)
		assert.Equal(t, "draft", ctr.Status)
		assert.Nil(t, ctr.SignedAt, "new contract must not have signed_at")
		return ctr
	}

	transition := func(t *testing.T, ctrID, status string) (*employee.Contract, error) {
		t.Helper()
		id, _ := uuid.Parse(ctrID)
		return svc.UpdateContractStatus(ctx, employee.UpdateContractStatusInput{
			TenantID: tenantID,
			ID:       id,
			ActorID:  actorID,
			Status:   status,
			IP:       &ip,
		})
	}

	t.Run("draft_to_active_sets_signed_at", func(t *testing.T) {
		ctr := newContract(t)
		active, err := transition(t, ctr.ID.String(), "active")
		require.NoError(t, err)
		assert.Equal(t, "active", active.Status)
		assert.NotNil(t, active.SignedAt, "signed_at must be set on draft→active")
	})

	t.Run("active_to_expired_allowed", func(t *testing.T) {
		ctr := newContract(t)
		_, err := transition(t, ctr.ID.String(), "active")
		require.NoError(t, err)
		expired, err := transition(t, ctr.ID.String(), "expired")
		require.NoError(t, err)
		assert.Equal(t, "expired", expired.Status)
	})

	t.Run("active_to_terminated_allowed", func(t *testing.T) {
		ctr := newContract(t)
		_, err := transition(t, ctr.ID.String(), "active")
		require.NoError(t, err)
		termed, err := transition(t, ctr.ID.String(), "terminated")
		require.NoError(t, err)
		assert.Equal(t, "terminated", termed.Status)
	})

	t.Run("draft_to_terminated_allowed", func(t *testing.T) {
		ctr := newContract(t)
		termed, err := transition(t, ctr.ID.String(), "terminated")
		require.NoError(t, err)
		assert.Equal(t, "terminated", termed.Status)
	})

	t.Run("expired_rollback_rejected", func(t *testing.T) {
		ctr := newContract(t)
		_, err := transition(t, ctr.ID.String(), "active")
		require.NoError(t, err)
		_, err = transition(t, ctr.ID.String(), "expired")
		require.NoError(t, err)
		// expired → active must be rejected.
		_, err = transition(t, ctr.ID.String(), "active")
		assert.ErrorIs(t, err, employee.ErrInvalidTransition,
			"rollback from expired to active must be rejected")
	})

	t.Run("terminated_rollback_rejected", func(t *testing.T) {
		ctr := newContract(t)
		_, err := transition(t, ctr.ID.String(), "terminated")
		require.NoError(t, err)
		// terminated → draft must be rejected.
		_, err = transition(t, ctr.ID.String(), "draft")
		assert.ErrorIs(t, err, employee.ErrInvalidTransition,
			"rollback from terminated to draft must be rejected")
	})

	t.Run("active_to_active_idempotent_rejected", func(t *testing.T) {
		ctr := newContract(t)
		_, err := transition(t, ctr.ID.String(), "active")
		require.NoError(t, err)
		// active → active must be rejected (re-signing not allowed).
		_, err = transition(t, ctr.ID.String(), "active")
		assert.ErrorIs(t, err, employee.ErrInvalidTransition,
			"active→active re-signing must be rejected")
	})

	t.Run("signed_at_not_overwritten_on_subsequent_update", func(t *testing.T) {
		// Draft → active sets signed_at. Then active → expired should NOT
		// clear or alter signed_at.
		ctr := newContract(t)
		active, err := transition(t, ctr.ID.String(), "active")
		require.NoError(t, err)
		require.NotNil(t, active.SignedAt)
		firstSignedAt := *active.SignedAt

		expired, err := transition(t, ctr.ID.String(), "expired")
		require.NoError(t, err)
		require.NotNil(t, expired.SignedAt, "signed_at must be preserved after expiry")
		assert.Equal(t, firstSignedAt, *expired.SignedAt,
			"signed_at must not be altered after initial signing")
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func parseDate(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil
	}
	return &t
}
