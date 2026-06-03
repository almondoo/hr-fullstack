package ledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/ledger"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go pattern; synthetic data only)
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
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status, hired_on)
		 VALUES (?, ?, ?, '山田', '太郎', 'full_time', ?, '2020-04-01')`,
		id, tenantID, code, status,
	).Error)
	return id
}

func seedAssignment(t *testing.T, adminDB *gorm.DB, tenantID, empID uuid.UUID, position, from string) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employee_assignments
		   (id, tenant_id, employee_id, position, grade, effective_from, reason)
		 VALUES (?, ?, ?, ?, 'G3', ?::date, '入社')`,
		uuid.New(), tenantID, empID, position, from,
	).Error)
}

func seedContract(t *testing.T, adminDB *gorm.DB, tenantID, empID uuid.UUID, start string) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employment_contracts
		   (id, tenant_id, employee_id, contract_type, start_date, status)
		 VALUES (?, ?, ?, 'permanent', ?::date, 'active')`,
		uuid.New(), tenantID, empID, start,
	).Error)
}

func seedWorkSummary(t *testing.T, adminDB *gorm.DB, tenantID, empID uuid.UUID, monthStart string, scheduled, actual, overtime, night, holiday, over60 int) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO work_summaries
		   (id, tenant_id, employee_id, period_month, scheduled_minutes, actual_minutes,
		    overtime_minutes, night_minutes, holiday_minutes, over60_minutes)
		 VALUES (?, ?, ?, ?::date, ?, ?, ?, ?, ?, ?)`,
		uuid.New(), tenantID, empID, monthStart, scheduled, actual, overtime, night, holiday, over60,
	).Error)
}

func seedAttendanceRecord(t *testing.T, adminDB *gorm.DB, tenantID, empID uuid.UUID, workDate string) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO attendance_records
		   (id, tenant_id, employee_id, work_date, clock_in, clock_out, break_minutes, source)
		 VALUES (?, ?, ?, ?::date, (?::date + time '09:00'), (?::date + time '18:00'), 60, 'web')`,
		uuid.New(), tenantID, empID, workDate, workDate, workDate,
	).Error)
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

func seedOffboardingPolicy(t *testing.T, adminDB *gorm.DB, tenantID, empID uuid.UUID, expiresOn string) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO offboarding_policies
		   (id, tenant_id, employee_id, retention_label, expires_on)
		 VALUES (?, ?, ?, '5years', ?::date)`,
		uuid.New(), tenantID, empID, expiresOn,
	).Error)
}

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"attendance_books",
		"wage_ledgers",
		"worker_rosters",
		"payroll_links",
		"ledger_settings",
		"offboarding_policies",
		"work_summaries",
		"attendance_records",
		"employment_contracts",
		"employee_assignments",
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// ---------------------------------------------------------------------------
// Settings (legal-config: retention is configurable, not hardcoded)
// ---------------------------------------------------------------------------

func TestUpsertAndGetSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Default (no row): GetSettings returns 5 years / resignation.
	st, err := svc.GetSettings(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 5, st.DefaultRetentionYears)
	assert.Equal(t, ledger.BasisResignation, st.DefaultRetentionBasis)

	// Configure the transitional measure (経過措置3年).
	st, err = svc.UpsertSettings(ctx, ledger.UpsertSettingsInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		RetentionYears:        3,
		RetentionBasis:        ledger.BasisLastEntry,
		ElectronicStorageJSON: []byte(`{"immutable_after_finalise":true,"export_formats":["csv"]}`),
	})
	require.NoError(t, err)
	assert.Equal(t, 3, st.DefaultRetentionYears)
	assert.Equal(t, ledger.BasisLastEntry, st.DefaultRetentionBasis)

	got, err := svc.GetSettings(ctx, tenantID)
	require.NoError(t, err)
	assert.Equal(t, 3, got.DefaultRetentionYears)
}

// ---------------------------------------------------------------------------
// 労働者名簿 builder (法定記載事項生成の正確性)
// ---------------------------------------------------------------------------

func TestBuildWorkerRoster(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	seedAssignment(t, h.AdminDB, tenantID, empID, "エンジニア", "2020-04-01")
	seedContract(t, h.AdminDB, tenantID, empID, "2020-04-01")
	t.Cleanup(func() { truncateAll(h) })

	// Default basis is resignation; provide a resignation date for retention.
	resignation := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		ResignationDate: &resignation,
	})
	require.NoError(t, err)
	assert.Equal(t, ledger.BasisResignation, roster.RetentionBasis)
	require.NotNil(t, roster.RetentionUntil)
	// 5-year default retention from 2026-03-31 → 2031-03-31.
	assert.Equal(t, "2031-03-31", roster.RetentionUntil.Format("2006-01-02"))

	// Statutory items are present in roster_json.
	var rosterData map[string]any
	require.NoError(t, json.Unmarshal(roster.RosterJSON, &rosterData))
	assert.Equal(t, "山田 太郎", rosterData["name"])
	assert.Equal(t, "EMP001", rosterData["employee_code"])
	assert.Equal(t, "2020-04-01", rosterData["hired_on"])
	assignments, ok := rosterData["assignments"].([]any)
	require.True(t, ok)
	assert.Len(t, assignments, 1, "従事業務履歴 (assignment) must be included")
	contracts, ok := rosterData["contracts"].([]any)
	require.True(t, ok)
	assert.Len(t, contracts, 1, "雇用契約 must be included")
}

// ---------------------------------------------------------------------------
// 出勤簿 builder (勤怠からの組み立て)
// ---------------------------------------------------------------------------

func TestBuildAttendanceBook(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP002", "active")
	// Monthly aggregate for 2026-06.
	seedWorkSummary(t, h.AdminDB, tenantID, empID, "2026-06-01", 9600, 10200, 600, 120, 0, 0)
	// Two daily records in June (労働日数 = 2).
	seedAttendanceRecord(t, h.AdminDB, tenantID, empID, "2026-06-01")
	seedAttendanceRecord(t, h.AdminDB, tenantID, empID, "2026-06-02")
	// One record in another month — must not be counted.
	seedAttendanceRecord(t, h.AdminDB, tenantID, empID, "2026-05-30")
	t.Cleanup(func() { truncateAll(h) })

	lastAttendance := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	book, err := svc.BuildAttendanceBook(ctx, ledger.BuildAttendanceBookInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		EmployeeID:         empID,
		PeriodMonth:        "2026-06",
		LastAttendanceDate: &lastAttendance,
	})
	require.NoError(t, err)
	assert.Equal(t, ledger.BasisLastAttendance, book.RetentionBasis)
	require.NotNil(t, book.RetentionUntil)
	assert.Equal(t, "2031-06-30", book.RetentionUntil.Format("2006-01-02"))

	var bookData map[string]any
	require.NoError(t, json.Unmarshal(book.BookJSON, &bookData))
	assert.Equal(t, float64(2), bookData["work_days"], "労働日数 must count only June records")
	assert.Equal(t, float64(10200), bookData["actual_minutes"])
	assert.Equal(t, float64(600), bookData["overtime_minutes"])
	assert.Equal(t, float64(120), bookData["night_minutes"])
}

// ---------------------------------------------------------------------------
// 賃金台帳 builder + 給与SaaS アダプタ冪等取込 + 正規化
// ---------------------------------------------------------------------------

func TestImportPayrollIdempotentAndBuildWageLedger(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP003", "active")
	t.Cleanup(func() { truncateAll(h) })

	importer := ledger.NewMockPayrollImporter(ledger.ProviderMoneyForward)
	importer.SetPayload("2026-06", json.RawMessage(`{"base_pay":300000,"allowances":20000,"overtime":15000,"deductions":45000}`))

	// First import.
	link1, err := svc.ImportPayroll(ctx, importer, ledger.ImportPayrollInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, Period: "2026-06",
	})
	require.NoError(t, err)
	assert.Equal(t, ledger.ProviderMoneyForward, link1.Provider)
	assert.Equal(t, ledger.PayrollStatusImported, link1.Status)

	// Re-import same period: idempotent — same row id, no duplicate.
	link2, err := svc.ImportPayroll(ctx, importer, ledger.ImportPayrollInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, Period: "2026-06",
	})
	require.NoError(t, err)
	assert.Equal(t, link1.ID, link2.ID, "re-import must update the same payroll_links row (idempotent)")

	var linkCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM payroll_links WHERE employee_id = ? AND period = ?`, empID, "2026-06",
	).Scan(&linkCount).Error)
	assert.Equal(t, int64(1), linkCount, "idempotent import must not duplicate")

	// Build the wage ledger from the import (normalisation).
	lastEntry := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	wl, err := svc.BuildWageLedger(ctx, ledger.BuildWageLedgerInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, Period: "2026-06",
		LastEntryDate: &lastEntry,
	})
	require.NoError(t, err)
	require.NotNil(t, wl.SourcePayrollLinkID)
	assert.Equal(t, link1.ID, *wl.SourcePayrollLinkID)
	assert.Equal(t, ledger.BasisLastEntry, wl.RetentionBasis)
	require.NotNil(t, wl.RetentionUntil)
	assert.Equal(t, "2031-06-30", wl.RetentionUntil.Format("2006-01-02"))

	var wage map[string]any
	require.NoError(t, json.Unmarshal(wl.WageJSON, &wage))
	assert.Equal(t, float64(300000), wage["base_pay"], "賃金データが台帳に正規化されること")

	// Source link is marked consumed.
	var status string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT status FROM payroll_links WHERE id = ?`, link1.ID,
	).Scan(&status).Error)
	assert.Equal(t, ledger.PayrollStatusConsumed, status)
}

// ---------------------------------------------------------------------------
// 保存満了日の算定: 起算日種別 × 保存年限, 経過措置切替 (設定変更で追従)
// ---------------------------------------------------------------------------

func TestRetentionComputationFollowsSettings(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP004", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Configure transitional measure: 3 years.
	_, err := svc.UpsertSettings(ctx, ledger.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, RetentionYears: 3, RetentionBasis: ledger.BasisResignation,
	})
	require.NoError(t, err)

	resignation := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, ResignationDate: &resignation,
	})
	require.NoError(t, err)
	require.NotNil(t, roster.RetentionUntil)
	// 3-year retention (経過措置) from 2026-03-31 → 2029-03-31 (NOT hardcoded 5y).
	assert.Equal(t, "2029-03-31", roster.RetentionUntil.Format("2006-01-02"))

	// Switch back to the 5-year principle and rebuild — algorithm follows config.
	_, err = svc.UpsertSettings(ctx, ledger.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, RetentionYears: 5, RetentionBasis: ledger.BasisResignation,
	})
	require.NoError(t, err)
	roster, err = svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, ResignationDate: &resignation,
	})
	require.NoError(t, err)
	require.NotNil(t, roster.RetentionUntil)
	assert.Equal(t, "2031-03-31", roster.RetentionUntil.Format("2006-01-02"))
}

// ---------------------------------------------------------------------------
// 法定保存年限が退職者データ削除ポリシーより優先 (NFR-011 整合)
// ---------------------------------------------------------------------------

func TestRetentionPrecedenceOverOffboardingPolicy(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP005", "active")
	// Offboarding deletion policy expires 2027-01-01.
	seedOffboardingPolicy(t, h.AdminDB, tenantID, empID, "2027-01-01")
	t.Cleanup(func() { truncateAll(h) })

	// Roster legal retention runs to 2031-03-31 (after the deletion date).
	resignation := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	_, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, ResignationDate: &resignation,
	})
	require.NoError(t, err)

	// Evaluate at a date AFTER the offboarding deletion date but BEFORE legal expiry.
	asOf := time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	decisions, err := svc.EvaluateRetentionPrecedence(ctx, tenantID, empID, asOf)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	d := decisions[0]
	assert.Equal(t, ledger.TypeWorkerRoster, d.LedgerType)
	assert.True(t, d.MustRetain,
		"legal retention (2031) must override offboarding deletion (2027): MustRetain=true")
	require.NotNil(t, d.OffboardingExpires)
	assert.Equal(t, "2027-01-01", d.OffboardingExpires.Format("2006-01-02"))

	// After legal retention expires, deletion policy may apply (MustRetain=false).
	asOfAfter := time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC)
	decisionsAfter, err := svc.EvaluateRetentionPrecedence(ctx, tenantID, empID, asOfAfter)
	require.NoError(t, err)
	require.Len(t, decisionsAfter, 1)
	assert.False(t, decisionsAfter[0].MustRetain,
		"after legal retention expires the deletion policy is no longer overridden")
}

// ---------------------------------------------------------------------------
// 確定 (finalised_at) 後の改変抑止 (真実性)
// ---------------------------------------------------------------------------

func TestFinaliseBlocksFurtherMutation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP006", "active")
	t.Cleanup(func() { truncateAll(h) })

	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// Finalise the roster (確定).
	require.NoError(t, svc.Finalise(ctx, ledger.FinaliseInput{
		TenantID: tenantID, ActorID: actorID, LedgerType: ledger.TypeWorkerRoster, ID: roster.ID,
	}))

	// Rebuild after finalise must be rejected (改変抑止).
	_, err = svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	assert.ErrorIs(t, err, ledger.ErrFinalised, "finalised roster must be immutable")

	// Re-finalise must be rejected as an invalid transition.
	err = svc.Finalise(ctx, ledger.FinaliseInput{
		TenantID: tenantID, ActorID: actorID, LedgerType: ledger.TypeWorkerRoster, ID: roster.ID,
	})
	assert.ErrorIs(t, err, ledger.ErrInvalidTransition, "re-finalising must fail")

	// Finalise with an unknown ledger type is rejected.
	err = svc.Finalise(ctx, ledger.FinaliseInput{
		TenantID: tenantID, ActorID: actorID, LedgerType: "bogus", ID: roster.ID,
	})
	assert.ErrorIs(t, err, ledger.ErrInvalidLedger)
}

// ---------------------------------------------------------------------------
// 確定後の改変抑止 — 書込み自体のガード (TOCTOU レース対策 / 真実性)
//
// The sequential TestFinaliseBlocksFurtherMutation only exercises the builder's
// prior `SELECT finalised_at` guard: it rebuilds AFTER finalise completes, so
// the SELECT always sees finalised_at != nil.  It cannot catch the race where a
// concurrent Finalise commits between a builder's SELECT and its ON CONFLICT DO
// UPDATE.  These tests drive the conflict-UPDATE path DIRECTLY against an
// already-finalised row (bypassing the SELECT guard) to prove the immutability
// invariant is enforced at the write itself — both by the app-level
// WHERE predicate (app-layer guard) and by the DB BEFORE UPDATE trigger.
// ---------------------------------------------------------------------------

// TestFinalisedConflictUpdateAffectsZeroRows simulates the lost-update race:
// the builder's SELECT already passed (saw finalised_at = NULL), the row was
// then finalised by a racing tx, and now the builder's ON CONFLICT DO UPDATE
// fires.  The trailing WHERE guard must make that UPDATE affect
// zero rows and leave the finalised roster_json byte-for-byte unchanged.
func TestFinalisedConflictUpdateAffectsZeroRows(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP010", "active")
	seedAssignment(t, h.AdminDB, tenantID, empID, "エンジニア", "2020-04-01")
	t.Cleanup(func() { truncateAll(h) })

	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// Finalise the roster (確定).
	require.NoError(t, svc.Finalise(ctx, ledger.FinaliseInput{
		TenantID: tenantID, ActorID: actorID, LedgerType: ledger.TypeWorkerRoster, ID: roster.ID,
	}))

	// Capture the finalised payload (the value that must never change).
	finalisedJSON := string(roster.RosterJSON)

	// Drive the exact conflict-UPDATE path the builder uses, directly — this is
	// the write that a racing builder would issue AFTER the row was finalised,
	// with its prior SELECT guard already (stalely) passed.
	var rowsAffected int64
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		res := tx.Exec( //nolint:misspell // SQL/gorm refers to finalized_at DB column (schema contract)
			`INSERT INTO worker_rosters
			   (id, tenant_id, employee_id, roster_json, retention_basis, retention_basis_date, retention_until)
			 VALUES (?, ?, ?, ?::jsonb, 'resignation', NULL, NULL)
			 ON CONFLICT (employee_id, tenant_id) DO UPDATE
			   SET roster_json = EXCLUDED.roster_json,
			       updated_at  = now()
			 WHERE worker_rosters.finalized_at IS NULL`,
			uuid.New(), tenantID, empID, `{"tampered":true}`,
		)
		rowsAffected = res.RowsAffected
		return res.Error
	}))
	assert.Equal(t, int64(0), rowsAffected,
		"conflict UPDATE on a finalised row must affect zero rows (write-level immutability guard)")

	// The finalised payload must be byte-for-byte unchanged.
	got, err := svc.GetWorkerRoster(ctx, tenantID, empID)
	require.NoError(t, err)
	assert.JSONEq(t, finalisedJSON, string(got.RosterJSON),
		"finalised roster_json must not be rewritten by a racing conflict UPDATE")
	require.NotNil(t, got.FinalisedAt, "row must remain finalised")
}

// TestFinalisedTriggerRejectsDirectMutation proves the DB BEFORE UPDATE trigger
// is the hard backstop: even a direct UPDATE that bypasses the application's
// WHERE guard (e.g. a future code path that forgot the finalised_at check)
// is rejected at the database level for a finalised row.
func TestFinalisedTriggerRejectsDirectMutation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP011", "active")
	t.Cleanup(func() { truncateAll(h) })

	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Finalise(ctx, ledger.FinaliseInput{
		TenantID: tenantID, ActorID: actorID, LedgerType: ledger.TypeWorkerRoster, ID: roster.ID,
	}))

	// A direct UPDATE of a business column on a finalised row, WITHOUT the
	// finalised_at IS NULL predicate, must be rejected by the trigger.
	err = tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`UPDATE worker_rosters SET roster_json = '{"tampered":true}'::jsonb WHERE id = ?`,
			roster.ID,
		).Error
	})
	require.Error(t, err, "DB trigger must reject business-column mutation of a finalised row")
	assert.Contains(t, err.Error(), "immutable",
		"trigger error must indicate the finalised record is immutable")

	// Re-finalising / un-finalising via a direct finalised_at change must also fail.
	err = tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`UPDATE worker_rosters SET finalized_at = NULL WHERE id = ?`, //nolint:misspell // DB column contract: finalized_at
			roster.ID,
		).Error
	})
	require.Error(t, err, "DB trigger must reject un-finalising a finalised row")

	// A pure updated_at bump (no business column change) is still permitted on a
	// finalised row (housekeeping must remain possible).
	require.NoError(t, tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`UPDATE worker_rosters SET updated_at = now() WHERE id = ?`, roster.ID,
		).Error
	}), "pure updated_at bump must remain allowed on a finalised row")

	// Payload still intact.
	got, err := svc.GetWorkerRoster(ctx, tenantID, empID)
	require.NoError(t, err)
	assert.NotContains(t, string(got.RosterJSON), "tampered",
		"finalised roster_json must remain unmodified after rejected mutations")
}

// TestConcurrentFinaliseAndRebuild stress-tests the race directly: many
// goroutines rebuild a roster while one goroutine finalises it.  Whichever
// ordering wins, the invariant must hold — once finalised, the stored
// roster_json must equal the value captured at finalise time, and the row must
// stay finalised.  No interleaving may produce a silent post-finalise rewrite.
func TestConcurrentFinaliseAndRebuild(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP012", "active")
	seedAssignment(t, h.AdminDB, tenantID, empID, "エンジニア", "2020-04-01")
	t.Cleanup(func() { truncateAll(h) })

	// Create the roster row so all rebuilds hit the ON CONFLICT path.
	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	require.NoError(t, err)

	const rebuilders = 8
	var wg sync.WaitGroup
	start := make(chan struct{})

	// Rebuilder goroutines: each repeatedly rebuilds; after finalise they must
	// get ErrFinalised and must never mutate the finalised payload.
	for i := 0; i < rebuilders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 10; j++ {
				_, e := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
					TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
				})
				// Either succeeds (pre-finalise) or is rejected as finalised.
				// Any other error (e.g. a DB trigger violation surfacing) is a
				// failure of the invariant we must not silently swallow.
				if e != nil && !errors.Is(e, ledger.ErrFinalised) {
					t.Errorf("unexpected rebuild error: %v", e)
					return
				}
			}
		}()
	}

	// Finaliser goroutine.
	var finalisedJSON string
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		// Give rebuilders a moment to interleave, then finalise.
		_ = svc.Finalise(ctx, ledger.FinaliseInput{
			TenantID: tenantID, ActorID: actorID, LedgerType: ledger.TypeWorkerRoster, ID: roster.ID,
		})
	}()

	close(start)
	wg.Wait()

	// Capture the post-run state.
	final, err := svc.GetWorkerRoster(ctx, tenantID, empID)
	require.NoError(t, err)
	require.NotNil(t, final.FinalisedAt, "roster must be finalised after the run")
	finalisedJSON = string(final.RosterJSON)

	// Any further rebuild must be rejected and must not change the payload.
	_, err = svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	assert.ErrorIs(t, err, ledger.ErrFinalised)

	after, err := svc.GetWorkerRoster(ctx, tenantID, empID)
	require.NoError(t, err)
	assert.JSONEq(t, finalisedJSON, string(after.RosterJSON),
		"finalised roster_json must be stable across concurrent rebuild attempts")
}

// ---------------------------------------------------------------------------
// CSV export (可視性)
// ---------------------------------------------------------------------------

func TestExportWageLedgerCSV(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP007", "active")
	t.Cleanup(func() { truncateAll(h) })

	importer := ledger.NewMockPayrollImporter(ledger.ProviderMock)
	importer.SetPayload("2026-06", json.RawMessage(`{"base_pay":250000}`))
	_, err := svc.ImportPayroll(ctx, importer, ledger.ImportPayrollInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, Period: "2026-06",
	})
	require.NoError(t, err)
	_, err = svc.BuildWageLedger(ctx, ledger.BuildWageLedgerInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, Period: "2026-06",
	})
	require.NoError(t, err)

	csvOut, err := svc.ExportWageLedgerCSV(ctx, tenantID, "2026-06")
	require.NoError(t, err)
	assert.Contains(t, csvOut, "employee_id,period,wage_json,retention_until,finalised")
	assert.Contains(t, csvOut, empID.String())
	assert.Contains(t, csvOut, "2026-06")
}

// ---------------------------------------------------------------------------
// RLS 越境拒否 (別テナントの帳簿参照/更新不可、BからのListは空)
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA01", "active")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Build a roster in tenant A.
	roster, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantA, ActorID: actorA, EmployeeID: empA,
	})
	require.NoError(t, err)

	// Tenant B context must NOT see tenant A's roster.
	_, err = svc.GetWorkerRoster(ctx, tenantB, empA)
	assert.ErrorIs(t, err, ledger.ErrNotFound, "tenant B must not read tenant A roster")

	// Tenant B context must NOT finalise tenant A's roster (cross-tenant update blocked).
	err = svc.Finalise(ctx, ledger.FinaliseInput{
		TenantID: tenantB, ActorID: actorB, LedgerType: ledger.TypeWorkerRoster, ID: roster.ID,
	})
	assert.ErrorIs(t, err, ledger.ErrNotFound, "tenant B must not finalise tenant A roster")

	// Confirm tenant A's roster is still NOT finalised (B's attempt had no effect).
	var rosterRow struct {
		FinalisedAt *time.Time `gorm:"column:finalized_at"` //nolint:misspell // SQL/gorm refers to finalized_at DB column (schema contract)
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT finalized_at FROM worker_rosters WHERE id = ?`, //nolint:misspell // DB column contract
		roster.ID,
	).Scan(&rosterRow).Error)
	assert.Nil(t, rosterRow.FinalisedAt, "tenant A roster must remain unfinalised after tenant B attempt")

	// Tenant B retention evaluation for empA returns no rows (cannot see A's ledgers).
	decisions, err := svc.EvaluateRetentionPrecedence(ctx, tenantB, empA, time.Now())
	require.NoError(t, err)
	assert.Empty(t, decisions, "tenant B must not see tenant A ledgers")

	// Building a payroll import in tenant B for tenant A's employee must fail
	// (employee does not exist in tenant B's RLS view).
	importer := ledger.NewMockPayrollImporter(ledger.ProviderMock)
	_, err = svc.ImportPayroll(ctx, importer, ledger.ImportPayrollInput{
		TenantID: tenantB, ActorID: actorB, EmployeeID: empA, Period: "2026-06",
	})
	assert.ErrorIs(t, err, ledger.ErrNotFound, "cross-tenant payroll import must fail")
}

// ---------------------------------------------------------------------------
// 監査PII非混入 (audit_logs に平文PIIが無いことを SELECT で0件確認)
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoPII(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP008", "active")
	t.Cleanup(func() { truncateAll(h) })

	// Build a roster (employee name 山田 太郎 ends up in roster_json, not audit).
	_, err := svc.BuildWorkerRoster(ctx, ledger.BuildRosterInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// audit_logs must contain at least one ledger action row...
	var actionCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs WHERE tenant_id = ? AND action = 'worker_roster.built'`,
		tenantID,
	).Scan(&actionCount).Error)
	assert.Equal(t, int64(1), actionCount)

	// ...but no audit row may contain the employee name (PII) in any field.
	var piiCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ? OR resource_type LIKE ?`,
		"%山田%", "%山田%", "%山田%",
	).Scan(&piiCount).Error)
	assert.Equal(t, int64(0), piiCount, "audit_logs must not contain employee name (PII)")

	// resource_id must be an opaque UUID (the roster id).
	var resourceID string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs WHERE action = 'worker_roster.built' LIMIT 1`,
	).Scan(&resourceID).Error)
	_, parseErr := uuid.Parse(resourceID)
	assert.NoError(t, parseErr, "audit resource_id must be an opaque UUID")
}

// ---------------------------------------------------------------------------
// Build for non-existent employee → ErrNotFound (defence-in-depth)
// ---------------------------------------------------------------------------

func TestBuildAttendanceBookUnknownEmployee(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.BuildAttendanceBook(ctx, ledger.BuildAttendanceBookInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: uuid.New(), PeriodMonth: "2026-06",
	})
	assert.ErrorIs(t, err, ledger.ErrNotFound)
}

// ---------------------------------------------------------------------------
// Build wage ledger without a prior import → ErrNotFound
// ---------------------------------------------------------------------------

func TestBuildWageLedgerWithoutImport(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := ledger.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP009", "active")
	t.Cleanup(func() { truncateAll(h) })

	_, err := svc.BuildWageLedger(ctx, ledger.BuildWageLedgerInput{
		TenantID: tenantID, ActorID: actorID, EmployeeID: empID, Period: "2026-06",
	})
	assert.ErrorIs(t, err, ledger.ErrNotFound, "wage ledger requires a prior payroll import")
}

// permsForLedger is a convenience for tests that exercise RBAC seeding.
// (kept for parity with onboarding test helpers; verifies role seeding works.)
func TestRoleSeedingForLedgerPermissions(t *testing.T) {
	h := testdb.NewHarness(t)
	_ = tenantdb.New(h.AppDB)
	tenantID := seedTenant(t, h.AdminDB)
	userID := seedUser(t, h.AdminDB, tenantID, "perm@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "ledger_admin",
		`{"perms":["ledger:read","ledger:write","ledger:finalize"]}`) //nolint:misspell // permission name contract
	assignRole(t, h.AdminDB, userID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	var roleName string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT r.name FROM roles r JOIN users u ON u.role_id = r.id WHERE u.id = ?`, userID,
	).Scan(&roleName).Error)
	assert.Equal(t, "ledger_admin", roleName)
}
