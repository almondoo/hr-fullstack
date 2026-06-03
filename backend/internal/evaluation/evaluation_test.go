package evaluation_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/evaluation"
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

// seedApprovalRoute inserts a single-step active approval route for the given
// request_type so the SubmitStage approval integration is exercised.
func seedApprovalRoute(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, requestType string, approverID uuid.UUID) {
	t.Helper()
	id := uuid.New()
	steps := `[{"step":0,"user_id":"` + approverID.String() + `"}]`
	require.NoError(t, adminDB.Exec(
		`INSERT INTO approval_routes
		   (id, tenant_id, request_type, department_id, name, steps_json, active)
		 VALUES (?, ?, ?, NULL, ?, ?::jsonb, true)`,
		id, tenantID, requestType, "route-"+requestType, steps,
	).Error)
}

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"calibration_sessions",
		"review_360_requests",
		"review_entries",
		"reviews",
		"review_templates",
		"approval_steps",
		"approval_requests",
		"approval_routes",
		"employees",
		"users",
		"roles",
		"tenants",
	)
}

func ptrF(f float64) *float64 { return &f }

// seedTemplate creates a review template with two weighted items and a rating
// scale, all config-driven (not hardcoded in the service).
func seedTemplate(t *testing.T, ctx context.Context, svc *evaluation.Service, tenantID, actorID uuid.UUID) *evaluation.Template {
	t.Helper()
	tmpl, err := svc.CreateTemplate(ctx, evaluation.CreateTemplateInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		Name:       "標準評価シート",
		StagesJSON: []byte(`[{"stage":"self"},{"stage":"primary"},{"stage":"secondary"}]`),
		// weights: ability 2, results 3 → weighted average uses these.
		ItemsJSON: []byte(`[
			{"item_key":"ability","weight":2,"competency_key":"comp_a","grade_key":"G3"},
			{"item_key":"results","weight":3,"grade_key":"G3"}
		]`),
		RatingScaleJSON: []byte(`{"S":5,"A":4,"B":3,"C":2,"D":1}`),
	})
	require.NoError(t, err)
	return tmpl
}

// driveToSecondary takes a review through self → primary → secondary submissions
// with the given secondary scores, leaving it at secondary_submitted.
func driveToSecondary(t *testing.T, ctx context.Context, svc *evaluation.Service, tenantID, actorID, reviewID uuid.UUID, abilityScore, resultsScore float64) {
	t.Helper()
	// self entries
	for _, k := range []string{"ability", "results"} {
		_, err := svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
			TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
			Stage: evaluation.StageSelf, ItemKey: k, Score: ptrF(3),
		})
		require.NoError(t, err)
	}
	_, err := svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
		NextStatus: evaluation.StatusSelfSubmitted,
	})
	require.NoError(t, err)

	// primary entries
	for _, k := range []string{"ability", "results"} {
		_, err := svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
			TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
			Stage: evaluation.StagePrimary, ItemKey: k, Score: ptrF(4),
		})
		require.NoError(t, err)
	}
	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
		NextStatus: evaluation.StatusPrimarySubmitted,
	})
	require.NoError(t, err)

	// secondary entries (these drive final_rating)
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
		Stage: evaluation.StageSecondary, ItemKey: "ability", Score: ptrF(abilityScore),
	})
	require.NoError(t, err)
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
		Stage: evaluation.StageSecondary, ItemKey: "results", Score: ptrF(resultsScore),
	})
	require.NoError(t, err)
	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: reviewID,
		NextStatus: evaluation.StatusSecondarySubmitted,
	})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Template + review CRUD
// ---------------------------------------------------------------------------

func TestCreateTemplateAndReview(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP001", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)

	list, err := svc.ListTemplates(ctx, tenantID)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	cycleID := uuid.New() // logical reference to review_cycles (ST-TM-01)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		CycleID:    cycleID,
		TemplateID: tmpl.ID,
		EmployeeID: empID,
	})
	require.NoError(t, err)
	assert.Equal(t, evaluation.StatusNotStarted, review.Status)
	assert.Nil(t, review.FinalRating)
}

func TestCreateReviewRejectsForeignEmployee(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "a@example.com")
	tmplA := seedTemplate(t, ctx, svc, tenantA, actorA)

	tenantB := seedTenant(t, h.AdminDB)
	empB := seedEmployee(t, h.AdminDB, tenantB, "EMPB1", "active")
	t.Cleanup(func() { truncateAll(h) })

	// tenantA review referencing tenantB employee — must fail (composite FK +
	// explicit existence check).
	_, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID:   tenantA,
		ActorID:    actorA,
		CycleID:    uuid.New(),
		TemplateID: tmplA.ID,
		EmployeeID: empB,
	})
	assert.Error(t, err, "cross-tenant employee reference must be rejected")
}

// ---------------------------------------------------------------------------
// FSM transitions
// ---------------------------------------------------------------------------

func TestStageFSMHappyPathAndConfirm(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP010", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// secondary scores: ability=5, results=5 → weighted avg = (5*2 + 5*3)/5 = 5
	driveToSecondary(t, ctx, svc, tenantID, actorID, review.ID, 5, 5)

	confirmed, err := svc.ConfirmReview(ctx, evaluation.ConfirmReviewInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, evaluation.StatusConfirmed, confirmed.Status)
	require.NotNil(t, confirmed.FinalRating)
	assert.InDelta(t, 5.0, *confirmed.FinalRating, 0.001)
	assert.NotNil(t, confirmed.ConfirmedAt)
}

func TestConfirmComputesConfigDrivenWeightedAverage(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP011", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// ability=2 (weight 2), results=4 (weight 3) → (2*2 + 4*3)/(2+3) = 16/5 = 3.2
	driveToSecondary(t, ctx, svc, tenantID, actorID, review.ID, 2, 4)

	confirmed, err := svc.ConfirmReview(ctx, evaluation.ConfirmReviewInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, confirmed.FinalRating)
	assert.InDelta(t, 3.2, *confirmed.FinalRating, 0.001,
		"weighted average must use config-driven item weights")
}

func TestInvalidTransitionRejected(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP012", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// not_started → secondary_submitted is not a legal move.
	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		NextStatus: evaluation.StatusSecondarySubmitted,
	})
	assert.ErrorIs(t, err, evaluation.ErrInvalidTransition,
		"skipping stages must be rejected by the FSM allow-list")
}

func TestSubmitStageRequiresCompleteItems(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP013", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// Only fill one of the two required items for the self stage.
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		Stage: evaluation.StageSelf, ItemKey: "ability", Score: ptrF(3),
	})
	require.NoError(t, err)

	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		NextStatus: evaluation.StatusSelfSubmitted,
	})
	assert.ErrorIs(t, err, evaluation.ErrIncomplete,
		"submitting a stage with unanswered items must fail")
}

func TestSelfEntriesLockAfterSubmission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP014", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	for _, k := range []string{"ability", "results"} {
		_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
			TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
			Stage: evaluation.StageSelf, ItemKey: k, Score: ptrF(3),
		})
		require.NoError(t, err)
	}
	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		NextStatus: evaluation.StatusSelfSubmitted,
	})
	require.NoError(t, err)

	// Attempting to edit a self entry after self_submitted must be locked.
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		Stage: evaluation.StageSelf, ItemKey: "ability", Score: ptrF(5),
	})
	assert.ErrorIs(t, err, evaluation.ErrInvalidTransition,
		"self entries must lock after the self stage is submitted")
}

func TestConfirmedReviewIsReadOnly(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP015", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)
	driveToSecondary(t, ctx, svc, tenantID, actorID, review.ID, 4, 4)
	_, err = svc.ConfirmReview(ctx, evaluation.ConfirmReviewInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
	})
	require.NoError(t, err)

	// Any write after confirm must be rejected.
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		Stage: evaluation.Stage360, ItemKey: "ability", Score: ptrF(2),
	})
	assert.ErrorIs(t, err, evaluation.ErrConfirmed, "confirmed review is read-only")
}

// ---------------------------------------------------------------------------
// Approval engine integration
// ---------------------------------------------------------------------------

func TestSubmitStageLinksApprovalWhenRouteExists(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	approverID := seedUser(t, h.AdminDB, tenantID, "approver@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP016", "active")
	// A route for the self-submission request type triggers an approval request.
	seedApprovalRoute(t, h.AdminDB, tenantID, "review_self_submitted", approverID)
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	for _, k := range []string{"ability", "results"} {
		_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
			TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
			Stage: evaluation.StageSelf, ItemKey: k, Score: ptrF(3),
		})
		require.NoError(t, err)
	}
	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		NextStatus: evaluation.StatusSelfSubmitted,
	})
	require.NoError(t, err)

	// An approval_request must have been created atomically in the same tx.
	var cnt int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM approval_requests
		 WHERE tenant_id = ? AND subject_ref = ? AND request_type = 'review_self_submitted'`,
		tenantID, review.ID.String(),
	).Scan(&cnt).Error)
	assert.Equal(t, int64(1), cnt, "approval request must be created when a route exists")
}

// ---------------------------------------------------------------------------
// 360-degree anonymity
// ---------------------------------------------------------------------------

func TestAggregate360HidesAnonymousRaters(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP020", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// 3 anonymous raters submit responses (meets the default min floor of 3).
	for i := 0; i < 3; i++ {
		raterEmp := seedEmployee(t, h.AdminDB, tenantID, "RATER"+uuid.New().String()[:6], "active")
		raterUser := seedUser(t, h.AdminDB, tenantID, "rater"+uuid.New().String()[:6]+"@example.com")
		req, err := svc.Create360Request(ctx, evaluation.Create360RequestInput{
			TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
			RaterEmployeeID: raterEmp, Relationship: "peer", Anonymous: true,
		})
		require.NoError(t, err)
		require.NoError(t, svc.Submit360Response(ctx, evaluation.Submit360ResponseInput{
			TenantID: tenantID, ActorID: raterUser, RequestID: req.ID,
			Entries: []evaluation.Item360{{ItemKey: "ability", Score: ptrF(4)}},
		}))
	}

	res, err := svc.Aggregate360(ctx, tenantID, review.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, 3, res.ResponseCount)
	assert.False(t, res.Suppressed, "3 responses meet the default floor")
	assert.InDelta(t, 4.0, res.ItemAverages["ability"], 0.001)
	assert.Empty(t, res.RaterEmployeeIDs,
		"anonymous raters must NOT be exposed in aggregation results")
}

func TestAggregate360SuppressesBelowMinResponses(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP021", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// Only 1 response — below the default floor of 3.
	raterEmp := seedEmployee(t, h.AdminDB, tenantID, "RATERX", "active")
	raterUser := seedUser(t, h.AdminDB, tenantID, "raterx@example.com")
	req, err := svc.Create360Request(ctx, evaluation.Create360RequestInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		RaterEmployeeID: raterEmp, Relationship: "peer", Anonymous: true,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Submit360Response(ctx, evaluation.Submit360ResponseInput{
		TenantID: tenantID, ActorID: raterUser, RequestID: req.ID,
		Entries: []evaluation.Item360{{ItemKey: "ability", Score: ptrF(5)}},
	}))

	res, err := svc.Aggregate360(ctx, tenantID, review.ID, 0)
	require.NoError(t, err)
	assert.True(t, res.Suppressed, "below-floor results must be suppressed")
	assert.Empty(t, res.ItemAverages, "suppressed result must not expose averages")
	assert.Empty(t, res.RaterEmployeeIDs)
}

func TestAggregate360ExposesRatersWhenAllNamed(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP022", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	var expected []uuid.UUID
	for i := 0; i < 3; i++ {
		raterEmp := seedEmployee(t, h.AdminDB, tenantID, "NRATER"+uuid.New().String()[:6], "active")
		raterUser := seedUser(t, h.AdminDB, tenantID, "nrater"+uuid.New().String()[:6]+"@example.com")
		expected = append(expected, raterEmp)
		req, err := svc.Create360Request(ctx, evaluation.Create360RequestInput{
			TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
			RaterEmployeeID: raterEmp, Relationship: "peer", Anonymous: false, // named
		})
		require.NoError(t, err)
		require.NoError(t, svc.Submit360Response(ctx, evaluation.Submit360ResponseInput{
			TenantID: tenantID, ActorID: raterUser, RequestID: req.ID,
			Entries: []evaluation.Item360{{ItemKey: "ability", Score: ptrF(3)}},
		}))
	}

	res, err := svc.Aggregate360(ctx, tenantID, review.ID, 0)
	require.NoError(t, err)
	assert.False(t, res.Suppressed)
	assert.ElementsMatch(t, expected, res.RaterEmployeeIDs,
		"named (non-anonymous) raters may be exposed")
}

// TestListEntriesNeverLeaks360ReviewerID is the regression guard for the MUST_FIX:
// the generic ListEntries read path (gated only by coarse review:read) must never
// surface an anonymous 360 rater's reviewer_user_id (or their free-text comment),
// which would completely bypass the suppression Aggregate360 implements.
//
// It asserts three things:
//  1. an unfiltered ListEntries returns the non-360 (self) entries but NO 360 rows;
//  2. ListEntries explicitly filtered to stage="360" is rejected (ErrForbidden) —
//     360 reads are forced through the anonymity-safe Aggregate360; and
//  3. the anonymous rater's user ID appears in NONE of the reachable outputs.
func TestListEntriesNeverLeaks360ReviewerID(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP023", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	// A self-stage entry exists so we can prove non-360 rows still come back.
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		Stage: evaluation.StageSelf, ItemKey: "ability", Score: ptrF(3),
	})
	require.NoError(t, err)

	// An ANONYMOUS 360 rater submits a response (with a sensitive comment).
	raterEmp := seedEmployee(t, h.AdminDB, tenantID, "ANONRTR", "active")
	anonRaterUser := seedUser(t, h.AdminDB, tenantID, "anonrater@example.com")
	req, err := svc.Create360Request(ctx, evaluation.Create360RequestInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		RaterEmployeeID: raterEmp, Relationship: "peer", Anonymous: true,
	})
	require.NoError(t, err)
	secretComment := "candid anonymous feedback"
	require.NoError(t, svc.Submit360Response(ctx, evaluation.Submit360ResponseInput{
		TenantID: tenantID, ActorID: anonRaterUser, RequestID: req.ID,
		Entries: []evaluation.Item360{{ItemKey: "ability", Score: ptrF(4), Comment: &secretComment}},
	}))

	// Sanity: the 360 entry IS physically persisted with the rater's user id in
	// the column (so the test proves the READ path suppresses it, not that the
	// write simply dropped it).
	var stored int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM review_entries
		 WHERE review_id = ? AND stage = '360' AND reviewer_user_id = ?`,
		review.ID, anonRaterUser,
	).Scan(&stored).Error)
	require.Equal(t, int64(1), stored, "precondition: 360 entry must carry reviewer_user_id in the column")

	assertNo360Leak := func(t *testing.T, entries []evaluation.Entry) {
		t.Helper()
		for _, e := range entries {
			assert.NotEqual(t, evaluation.Stage360, e.Stage,
				"ListEntries must not return any 360-stage row")
			if e.ReviewerUserID != nil {
				assert.NotEqual(t, anonRaterUser, *e.ReviewerUserID,
					"anonymous 360 rater's user id must never leak via ListEntries")
			}
			if e.Comment != nil {
				assert.NotEqual(t, secretComment, *e.Comment,
					"anonymous 360 rater's comment must never leak via ListEntries")
			}
		}
	}

	// (1) Unfiltered list: self entry present, 360 entry absent, no id/comment leak.
	all, err := svc.ListEntries(ctx, tenantID, review.ID, "")
	require.NoError(t, err)
	assertNo360Leak(t, all)
	var sawSelf bool
	for _, e := range all {
		if e.Stage == evaluation.StageSelf {
			sawSelf = true
		}
	}
	assert.True(t, sawSelf, "non-360 (self) entries must still be returned")

	// (2) Explicit stage=360 read is rejected — forced through Aggregate360.
	_, err = svc.ListEntries(ctx, tenantID, review.ID, evaluation.Stage360)
	require.ErrorIs(t, err, evaluation.ErrForbidden,
		"ListEntries(stage=360) must be forbidden; use Aggregate360 instead")

	// (3) Filtering by a non-360 stage also never surfaces the 360 row.
	selfOnly, err := svc.ListEntries(ctx, tenantID, review.ID, evaluation.StageSelf)
	require.NoError(t, err)
	assertNo360Leak(t, selfOnly)
}

// ---------------------------------------------------------------------------
// Calibration immutability
// ---------------------------------------------------------------------------

func TestCalibrationPreservesFinalRating(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP030", "active")
	t.Cleanup(func() { truncateAll(h) })

	cycleID := uuid.New()
	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID,
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)
	// ability=4, results=4 → (4*2 + 4*3)/5 = 4.0
	driveToSecondary(t, ctx, svc, tenantID, actorID, review.ID, 4, 4)

	// We need a final_rating before calibration; compute it but stay un-confirmed.
	// secondary_submitted reviews can be calibrated directly (per spec).
	sess, err := svc.CreateCalibrationSession(ctx, evaluation.CreateCalibrationInput{
		TenantID: tenantID, ActorID: actorID, CycleID: cycleID, Name: "Q2 calibration",
	})
	require.NoError(t, err)

	adjusted, err := svc.ApplyCalibration(ctx, evaluation.ApplyCalibrationInput{
		TenantID: tenantID, ActorID: actorID, SessionID: sess.ID, ReviewID: review.ID,
		After: 3.0, Reason: "横並び調整: 部門間バランス",
	})
	require.NoError(t, err)
	assert.Equal(t, evaluation.StatusCalibrated, adjusted.Status)
	require.NotNil(t, adjusted.AdjustedRating)
	assert.InDelta(t, 3.0, *adjusted.AdjustedRating, 0.001)

	// final_rating must remain whatever it was before calibration (here it was
	// NULL because we never confirmed; the key invariant is that calibration
	// never overwrites it).  Confirm afterwards and check final_rating is the
	// computed value, NOT the adjusted one.
	confirmed, err := svc.ConfirmReview(ctx, evaluation.ConfirmReviewInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, confirmed.FinalRating)
	assert.InDelta(t, 4.0, *confirmed.FinalRating, 0.001,
		"final_rating must be the computed weighted average, unaffected by adjusted_rating")
	require.NotNil(t, confirmed.AdjustedRating)
	assert.InDelta(t, 3.0, *confirmed.AdjustedRating, 0.001,
		"adjusted_rating must be preserved across confirmation")

	// The decisions_json must record the adjustment history.  JSONB columns must
	// be scanned via a struct field tagged type:jsonb (a bare []byte Scan
	// misinterprets the value), matching the approval package's routeStepsRow.
	var decisionsRow struct {
		DecisionsJSON []byte `gorm:"column:decisions_json;type:jsonb"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT decisions_json FROM calibration_sessions WHERE id = ?`, sess.ID,
	).Scan(&decisionsRow).Error)
	assert.Contains(t, string(decisionsRow.DecisionsJSON), `"after": 3`,
		"decisions_json must record the adjustment")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	empA := seedEmployee(t, h.AdminDB, tenantA, "EMPA1", "active")
	tmplA := seedTemplate(t, ctx, svc, tenantA, actorA)

	cycleA := uuid.New()
	reviewA, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantA, ActorID: actorA, CycleID: cycleA,
		TemplateID: tmplA.ID, EmployeeID: empA,
	})
	require.NoError(t, err)

	tenantB := seedTenant(t, h.AdminDB)
	t.Cleanup(func() { truncateAll(h) })

	// tenantB cannot read tenantA's review.
	_, err = svc.GetReview(ctx, tenantB, reviewA.ID)
	assert.ErrorIs(t, err, evaluation.ErrNotFound,
		"tenantB must not see tenantA's review")

	// tenantB listing the same cycle returns nothing.
	reviews, err := svc.ListReviewsByCycle(ctx, tenantB, cycleA)
	require.NoError(t, err)
	assert.Empty(t, reviews, "tenantB must not see tenantA cycle reviews")

	// tenantB cannot submit a stage on tenantA's review.
	_, err = svc.SubmitStage(ctx, evaluation.SubmitStageInput{
		TenantID: tenantB, ActorID: actorA, ReviewID: reviewA.ID,
		NextStatus: evaluation.StatusSelfSubmitted,
	})
	assert.ErrorIs(t, err, evaluation.ErrNotFound,
		"tenantB must not mutate tenantA's review")
}

// ---------------------------------------------------------------------------
// Audit log must not contain comment bodies / PII
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoCommentPII(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := evaluation.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	empID := seedEmployee(t, h.AdminDB, tenantID, "EMP040", "active")
	t.Cleanup(func() { truncateAll(h) })

	tmpl := seedTemplate(t, ctx, svc, tenantID, actorID)
	review, err := svc.CreateReview(ctx, evaluation.CreateReviewInput{
		TenantID: tenantID, ActorID: actorID, CycleID: uuid.New(),
		TemplateID: tmpl.ID, EmployeeID: empID,
	})
	require.NoError(t, err)

	secret := "機密評価コメント_本人にしか見せない"
	cmt := secret
	_, err = svc.UpsertEntry(ctx, evaluation.UpsertEntryInput{
		TenantID: tenantID, ActorID: actorID, ReviewID: review.ID,
		Stage: evaluation.StageSelf, ItemKey: "ability", Score: ptrF(3), Comment: &cmt,
	})
	require.NoError(t, err)

	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%"+secret+"%", "%"+secret+"%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount,
		"audit_logs must not contain evaluation comment bodies")
}
