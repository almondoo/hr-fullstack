package onboarding_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/onboarding"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers
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

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"offboarding_policies",
		"employee_intake_forms",
		"onboarding_tasks",
		"onboarding_checklist_templates",
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// syntheticKey returns a synthetic 32-byte test key (not a real secret).
func syntheticKey() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

// setupCrypto injects a synthetic key for the global cipher so tests run
// without FIELD_ENCRYPTION_KEY set in the environment.
func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

// seedRoleWithPermissions inserts a role with the given permissions and returns
// its ID.  permissions is a JSON-encoded perms array, e.g.
// `{"perms":["intake:read_sensitive"]}`.
func seedRoleWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, name string, permsJSON string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, ?, ?::jsonb)`,
		id, tenantID, name, permsJSON,
	).Error)
	return id
}

// assignRole updates a user's role_id to the given role.
func assignRole(t *testing.T, adminDB *gorm.DB, userID, roleID uuid.UUID) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`UPDATE users SET role_id = ? WHERE id = ?`, roleID, userID,
	).Error)
}

// ---------------------------------------------------------------------------
// Checklist template tests
// ---------------------------------------------------------------------------

func TestCreateAndListTemplates(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	items := json.RawMessage(`[{"title":"PC設定","category":"IT","due_offset_days":0}]`)

	tmpl, err := svc.CreateTemplate(ctx, onboarding.CreateTemplateInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		Name:      "標準入社テンプレート",
		Kind:      "onboarding",
		ItemsJSON: []byte(items),
	})
	require.NoError(t, err)
	assert.Equal(t, "onboarding", tmpl.Kind)
	assert.True(t, tmpl.Active)

	list, err := svc.ListTemplates(ctx, tenantID, "onboarding")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, tmpl.ID, list[0].ID)
}

// ---------------------------------------------------------------------------
// Task generation and status transition tests (LM-001)
// ---------------------------------------------------------------------------

func TestGenerateTasksFromTemplate(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	items := json.RawMessage(`[
		{"title":"社員証発行","category":"総務","due_offset_days":0},
		{"title":"PC設定","category":"IT","due_offset_days":3}
	]`)
	tmpl, err := svc.CreateTemplate(ctx, onboarding.CreateTemplateInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		Name:      "入社チェックリスト",
		Kind:      "onboarding",
		ItemsJSON: []byte(items),
	})
	require.NoError(t, err)

	baseDate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tasks, err := svc.GenerateTasks(ctx, onboarding.GenerateTasksInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TemplateID: tmpl.ID,
		BaseDate:   baseDate,
	})
	require.NoError(t, err)
	assert.Len(t, tasks, 2)
	assert.Equal(t, "pending", tasks[0].Status)
	assert.Nil(t, tasks[0].DueDate)                                      // offset 0
	assert.Equal(t, "2026-06-04", tasks[1].DueDate.Format("2006-01-02")) // offset 3
}

func TestTaskStatusTransition(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP002", "active")
	t.Cleanup(func() { truncateAll(h) })

	items := json.RawMessage(`[{"title":"手続き1","category":"","due_offset_days":0}]`)
	tmpl, err := svc.CreateTemplate(ctx, onboarding.CreateTemplateInput{
		TenantID: tenantID, ActorID: actorID, Name: "T", Kind: "onboarding",
		ItemsJSON: []byte(items),
	})
	require.NoError(t, err)

	tasks, err := svc.GenerateTasks(ctx, onboarding.GenerateTasksInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		TemplateID: tmpl.ID, BaseDate: time.Now(),
	})
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	taskID := tasks[0].ID

	// pending → in_progress
	updated, err := svc.UpdateTaskStatus(ctx, onboarding.UpdateTaskStatusInput{
		TenantID: tenantID, ID: taskID, ActorID: actorID, Status: "in_progress",
	})
	require.NoError(t, err)
	assert.Equal(t, "in_progress", updated.Status)
	assert.Nil(t, updated.CompletedAt)

	// in_progress → done
	updated, err = svc.UpdateTaskStatus(ctx, onboarding.UpdateTaskStatusInput{
		TenantID: tenantID, ID: taskID, ActorID: actorID, Status: "done",
	})
	require.NoError(t, err)
	assert.Equal(t, "done", updated.Status)
	assert.NotNil(t, updated.CompletedAt, "completed_at must be set when status = done")

	// done → in_progress must fail (terminal)
	_, err = svc.UpdateTaskStatus(ctx, onboarding.UpdateTaskStatusInput{
		TenantID: tenantID, ID: taskID, ActorID: actorID, Status: "in_progress",
	})
	assert.ErrorIs(t, err, onboarding.ErrInvalidTransition, "done is a terminal state")
}

// ---------------------------------------------------------------------------
// Intake form tests (LM-003)
// ---------------------------------------------------------------------------

func TestSubmitIntakeFormEncryptsBankAccount(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP003", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Synthetic bank account number — NOT a real account.
	syntheticBankAccount := "合成銀行 001-1234567"

	form, err := svc.SubmitIntakeForm(ctx, onboarding.SubmitIntakeFormInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		EmployeeID:           empID,
		EmergencyContactJSON: []byte(`{"name":"合成太郎","relationship":"配偶者","phone":"090-0000-0000"}`),
		CommuteJSON:          []byte(`{"route":"自宅〜最寄駅"}`),
		DependentsJSON:       []byte(`[]`),
		BankAccountPlaintext: []byte(syntheticBankAccount),
	})
	require.NoError(t, err)
	assert.Equal(t, "submitted", form.Status)
	// BankAccountEnc is cleared from the returned struct.
	assert.Nil(t, form.BankAccountEnc, "BankAccountEnc must not be returned from SubmitIntakeForm")

	// Verify the ciphertext is stored in the DB and is NOT the plaintext.
	var row struct {
		BankAccountEnc []byte `gorm:"column:bank_account_enc"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT bank_account_enc FROM employee_intake_forms WHERE employee_id = ? LIMIT 1`,
		empID,
	).Scan(&row).Error)

	require.NotNil(t, row.BankAccountEnc, "bank_account_enc must be stored in DB")
	assert.NotEqual(t, []byte(syntheticBankAccount), row.BankAccountEnc,
		"plaintext must NOT be stored in DB")
}

func TestGetIntakeFormSensitiveDecryptsBankAccount(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP004", "active")
	// Grant the actor intake:read_sensitive so the service-layer check passes.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["intake:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	syntheticBankAccount := "合成銀行 002-9876543"

	_, err := svc.SubmitIntakeForm(ctx, onboarding.SubmitIntakeFormInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		EmployeeID:           empID,
		EmergencyContactJSON: []byte(`{}`),
		CommuteJSON:          []byte(`{}`),
		DependentsJSON:       []byte(`[]`),
		BankAccountPlaintext: []byte(syntheticBankAccount),
	})
	require.NoError(t, err)

	// Read with sensitive permission — service layer verifies the permission
	// against the DB; bank account must be decrypted.
	form, bankPlaintext, err := svc.GetIntakeForm(ctx, onboarding.GetIntakeFormInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		ReadSensitive: true,
	})
	require.NoError(t, err)
	assert.Nil(t, form.BankAccountEnc, "ciphertext must not be returned in form struct")
	assert.Equal(t, []byte(syntheticBankAccount), bankPlaintext,
		"decrypted bank account must match original plaintext")
}

func TestGetIntakeFormWithoutSensitivePermissionMasksBankAccount(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP005", "active")
	t.Cleanup(func() { truncateAll(h) })

	syntheticBankAccount := "合成銀行 003-1112222"

	_, err := svc.SubmitIntakeForm(ctx, onboarding.SubmitIntakeFormInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		EmployeeID:           empID,
		EmergencyContactJSON: []byte(`{}`),
		CommuteJSON:          []byte(`{}`),
		DependentsJSON:       []byte(`[]`),
		BankAccountPlaintext: []byte(syntheticBankAccount),
	})
	require.NoError(t, err)

	// Read WITHOUT sensitive permission.
	form, bankPlaintext, err := svc.GetIntakeForm(ctx, onboarding.GetIntakeFormInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		ReadSensitive: false, // no sensitive permission
	})
	require.NoError(t, err)
	assert.Nil(t, form.BankAccountEnc, "ciphertext must not be returned")
	assert.Nil(t, bankPlaintext, "plaintext must not be returned without sensitive permission")
}

// TestGetIntakeFormServiceLayerBlocksUnpermittedSensitiveRead verifies that the
// service-layer permission check rejects ReadSensitive=true when the actor does
// not hold intake:read_sensitive — even if the HTTP middleware were bypassed.
// This is the multi-layer defence-in-depth test for MUSTFIX 2.
func TestGetIntakeFormServiceLayerBlocksUnpermittedSensitiveRead(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	// Actor has no role — no intake:read_sensitive.
	actorID := seedUser(t, h.AdminDB, tenantID, "unpermitted@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP005b", "active")
	t.Cleanup(func() { truncateAll(h) })

	syntheticBankAccount := "合成銀行 099-0000000"

	_, err := svc.SubmitIntakeForm(ctx, onboarding.SubmitIntakeFormInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		EmployeeID:           empID,
		EmergencyContactJSON: []byte(`{}`),
		CommuteJSON:          []byte(`{}`),
		DependentsJSON:       []byte(`[]`),
		BankAccountPlaintext: []byte(syntheticBankAccount),
	})
	require.NoError(t, err)

	// Call service directly with ReadSensitive=true but no permission — must fail.
	_, bankPlaintext, err := svc.GetIntakeForm(ctx, onboarding.GetIntakeFormInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		ReadSensitive: true, // requested but actor has no permission
	})
	assert.ErrorIs(t, err, onboarding.ErrForbidden,
		"service layer must return ErrForbidden when actor lacks intake:read_sensitive")
	assert.Nil(t, bankPlaintext, "plaintext must not be returned when service layer rejects the request")
}

// ---------------------------------------------------------------------------
// Offboarding tests (LM-004)
// ---------------------------------------------------------------------------

func TestInitiateOffboarding(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP006", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Create offboarding template.
	items := json.RawMessage(`[
		{"title":"貸与PCの返却","category":"IT","due_offset_days":-3},
		{"title":"アカウント停止","category":"IT","due_offset_days":0}
	]`)
	tmpl, err := svc.CreateTemplate(ctx, onboarding.CreateTemplateInput{
		TenantID: tenantID, ActorID: actorID, Name: "退職チェックリスト",
		Kind: "offboarding", ItemsJSON: []byte(items),
	})
	require.NoError(t, err)

	lastDay := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	expiresOn := time.Date(2033, 6, 30, 0, 0, 0, 0, time.UTC) // 7 years retention

	tasks, policy, err := svc.InitiateOffboarding(ctx, onboarding.InitiateOffboardingInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		TemplateID:      &tmpl.ID,
		LastWorkingDate: &lastDay,
		RetentionLabel:  "7years",
		ExpiresOn:       &expiresOn,
	})
	require.NoError(t, err)
	assert.Len(t, tasks, 2)
	assert.Equal(t, "offboarding", tasks[0].Kind)
	assert.Equal(t, "pending", tasks[0].Status)
	assert.Equal(t, "7years", policy.RetentionLabel)

	// Verify employee status is now "leaving".
	var empRow struct {
		Status string `gorm:"column:status"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT status FROM employees WHERE id = ? LIMIT 1`, empID,
	).Scan(&empRow).Error)
	assert.Equal(t, "leaving", empRow.Status)

	// Verify data is NOT deleted — employee row still exists.
	var empCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE id = ?`, empID,
	).Scan(&empCount).Error)
	assert.Equal(t, int64(1), empCount, "employee must not be deleted during offboarding")
}

func TestCompleteOffboarding(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP007", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Initiate offboarding with no template (no tasks to complete).
	_, _, err := svc.InitiateOffboarding(ctx, onboarding.InitiateOffboardingInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeID:     empID,
		RetentionLabel: "7years",
	})
	require.NoError(t, err)

	// Complete offboarding (no pending tasks).
	err = svc.CompleteOffboarding(ctx, onboarding.CompleteOffboardingInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
	})
	require.NoError(t, err)

	// Verify employee status is "left".
	var empRow struct {
		Status string `gorm:"column:status"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT status FROM employees WHERE id = ? LIMIT 1`, empID,
	).Scan(&empRow).Error)
	assert.Equal(t, "left", empRow.Status)

	// Verify employee row still exists (no physical deletion).
	var empCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE id = ?`, empID,
	).Scan(&empCount).Error)
	assert.Equal(t, int64(1), empCount, "employee must never be physically deleted")
}

func TestCompleteOffboardingBlockedByPendingTasks(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP008", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Create template with one task.
	items := json.RawMessage(`[{"title":"PC返却","category":"IT","due_offset_days":0}]`)
	tmpl, err := svc.CreateTemplate(ctx, onboarding.CreateTemplateInput{
		TenantID: tenantID, ActorID: actorID, Name: "Offboarding", Kind: "offboarding",
		ItemsJSON: []byte(items),
	})
	require.NoError(t, err)

	_, _, err = svc.InitiateOffboarding(ctx, onboarding.InitiateOffboardingInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, TemplateID: &tmpl.ID,
		RetentionLabel: "7years",
	})
	require.NoError(t, err)

	// Attempt to complete while task is still pending.
	err = svc.CompleteOffboarding(ctx, onboarding.CompleteOffboardingInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	assert.ErrorIs(t, err, onboarding.ErrInvalidTransition,
		"complete offboarding must fail when pending tasks exist")
}

func TestOffboardingStatusTransitionValidation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	// Create employee with status "inactive" — cannot go directly to "leaving".
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP009", "inactive")
	t.Cleanup(func() { truncateAll(h) })

	_, _, err := svc.InitiateOffboarding(ctx, onboarding.InitiateOffboardingInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		RetentionLabel: "7years",
	})
	assert.ErrorIs(t, err, onboarding.ErrInvalidTransition,
		"inactive → leaving must be rejected by the transition allow-list")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation test
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA01", "active")

	tenantB := seedTenant(t, h.AdminDB)
	t.Cleanup(func() { truncateAll(h) })

	// Create a template in tenant A.
	items := json.RawMessage(`[{"title":"Task","category":"","due_offset_days":0}]`)
	tmpl, err := svc.CreateTemplate(ctx, onboarding.CreateTemplateInput{
		TenantID: tenantA, ActorID: actorA, Name: "T", Kind: "onboarding",
		ItemsJSON: []byte(items),
	})
	require.NoError(t, err)

	// Attempt to use tenant A's template from tenant B's context — must fail.
	_, err = svc.GenerateTasks(ctx, onboarding.GenerateTasksInput{
		TenantID:   tenantB, // tenant B context
		ActorID:    actorA,
		EmployeeID: empA,    // employee belongs to tenant A
		TemplateID: tmpl.ID, // template belongs to tenant A
		BaseDate:   time.Now(),
	})
	assert.Error(t, err, "cross-tenant task generation must fail (RLS + explicit tenant_id checks)")

	// Verify that listing tasks for empA from tenantB context returns nothing.
	tasks, err := svc.ListTasks(ctx, tenantB, empA, "")
	require.NoError(t, err)
	assert.Empty(t, tasks, "tenantB must not see tenantA tasks")
}

// ---------------------------------------------------------------------------
// Audit log PII check
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := onboarding.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP010", "active")
	t.Cleanup(func() { truncateAll(h) })

	syntheticBankAccount := "合成銀行 004-5556666"

	_, err := svc.SubmitIntakeForm(ctx, onboarding.SubmitIntakeFormInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		EmployeeID:           empID,
		EmergencyContactJSON: []byte(`{"name":"合成次郎"}`),
		CommuteJSON:          []byte(`{}`),
		DependentsJSON:       []byte(`[]`),
		BankAccountPlaintext: []byte(syntheticBankAccount),
	})
	require.NoError(t, err)

	// Verify no audit_logs row contains the synthetic bank account number
	// or any fragment that looks like plaintext PII.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%合成銀行%", "%合成銀行%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain bank account number or other PII")
}
