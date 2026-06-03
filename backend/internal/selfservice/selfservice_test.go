package selfservice_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
	"github.com/your-org/hr-saas/internal/selfservice"
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

func seedEmployee(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code, lastName, firstName string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status)
		 VALUES (?, ?, ?, ?, ?, 'full_time', 'active')`,
		id, tenantID, code, lastName, firstName,
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
		"document_versions",
		"documents",
		"csv_import_rows",
		"csv_import_jobs",
		"self_service_change_requests",
		"approval_steps",
		"approval_requests",
		"approval_routes",
		"employees",
		"departments",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

func syntheticKey() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ===========================================================================
// Self-service change requests
// ===========================================================================

// TestChangeRequestReflectsOnlyOnApprove verifies that a submitted change is NOT
// written to the master until approval, and that reject leaves it unreflected.
func TestChangeRequestReflectsOnlyOnApprove(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "山田", "太郎")
	t.Cleanup(func() { truncateAll(h) })

	req, err := svc.SubmitChangeRequest(ctx, selfservice.SubmitChangeRequestInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		EmployeeID:  empID,
		TargetType:  selfservice.TargetEmployeeProfile,
		ChangesJSON: []byte(`{"last_name":"佐藤","first_name":"花子"}`),
	})
	require.NoError(t, err)
	assert.Equal(t, selfservice.ChangeStatusPending, req.Status)

	// Master must NOT be updated yet.
	var lastName string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT last_name FROM employees WHERE id = ?`, empID,
	).Scan(&lastName).Error)
	assert.Equal(t, "山田", lastName, "master must not change before approval")

	// Approve → master reflected within the approval transaction.
	approved, err := svc.ApproveChangeRequest(ctx, selfservice.ApproveChangeRequestInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		RequestID: req.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, selfservice.ChangeStatusApproved, approved.Status)
	require.NotNil(t, approved.ReflectedAt)

	require.NoError(t, h.AdminDB.Raw(
		`SELECT last_name FROM employees WHERE id = ?`, empID,
	).Scan(&lastName).Error)
	assert.Equal(t, "佐藤", lastName, "master must reflect approved change")
}

func TestChangeRequestRejectDoesNotReflect(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP002", "鈴木", "一郎")
	t.Cleanup(func() { truncateAll(h) })

	req, err := svc.SubmitChangeRequest(ctx, selfservice.SubmitChangeRequestInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		EmployeeID:  empID,
		TargetType:  selfservice.TargetEmployeeProfile,
		ChangesJSON: []byte(`{"last_name":"改ざん"}`),
	})
	require.NoError(t, err)

	rejected, err := svc.RejectChangeRequest(ctx, selfservice.RejectChangeRequestInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		RequestID: req.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, selfservice.ChangeStatusRejected, rejected.Status)

	var lastName string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT last_name FROM employees WHERE id = ?`, empID,
	).Scan(&lastName).Error)
	assert.Equal(t, "鈴木", lastName, "rejected change must never reflect to master")

	// Approving a rejected request is an invalid transition.
	_, err = svc.ApproveChangeRequest(ctx, selfservice.ApproveChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrInvalidTransition)
}

func TestChangeRequestSensitiveEncryptionAndRBAC(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP003", "高橋", "次郎")
	t.Cleanup(func() { truncateAll(h) })

	secret := []byte("合成口座 1234567")
	req, err := svc.SubmitChangeRequest(ctx, selfservice.SubmitChangeRequestInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		EmployeeID:         empID,
		TargetType:         selfservice.TargetBankAccount,
		ChangesJSON:        []byte(`{"masked":"****567"}`),
		SensitivePlaintext: secret,
	})
	require.NoError(t, err)

	// The persisted enc column must NOT contain the plaintext.
	// Scan into a struct field typed as bytea to avoid GORM's scalar []byte
	// scan misinterpreting the bytea driver value.
	var encRow struct {
		Enc []byte `gorm:"column:changes_sensitive_enc;type:bytea"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT changes_sensitive_enc FROM self_service_change_requests WHERE id = ?`, req.ID,
	).Scan(&encRow).Error)
	require.NotEmpty(t, encRow.Enc)
	assert.False(t, bytes.Contains(encRow.Enc, secret), "ciphertext must not contain plaintext")

	// Without read_sensitive permission: no plaintext returned.
	_, plain, err := svc.GetChangeRequest(ctx, selfservice.GetChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID, ReadSensitive: false,
	})
	require.NoError(t, err)
	assert.Nil(t, plain, "no sensitive plaintext without permission")

	// ReadSensitive=true but actor lacks permission → ErrForbidden and no leak.
	_, plain, err = svc.GetChangeRequest(ctx, selfservice.GetChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID, ReadSensitive: true,
	})
	assert.ErrorIs(t, err, selfservice.ErrForbidden)
	assert.Nil(t, plain)

	// Grant permission → decrypted plaintext returned.
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive-reader", `{"perms":["selfservice:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)

	_, plain, err = svc.GetChangeRequest(ctx, selfservice.GetChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID, ReadSensitive: true,
	})
	require.NoError(t, err)
	assert.Equal(t, secret, plain, "decrypted plaintext matches with permission")
}

// ===========================================================================
// CSV import
// ===========================================================================

func TestCSVDryRunDoesNotChangeMasterAndReportsErrors(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Row 1 valid; row 2 missing employee_code; row 3 duplicate code of row 1.
	csv := "employee_code,last_name,first_name\n" +
		"E100,山田,太郎\n" +
		",佐藤,花子\n" +
		"E100,鈴木,一郎\n"

	res, err := svc.ValidateCSV(ctx, selfservice.ValidateCSVInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		ImportType: selfservice.ImportTypeEmployees,
		Encoding:   selfservice.EncodingUTF8,
		CSVData:    []byte(csv),
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.TotalRows)
	assert.Equal(t, 1, res.ValidRows)
	assert.Equal(t, 2, res.ErrorRows)
	assert.Equal(t, selfservice.JobStatusValidated, res.Job.Status)
	assert.Equal(t, selfservice.ModeDryRun, res.Job.Mode)
	require.NotEmpty(t, res.RowErrors)

	// Row-numbered errors should reference rows 2 and 3.
	rowsWithErrors := map[int]bool{}
	for _, e := range res.RowErrors {
		rowsWithErrors[e.RowNumber] = true
	}
	assert.True(t, rowsWithErrors[2], "missing-code error on row 2")
	assert.True(t, rowsWithErrors[3], "duplicate-code error on row 3")

	// Dry-run must NOT have created any employees.
	var empCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE tenant_id = ?`, tenantID,
	).Scan(&empCount).Error)
	assert.Equal(t, int64(0), empCount, "dry-run must not change the master")
}

func TestCSVApplyAllOrNothingRejectsOnError(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	csv := "employee_code,last_name,first_name\n" +
		"E200,山田,太郎\n" +
		",佐藤,花子\n" // invalid

	res, err := svc.ApplyCSV(ctx, selfservice.ApplyCSVInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ImportType:  selfservice.ImportTypeEmployees,
		Encoding:    selfservice.EncodingUTF8,
		ApplyPolicy: selfservice.PolicyAllOrNothing,
		CSVData:     []byte(csv),
	})
	require.NoError(t, err)
	assert.Equal(t, selfservice.JobStatusFailed, res.Job.Status)
	assert.Equal(t, 0, res.AppliedRows)

	var empCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE tenant_id = ?`, tenantID,
	).Scan(&empCount).Error)
	assert.Equal(t, int64(0), empCount, "all_or_nothing must apply no rows when any row is invalid")
}

func TestCSVApplySkipErrorsAppliesValidRows(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	csv := "employee_code,last_name,first_name\n" +
		"E300,山田,太郎\n" +
		",佐藤,花子\n" + // invalid → skipped
		"E301,鈴木,一郎\n"

	res, err := svc.ApplyCSV(ctx, selfservice.ApplyCSVInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ImportType:  selfservice.ImportTypeEmployees,
		Encoding:    selfservice.EncodingUTF8,
		ApplyPolicy: selfservice.PolicySkipErrors,
		CSVData:     []byte(csv),
	})
	require.NoError(t, err)
	assert.Equal(t, selfservice.JobStatusCompleted, res.Job.Status)
	assert.Equal(t, 2, res.AppliedRows)
	assert.Equal(t, 1, res.ErrorRows)

	var empCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE tenant_id = ?`, tenantID,
	).Scan(&empCount).Error)
	assert.Equal(t, int64(2), empCount, "skip_errors applies only valid rows")
}

func TestCSVApplyUpsertUpdatesExisting(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	// Pre-existing employee with code E400.
	seedEmployee(t, h.AdminDB, tenantID, "E400", "旧姓", "旧名")
	t.Cleanup(func() { truncateAll(h) })

	csv := "employee_code,last_name,first_name\nE400,新姓,新名\n"
	res, err := svc.ApplyCSV(ctx, selfservice.ApplyCSVInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ImportType:  selfservice.ImportTypeEmployees,
		Encoding:    selfservice.EncodingUTF8,
		ApplyPolicy: selfservice.PolicyAllOrNothing,
		CSVData:     []byte(csv),
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.AppliedRows)

	// Still exactly one employee with code E400, updated in place (upsert).
	var count int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM employees WHERE tenant_id = ? AND employee_code = 'E400'`, tenantID,
	).Scan(&count).Error)
	assert.Equal(t, int64(1), count)

	var lastName string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT last_name FROM employees WHERE tenant_id = ? AND employee_code = 'E400'`, tenantID,
	).Scan(&lastName).Error)
	assert.Equal(t, "新姓", lastName, "upsert updates the existing row")
}

func TestCSVShiftJISDecodingAndHeaderMapping(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// UTF-8 source, encoded to Shift_JIS bytes. Header uses mixed case to verify
	// header mapping is case-insensitive.
	utf8CSV := "Employee_Code,Last_Name,First_Name\nE500,山田,太郎\n"
	sjisBytes, _, err := transform.Bytes(japanese.ShiftJIS.NewEncoder(), []byte(utf8CSV))
	require.NoError(t, err)

	res, err := svc.ApplyCSV(ctx, selfservice.ApplyCSVInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ImportType:  selfservice.ImportTypeEmployees,
		Encoding:    selfservice.EncodingShiftJIS,
		ApplyPolicy: selfservice.PolicyAllOrNothing,
		CSVData:     sjisBytes,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.AppliedRows)

	var lastName string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT last_name FROM employees WHERE tenant_id = ? AND employee_code = 'E500'`, tenantID,
	).Scan(&lastName).Error)
	assert.Equal(t, "山田", lastName, "shift_jis decoding + header mapping works")
}

// ===========================================================================
// Document store
// ===========================================================================

func TestDocumentCreateVersioningAndHash(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPDOC", "文書", "管理")
	t.Cleanup(func() { truncateAll(h) })

	content1 := []byte("contract v1 合成内容")
	doc, ver1, err := svc.CreateDocument(ctx, selfservice.CreateDocumentInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		OwnerEmployeeID: &empID,
		Category:        selfservice.CategoryContract,
		Title:           "雇用契約書",
		MimeType:        "application/pdf",
		Filename:        "contract.pdf",
		Content:         content1,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, ver1.VersionNo)
	require.NotNil(t, doc.CurrentVersionID)
	assert.Equal(t, ver1.ID, *doc.CurrentVersionID)

	// Add a new version → current switches, old version retained.
	content2 := []byte("contract v2 合成内容")
	ver2, err := svc.AddVersion(ctx, selfservice.AddVersionInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		DocumentID: doc.ID,
		MimeType:   "application/pdf",
		Filename:   "contract.pdf",
		Content:    content2,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, ver2.VersionNo)

	got, err := svc.GetDocument(ctx, tenantID, doc.ID)
	require.NoError(t, err)
	require.NotNil(t, got.CurrentVersionID)
	assert.Equal(t, ver2.ID, *got.CurrentVersionID, "current version switched to v2")

	versions, err := svc.ListVersions(ctx, tenantID, doc.ID)
	require.NoError(t, err)
	assert.Len(t, versions, 2, "old version retained as history")

	// Download v1 and verify content hash (tamper detection / CMP-006).
	dl, err := svc.DownloadVersion(ctx, selfservice.DownloadVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: ver1.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, content1, dl.Content)
	assert.True(t, dl.HashVerified, "content hash must match (no tampering)")

	// Tamper with the persisted hash → download reports hash mismatch.
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE document_versions SET content_hash = 'deadbeef' WHERE id = ?`, ver1.ID,
	).Error)
	dl, err = svc.DownloadVersion(ctx, selfservice.DownloadVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: ver1.ID,
	})
	require.NoError(t, err)
	assert.False(t, dl.HashVerified, "tampered hash must be detected")
}

func TestDocumentUploadValidation(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Disallowed MIME type.
	_, _, err := svc.CreateDocument(ctx, selfservice.CreateDocumentInput{
		TenantID: tenantID, ActorID: actorID,
		Category: selfservice.CategoryMisc, Title: "bad",
		MimeType: "application/x-msdownload", Filename: "evil.exe",
		Content: []byte("x"),
	})
	assert.ErrorIs(t, err, selfservice.ErrValidation, "disallowed MIME rejected")

	// Disallowed extension.
	_, _, err = svc.CreateDocument(ctx, selfservice.CreateDocumentInput{
		TenantID: tenantID, ActorID: actorID,
		Category: selfservice.CategoryMisc, Title: "bad",
		MimeType: "application/pdf", Filename: "evil.exe",
		Content: []byte("x"),
	})
	assert.ErrorIs(t, err, selfservice.ErrValidation, "disallowed extension rejected")
}

func TestDocumentRetentionLogicalExpiryAndLegalHold(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Legal-hold document cannot be expired.
	legalDoc, _, err := svc.CreateDocument(ctx, selfservice.CreateDocumentInput{
		TenantID: tenantID, ActorID: actorID,
		Category: selfservice.CategoryPayslip, Title: "給与明細",
		MimeType: "application/pdf", Filename: "payslip.pdf",
		Content:   []byte("payslip"),
		LegalHold: true,
	})
	require.NoError(t, err)

	_, err = svc.ExpireDocument(ctx, selfservice.ExpireDocumentInput{
		TenantID: tenantID, ActorID: actorID, DocumentID: legalDoc.ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrLegalHold, "legal-hold document must not be expirable")

	// Verify it was NOT physically deleted and is not expired.
	got, err := svc.GetDocument(ctx, tenantID, legalDoc.ID)
	require.NoError(t, err)
	assert.False(t, got.LogicallyExpired)

	// Non-legal-hold document can be logically expired (not physically deleted).
	doc, _, err := svc.CreateDocument(ctx, selfservice.CreateDocumentInput{
		TenantID: tenantID, ActorID: actorID,
		Category: selfservice.CategoryMisc, Title: "雑書類",
		MimeType: "text/plain", Filename: "note.txt",
		Content: []byte("note"),
	})
	require.NoError(t, err)

	expired, err := svc.ExpireDocument(ctx, selfservice.ExpireDocumentInput{
		TenantID: tenantID, ActorID: actorID, DocumentID: doc.ID,
	})
	require.NoError(t, err)
	assert.True(t, expired.LogicallyExpired)

	// Row still exists (no physical delete).
	var cnt int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM documents WHERE id = ?`, doc.ID,
	).Scan(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "logical expiry must not physically delete the row")

	// Download of an expired document is forbidden.
	versions, err := svc.ListVersions(ctx, tenantID, doc.ID)
	require.NoError(t, err)
	require.Len(t, versions, 1)
	_, err = svc.DownloadVersion(ctx, selfservice.DownloadVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: versions[0].ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrForbidden, "expired document download forbidden")
}

// ===========================================================================
// RLS cross-tenant isolation
// ===========================================================================

func TestCrossTenantIsolation(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "a@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "TA01", "山田", "太郎")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "b@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Tenant A: change request + document.
	reqA, err := svc.SubmitChangeRequest(ctx, selfservice.SubmitChangeRequestInput{
		TenantID: tenantA, ActorID: actorA, EmployeeID: empA,
		TargetType: selfservice.TargetEmployeeProfile, ChangesJSON: []byte(`{"last_name":"佐藤"}`),
	})
	require.NoError(t, err)

	docA, verA, err := svc.CreateDocument(ctx, selfservice.CreateDocumentInput{
		TenantID: tenantA, ActorID: actorA,
		Category: selfservice.CategoryMisc, Title: "A社書類",
		MimeType: "text/plain", Filename: "a.txt", Content: []byte("A"),
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's change request.
	_, _, err = svc.GetChangeRequest(ctx, selfservice.GetChangeRequestInput{
		TenantID: tenantB, ActorID: actorB, RequestID: reqA.ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrNotFound, "tenantB must not read tenantA change request")

	// Tenant B cannot approve tenant A's change request.
	_, err = svc.ApproveChangeRequest(ctx, selfservice.ApproveChangeRequestInput{
		TenantID: tenantB, ActorID: actorB, RequestID: reqA.ID,
	})
	assert.Error(t, err, "tenantB must not approve tenantA change request")

	// Tenant B cannot read tenant A's document.
	_, err = svc.GetDocument(ctx, tenantB, docA.ID)
	assert.ErrorIs(t, err, selfservice.ErrNotFound, "tenantB must not read tenantA document")

	// Tenant B cannot download tenant A's version.
	_, err = svc.DownloadVersion(ctx, selfservice.DownloadVersionInput{
		TenantID: tenantB, ActorID: actorB, VersionID: verA.ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrNotFound, "tenantB must not download tenantA version")

	// Tenant B list views are empty.
	reqs, err := svc.ListChangeRequests(ctx, tenantB, nil, "")
	require.NoError(t, err)
	assert.Empty(t, reqs, "tenantB sees no tenantA change requests")

	docs, err := svc.ListDocuments(ctx, tenantB, "", true)
	require.NoError(t, err)
	assert.Empty(t, docs, "tenantB sees no tenantA documents")
}

// ===========================================================================
// Audit log PII non-inclusion
// ===========================================================================

func TestAuditLogContainsNoPII(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPPII", "山田", "太郎")
	t.Cleanup(func() { truncateAll(h) })

	secret := "合成口座 9998887"
	_, err := svc.SubmitChangeRequest(ctx, selfservice.SubmitChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		TargetType:         selfservice.TargetBankAccount,
		ChangesJSON:        []byte(`{"masked":"****887"}`),
		SensitivePlaintext: []byte(secret),
	})
	require.NoError(t, err)

	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%合成口座%", "%合成口座%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount, "audit_logs must not contain sensitive PII")
}

// ===========================================================================
// State transition boundary
// ===========================================================================

func TestChangeRequestInvalidTransition(t *testing.T) {
	setupCrypto(t)
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := selfservice.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMPTR", "山田", "太郎")
	t.Cleanup(func() { truncateAll(h) })

	req, err := svc.SubmitChangeRequest(ctx, selfservice.SubmitChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
		TargetType: selfservice.TargetCommute, ChangesJSON: []byte(`{"route":"x"}`),
	})
	require.NoError(t, err)

	// Approve once (pending → approved).
	_, err = svc.ApproveChangeRequest(ctx, selfservice.ApproveChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID,
	})
	require.NoError(t, err)

	// Approving again (approved → approved) is invalid.
	_, err = svc.ApproveChangeRequest(ctx, selfservice.ApproveChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrInvalidTransition)

	// Rejecting an approved request is invalid.
	_, err = svc.RejectChangeRequest(ctx, selfservice.RejectChangeRequestInput{
		TenantID: tenantID, ActorID: actorID, RequestID: req.ID,
	})
	assert.ErrorIs(t, err, selfservice.ErrInvalidTransition)
}
