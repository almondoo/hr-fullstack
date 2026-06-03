package mynumber_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/mynumber"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go; synthetic data only)
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
// (e.g. `{"perms":["mynumber:reveal"]}`) and returns its ID.
func seedRoleWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, name, permsJSON string) uuid.UUID {
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

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"mynumber_disposals",
		"mynumber_access_logs",
		"mynumber_purposes",
		"mynumber_records",
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

// setupCrypto injects a synthetic key for the global cipher so tests run without
// FIELD_ENCRYPTION_KEY set in the environment.
func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

// Convenience permission bundles for tests.
const (
	// revealRoleJSON grants the full mynumber permission set (read+write+reveal).
	revealRoleJSON = `{"perms":["mynumber:read","mynumber:write","mynumber:reveal"]}`
	// noRevealJSON grants read+write but NOT the dedicated reveal permission.
	noRevealJSON = `{"perms":["mynumber:read","mynumber:write"]}`
	// readOnlyJSON grants read only (no write, no reveal).
	readOnlyJSON = `{"perms":["mynumber:read"]}`
	// writeOnlyJSON grants write only (no read, no reveal).
	writeOnlyJSON = `{"perms":["mynumber:write"]}`
	// unrelatedRoleJSON grants an unrelated permission — no mynumber access at all.
	unrelatedRoleJSON = `{"perms":["employee:read"]}`
)

// syntheticMyNumber is a synthetic (fake) 12-digit number — NOT a real マイナンバー.
const syntheticMyNumber = "123456789012"

// ---------------------------------------------------------------------------
// Collect — 収集・暗号化保管
// ---------------------------------------------------------------------------

func TestCollectStoresEncryptedAndPurposes(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_writer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		SubjectType:     mynumber.SubjectSelf,
		NumberPlaintext: []byte(syntheticMyNumber),
		Purposes:        []string{mynumber.PurposePayroll, mynumber.PurposeTax},
	})
	require.NoError(t, err)
	assert.Equal(t, mynumber.StatusActive, rec.Status)
	assert.Nil(t, rec.NumberEnc, "ciphertext must not be returned to caller")

	// The number_enc column must hold ciphertext, never the plaintext.
	var encRow struct {
		Enc []byte `gorm:"column:number_enc"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT number_enc FROM mynumber_records WHERE id = ?`, rec.ID,
	).Scan(&encRow).Error)
	assert.NotEmpty(t, encRow.Enc, "number_enc must be populated with ciphertext")
	assert.NotContains(t, string(encRow.Enc), syntheticMyNumber,
		"plaintext マイナンバー must never appear in number_enc")

	// Purposes recorded.
	purposes, err := svc.ListPurposes(ctx, tenantID, rec.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{mynumber.PurposePayroll, mynumber.PurposeTax}, purposes)
}

func TestCollectRejectsUnknownPurpose(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP002", "active")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		SubjectType:     mynumber.SubjectSelf,
		NumberPlaintext: []byte(syntheticMyNumber),
		Purposes:        []string{"marketing"}, // not in enumerated list
	})
	assert.ErrorIs(t, err, mynumber.ErrInvalidPurpose)
}

// ---------------------------------------------------------------------------
// Reveal — 復号: 専用権限 + 利用目的の二重検証
// ---------------------------------------------------------------------------

func TestRevealWithPermissionAndPurpose(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP003", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposeSocialInsurance},
	})
	require.NoError(t, err)

	plain, err := svc.Reveal(ctx, mynumber.RevealInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Purpose: mynumber.PurposeSocialInsurance,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte(syntheticMyNumber), plain, "decrypted number must match original")

	// A decrypt access-log entry must have been recorded (log-and-reveal atomicity).
	logs, err := svc.ListAccessLogs(ctx, tenantID, actorID, rec.ID)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, mynumber.ActionDecrypt, logs[0].Action)
	assert.Equal(t, mynumber.PurposeSocialInsurance, logs[0].Purpose)
}

func TestRevealWithoutPermissionDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP004", "active")
	// Role WITHOUT mynumber:reveal.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_no_reveal", noRevealJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	plain, err := svc.Reveal(ctx, mynumber.RevealInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Purpose: mynumber.PurposePayroll,
	})
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"reveal must be denied without the dedicated mynumber:reveal permission")
	assert.Nil(t, plain, "plaintext must not be returned when permission is denied")

	// No decrypt log should have been written (rolled back with the denial).
	logs, err := svc.ListAccessLogs(ctx, tenantID, actorID, rec.ID)
	require.NoError(t, err)
	assert.Empty(t, logs, "no access log should be written when reveal is denied")
}

func TestRevealWithUnregisteredPurposeDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP005", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	// Registered ONLY for payroll.
	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	// Request reveal for tax — a valid enumerated purpose but NOT registered.
	_, err = svc.Reveal(ctx, mynumber.RevealInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Purpose: mynumber.PurposeTax,
	})
	assert.ErrorIs(t, err, mynumber.ErrPurposeNotAllowed,
		"目的外利用 (unregistered purpose) must be rejected")
}

// ---------------------------------------------------------------------------
// Provide — 第三者提供 (社保手続き等)
// ---------------------------------------------------------------------------

func TestProvideRecordsProvidedTo(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP006", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposeSocialInsurance},
	})
	require.NoError(t, err)

	plain, err := svc.Provide(ctx, mynumber.ProvideInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Purpose: mynumber.PurposeSocialInsurance, ProvidedTo: "social_insurance_office_procedure",
	})
	require.NoError(t, err)
	assert.Equal(t, []byte(syntheticMyNumber), plain)

	logs, err := svc.ListAccessLogs(ctx, tenantID, actorID, rec.ID)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	assert.Equal(t, mynumber.ActionProvide, logs[0].Action)
	require.NotNil(t, logs[0].ProvidedTo)
	assert.Equal(t, "social_insurance_office_procedure", *logs[0].ProvidedTo)
}

// ---------------------------------------------------------------------------
// Disposal — 廃棄: 論理失効 + 復号不能化 + 廃棄後の全拒否
// ---------------------------------------------------------------------------

func TestDisposeDestroysCiphertextAndRejectsReveal(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP007", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	disposal, err := svc.Dispose(ctx, mynumber.DisposeInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Reason: mynumber.ReasonResignation, Method: mynumber.MethodCiphertextDeleted,
	})
	require.NoError(t, err)
	assert.Equal(t, mynumber.ReasonResignation, disposal.Reason)

	// Ciphertext must be destroyed (NULL) and status disposed.
	var row struct {
		Status string `gorm:"column:status"`
		Enc    []byte `gorm:"column:number_enc"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT status, number_enc FROM mynumber_records WHERE id = ?`, rec.ID,
	).Scan(&row).Error)
	assert.Equal(t, mynumber.StatusDisposed, row.Status)
	assert.Empty(t, row.Enc, "ciphertext must be destroyed on disposal (復号不能化)")

	// After disposal, reveal / provide must be rejected.
	_, err = svc.Reveal(ctx, mynumber.RevealInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID, Purpose: mynumber.PurposePayroll,
	})
	assert.ErrorIs(t, err, mynumber.ErrDisposed, "reveal after disposal must be rejected")

	_, err = svc.Provide(ctx, mynumber.ProvideInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Purpose: mynumber.PurposePayroll, ProvidedTo: "x",
	})
	assert.ErrorIs(t, err, mynumber.ErrDisposed, "provide after disposal must be rejected")
}

func TestDisposeWithoutPermissionDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP008", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_no_reveal", noRevealJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	_, err = svc.Dispose(ctx, mynumber.DisposeInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Reason: mynumber.ReasonManual, Method: mynumber.MethodCiphertextDeleted,
	})
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"dispose must require the dedicated mynumber:reveal permission")
}

// ---------------------------------------------------------------------------
// Status transition boundary — 不正遷移
// ---------------------------------------------------------------------------

func TestDisposeAlreadyDisposedRejected(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP009", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	_, err = svc.Dispose(ctx, mynumber.DisposeInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Reason: mynumber.ReasonManual, Method: mynumber.MethodKeyDestroyed,
	})
	require.NoError(t, err)

	// Second disposal must be rejected (terminal state).
	_, err = svc.Dispose(ctx, mynumber.DisposeInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Reason: mynumber.ReasonManual, Method: mynumber.MethodKeyDestroyed,
	})
	assert.ErrorIs(t, err, mynumber.ErrDisposed)
}

func TestExpireThenDisposeAllowed(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP010", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	require.NoError(t, svc.Expire(ctx, mynumber.ExpireInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
	}))

	// expired → disposed is allowed.
	_, err = svc.Dispose(ctx, mynumber.DisposeInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Reason: mynumber.ReasonRetentionExpired, Method: mynumber.MethodCiphertextDeleted,
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Access-log hash chain — 改ざん検知
// ---------------------------------------------------------------------------

func TestAccessLogChainVerifiesAndDetectsTampering(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP011", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll, mynumber.PurposeTax},
	})
	require.NoError(t, err)

	// Produce several access-log entries.
	for i := 0; i < 3; i++ {
		_, err := svc.Reveal(ctx, mynumber.RevealInput{
			TenantID: tenantID, ActorID: actorID, RecordID: rec.ID, Purpose: mynumber.PurposePayroll,
		})
		require.NoError(t, err)
	}

	intact, err := svc.VerifyAccessLogChain(ctx, tenantID)
	require.NoError(t, err)
	assert.True(t, intact, "intact chain must verify")

	// Tamper with a row's purpose (admin bypasses RLS) and re-verify.
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE mynumber_access_logs SET purpose = 'tax'
		 WHERE tenant_id = ? AND seq = (SELECT MIN(seq) FROM mynumber_access_logs WHERE tenant_id = ?)`,
		tenantID, tenantID,
	).Error)

	intact, err = svc.VerifyAccessLogChain(ctx, tenantID)
	require.NoError(t, err)
	assert.False(t, intact, "tampered chain must be detected")
}

// ---------------------------------------------------------------------------
// Dependent (扶養家族) — 独立した保管・廃棄
// ---------------------------------------------------------------------------

func TestDependentRecordIndependentLifecycle(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP012", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	dependentRef := uuid.New()

	selfRec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposeTax},
	})
	require.NoError(t, err)

	depRec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectDependent, DependentRef: &dependentRef,
		NumberPlaintext: []byte("210987654321"),
		Purposes:        []string{mynumber.PurposeTax},
	})
	require.NoError(t, err)
	require.NotNil(t, depRec.DependentRef)
	assert.Equal(t, dependentRef, *depRec.DependentRef)

	// Both records returned for the employee.
	recs, err := svc.ListRecords(ctx, tenantID, actorID, empID)
	require.NoError(t, err)
	assert.Len(t, recs, 2)

	// Dispose ONLY the dependent's record — the self record must remain active.
	_, err = svc.Dispose(ctx, mynumber.DisposeInput{
		TenantID: tenantID, ActorID: actorID, RecordID: depRec.ID,
		Reason: mynumber.ReasonManual, Method: mynumber.MethodCiphertextDeleted,
	})
	require.NoError(t, err)

	selfAfter, err := svc.GetRecord(ctx, tenantID, actorID, selfRec.ID)
	require.NoError(t, err)
	assert.Equal(t, mynumber.StatusActive, selfAfter.Status,
		"disposing the dependent record must not affect the self record")

	depAfter, err := svc.GetRecord(ctx, tenantID, actorID, depRec.ID)
	require.NoError(t, err)
	assert.Equal(t, mynumber.StatusDisposed, depAfter.Status)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation — 別テナントの存在/参照/復号不可
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA01", "active")
	roleA := seedRoleWithPermissions(t, h.AdminDB, tenantA, "mn_revealer_a", revealRoleJSON)
	assignRole(t, h.AdminDB, actorA, roleA)

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	roleB := seedRoleWithPermissions(t, h.AdminDB, tenantB, "mn_revealer_b", revealRoleJSON)
	assignRole(t, h.AdminDB, actorB, roleB)
	t.Cleanup(func() { truncateAll(h) })

	recA, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantA, ActorID: actorA, EmployeeID: empA,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)

	// Tenant B context must not see / fetch tenant A's record.
	_, err = svc.GetRecord(ctx, tenantB, actorB, recA.ID)
	assert.ErrorIs(t, err, mynumber.ErrNotFound, "tenantB must not read tenantA record")

	// Tenant B context must not reveal tenant A's number.
	_, err = svc.Reveal(ctx, mynumber.RevealInput{
		TenantID: tenantB, ActorID: actorB, RecordID: recA.ID, Purpose: mynumber.PurposePayroll,
	})
	assert.Error(t, err, "tenantB must not reveal tenantA number")
	assert.NotErrorIs(t, err, nil)

	// List for empA from tenantB returns empty.
	recs, err := svc.ListRecords(ctx, tenantB, actorB, empA)
	require.NoError(t, err)
	assert.Empty(t, recs, "tenantB must not see tenantA records")
}

// ---------------------------------------------------------------------------
// Negative: plaintext/decrypted value never lands in non-encrypted columns,
// access logs, or audit logs.
// ---------------------------------------------------------------------------

func TestNoPlaintextInLogsOrNonEncryptedColumns(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP013", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_revealer", revealRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposeSocialInsurance},
	})
	require.NoError(t, err)

	// Exercise decrypt + provide so any leak path would have fired.
	_, err = svc.Reveal(ctx, mynumber.RevealInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID, Purpose: mynumber.PurposeSocialInsurance,
	})
	require.NoError(t, err)
	_, err = svc.Provide(ctx, mynumber.ProvideInput{
		TenantID: tenantID, ActorID: actorID, RecordID: rec.ID,
		Purpose: mynumber.PurposeSocialInsurance, ProvidedTo: syntheticMyNumber[:4] + "_office",
	})
	require.NoError(t, err)

	like := "%" + syntheticMyNumber + "%"

	// audit_logs: resource_id / action must not contain the number.
	var auditHits int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ? OR resource_type LIKE ?`,
		like, like, like,
	).Scan(&auditHits).Error)
	assert.Equal(t, int64(0), auditHits, "audit_logs must not contain the マイナンバー")

	// mynumber_access_logs: no textual column may contain the number.
	var logHits int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM mynumber_access_logs
		 WHERE action LIKE ? OR purpose LIKE ? OR COALESCE(provided_to,'') LIKE ?`,
		like, like, like,
	).Scan(&logHits).Error)
	assert.Equal(t, int64(0), logHits, "access logs must not contain the マイナンバー")

	// mynumber_records: the only place the number may exist is number_enc (bytea
	// ciphertext).  Casting it to text must NOT reveal the plaintext.
	var recHits int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM mynumber_records
		 WHERE subject_type LIKE ? OR status LIKE ?
		    OR COALESCE(encode(number_enc,'escape'),'') LIKE ?`,
		like, like, like,
	).Scan(&recHits).Error)
	assert.Equal(t, int64(0), recHits,
		"plaintext マイナンバー must not appear in any column including the encrypted one")
}

// ---------------------------------------------------------------------------
// Service-layer RBAC for collect/read operations — authoritative permission
// gate that does NOT depend on the HTTP route middleware being wired.
//
// These guard against the unwired-router regression: even when invoked directly
// (bypassing the route-layer RequirePermission middleware), Collect requires
// mynumber:write and GetRecord / ListRecords / ListAccessLogs require
// mynumber:read.  A caller lacking the permission is denied with ErrForbidden.
// ---------------------------------------------------------------------------

// collectAs is a helper that collects a record using an actor seeded with the
// full reveal role, returning the new record ID.  Used to set up read-gate tests
// independently of the actor under test.
func collectAs(t *testing.T, svc *mynumber.Service, h *testdb.Harness, tenantID, empID uuid.UUID) uuid.UUID {
	t.Helper()
	writer := seedUser(t, h.AdminDB, tenantID, "writer-"+uuid.NewString()+"@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_full_"+uuid.NewString()[:8], revealRoleJSON)
	assignRole(t, h.AdminDB, writer, roleID)
	rec, err := svc.Collect(context.Background(), mynumber.CollectInput{
		TenantID: tenantID, ActorID: writer, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	require.NoError(t, err)
	return rec.ID
}

func TestCollectWithoutWritePermissionDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP020", "active")
	// Read-only role — must NOT be able to collect.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_read_only", readOnlyJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"collect must require mynumber:write at the service layer")

	// Nothing must have been inserted (the whole tx rolled back).
	var count int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM mynumber_records WHERE tenant_id = ?`, tenantID,
	).Scan(&count).Error)
	assert.Equal(t, int64(0), count, "no record may be written when collect is denied")
}

func TestCollectWithNoRoleDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com") // no role assigned
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP021", "active")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.Collect(ctx, mynumber.CollectInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		SubjectType: mynumber.SubjectSelf, NumberPlaintext: []byte(syntheticMyNumber),
		Purposes: []string{mynumber.PurposePayroll},
	})
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"collect must be denied when the actor has no role / no permissions")
}

func TestGetRecordWithoutReadPermissionDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP022", "active")
	recID := collectAs(t, svc, h, tenantID, empID)

	// Actor with only an unrelated permission — must NOT read mynumber metadata.
	actorID := seedUser(t, h.AdminDB, tenantID, "reader@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_unrelated", unrelatedRoleJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.GetRecord(ctx, tenantID, actorID, recID)
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"GetRecord must require mynumber:read at the service layer")
}

func TestListRecordsWithoutReadPermissionDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP023", "active")
	collectAs(t, svc, h, tenantID, empID)

	// write-only actor — has no read permission.
	actorID := seedUser(t, h.AdminDB, tenantID, "writer-only@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_write_only", writeOnlyJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.ListRecords(ctx, tenantID, actorID, empID)
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"ListRecords must require mynumber:read at the service layer")
}

func TestListAccessLogsWithoutReadPermissionDenied(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP024", "active")
	recID := collectAs(t, svc, h, tenantID, empID)

	// write-only actor — has no read permission.
	actorID := seedUser(t, h.AdminDB, tenantID, "writer-only2@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_write_only2", writeOnlyJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.ListAccessLogs(ctx, tenantID, actorID, recID)
	assert.ErrorIs(t, err, mynumber.ErrForbidden,
		"ListAccessLogs must require mynumber:read at the service layer")
}

// TestReadOperationsAllowedWithReadPermission confirms the read gate is a gate,
// not a block: a read-only actor CAN perform the metadata reads.
func TestReadOperationsAllowedWithReadPermission(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := mynumber.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP025", "active")
	recID := collectAs(t, svc, h, tenantID, empID)

	actorID := seedUser(t, h.AdminDB, tenantID, "reader2@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "mn_read_only2", readOnlyJSON)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	rec, err := svc.GetRecord(ctx, tenantID, actorID, recID)
	require.NoError(t, err)
	assert.Equal(t, recID, rec.ID)

	recs, err := svc.ListRecords(ctx, tenantID, actorID, empID)
	require.NoError(t, err)
	assert.Len(t, recs, 1)

	logs, err := svc.ListAccessLogs(ctx, tenantID, actorID, recID)
	require.NoError(t, err)
	assert.Empty(t, logs, "no access events yet")
}
