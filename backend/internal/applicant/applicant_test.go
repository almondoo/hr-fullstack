package applicant_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/applicant"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go conventions)
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
		`INSERT INTO departments (id, tenant_id, name, code) VALUES (?, ?, ?, ?)`,
		id, tenantID, "合成部門", code,
	).Error)
	return id
}

// seedJobPosting inserts a minimal job_postings row (parent of applicants).
func seedJobPosting(t *testing.T, adminDB *gorm.DB, tenantID, deptID uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO job_postings
		   (id, tenant_id, title, status, employment_type, department_id, public_slug)
		 VALUES (?, ?, '合成求人', 'open', 'full_time', ?, ?)`,
		id, tenantID, deptID, slug,
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
		"applicant_merges",
		"applicant_consents",
		"applicant_documents",
		"applicants",
		"job_postings",
		"departments",
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

func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

// ---------------------------------------------------------------------------
// CRUD + job_posting FK
// ---------------------------------------------------------------------------

func TestCreateAndGetApplicant(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	deptID := seedDepartment(t, h.AdminDB, tenantID, "D001")
	jobID := seedJobPosting(t, h.AdminDB, tenantID, deptID, "slug-001")
	t.Cleanup(func() { truncateAll(h) })

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		JobPostingID:   &jobID,
		LastName:       "山田",
		FirstName:      "太郎",
		EmailPlaintext: "Taro.Yamada@example.com",
		PhonePlaintext: "090-0000-0000",
		ConsentStatus:  applicant.ConsentGranted,
		Source:         applicant.SourceDirect,
		RetentionLabel: "6months",
	})
	require.NoError(t, err)
	assert.Equal(t, applicant.StatusApplied, a.Status)
	assert.Nil(t, a.EmailEnc, "ciphertext must not be returned from CreateApplicant")
	assert.Nil(t, a.PhoneEnc, "ciphertext must not be returned from CreateApplicant")

	// Read back (masked — no sensitive permission requested).
	got, sc, err := svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, ReadSensitive: false,
	})
	require.NoError(t, err)
	assert.Nil(t, sc, "no sensitive contact must be returned without read_sensitive")
	assert.Equal(t, "山田", got.LastName)
	assert.Nil(t, got.EmailEnc)
}

func TestCreateApplicantInvalidJobPostingRejected(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	bogus := uuid.New()
	_, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, JobPostingID: &bogus,
		LastName: "山田", FirstName: "太郎", RetentionLabel: "6months",
	})
	assert.ErrorIs(t, err, applicant.ErrNotFound,
		"referencing a non-existent job posting must fail")
}

// ---------------------------------------------------------------------------
// email/phone column encryption round-trip + read_sensitive gate
// ---------------------------------------------------------------------------

func TestContactEncryptionRoundTripAndPermissionGate(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["ats:applicant:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	email := "synthetic.candidate@example.com"
	phone := "080-1234-5678"

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID,
		LastName: "佐藤", FirstName: "花子",
		EmailPlaintext: email, PhonePlaintext: phone,
		RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// Ciphertext is stored and is NOT the plaintext.
	var row struct {
		EmailEnc []byte `gorm:"column:email_enc"`
		PhoneEnc []byte `gorm:"column:phone_enc"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT email_enc, phone_enc FROM applicants WHERE id = ? LIMIT 1`, a.ID,
	).Scan(&row).Error)
	require.NotNil(t, row.EmailEnc)
	require.NotNil(t, row.PhoneEnc)
	assert.NotEqual(t, []byte(email), row.EmailEnc, "email plaintext must NOT be stored")
	assert.NotEqual(t, []byte(phone), row.PhoneEnc, "phone plaintext must NOT be stored")

	// With sensitive permission → decrypt round-trips correctly.
	got, sc, err := svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, ReadSensitive: true,
	})
	require.NoError(t, err)
	require.NotNil(t, sc)
	assert.Equal(t, email, sc.Email, "decrypted email must match original")
	assert.Equal(t, phone, sc.Phone, "decrypted phone must match original")
	assert.Nil(t, got.EmailEnc, "ciphertext must not be returned in struct")
}

func TestSensitiveReadBlockedWithoutPermission(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	// Actor has no role — no ats:applicant:read_sensitive.
	actorID := seedUser(t, h.AdminDB, tenantID, "noperm@example.com")
	t.Cleanup(func() { truncateAll(h) })

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID,
		LastName: "鈴木", FirstName: "一郎",
		EmailPlaintext: "blocked@example.com", PhonePlaintext: "070-1111-2222",
		RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// Service-layer re-validation must reject ReadSensitive=true with no permission.
	_, sc, err := svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, ReadSensitive: true,
	})
	assert.ErrorIs(t, err, applicant.ErrForbidden,
		"service layer must return ErrForbidden without read_sensitive")
	assert.Nil(t, sc, "no plaintext must be returned when rejected")
}

// ---------------------------------------------------------------------------
// Status transition boundaries
// ---------------------------------------------------------------------------

func TestStatusTransition(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "高橋", FirstName: "次郎",
		RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// applied → screening (valid)
	u, err := svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, Status: applicant.StatusScreening,
	})
	require.NoError(t, err)
	assert.Equal(t, applicant.StatusScreening, u.Status)

	// screening → hired (invalid: must go through interviewing/offered)
	_, err = svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, Status: applicant.StatusHired,
	})
	assert.ErrorIs(t, err, applicant.ErrInvalidTransition,
		"screening → hired must be rejected by the allow-list")

	// Move to rejected (valid terminal), then any transition out must fail.
	_, err = svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, Status: applicant.StatusRejected,
	})
	require.NoError(t, err)
	_, err = svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, Status: applicant.StatusScreening,
	})
	assert.ErrorIs(t, err, applicant.ErrInvalidTransition, "rejected is terminal")
}

// ---------------------------------------------------------------------------
// Consent withdrawal → usage restriction (aggregate consent_status)
// ---------------------------------------------------------------------------

func TestConsentGrantAndWithdraw(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "田中", FirstName: "三郎",
		RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// Grant consent for 採用選考.
	c1, err := svc.RecordConsent(ctx, applicant.RecordConsentInput{
		TenantID: tenantID, ActorID: actorID, ApplicantID: a.ID,
		Purpose: "採用選考", Granted: true,
	})
	require.NoError(t, err)
	require.NotNil(t, c1.GrantedAt)
	assert.Nil(t, c1.WithdrawnAt)

	got, _, err := svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, applicant.ConsentGranted, got.ConsentStatus,
		"aggregate consent must become granted")

	// Withdraw the same purpose → aggregate must become withdrawn (usage restriction).
	c2, err := svc.RecordConsent(ctx, applicant.RecordConsentInput{
		TenantID: tenantID, ActorID: actorID, ApplicantID: a.ID,
		Purpose: "採用選考", Granted: false,
	})
	require.NoError(t, err)
	require.NotNil(t, c2.WithdrawnAt)

	got, _, err = svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, applicant.ConsentWithdrawn, got.ConsentStatus,
		"withdrawal must flip aggregate consent to withdrawn")

	// One row per purpose (upsert, not duplicate).
	consents, err := svc.ListConsents(ctx, tenantID, a.ID)
	require.NoError(t, err)
	assert.Len(t, consents, 1)
}

// ---------------------------------------------------------------------------
// Duplicate detection + merge atomicity (no orphans) + merge audit
// ---------------------------------------------------------------------------

func TestFindDuplicatesAndMerge(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Two applicants sharing the same normalised email → duplicate candidates.
	target, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "渡辺", FirstName: "健",
		EmailPlaintext: "Dup@Example.com", RetentionLabel: "6months",
	})
	require.NoError(t, err)
	source, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "渡辺", FirstName: "健",
		EmailPlaintext: "dup@example.com", RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// Duplicate detection from target should surface the source.
	dups, err := svc.FindDuplicates(ctx, tenantID, target.ID)
	require.NoError(t, err)
	require.Len(t, dups, 1)
	assert.Equal(t, source.ID, dups[0].ID)

	// Attach a document + consent to the source so we can prove re-parenting.
	doc, err := svc.AddDocument(ctx, applicant.AddDocumentInput{
		TenantID: tenantID, ActorID: actorID, ApplicantID: source.ID,
		DocType: applicant.DocTypeResume, FileRef: "file-ref-opaque-001",
	})
	require.NoError(t, err)
	_, err = svc.RecordConsent(ctx, applicant.RecordConsentInput{
		TenantID: tenantID, ActorID: actorID, ApplicantID: source.ID,
		Purpose: "タレントプール保持", Granted: true,
	})
	require.NoError(t, err)

	// Merge source → target.
	m, err := svc.Merge(ctx, applicant.MergeInput{
		TenantID: tenantID, ActorID: actorID, SourceID: source.ID, TargetID: target.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, source.ID, m.SourceApplicantID)
	assert.Equal(t, target.ID, m.TargetApplicantID)

	// Source is logically merged (merged_into_id set), NOT deleted.
	var srcRow struct {
		MergedIntoID *uuid.UUID `gorm:"column:merged_into_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT merged_into_id FROM applicants WHERE id = ? LIMIT 1`, source.ID,
	).Scan(&srcRow).Error)
	require.NotNil(t, srcRow.MergedIntoID)
	assert.Equal(t, target.ID, *srcRow.MergedIntoID)

	var srcCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM applicants WHERE id = ?`, source.ID,
	).Scan(&srcCount).Error)
	assert.Equal(t, int64(1), srcCount, "merged source must not be physically deleted")

	// Child document was re-parented to target (no orphan).
	var docOwner struct {
		ApplicantID uuid.UUID `gorm:"column:applicant_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT applicant_id FROM applicant_documents WHERE id = ? LIMIT 1`, doc.ID,
	).Scan(&docOwner).Error)
	assert.Equal(t, target.ID, docOwner.ApplicantID, "document must be re-parented to target")

	// Consent was re-parented to target.
	var consentOwner struct {
		ApplicantID uuid.UUID `gorm:"column:applicant_id"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT applicant_id FROM applicant_consents WHERE applicant_id = ? AND purpose = ? LIMIT 1`,
		target.ID, "タレントプール保持",
	).Scan(&consentOwner).Error)
	assert.Equal(t, target.ID, consentOwner.ApplicantID, "consent must be re-parented to target")

	// Merge history recorded.
	merges, err := svc.ListMerges(ctx, tenantID, &target.ID)
	require.NoError(t, err)
	assert.Len(t, merges, 1)

	// Default listing excludes the merged source.
	list, err := svc.ListApplicants(ctx, tenantID, nil, "", false)
	require.NoError(t, err)
	for _, a := range list {
		assert.NotEqual(t, source.ID, a.ID, "merged applicant must be excluded by default")
	}
}

func TestMergeSelfRejected(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "伊藤", FirstName: "誠",
		RetentionLabel: "6months",
	})
	require.NoError(t, err)

	_, err = svc.Merge(ctx, applicant.MergeInput{
		TenantID: tenantID, ActorID: actorID, SourceID: a.ID, TargetID: a.ID,
	})
	assert.ErrorIs(t, err, applicant.ErrInvalidMerge, "self-merge must be rejected")
}

// ---------------------------------------------------------------------------
// Retention → logical expiry (anonymisation, NO physical delete)
// ---------------------------------------------------------------------------

func TestAnonymizeLogicalExpiry(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	pastExpiry := time.Now().UTC().AddDate(0, 0, -1)
	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "中村", FirstName: "薫",
		EmailPlaintext: "expire@example.com", PhonePlaintext: "090-9999-8888",
		RetentionLabel: "6months", RetentionExpiresOn: &pastExpiry,
	})
	require.NoError(t, err)

	// Move to rejected (only rejected/withdrawn past retention may be anonymised).
	_, err = svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, Status: applicant.StatusRejected,
	})
	require.NoError(t, err)

	require.NoError(t, svc.Anonymize(ctx, applicant.AnonymizeInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID,
	}))

	// PII cleared, anonymized_at set, row still present (no physical delete).
	var row struct {
		EmailEnc        []byte     `gorm:"column:email_enc"`
		PhoneEnc        []byte     `gorm:"column:phone_enc"`
		EmailNormalized *string    `gorm:"column:email_normalized"` //nolint:misspell // DB column name is schema contract
		AnonymizedAt    *time.Time `gorm:"column:anonymized_at"`
	}
	require.NoError(t, h.AdminDB.Raw(
		"SELECT email_enc, phone_enc, anonymized_at, email_normalized FROM applicants WHERE id = ? LIMIT 1", //nolint:misspell // DB column name is schema contract
		a.ID,
	).Scan(&row).Error)
	assert.Nil(t, row.EmailEnc, "email ciphertext must be cleared on anonymise")
	assert.Nil(t, row.PhoneEnc, "phone ciphertext must be cleared on anonymise")
	assert.Nil(t, row.EmailNormalized, "normalised match key must be cleared")
	require.NotNil(t, row.AnonymizedAt, "anonymized_at must be set")

	var cnt int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM applicants WHERE id = ?`, a.ID,
	).Scan(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "applicant must NEVER be physically deleted")
}

func TestAnonymizeBlockedBeforeRetentionElapsed(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	future := time.Now().UTC().AddDate(1, 0, 0)
	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "小林", FirstName: "蓮",
		EmailPlaintext: "future@example.com",
		RetentionLabel: "6months", RetentionExpiresOn: &future,
	})
	require.NoError(t, err)
	_, err = svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, Status: applicant.StatusRejected,
	})
	require.NoError(t, err)

	// Retention window not elapsed → must be rejected.
	err = svc.Anonymize(ctx, applicant.AnonymizeInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID,
	})
	assert.ErrorIs(t, err, applicant.ErrInvalidTransition,
		"anonymise must fail before retention window elapses")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation (applicants / documents / merge)
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "a@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "b@example.com")
	t.Cleanup(func() { truncateAll(h) })

	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantA, ActorID: actorA, LastName: "加藤", FirstName: "葵",
		EmailPlaintext: "cross@example.com", RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's applicant.
	_, _, err = svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantB, ActorID: actorB, ID: a.ID,
	})
	assert.ErrorIs(t, err, applicant.ErrNotFound,
		"tenant B must not read tenant A applicant")

	// Tenant B cannot update tenant A's applicant status.
	_, err = svc.UpdateStatus(ctx, applicant.UpdateStatusInput{
		TenantID: tenantB, ActorID: actorB, ID: a.ID, Status: applicant.StatusScreening,
	})
	assert.ErrorIs(t, err, applicant.ErrNotFound,
		"tenant B must not update tenant A applicant")

	// Tenant B cannot attach a document to tenant A's applicant.
	_, err = svc.AddDocument(ctx, applicant.AddDocumentInput{
		TenantID: tenantB, ActorID: actorB, ApplicantID: a.ID,
		DocType: applicant.DocTypeResume, FileRef: "x",
	})
	assert.ErrorIs(t, err, applicant.ErrNotFound,
		"tenant B must not attach documents to tenant A applicant")

	// Tenant B's listing must be empty.
	list, err := svc.ListApplicants(ctx, tenantB, nil, "", false)
	require.NoError(t, err)
	assert.Empty(t, list, "tenant B must not see tenant A applicants")

	// Tenant B cannot merge using tenant A applicant ids.
	a2, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantA, ActorID: actorA, LastName: "加藤", FirstName: "葵",
		EmailPlaintext: "cross@example.com", RetentionLabel: "6months",
	})
	require.NoError(t, err)
	_, err = svc.Merge(ctx, applicant.MergeInput{
		TenantID: tenantB, ActorID: actorB, SourceID: a2.ID, TargetID: a.ID,
	})
	assert.Error(t, err, "tenant B must not merge tenant A applicants")
}

// ---------------------------------------------------------------------------
// Audit log contains no PII (email/phone/name plaintext)
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := applicant.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["ats:applicant:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	email := "pii.candidate@example.com"
	phone := "090-7777-6666"
	a, err := svc.CreateApplicant(ctx, applicant.CreateApplicantInput{
		TenantID: tenantID, ActorID: actorID, LastName: "森田PIITAG", FirstName: "翼",
		EmailPlaintext: email, PhonePlaintext: phone, RetentionLabel: "6months",
	})
	require.NoError(t, err)

	// Trigger a sensitive read (which decrypts) — must still not log plaintext.
	_, _, err = svc.GetApplicant(ctx, applicant.GetApplicantInput{
		TenantID: tenantID, ActorID: actorID, ID: a.ID, ReadSensitive: true,
	})
	require.NoError(t, err)

	// No audit_logs row may contain any PII fragment.
	for _, frag := range []string{"%pii.candidate%", "%090-7777-6666%", "%森田PIITAG%"} {
		var cnt int64
		require.NoError(t, h.AdminDB.Raw(
			`SELECT COUNT(1) FROM audit_logs WHERE resource_id LIKE ? OR action LIKE ?`,
			frag, frag,
		).Scan(&cnt).Error)
		assert.Equal(t, int64(0), cnt, "audit_logs must not contain PII fragment %s", frag)
	}

	// resource_id values must all be valid UUIDs (opaque), never PII strings.
	var resourceIDs []string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs WHERE tenant_id = ? AND resource_id IS NOT NULL`,
		tenantID,
	).Scan(&resourceIDs).Error)
	require.NotEmpty(t, resourceIDs)
	for _, rid := range resourceIDs {
		_, perr := uuid.Parse(rid)
		assert.NoError(t, perr, "audit resource_id must be an opaque UUID, got %q", rid)
	}
}
