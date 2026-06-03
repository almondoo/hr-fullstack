package govfiling_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/govfiling"
	"github.com/your-org/hr-saas/internal/platform/crypto"
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

func seedRoleWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, name string, permsJSON string) uuid.UUID {
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
		"gov_filing_status_history",
		"gov_filing_documents",
		"gov_filings",
		"insurance_settings",
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// syntheticKey returns a synthetic 32-byte test key (not a real secret).
func syntheticKey() []byte {
	return bytes.Repeat([]byte{0x37}, 32)
}

func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

// gradeTableJSON returns a synthetic grade table covering the test amounts.
// These are NOT real statutory values; they exercise the config-driven path.
const gradeTableJSON = `{"grades":[
	{"grade":1,"lower":0,"upper":100000,"monthly":90000},
	{"grade":2,"lower":100000,"upper":200000,"monthly":150000},
	{"grade":3,"lower":200000,"upper":300000,"monthly":250000},
	{"grade":4,"lower":300000,"upper":0,"monthly":350000}
]}`

func seedSettings(t *testing.T, svc *govfiling.Service, tenantID, actorID uuid.UUID, gradeTable, thresholdJSON string) {
	t.Helper()
	_, err := svc.UpsertSettings(context.Background(), govfiling.UpsertSettingsInput{
		TenantID:               tenantID,
		ActorID:                actorID,
		InsurerKind:            govfiling.InsurerKyokai,
		RateTableJSON:          []byte(`{"health_insurance_rate":"0.0998"}`),
		GradeTableJSON:         []byte(gradeTable),
		JudgementThresholdJSON: []byte(thresholdJSON),
		FormVersionJSON:        []byte(`{"egov":{"health_insurance_acquire":"v2024.1"}}`),
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Settings tests (法令値の設定化)
// ---------------------------------------------------------------------------

func TestUpsertAndGetSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	s, err := svc.UpsertSettings(ctx, govfiling.UpsertSettingsInput{
		TenantID:               tenantID,
		ActorID:                actorID,
		InsurerKind:            govfiling.InsurerKumiai,
		RateTableJSON:          []byte(`{"pension_rate":"0.183"}`),
		GradeTableJSON:         []byte(gradeTableJSON),
		JudgementThresholdJSON: []byte(`{"monthly_change_grade_diff":2}`),
		FormVersionJSON:        []byte(`{}`),
	})
	require.NoError(t, err)
	assert.Equal(t, govfiling.InsurerKumiai, s.InsurerKind)

	got, err := svc.GetSettings(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, s.ID, got.ID)
	assert.JSONEq(t, `{"pension_rate":"0.183"}`, string(got.RateTableJSON))

	// Upsert again updates the same row (1 per tenant).
	s2, err := svc.UpsertSettings(ctx, govfiling.UpsertSettingsInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		InsurerKind: govfiling.InsurerKyokai,
	})
	require.NoError(t, err)
	assert.Equal(t, s.ID, s2.ID, "upsert must update the existing settings row, not create a new one")
	assert.Equal(t, govfiling.InsurerKyokai, s2.InsurerKind)
}

func TestGetSettingsNotFound(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.GetSettings(context.Background(), tenantID)
	assert.ErrorIs(t, err, govfiling.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Grade judgement tests (設定駆動・ハードコード非依存 LM-010)
// ---------------------------------------------------------------------------

func TestJudgeMonthlyChangeUsesConfiguredTable(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Threshold = 2 grades.
	seedSettings(t, svc, tenantID, actorID, gradeTableJSON, `{"monthly_change_grade_diff":2}`)

	// 90000 → grade 1, 250000 → grade 3, diff = 2 ⇒ required.
	res, err := svc.JudgeMonthlyChange(ctx, govfiling.JudgeMonthlyChangeInput{
		TenantID:       tenantID,
		CurrentMonthly: 90000,
		NewMonthly:     250000,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res.CurrentGrade)
	assert.Equal(t, 3, res.NewGrade)
	assert.Equal(t, 2, res.GradeDiff)
	assert.True(t, res.MonthlyChangeRequired, "grade diff 2 meets threshold 2 ⇒ change required")

	// 90000 → grade 1, 150000 → grade 2, diff = 1 ⇒ not required.
	res2, err := svc.JudgeMonthlyChange(ctx, govfiling.JudgeMonthlyChangeInput{
		TenantID:       tenantID,
		CurrentMonthly: 90000,
		NewMonthly:     150000,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, res2.GradeDiff)
	assert.False(t, res2.MonthlyChangeRequired, "grade diff 1 below threshold 2 ⇒ not required")
}

// TestJudgeMonthlyChangeFollowsSettingsChange verifies the judgement follows a
// settings change (改正追従): lowering the threshold to 1 flips a diff-1 case
// from "not required" to "required" without any code change.
func TestJudgeMonthlyChangeFollowsSettingsChange(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Initially threshold = 2: diff-1 ⇒ not required.
	seedSettings(t, svc, tenantID, actorID, gradeTableJSON, `{"monthly_change_grade_diff":2}`)
	res, err := svc.JudgeMonthlyChange(ctx, govfiling.JudgeMonthlyChangeInput{
		TenantID: tenantID, CurrentMonthly: 90000, NewMonthly: 150000,
	})
	require.NoError(t, err)
	require.False(t, res.MonthlyChangeRequired)

	// Revise threshold to 1 (改正): same inputs ⇒ now required.
	seedSettings(t, svc, tenantID, actorID, gradeTableJSON, `{"monthly_change_grade_diff":1}`)
	res2, err := svc.JudgeMonthlyChange(ctx, govfiling.JudgeMonthlyChangeInput{
		TenantID: tenantID, CurrentMonthly: 90000, NewMonthly: 150000,
	})
	require.NoError(t, err)
	assert.True(t, res2.MonthlyChangeRequired,
		"after lowering the threshold to 1 in settings, a diff-1 change must now require a filing")
}

func TestJudgeMonthlyChangeWithoutSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.JudgeMonthlyChange(context.Background(), govfiling.JudgeMonthlyChangeInput{
		TenantID: tenantID, CurrentMonthly: 90000, NewMonthly: 250000,
	})
	assert.ErrorIs(t, err, govfiling.ErrSettingsMissing)
}

// ---------------------------------------------------------------------------
// Filing lifecycle + status machine tests (LM-012/013)
// ---------------------------------------------------------------------------

func createDraft(t *testing.T, svc *govfiling.Service, tenantID, actorID, empID uuid.UUID, idemKey string) *govfiling.Filing {
	t.Helper()
	f, err := svc.CreateFiling(context.Background(), govfiling.CreateFilingInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeID:     empID,
		FilingType:     govfiling.FilingHealthInsuranceAcquire,
		Channel:        govfiling.ChannelEgov,
		PayloadJSON:    []byte(`{"employee_ref":"` + empID.String() + `"}`),
		IdempotencyKey: idemKey,
	})
	require.NoError(t, err)
	return f
}

func TestCreateAndSubmitFiling(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-01", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-001")
	assert.Equal(t, govfiling.StatusDraft, f.Status)
	assert.Nil(t, f.ExternalRef)

	// draft → submitted via mock submitter.
	submitted, err := svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, govfiling.StatusSubmitted, submitted.Status)
	require.NotNil(t, submitted.ExternalRef)
	assert.NotEmpty(t, *submitted.ExternalRef, "external_ref must be set after submission")
	require.NotNil(t, submitted.SubmittedAt)

	// History should now contain the draft→submitted transition.
	hist, err := svc.ListStatusHistory(ctx, tenantID, f.ID)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, govfiling.StatusDraft, hist[0].FromStatus)
	assert.Equal(t, govfiling.StatusSubmitted, hist[0].ToStatus)
}

func TestFilingStatusMachineReturnAndResubmit(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-02", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-002")

	// draft → submitted
	_, err := svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err)

	// submitted → returned (返戻) with external_message.
	returnReason := "添付書類不備"
	returned, err := svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID,
		ToStatus: govfiling.StatusReturned, ExternalMessage: &returnReason,
	})
	require.NoError(t, err)
	assert.Equal(t, govfiling.StatusReturned, returned.Status)

	// returned → submitted (再申請)
	resubmitted, err := svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err)
	assert.Equal(t, govfiling.StatusSubmitted, resubmitted.Status)

	// submitted → accepted → completed
	_, err = svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID, ToStatus: govfiling.StatusAccepted,
	})
	require.NoError(t, err)
	completed, err := svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID, ToStatus: govfiling.StatusCompleted,
	})
	require.NoError(t, err)
	assert.Equal(t, govfiling.StatusCompleted, completed.Status)

	// completed is terminal: completed → submitted must be rejected.
	_, err = svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID, ToStatus: govfiling.StatusSubmitted,
	})
	assert.ErrorIs(t, err, govfiling.ErrInvalidTransition, "completed is a terminal state")

	// The return reason was captured in the history.
	hist, err := svc.ListStatusHistory(ctx, tenantID, f.ID)
	require.NoError(t, err)
	var foundReason bool
	for _, hrow := range hist {
		if hrow.ToStatus == govfiling.StatusReturned && hrow.ExternalMessage != nil && *hrow.ExternalMessage == returnReason {
			foundReason = true
		}
	}
	assert.True(t, foundReason, "return reason must be stored in status history")
}

func TestFilingInvalidTransitionRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-03", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-003")

	// draft → completed is not allowed (must go through submit/accept).
	_, err := svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID, ToStatus: govfiling.StatusCompleted,
	})
	assert.ErrorIs(t, err, govfiling.ErrInvalidTransition)
}

// failingSubmitter always errors — exercises the error-retention path.
type failingSubmitter struct{}

func (failingSubmitter) Submit(_ context.Context, _ govfiling.SubmitRequest) (govfiling.SubmitResult, error) {
	return govfiling.SubmitResult{}, errors.New("mock channel unavailable")
}

func TestSubmitFailureHoldsErrorAndRetries(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-04", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Create with the default (mock) service; submit with a failing submitter.
	base := govfiling.NewService(tdb)
	f := createDraft(t, base, tenantID, actorID, empID, "idem-004")

	failSvc := base.WithSubmitter(failingSubmitter{})
	errored, err := failSvc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err, "submit-failure is handled (status held as error), not surfaced as call error")
	assert.Equal(t, govfiling.StatusError, errored.Status)
	require.NotNil(t, errored.LastError)
	assert.NotEmpty(t, *errored.LastError, "the submit error must be retained for diagnosis")

	// error → submitted (再送) succeeds once the channel is back (default mock).
	retried, err := base.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err)
	assert.Equal(t, govfiling.StatusSubmitted, retried.Status)
	require.NotNil(t, retried.ExternalRef)
	assert.Nil(t, retried.LastError, "last_error must be cleared after a successful re-send")
}

// ---------------------------------------------------------------------------
// Idempotency (冪等キーによる二重送信防止)
// ---------------------------------------------------------------------------

func TestIdempotencyKeyPreventsDuplicateFiling(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-05", "active")
	t.Cleanup(func() { truncateAll(h) })

	_ = createDraft(t, svc, tenantID, actorID, empID, "dup-key")

	// Re-using the same idempotency key in the same tenant must fail
	// (uq_gov_filings_idempotency).
	_, err := svc.CreateFiling(ctx, govfiling.CreateFilingInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeID:     empID,
		FilingType:     govfiling.FilingHealthInsuranceLose,
		Channel:        govfiling.ChannelMyna,
		IdempotencyKey: "dup-key",
	})
	assert.Error(t, err, "duplicate idempotency key in the same tenant must be rejected")
}

func TestSubmitIsIdempotentExternalRef(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-06", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-stable")
	s1, err := svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err)
	require.NotNil(t, s1.ExternalRef)

	// Drive returned → submitted again; the mock derives external_ref from the
	// idempotency key, so the same key yields the same external_ref (二重送信防止).
	_, err = svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: f.ID, ToStatus: govfiling.StatusReturned,
	})
	require.NoError(t, err)
	s2, err := svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err)
	require.NotNil(t, s2.ExternalRef)
	assert.Equal(t, *s1.ExternalRef, *s2.ExternalRef,
		"the same idempotency key must produce the same external_ref (no duplicate registration)")
}

// ---------------------------------------------------------------------------
// Document encryption + RBAC tests (content_enc AES-256-GCM, LM-013/CMP-006)
// ---------------------------------------------------------------------------

func TestAttachDocumentEncryptsContent(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-07", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-doc-1")

	docBody := "受付控: 合成 太郎 健康保険資格取得 受付番号 X-0001"
	doc, err := svc.AttachDocument(ctx, govfiling.AttachDocumentInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		FilingID:         f.ID,
		DocKind:          govfiling.DocKindReceipt,
		ContentPlaintext: []byte(docBody),
	})
	require.NoError(t, err)
	assert.Nil(t, doc.ContentEnc, "ContentEnc must not be returned from AttachDocument")

	// The stored ciphertext must not equal the plaintext.
	var row struct {
		ContentEnc []byte `gorm:"column:content_enc"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT content_enc FROM gov_filing_documents WHERE id = ? LIMIT 1`, doc.ID,
	).Scan(&row).Error)
	require.NotNil(t, row.ContentEnc)
	assert.NotEqual(t, []byte(docBody), row.ContentEnc, "plaintext must NOT be stored in DB")
}

func TestGetDocumentContentSensitiveDecrypts(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-08", "active")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "filing_sensitive",
		`{"perms":["filing:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-doc-2")
	docBody := "決定通知: 合成 太郎 受付番号 Y-9999"
	doc, err := svc.AttachDocument(ctx, govfiling.AttachDocumentInput{
		TenantID: tenantID, ActorID: actorID, FilingID: f.ID,
		DocKind: govfiling.DocKindDecision, ContentPlaintext: []byte(docBody),
	})
	require.NoError(t, err)

	// With sensitive permission ⇒ decrypted content returned.
	got, plaintext, err := svc.GetDocumentContent(ctx, govfiling.GetDocumentContentInput{
		TenantID: tenantID, ActorID: actorID, DocumentID: doc.ID, ReadSensitive: true,
	})
	require.NoError(t, err)
	assert.Nil(t, got.ContentEnc, "ciphertext must not be returned in the struct")
	assert.Equal(t, []byte(docBody), plaintext, "decrypted content must match the original")
}

func TestGetDocumentContentWithoutSensitivePermissionDenied(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	// Actor has no role ⇒ no filing:read_sensitive.
	actorID := seedUser(t, h.AdminDB, tenantID, "unpermitted@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-09", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-doc-3")
	doc, err := svc.AttachDocument(ctx, govfiling.AttachDocumentInput{
		TenantID: tenantID, ActorID: actorID, FilingID: f.ID,
		DocKind: govfiling.DocKindReturnReason, ContentPlaintext: []byte("返戻理由: 合成データ"),
	})
	require.NoError(t, err)

	// ReadSensitive=true but actor lacks the permission ⇒ ErrForbidden, no plaintext.
	_, plaintext, err := svc.GetDocumentContent(ctx, govfiling.GetDocumentContentInput{
		TenantID: tenantID, ActorID: actorID, DocumentID: doc.ID, ReadSensitive: true,
	})
	assert.ErrorIs(t, err, govfiling.ErrForbidden)
	assert.Nil(t, plaintext, "plaintext must not be returned when the service rejects the request")

	// Non-sensitive read returns metadata only, no plaintext, no error.
	got, plaintext2, err := svc.GetDocumentContent(ctx, govfiling.GetDocumentContentInput{
		TenantID: tenantID, ActorID: actorID, DocumentID: doc.ID, ReadSensitive: false,
	})
	require.NoError(t, err)
	assert.Nil(t, plaintext2)
	assert.Nil(t, got.ContentEnc)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA-GF", "active")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Filing created in tenant A.
	fA := createDraft(t, svc, tenantA, actorA, empA, "idem-rls")

	// Tenant B cannot read tenant A's filing.
	_, err := svc.GetFiling(ctx, tenantB, fA.ID)
	assert.ErrorIs(t, err, govfiling.ErrNotFound, "tenant B must not read tenant A's filing")

	// Tenant B cannot transition tenant A's filing.
	_, err = svc.UpdateStatus(ctx, govfiling.UpdateStatusInput{
		TenantID: tenantB, ActorID: actorB, ID: fA.ID, ToStatus: govfiling.StatusSubmitted,
	})
	assert.ErrorIs(t, err, govfiling.ErrNotFound, "tenant B must not update tenant A's filing")

	// Tenant B cannot submit tenant A's filing.
	_, err = svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantB, ActorID: actorB, ID: fA.ID})
	assert.ErrorIs(t, err, govfiling.ErrNotFound)

	// Tenant B's list of empA filings is empty.
	listB, err := svc.ListFilings(ctx, tenantB, empA)
	require.NoError(t, err)
	assert.Empty(t, listB, "tenant B must not see tenant A's filings")

	// Tenant A still sees its filing.
	listA, err := svc.ListFilings(ctx, tenantA, empA)
	require.NoError(t, err)
	assert.Len(t, listA, 1)
}

func TestCrossTenantDocumentIsolation(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA-DOC", "active")

	tenantB := seedTenant(t, h.AdminDB)
	// Grant tenant B's actor filing:read_sensitive — proves isolation is by
	// tenant, not by missing permission.
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	roleB := seedRoleWithPermissions(t, h.AdminDB, tenantB, "sensitive", `{"perms":["filing:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorB, roleB)
	t.Cleanup(func() { truncateAll(h) })

	fA := createDraft(t, svc, tenantA, actorA, empA, "idem-doc-rls")
	docA, err := svc.AttachDocument(ctx, govfiling.AttachDocumentInput{
		TenantID: tenantA, ActorID: actorA, FilingID: fA.ID,
		DocKind: govfiling.DocKindReceipt, ContentPlaintext: []byte("受付控 合成"),
	})
	require.NoError(t, err)

	// Tenant B (even with filing:read_sensitive) cannot read tenant A's document.
	_, plaintext, err := svc.GetDocumentContent(ctx, govfiling.GetDocumentContentInput{
		TenantID: tenantB, ActorID: actorB, DocumentID: docA.ID, ReadSensitive: true,
	})
	assert.ErrorIs(t, err, govfiling.ErrNotFound, "tenant B must not read tenant A's document body")
	assert.Nil(t, plaintext)
}

// ---------------------------------------------------------------------------
// Audit log PII non-inclusion
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP-GF-AUD", "active")
	t.Cleanup(func() { truncateAll(h) })

	f := createDraft(t, svc, tenantID, actorID, empID, "idem-audit")

	// Attach a document containing synthetic PII-looking content.
	piiContent := "決定通知 合成太郎 マイナンバー風 123456789012"
	_, err := svc.AttachDocument(ctx, govfiling.AttachDocumentInput{
		TenantID: tenantID, ActorID: actorID, FilingID: f.ID,
		DocKind: govfiling.DocKindDecision, ContentPlaintext: []byte(piiContent),
	})
	require.NoError(t, err)
	_, err = svc.SubmitFiling(ctx, govfiling.SubmitFilingInput{TenantID: tenantID, ActorID: actorID, ID: f.ID})
	require.NoError(t, err)

	// audit_logs must not contain the document content fragments anywhere.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ? OR COALESCE(resource_id,'') LIKE ?`,
		"%合成太郎%", "%合成太郎%", "%123456789012%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain document content / PII fragments")

	// resource_id of every govfiling audit row must be a valid UUID (opaque).
	var resourceIDs []string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs
		 WHERE tenant_id = ? AND resource_id IS NOT NULL`, tenantID,
	).Scan(&resourceIDs).Error)
	require.NotEmpty(t, resourceIDs)
	for _, rid := range resourceIDs {
		_, perr := uuid.Parse(rid)
		assert.NoErrorf(t, perr, "audit resource_id must be an opaque UUID, got %q", rid)
	}
}

// ---------------------------------------------------------------------------
// CreateFiling employee existence (composite FK + explicit check)
// ---------------------------------------------------------------------------

func TestCreateFilingUnknownEmployee(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := govfiling.NewService(tdb)

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.CreateFiling(context.Background(), govfiling.CreateFilingInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeID:     uuid.New(), // does not exist
		FilingType:     govfiling.FilingPensionCalc,
		Channel:        govfiling.ChannelEgov,
		IdempotencyKey: "idem-noemp",
	})
	assert.ErrorIs(t, err, govfiling.ErrNotFound)
}
