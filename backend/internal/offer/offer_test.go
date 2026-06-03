package offer_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/offer"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go pattern; AdminDB-based)
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
		"econtract_webhook_events",
		"offer_responses",
		"offer_letters",
		"offers",
		"offer_settings",
		"approval_steps",
		"approval_requests",
		"approval_routes",
		"roles",
		"users",
		"tenants",
	)
}

// syntheticKey returns a synthetic 32-byte test key (not a real secret).
func syntheticKey() []byte {
	return bytes.Repeat([]byte{0x37}, 32)
}

// setupCrypto injects a synthetic key for the global cipher.
func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

// fixedClockOffer constructs an offer in 'sent' state directly via AdminDB,
// used to exercise transitions without going through approval. Returns offer ID.
func seedSentOffer(t *testing.T, adminDB *gorm.DB, tenantID, applicationID uuid.UUID, expiry *time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO offers (id, tenant_id, application_id, status, position, employment_type, expiry_date)
		 VALUES (?, ?, ?, 'sent', 'Engineer', 'full_time', ?)`,
		id, tenantID, applicationID, expiry,
	).Error)
	return id
}

// ---------------------------------------------------------------------------
// Settings tests
// ---------------------------------------------------------------------------

func TestUpsertAndGetSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	s, err := svc.UpsertSettings(ctx, offer.UpsertSettingsInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		RequiredFieldsJSON:    []byte(`{"fields":["position","annual_salary","start_date"]}`),
		RetentionYears:        7,
		EsignStorageMode:      "evidence_hash",
		DefaultExpiryLeadDays: 14,
	})
	require.NoError(t, err)
	assert.Equal(t, 7, s.RetentionYears)

	// Update (upsert) changes the row in place.
	s2, err := svc.UpsertSettings(ctx, offer.UpsertSettingsInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		RequiredFieldsJSON:    []byte(`{"fields":["position"]}`),
		RetentionYears:        10,
		EsignStorageMode:      "evidence_hash",
		DefaultExpiryLeadDays: 7,
	})
	require.NoError(t, err)
	assert.Equal(t, s.ID, s2.ID, "upsert must update the same row")
	assert.Equal(t, 10, s2.RetentionYears)

	got, err := svc.GetSettings(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 10, got.RetentionYears)
}

func TestGetSettingsNotFound(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.GetSettings(ctx, tenantID)
	assert.ErrorIs(t, err, offer.ErrSettingNotFound)
}

// ---------------------------------------------------------------------------
// Offer CRUD + crypto round-trip + read_sensitive gate
// ---------------------------------------------------------------------------

func TestCreateOfferEncryptsSalary(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New() // logical reference (ST-ATS-03), no FK
	t.Cleanup(func() { truncateAll(h) })

	syntheticSalary := "想定年収 6,000,000円"

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		ApplicationID:         applicationID,
		Position:              "シニアエンジニア",
		EmploymentType:        "full_time",
		AnnualSalaryPlaintext: []byte(syntheticSalary),
	})
	require.NoError(t, err)
	assert.Equal(t, offer.StatusDraft, off.Status)
	assert.Nil(t, off.AnnualSalaryEnc, "ciphertext must not be returned from CreateOffer")

	// The ciphertext stored in the DB must NOT equal the plaintext.
	var row struct {
		AnnualSalaryEnc []byte `gorm:"column:annual_salary_enc"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT annual_salary_enc FROM offers WHERE id = ? LIMIT 1`, off.ID,
	).Scan(&row).Error)
	require.NotNil(t, row.AnnualSalaryEnc, "annual_salary_enc must be stored")
	assert.NotEqual(t, []byte(syntheticSalary), row.AnnualSalaryEnc,
		"plaintext salary must NOT be stored in DB")
}

func TestGetOfferSensitiveDecryptsSalary(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["offer:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	syntheticSalary := "想定年収 7,200,000円"
	syntheticComp := "賞与年2回・通勤手当上限3万円"

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID:                    tenantID,
		ActorID:                     actorID,
		ApplicationID:               applicationID,
		AnnualSalaryPlaintext:       []byte(syntheticSalary),
		CompensationDetailPlaintext: []byte(syntheticComp),
	})
	require.NoError(t, err)

	got, terms, err := svc.GetOffer(ctx, offer.GetOfferInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		ID:            off.ID,
		ReadSensitive: true,
	})
	require.NoError(t, err)
	assert.Nil(t, got.AnnualSalaryEnc, "ciphertext must not be in returned struct")
	require.NotNil(t, terms)
	assert.Equal(t, []byte(syntheticSalary), terms.AnnualSalary)
	assert.Equal(t, []byte(syntheticComp), terms.CompensationDetail)
}

func TestGetOfferWithoutSensitivePermissionMasks(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		ApplicationID:         applicationID,
		AnnualSalaryPlaintext: []byte("想定年収 5,000,000円"),
	})
	require.NoError(t, err)

	got, terms, err := svc.GetOffer(ctx, offer.GetOfferInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		ID:            off.ID,
		ReadSensitive: false,
	})
	require.NoError(t, err)
	assert.Nil(t, got.AnnualSalaryEnc)
	assert.Nil(t, terms, "no decrypted terms without sensitive read")
}

// Service-layer defence-in-depth: ReadSensitive=true but no permission → ErrForbidden,
// and no plaintext is returned.
func TestGetOfferServiceLayerBlocksUnpermittedSensitiveRead(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "unpermitted@example.com") // no role
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		ApplicationID:         applicationID,
		AnnualSalaryPlaintext: []byte("想定年収 9,000,000円"),
	})
	require.NoError(t, err)

	_, terms, err := svc.GetOffer(ctx, offer.GetOfferInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		ID:            off.ID,
		ReadSensitive: true, // requested but no permission
	})
	assert.ErrorIs(t, err, offer.ErrForbidden)
	assert.Nil(t, terms, "plaintext must not be returned when service rejects")
}

// ---------------------------------------------------------------------------
// State transition boundaries
// ---------------------------------------------------------------------------

func TestSendOfferWithoutApprovalRoute(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)

	// No approval link → may send directly (manual flow).
	sent, err := svc.SendOffer(ctx, offer.SendOfferInput{
		TenantID: tenantID, ActorID: actorID, ID: off.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, offer.StatusSent, sent.Status)
}

func TestTerminalOfferRejectsReTransition(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)

	_, err = svc.SendOffer(ctx, offer.SendOfferInput{TenantID: tenantID, ActorID: actorID, ID: off.ID})
	require.NoError(t, err)

	// Accept → terminal.
	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID, Response: offer.ResponseAccepted,
	})
	require.NoError(t, err)

	// Rescind an accepted (terminal) offer must fail.
	_, err = svc.RescindOffer(ctx, offer.RescindOfferInput{
		TenantID: tenantID, ActorID: actorID, ID: off.ID,
	})
	assert.ErrorIs(t, err, offer.ErrInvalidTransition, "accepted is terminal; rescind must fail")

	// A second accept must also fail (offer no longer in sent).
	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID, Response: offer.ResponseAccepted,
	})
	assert.ErrorIs(t, err, offer.ErrInvalidTransition)
}

func TestRespondToDraftRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)

	// draft → accepted is not allowed (must be sent first).
	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID, Response: offer.ResponseAccepted,
	})
	assert.ErrorIs(t, err, offer.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Expiry auto-expired (read-time reflection, no physical delete)
// ---------------------------------------------------------------------------

func TestExpiredOfferReportedExpiredAndRejectsResponse(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	// Seed a sent offer whose expiry is well in the past.
	past := time.Now().AddDate(0, 0, -10)
	offerID := seedSentOffer(t, h.AdminDB, tenantID, applicationID, &past)

	// GetOffer reports expired (logical) without a sensitive read.
	got, _, err := svc.GetOffer(ctx, offer.GetOfferInput{
		TenantID: tenantID, ActorID: actorID, ID: offerID,
	})
	require.NoError(t, err)
	assert.Equal(t, offer.StatusExpired, got.Status, "elapsed sent offer must read as expired")

	// Responding to an expired offer must be rejected.
	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: offerID, Response: offer.ResponseAccepted,
	})
	assert.ErrorIs(t, err, offer.ErrInvalidTransition)

	// Data is NOT physically deleted — row still exists.
	var cnt int64
	require.NoError(t, h.AdminDB.Raw(`SELECT COUNT(1) FROM offers WHERE id = ?`, offerID).Scan(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "expired offer must not be physically deleted")
}

// ---------------------------------------------------------------------------
// Approval engine integration (atomicity + approval gate)
// ---------------------------------------------------------------------------

// seedApprovalRoute creates a single-step approval route for offer_issue so
// SubmitForApproval links an approval request. Steps live in the steps_json
// JSONB array on approval_routes (see migration 00006).
func seedApprovalRoute(t *testing.T, adminDB *gorm.DB, tenantID, approverUserID uuid.UUID) {
	t.Helper()
	routeID := uuid.New()
	stepsJSON := `[{"step":0,"role":null,"user_id":"` + approverUserID.String() + `"}]`
	require.NoError(t, adminDB.Exec(
		`INSERT INTO approval_routes (id, tenant_id, request_type, name, steps_json, active)
		 VALUES (?, ?, 'offer_issue', 'Offer Issue Route', ?::jsonb, true)`,
		routeID, tenantID, stepsJSON,
	).Error)
}

func TestSubmitForApprovalLinksAndBlocksSendUntilApproved(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	approverID := seedUser(t, h.AdminDB, tenantID, "approver@example.com")
	seedApprovalRoute(t, h.AdminDB, tenantID, approverID)
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)

	// Submit for approval → links an approval request.
	submitted, err := svc.SubmitForApproval(ctx, offer.SubmitForApprovalInput{
		TenantID: tenantID, ActorID: actorID, ID: off.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, submitted.ApprovalRequestID, "approval request must be linked")

	// Atomicity: exactly one approval_request row exists for this offer.
	var arCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM approval_requests WHERE id = ? AND tenant_id = ?`,
		*submitted.ApprovalRequestID, tenantID,
	).Scan(&arCount).Error)
	assert.Equal(t, int64(1), arCount)

	// Send must be blocked while approval is pending.
	_, err = svc.SendOffer(ctx, offer.SendOfferInput{TenantID: tenantID, ActorID: actorID, ID: off.ID})
	assert.ErrorIs(t, err, offer.ErrNotApproved, "cannot send while approval pending")

	// Force-approve the request (simulate approval engine completion).
	require.NoError(t, h.AdminDB.Exec(
		`UPDATE approval_requests SET status = 'approved' WHERE id = ?`,
		*submitted.ApprovalRequestID,
	).Error)

	// Now send succeeds.
	sent, err := svc.SendOffer(ctx, offer.SendOfferInput{TenantID: tenantID, ActorID: actorID, ID: off.ID})
	require.NoError(t, err)
	assert.Equal(t, offer.StatusSent, sent.Status)
}

// ---------------------------------------------------------------------------
// ST-ATS-06 acceptance trigger (idempotent employee creation)
// ---------------------------------------------------------------------------

// countingCreator records how many times Create is invoked and inserts a marker
// row in offer_responses-adjacent counter via a temp table. To stay self-contained
// we count via an in-memory counter guarded per-call (idempotency check below
// re-accepts and expects only one fire because the second accept is rejected).
type countingCreator struct {
	calls       int
	lastOffer   uuid.UUID
	lastTenant  uuid.UUID
	lastAppl    uuid.UUID
	returnError error
}

func (c *countingCreator) Create(tx *gorm.DB, tenantID, offerID, applicationID uuid.UUID) error {
	c.calls++
	c.lastTenant = tenantID
	c.lastOffer = offerID
	c.lastAppl = applicationID
	return c.returnError
}

func TestAcceptanceFiresEmployeeCreationTriggerOnce(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	creator := &countingCreator{}
	svc := offer.NewService(tdb).WithEmployeeCreator(creator)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)
	_, err = svc.SendOffer(ctx, offer.SendOfferInput{TenantID: tenantID, ActorID: actorID, ID: off.ID})
	require.NoError(t, err)

	// Accept → trigger fires exactly once.
	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID,
		Response: offer.ResponseAccepted, RespondedVia: offer.ViaPortal,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, creator.calls, "employee creation trigger must fire once on acceptance")
	assert.Equal(t, off.ID, creator.lastOffer)
	assert.Equal(t, applicationID, creator.lastAppl)

	// A second accept is rejected (offer no longer sent) — trigger must not fire again.
	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID, Response: offer.ResponseAccepted,
	})
	assert.ErrorIs(t, err, offer.ErrInvalidTransition)
	assert.Equal(t, 1, creator.calls, "trigger must not fire on a rejected duplicate acceptance")
}

func TestDeclineDoesNotFireEmployeeCreation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	creator := &countingCreator{}
	svc := offer.NewService(tdb).WithEmployeeCreator(creator)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)
	_, err = svc.SendOffer(ctx, offer.SendOfferInput{TenantID: tenantID, ActorID: actorID, ID: off.ID})
	require.NoError(t, err)

	_, err = svc.Respond(ctx, offer.RespondInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID, Response: offer.ResponseDeclined,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, creator.calls, "decline must not fire employee creation")

	got, _, err := svc.GetOffer(ctx, offer.GetOfferInput{TenantID: tenantID, ActorID: actorID, ID: off.ID})
	require.NoError(t, err)
	assert.Equal(t, offer.StatusDeclined, got.Status)
}

// ---------------------------------------------------------------------------
// Offer letter signing evidence (content_hash tamper detection)
// ---------------------------------------------------------------------------

func TestIssueLetterAndVerifyContentHash(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
	})
	require.NoError(t, err)

	signedAt := time.Now().UTC()
	signer := "signer-opaque-ref-001"
	letter, err := svc.IssueLetter(ctx, offer.IssueLetterInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		OfferID:         off.ID,
		FileRef:         "store://offer-letters/abc",
		EsignProvider:   "mock-esign",
		EsignEnvelopeID: "env-12345",
		ContentHash:     "sha256:deadbeef",
		SignerRef:       &signer,
		SignedAt:        &signedAt,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, letter.Version)

	// A second letter increments the version.
	letter2, err := svc.IssueLetter(ctx, offer.IssueLetterInput{
		TenantID: tenantID, ActorID: actorID, OfferID: off.ID,
		ContentHash: "sha256:cafef00d",
	})
	require.NoError(t, err)
	assert.Equal(t, 2, letter2.Version)

	// Verify content hash: matching hash → true; tampered hash → false.
	ok, err := svc.VerifyLetterContentHash(ctx, tenantID, letter.ID, "sha256:deadbeef")
	require.NoError(t, err)
	assert.True(t, ok, "content_hash must match the stored hash")

	tampered, err := svc.VerifyLetterContentHash(ctx, tenantID, letter.ID, "sha256:00000000")
	require.NoError(t, err)
	assert.False(t, tampered, "mismatched hash must be detected (tamper detection)")
}

func TestIssueLetterForMissingOfferRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.IssueLetter(ctx, offer.IssueLetterInput{
		TenantID: tenantID, ActorID: actorID, OfferID: uuid.New(), ContentHash: "x",
	})
	assert.ErrorIs(t, err, offer.ErrNotFound)
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantA, ActorID: actorA, ApplicationID: applicationID,
		AnnualSalaryPlaintext: []byte("年収X"),
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's offer.
	_, _, err = svc.GetOffer(ctx, offer.GetOfferInput{
		TenantID: tenantB, ActorID: actorB, ID: off.ID,
	})
	assert.ErrorIs(t, err, offer.ErrNotFound, "tenant B must not read tenant A's offer")

	// Tenant B cannot send tenant A's offer.
	_, err = svc.SendOffer(ctx, offer.SendOfferInput{TenantID: tenantB, ActorID: actorB, ID: off.ID})
	assert.ErrorIs(t, err, offer.ErrNotFound)

	// Tenant B's list is empty.
	listB, err := svc.ListOffers(ctx, tenantB, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, listB, "tenant B must not see tenant A's offers")

	// Tenant A sees its own offer.
	listA, err := svc.ListOffers(ctx, tenantA, uuid.Nil)
	require.NoError(t, err)
	assert.Len(t, listA, 1)
}

// ---------------------------------------------------------------------------
// Audit PII non-inclusion
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoSalaryPII(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := offer.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["offer:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	applicationID := uuid.New()
	t.Cleanup(func() { truncateAll(h) })

	syntheticSalary := "想定年収 8888888円マーカー"

	off, err := svc.CreateOffer(ctx, offer.CreateOfferInput{
		TenantID: tenantID, ActorID: actorID, ApplicationID: applicationID,
		AnnualSalaryPlaintext: []byte(syntheticSalary),
	})
	require.NoError(t, err)

	// Sensitive read (decrypts) — must not leak plaintext into audit.
	_, _, err = svc.GetOffer(ctx, offer.GetOfferInput{
		TenantID: tenantID, ActorID: actorID, ID: off.ID, ReadSensitive: true,
	})
	require.NoError(t, err)

	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%8888888%", "%8888888%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain salary plaintext / PII")
}
