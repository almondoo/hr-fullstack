package workrule_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
	"github.com/your-org/hr-saas/internal/workrule"
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

// seedLaborAgreement inserts a row into the existing attendance.labor_agreements //nolint:misspell // DB table name is schema contract
// table (the 36協定 upper-limit source of truth) and returns its id.
func seedLaborAgreement(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, workplace string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO labor_agreements (id, tenant_id, workplace, valid_from, valid_to, monthly_limit_minutes, yearly_limit_minutes, special_clause, special_monthly_limit_minutes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, //nolint:misspell // DB table name is schema contract
		id, tenantID, workplace,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC),
		2700, 21600, true, 4800,
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
		"labor_agreement_documents", //nolint:misspell // DB table name is schema contract
		"work_rule_acknowledgements",
		"work_rule_versions",
		"work_rules",
		"workrule_settings",
		"labor_agreements", //nolint:misspell // DB table name is schema contract
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// ---------------------------------------------------------------------------
// Work rule version lifecycle (testFocus: 版作成・現行版一意性・旧版superseded)
// ---------------------------------------------------------------------------

func TestWorkRuleVersionLifecycle(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	wr, err := svc.CreateWorkRule(ctx, workrule.CreateWorkRuleInput{
		TenantID: tenantID, ActorID: actorID, Title: "就業規則(本則)", Category: "main",
	})
	require.NoError(t, err)
	assert.Nil(t, wr.CurrentVersionID)

	// First version → publish → becomes current.
	v1, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantID, ActorID: actorID, WorkRuleID: wr.ID,
		RevisionReason: "初版",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, v1.Version)
	assert.Equal(t, workrule.VersionStatusDraft, v1.Status)

	pubV1, err := svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, workrule.VersionStatusPublished, pubV1.Status)
	require.NotNil(t, pubV1.PublishedAt)

	wrAfter1, err := svc.GetWorkRule(ctx, tenantID, wr.ID)
	require.NoError(t, err)
	require.NotNil(t, wrAfter1.CurrentVersionID)
	assert.Equal(t, v1.ID, *wrAfter1.CurrentVersionID, "current version must point to v1")

	// Second version (CMP-009 style amendment) → publish → v1 superseded, v2 current.
	v2, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantID, ActorID: actorID, WorkRuleID: wr.ID,
		RevisionReason: "育児介護休業法改正反映", RequiresExpertReview: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, v2.Version, "version number must auto-increment")
	assert.True(t, v2.RequiresExpertReview)

	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v2.ID,
	})
	require.NoError(t, err)

	versions, err := svc.ListVersions(ctx, tenantID, wr.ID)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	// versions[0] is v2 (newest first).
	statusByVersion := map[int]string{}
	for _, vv := range versions {
		statusByVersion[vv.Version] = vv.Status
	}
	assert.Equal(t, workrule.VersionStatusSuperseded, statusByVersion[1], "old published version must become superseded")
	assert.Equal(t, workrule.VersionStatusPublished, statusByVersion[2], "newest version must be published")

	// Current version uniqueness: exactly one published version for this rule.
	var publishedCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM work_rule_versions WHERE work_rule_id = ? AND status = 'published'`,
		wr.ID,
	).Scan(&publishedCount).Error)
	assert.Equal(t, int64(1), publishedCount, "exactly one published (current) version per rule")

	wrAfter2, err := svc.GetWorkRule(ctx, tenantID, wr.ID)
	require.NoError(t, err)
	require.NotNil(t, wrAfter2.CurrentVersionID)
	assert.Equal(t, v2.ID, *wrAfter2.CurrentVersionID)
}

func TestPublishNonDraftRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	wr, err := svc.CreateWorkRule(ctx, workrule.CreateWorkRuleInput{
		TenantID: tenantID, ActorID: actorID, Title: "規則",
	})
	require.NoError(t, err)
	v1, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantID, ActorID: actorID, WorkRuleID: wr.ID,
	})
	require.NoError(t, err)
	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
	})
	require.NoError(t, err)

	// Re-publishing an already-published version must be rejected.
	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
	})
	assert.ErrorIs(t, err, workrule.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Acknowledgements (testFocus: 周知/同意記録と未同意者抽出)
// ---------------------------------------------------------------------------

func TestAcknowledgeAndUnacknowledged(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	emp1 := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	emp2 := seedEmployee(t, h.AdminDB, tenantID, "EMP002", "active")
	emp3 := seedEmployee(t, h.AdminDB, tenantID, "EMP003", "active")
	t.Cleanup(func() { truncateAll(h) })

	wr, err := svc.CreateWorkRule(ctx, workrule.CreateWorkRuleInput{
		TenantID: tenantID, ActorID: actorID, Title: "規則",
	})
	require.NoError(t, err)
	v1, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantID, ActorID: actorID, WorkRuleID: wr.ID,
	})
	require.NoError(t, err)
	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
	})
	require.NoError(t, err)

	// emp1 reads, emp2 agrees. emp3 does nothing.
	ack1, err := svc.Acknowledge(ctx, workrule.AcknowledgeInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
		EmployeeID: emp1, Consent: workrule.ConsentRead,
	})
	require.NoError(t, err)
	assert.Equal(t, workrule.ConsentRead, ack1.Consent)

	_, err = svc.Acknowledge(ctx, workrule.AcknowledgeInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
		EmployeeID: emp2, Consent: workrule.ConsentAgreed,
	})
	require.NoError(t, err)

	// emp1 upgrades read → agreed (ON CONFLICT update).
	ack1b, err := svc.Acknowledge(ctx, workrule.AcknowledgeInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
		EmployeeID: emp1, Consent: workrule.ConsentAgreed,
	})
	require.NoError(t, err)
	assert.Equal(t, ack1.ID, ack1b.ID, "re-acknowledge must update the same row")
	assert.Equal(t, workrule.ConsentAgreed, ack1b.Consent)

	acks, err := svc.ListAcknowledgements(ctx, tenantID, v1.ID)
	require.NoError(t, err)
	assert.Len(t, acks, 2, "two distinct employees acknowledged")

	// Unacknowledged should list only emp3.
	unacked, err := svc.ListUnacknowledgedEmployees(ctx, tenantID, v1.ID)
	require.NoError(t, err)
	require.Len(t, unacked, 1)
	assert.Equal(t, emp3, unacked[0])
}

func TestAcknowledgeUnpublishedRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	emp1 := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	wr, err := svc.CreateWorkRule(ctx, workrule.CreateWorkRuleInput{
		TenantID: tenantID, ActorID: actorID, Title: "規則",
	})
	require.NoError(t, err)
	v1, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantID, ActorID: actorID, WorkRuleID: wr.ID,
	})
	require.NoError(t, err)

	// Version is still draft — acknowledging must be rejected.
	_, err = svc.Acknowledge(ctx, workrule.AcknowledgeInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
		EmployeeID: emp1, Consent: workrule.ConsentRead,
	})
	assert.ErrorIs(t, err, workrule.ErrInvalidTransition)
}

// ---------------------------------------------------------------------------
// Labour agreement filing + validity + renewal alert
// (testFocus: 届出ステータス遷移, 有効期限アラート算定, リードタイム境界)
// ---------------------------------------------------------------------------

func TestLaborAgreementFilingAndRenewalAlert(t *testing.T) { //nolint:misspell // function name used in test output
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Configure a 90-day renewal-alert lead time (no hardcoding — from settings).
	_, err := svc.UpsertSettings(ctx, workrule.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, AgreementAlertLeadDays: 90,
		RetentionPolicy: "3years",
	})
	require.NoError(t, err)

	validFrom := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	validTo := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)

	doc, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantID, ActorID: actorID, Title: "36協定(本社)",
		Type: workrule.AgreementTypeArticle36, ValidFrom: validFrom, ValidTo: validTo,
	})
	require.NoError(t, err)
	assert.Equal(t, workrule.FilingStatusDraft, doc.FilingStatus)
	require.NotNil(t, doc.RenewalAlertAt)
	// renewal alert = valid_to - 90 days.
	expectedAlert := validTo.AddDate(0, 0, -90)
	assert.Equal(t, expectedAlert.Format("2006-01-02"), doc.RenewalAlertAt.Format("2006-01-02"),
		"renewal alert must be valid_to minus the configured lead time")

	// Filing status machine: draft → filed → accepted.
	filed, err := svc.UpdateFilingStatus(ctx, workrule.UpdateFilingStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: doc.ID, Status: workrule.FilingStatusFiled,
	})
	require.NoError(t, err)
	assert.Equal(t, workrule.FilingStatusFiled, filed.FilingStatus)
	require.NotNil(t, filed.FiledAt)

	accepted, err := svc.UpdateFilingStatus(ctx, workrule.UpdateFilingStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: doc.ID, Status: workrule.FilingStatusAccepted,
	})
	require.NoError(t, err)
	assert.Equal(t, workrule.FilingStatusAccepted, accepted.FilingStatus)
	require.NotNil(t, accepted.AcceptedAt)

	// Illegal jump: accepted → filed must be rejected (terminal accepted).
	_, err = svc.UpdateFilingStatus(ctx, workrule.UpdateFilingStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: doc.ID, Status: workrule.FilingStatusFiled,
	})
	assert.ErrorIs(t, err, workrule.ErrInvalidTransition)

	// Illegal jump: draft → accepted (skipping filed) on a fresh doc.
	doc2, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantID, ActorID: actorID, Title: "その他協定",
		Type: workrule.AgreementTypeOther, ValidFrom: validFrom, ValidTo: validTo,
	})
	require.NoError(t, err)
	_, err = svc.UpdateFilingStatus(ctx, workrule.UpdateFilingStatusInput{
		TenantID: tenantID, ActorID: actorID, ID: doc2.ID, Status: workrule.FilingStatusAccepted,
	})
	assert.ErrorIs(t, err, workrule.ErrInvalidTransition, "draft→accepted must skip-jump fail")
}

func TestRenewalAlertLeadTimeDefaultWhenNoSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	validFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	validTo := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)

	// No settings row → service falls back to the bootstrap default (60 days).
	doc, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantID, ActorID: actorID, Title: "協定", Type: workrule.AgreementTypeOther,
		ValidFrom: validFrom, ValidTo: validTo,
	})
	require.NoError(t, err)
	require.NotNil(t, doc.RenewalAlertAt)
	assert.Equal(t, validTo.AddDate(0, 0, -60).Format("2006-01-02"), doc.RenewalAlertAt.Format("2006-01-02"))
}

func TestListExpiringAgreements(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.UpsertSettings(ctx, workrule.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, AgreementAlertLeadDays: 30,
	})
	require.NoError(t, err)

	// expiring soon: valid_to in 20 days → alert = valid_to-30 = 10 days ago.
	soon := time.Now().AddDate(0, 0, 20)
	docSoon, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantID, ActorID: actorID, Title: "間もなく更新", Type: workrule.AgreementTypeOther,
		ValidFrom: time.Now().AddDate(-1, 0, 0), ValidTo: soon,
	})
	require.NoError(t, err)

	// far future: valid_to in 1 year → alert not yet reached.
	_, err = svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantID, ActorID: actorID, Title: "先の更新", Type: workrule.AgreementTypeOther,
		ValidFrom: time.Now(), ValidTo: time.Now().AddDate(1, 0, 0),
	})
	require.NoError(t, err)

	expiring, err := svc.ListExpiringAgreements(ctx, tenantID, time.Now())
	require.NoError(t, err)
	require.Len(t, expiring, 1, "only the soon-expiring agreement should be flagged")
	assert.Equal(t, docSoon.ID, expiring[0].ID)
}

// ---------------------------------------------------------------------------
// 36協定上限値の参照連携(重複保持しないことの検証)
// testFocus: 36協定上限値を既存 labour_agreements から参照し重複保持しない //nolint:misspell // DB table name uses American spelling in schema
// ---------------------------------------------------------------------------

func TestLinkedLaborAgreementLimitsNotDuplicated(t *testing.T) { //nolint:misspell // function name used in test output
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	laID := seedLaborAgreement(t, h.AdminDB, tenantID, "本社")
	t.Cleanup(func() { truncateAll(h) })

	validFrom := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	validTo := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)

	doc, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantID, ActorID: actorID, Title: "36協定", Type: workrule.AgreementTypeArticle36,
		ValidFrom: validFrom, ValidTo: validTo, LinkedLaborAgreementID: &laID,
	})
	require.NoError(t, err)
	require.NotNil(t, doc.LinkedLaborAgreementID)
	assert.Equal(t, laID, *doc.LinkedLaborAgreementID)

	// The limit values must be readable via the link (source of truth in attendance).
	limits, err := svc.GetLinkedLimits(ctx, tenantID, doc.ID)
	require.NoError(t, err)
	assert.Equal(t, laID, limits.LaborAgreementID)
	assert.Equal(t, 2700, limits.MonthlyLimitMinutes)
	assert.Equal(t, 21600, limits.YearlyLimitMinutes)
	assert.True(t, limits.SpecialClause)

	// Proof of non-duplication: labour_agreement_documents has no limit columns.
	var dupCols int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM information_schema.columns WHERE table_name = 'labor_agreement_documents' AND column_name IN ('monthly_limit_minutes','yearly_limit_minutes','special_monthly_limit_minutes','special_clause')`, //nolint:misspell // DB table name is schema contract
	).Scan(&dupCols).Error)
	assert.Equal(t, int64(0), dupCols,
		"labor_agreement_documents must not duplicate 36協定 limit columns (source of truth is attendance.labor_agreements)") //nolint:misspell // DB table names are schema contract
}

func TestCreateAgreementWithCrossTenantLinkRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	// labour agreement belongs to tenant B.
	laB := seedLaborAgreement(t, h.AdminDB, tenantB, "B社")
	t.Cleanup(func() { truncateAll(h) })

	// tenant A tries to link tenant B's labour agreement — must be rejected (RLS hides it).
	_, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantA, ActorID: actorA, Title: "36協定", Type: workrule.AgreementTypeArticle36,
		ValidFrom:              time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		ValidTo:                time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC),
		LinkedLaborAgreementID: &laB,
	})
	assert.ErrorIs(t, err, workrule.ErrNotFound,
		"linking a cross-tenant labour agreement must fail")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation (testFocus: 別テナントの規程/協定/同意の参照/更新不可)
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Tenant A creates a rule + published version + labour agreement.
	wrA, err := svc.CreateWorkRule(ctx, workrule.CreateWorkRuleInput{
		TenantID: tenantA, ActorID: actorA, Title: "A社規則",
	})
	require.NoError(t, err)
	vA, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantA, ActorID: actorA, WorkRuleID: wrA.ID,
	})
	require.NoError(t, err)
	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantA, ActorID: actorA, VersionID: vA.ID,
	})
	require.NoError(t, err)
	docA, err := svc.CreateAgreement(ctx, workrule.CreateAgreementInput{
		TenantID: tenantA, ActorID: actorA, Title: "A協定", Type: workrule.AgreementTypeOther,
		ValidFrom: time.Now(), ValidTo: time.Now().AddDate(1, 0, 0),
	})
	require.NoError(t, err)

	// Tenant B cannot read tenant A's work rule.
	_, err = svc.GetWorkRule(ctx, tenantB, wrA.ID)
	assert.ErrorIs(t, err, workrule.ErrNotFound, "tenant B must not read tenant A's work rule")

	// Tenant B cannot read tenant A's agreement.
	_, err = svc.GetAgreement(ctx, tenantB, docA.ID)
	assert.ErrorIs(t, err, workrule.ErrNotFound, "tenant B must not read tenant A's agreement")

	// Tenant B cannot publish tenant A's version.
	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantB, ActorID: actorB, VersionID: vA.ID,
	})
	assert.Error(t, err, "tenant B must not publish tenant A's version")

	// Tenant B cannot change filing status of tenant A's agreement.
	_, err = svc.UpdateFilingStatus(ctx, workrule.UpdateFilingStatusInput{
		TenantID: tenantB, ActorID: actorB, ID: docA.ID, Status: workrule.FilingStatusFiled,
	})
	assert.Error(t, err, "tenant B must not file tenant A's agreement")

	// Tenant B list of work rules / agreements is empty.
	rules, err := svc.ListWorkRules(ctx, tenantB, "")
	require.NoError(t, err)
	assert.Empty(t, rules, "tenant B must not see tenant A's work rules")

	agreements, err := svc.ListAgreements(ctx, tenantB, "")
	require.NoError(t, err)
	assert.Empty(t, agreements, "tenant B must not see tenant A's agreements")
}

// ---------------------------------------------------------------------------
// Audit: writes are recorded, and contain no PII (testFocus: 監査ログ記録)
// ---------------------------------------------------------------------------

func TestAuditRecordedWithoutPII(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	emp1 := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	sensitiveTitle := "合成機密就業規則タイトル"

	wr, err := svc.CreateWorkRule(ctx, workrule.CreateWorkRuleInput{
		TenantID: tenantID, ActorID: actorID, Title: sensitiveTitle,
	})
	require.NoError(t, err)
	v1, err := svc.CreateVersion(ctx, workrule.CreateVersionInput{
		TenantID: tenantID, ActorID: actorID, WorkRuleID: wr.ID,
		RevisionReason: "合成改定理由メモ",
	})
	require.NoError(t, err)
	_, err = svc.PublishVersion(ctx, workrule.PublishVersionInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
	})
	require.NoError(t, err)
	_, err = svc.Acknowledge(ctx, workrule.AcknowledgeInput{
		TenantID: tenantID, ActorID: actorID, VersionID: v1.ID,
		EmployeeID: emp1, Consent: workrule.ConsentAgreed,
	})
	require.NoError(t, err)

	// Audit rows exist for the operations.
	var auditCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs WHERE tenant_id = ?
		 AND action IN ('work_rule.created','work_rule_version.created',
		                'work_rule_version.published','work_rule.acknowledged')`,
		tenantID,
	).Scan(&auditCount).Error)
	assert.GreaterOrEqual(t, auditCount, int64(4), "all workrule writes must be audited")

	// No audit row leaks the sensitive title or revision memo as PII.
	var piiCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ? OR resource_type LIKE ?`,
		"%合成%", "%合成%", "%合成%",
	).Scan(&piiCount).Error)
	assert.Equal(t, int64(0), piiCount, "audit_logs must not contain title / revision-reason PII")
}

// ---------------------------------------------------------------------------
// Settings upsert idempotency + retention policy config
// ---------------------------------------------------------------------------

func TestSettingsUpsert(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := workrule.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.GetSettings(ctx, tenantID)
	assert.ErrorIs(t, err, workrule.ErrNotFound, "no settings yet")

	s1, err := svc.UpsertSettings(ctx, workrule.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, AgreementAlertLeadDays: 45, RetentionPolicy: "7years",
	})
	require.NoError(t, err)
	assert.Equal(t, 45, s1.AgreementAlertLeadDays)
	assert.Equal(t, "7years", s1.RetentionPolicy)

	// Upsert again — same row updated (idempotent on tenant_id).
	s2, err := svc.UpsertSettings(ctx, workrule.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, AgreementAlertLeadDays: 120, RetentionPolicy: "10years",
	})
	require.NoError(t, err)
	assert.Equal(t, s1.ID, s2.ID, "settings upsert must reuse the same tenant row")
	assert.Equal(t, 120, s2.AgreementAlertLeadDays)

	var rows int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM workrule_settings WHERE tenant_id = ?`, tenantID,
	).Scan(&rows).Error)
	assert.Equal(t, int64(1), rows, "exactly one settings row per tenant")
}

// ---------------------------------------------------------------------------
// RBAC: RequirePermission denies unauthorised callers (testFocus: 権限強制)
// ---------------------------------------------------------------------------

// fakeAuth sets the same gin.Context keys that platformauth.RequireAuth would
// set, so RequirePermission can run without a full session.  The key string
// literals mirror the unexported keys in platform/auth/middleware.go.
func fakeAuth(tenantID, userID uuid.UUID) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("auth_tenant_id", tenantID)
		c.Set("auth_user_id", userID)
		c.Next()
	}
}

func TestRequirePermissionEnforced(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)

	tenantID := seedTenant(t, h.AdminDB)
	// user with only workrule:read.
	readerRole := seedRoleWithPermissions(t, h.AdminDB, tenantID, "reader", `{"perms":["workrule:read"]}`)
	reader := seedUser(t, h.AdminDB, tenantID, "reader@example.com")
	assignRole(t, h.AdminDB, reader, readerRole)
	// user with no role.
	noRole := seedUser(t, h.AdminDB, tenantID, "norole@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Drive RequirePermission through a gin engine to confirm it grants/denies
	// based on the caller's actual DB-backed permissions.
	doReq := func(userID uuid.UUID, need string) int {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		r.GET("/probe",
			fakeAuth(tenantID, userID),
			platformauth.RequirePermission(tdb, need),
			func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) },
		)
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/probe", nil)
		r.ServeHTTP(w, req)
		return w.Code
	}

	assert.Equal(t, http.StatusOK, doReq(reader, "workrule:read"), "reader granted workrule:read")
	assert.Equal(t, http.StatusForbidden, doReq(reader, "workrule:write"), "reader denied workrule:write")
	assert.Equal(t, http.StatusForbidden, doReq(noRole, "workrule:read"), "no-role user denied")
}
