package oneonone_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/oneonone"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (mirrors onboarding_test.go; synthetic data only)
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

// seedUserForEmployee seeds a user linked (users.employee_id) to the given
// employee.  The participant gate resolves the actor via this column, so a user
// who is a series participant must be linked to the participant employee.
func seedUserForEmployee(t *testing.T, adminDB *gorm.DB, tenantID, employeeID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, employee_id, status) VALUES (?, ?, ?, ?, 'active')`,
		id, tenantID, email, employeeID,
	).Error)
	return id
}

func seedEmployee(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, code string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO employees
		   (id, tenant_id, employee_code, last_name, first_name, employment_type, status)
		 VALUES (?, ?, ?, '合成', '太郎', 'full_time', 'active')`,
		id, tenantID, code,
	).Error)
	return id
}

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"tm_settings",
		"one_on_one_actions",
		"one_on_one_notes",
		"one_on_one_agenda_items",
		"one_on_one_sessions",
		"one_on_one_series",
		"employees",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// newSeriesFixture seeds a tenant, manager+member employees with their linked
// participant users, and creates an active series, returning the common IDs used
// across tests.
//
// actorID is the manager's user (a series participant) so that read paths gated
// by the participant check succeed for the common-case fixture.  managerUser and
// memberUser are the two participants' user IDs; nonParticipant is a same-tenant
// user linked to an unrelated employee (holds oneonone:read at the route layer
// but is NOT a participant) used to exercise the ErrForbidden gate.
type fixture struct {
	tenantID       uuid.UUID
	actorID        uuid.UUID
	managerUser    uuid.UUID
	memberUser     uuid.UUID
	nonParticipant uuid.UUID
	manager        uuid.UUID
	member         uuid.UUID
	series         *oneonone.Series
}

func newSeriesFixture(t *testing.T, h *testdb.Harness, svc *oneonone.Service, ctx context.Context, code string) fixture {
	t.Helper()
	tenantID := seedTenant(t, h.AdminDB)
	manager := seedEmployee(t, h.AdminDB, tenantID, code+"-MGR")
	member := seedEmployee(t, h.AdminDB, tenantID, code+"-MEM")
	// Participant users linked to their employee rows (users.employee_id).
	managerUser := seedUserForEmployee(t, h.AdminDB, tenantID, manager, code+"-mgruser@example.com")
	memberUser := seedUserForEmployee(t, h.AdminDB, tenantID, member, code+"-memuser@example.com")
	// A same-tenant user linked to an unrelated employee: holds oneonone:read at
	// the route layer but is NOT a participant of this series.
	otherEmp := seedEmployee(t, h.AdminDB, tenantID, code+"-OTHEREMP")
	nonParticipant := seedUserForEmployee(t, h.AdminDB, tenantID, otherEmp, code+"-other@example.com")

	series, err := svc.CreateSeries(ctx, oneonone.CreateSeriesInput{
		TenantID:          tenantID,
		ActorID:           managerUser,
		ManagerEmployeeID: manager,
		MemberEmployeeID:  member,
		Title:             "週次1on1",
		Cadence:           oneonone.CadenceWeekly,
	})
	require.NoError(t, err)
	return fixture{
		tenantID:       tenantID,
		actorID:        managerUser,
		managerUser:    managerUser,
		memberUser:     memberUser,
		nonParticipant: nonParticipant,
		manager:        manager,
		member:         member,
		series:         series,
	}
}

// ---------------------------------------------------------------------------
// Series tests
// ---------------------------------------------------------------------------

func TestCreateAndListSeries(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "S1")
	assert.Equal(t, oneonone.SeriesStatusActive, f.series.Status)
	assert.Equal(t, oneonone.CadenceWeekly, f.series.Cadence)

	// List filtered by member participant.
	list, err := svc.ListSeries(ctx, f.tenantID, f.member)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, f.series.ID, list[0].ID)

	// List filtered by a non-participant employee → empty.
	other := seedEmployee(t, h.AdminDB, f.tenantID, "S1-OTHER")
	list, err = svc.ListSeries(ctx, f.tenantID, other)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestUpdateSeriesManagerHandover(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "S2")
	newMgr := seedEmployee(t, h.AdminDB, f.tenantID, "S2-NEWMGR")

	updated, err := svc.UpdateSeriesManager(ctx, oneonone.UpdateSeriesManagerInput{
		TenantID:             f.tenantID,
		ActorID:              f.actorID,
		SeriesID:             f.series.ID,
		NewManagerEmployeeID: newMgr,
	})
	require.NoError(t, err)
	assert.Equal(t, newMgr, updated.ManagerEmployeeID, "series must carry over to new manager")
	assert.Equal(t, f.series.ID, updated.ID, "same series ID preserved (履歴/関係を引き継ぐ)")
}

func TestCreateSeriesRejectsForeignTenantEmployee(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "A-actor@example.com")
	mgrA := seedEmployee(t, h.AdminDB, tenantA, "A-MGR")

	// Member belongs to another tenant.
	tenantB := seedTenant(t, h.AdminDB)
	memberB := seedEmployee(t, h.AdminDB, tenantB, "B-MEM")

	_, err := svc.CreateSeries(ctx, oneonone.CreateSeriesInput{
		TenantID:          tenantA,
		ActorID:           actorA,
		ManagerEmployeeID: mgrA,
		MemberEmployeeID:  memberB, // foreign tenant
	})
	assert.Error(t, err, "member from another tenant must be rejected (複合FK + employee check)")
}

// ---------------------------------------------------------------------------
// Session tests + transition boundary
// ---------------------------------------------------------------------------

func TestSessionStatusTransition(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "SES1")
	sched := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	session, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID:    f.tenantID,
		ActorID:     f.actorID,
		SeriesID:    f.series.ID,
		ScheduledAt: &sched,
	})
	require.NoError(t, err)
	assert.Equal(t, oneonone.SessionStatusScheduled, session.Status)

	// scheduled → done sets held_at.
	done, err := svc.UpdateSessionStatus(ctx, oneonone.UpdateSessionStatusInput{
		TenantID:  f.tenantID,
		ActorID:   f.actorID,
		SessionID: session.ID,
		Status:    oneonone.SessionStatusDone,
	})
	require.NoError(t, err)
	assert.Equal(t, oneonone.SessionStatusDone, done.Status)
	require.NotNil(t, done.HeldAt, "held_at must be set when session marked done")

	// done → canceled must fail (terminal).
	_, err = svc.UpdateSessionStatus(ctx, oneonone.UpdateSessionStatusInput{
		TenantID:  f.tenantID,
		ActorID:   f.actorID,
		SessionID: session.ID,
		Status:    oneonone.SessionStatusCanceled,
	})
	assert.ErrorIs(t, err, oneonone.ErrInvalidTransition, "done is terminal")
}

// ---------------------------------------------------------------------------
// Agenda carry-over test
// ---------------------------------------------------------------------------

func TestAgendaCarryOver(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "AG1")

	s1, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)
	s2, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)

	a1, err := svc.AddAgendaItem(ctx, oneonone.AddAgendaItemInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: s1.ID,
		Topic: "キャリア相談", SortOrder: 1,
	})
	require.NoError(t, err)
	_, err = svc.AddAgendaItem(ctx, oneonone.AddAgendaItemInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: s1.ID,
		Topic: "業務の悩み", SortOrder: 2,
	})
	require.NoError(t, err)

	created, err := svc.CarryOverAgenda(ctx, oneonone.CarryOverAgendaInput{
		TenantID:      f.tenantID,
		ActorID:       f.actorID,
		FromSessionID: s1.ID,
		ToSessionID:   s2.ID,
	})
	require.NoError(t, err)
	require.Len(t, created, 2)

	// Confirm the carried-over items live under s2 and reference their source.
	items, err := svc.ListAgendaItems(ctx, f.tenantID, f.actorID, s2.ID)
	require.NoError(t, err)
	require.Len(t, items, 2)
	var foundLink bool
	for _, it := range items {
		assert.Equal(t, s2.ID, it.SessionID)
		require.NotNil(t, it.CarriedOverFromID, "carried-over item must reference its source")
		if *it.CarriedOverFromID == a1.ID {
			foundLink = true
		}
	}
	assert.True(t, foundLink, "carried_over_from_id must point at the original agenda item")

	// Cross-series carry-over must fail.
	f2 := newSeriesFixture(t, h, svc, ctx, "AG1B")
	otherSession, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f2.tenantID, ActorID: f2.actorID, SeriesID: f2.series.ID,
	})
	require.NoError(t, err)
	_, err = svc.CarryOverAgenda(ctx, oneonone.CarryOverAgendaInput{
		TenantID: f.tenantID, ActorID: f.actorID,
		FromSessionID: s1.ID, ToSessionID: otherSession.ID, // belongs to another tenant entirely
	})
	assert.Error(t, err, "carry-over across tenants/sessions of a different series must fail")
}

// ---------------------------------------------------------------------------
// Notes — private memo protection (the core privacy invariant)
// ---------------------------------------------------------------------------

func TestPrivateNoteNotVisibleToOtherParticipant(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "N1")
	// Two distinct participant users from the fixture: manager-user and
	// member-user (each linked to its participant employee).
	managerUser := f.managerUser
	memberUser := f.memberUser

	session, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)

	// Manager writes a private note (sensitive content).
	privateBody := "部下の健康事情に関する機微メモ-合成"
	_, err = svc.AddNote(ctx, oneonone.AddNoteInput{
		TenantID: f.tenantID, ActorID: managerUser, SessionID: session.ID,
		AuthorUserID: managerUser, Visibility: oneonone.VisibilityPrivate, Body: privateBody,
	})
	require.NoError(t, err)

	// Manager writes a shared note (both participants can read).
	sharedBody := "次回までの宿題を確認-合成"
	_, err = svc.AddNote(ctx, oneonone.AddNoteInput{
		TenantID: f.tenantID, ActorID: managerUser, SessionID: session.ID,
		AuthorUserID: managerUser, Visibility: oneonone.VisibilityShared, Body: sharedBody,
	})
	require.NoError(t, err)

	// The OTHER participant (member) lists notes: must see the shared note but
	// NOT the manager's private note.
	memberView, err := svc.ListNotes(ctx, oneonone.ListNotesInput{
		TenantID: f.tenantID, ActorID: memberUser, SessionID: session.ID,
		ViewerUserID: memberUser,
	})
	require.NoError(t, err)
	require.Len(t, memberView, 1, "member must see only the shared note")
	assert.Equal(t, oneonone.VisibilityShared, memberView[0].Visibility)
	assert.Equal(t, sharedBody, memberView[0].Body)
	for _, n := range memberView {
		assert.NotEqual(t, privateBody, n.Body, "private note body must NEVER reach the other participant")
	}

	// The AUTHOR (manager) sees both the shared and own private note.
	managerView, err := svc.ListNotes(ctx, oneonone.ListNotesInput{
		TenantID: f.tenantID, ActorID: managerUser, SessionID: session.ID,
		ViewerUserID: managerUser,
	})
	require.NoError(t, err)
	require.Len(t, managerView, 2, "author sees shared + own private note")
	var sawPrivate bool
	for _, n := range managerView {
		if n.Visibility == oneonone.VisibilityPrivate {
			sawPrivate = true
			assert.Equal(t, privateBody, n.Body)
		}
	}
	assert.True(t, sawPrivate, "author must see their own private note")

	// A same-tenant NON-participant (holds oneonone:read at the route layer but is
	// not the series' manager/member) must NOT read the notes at all — not even the
	// shared note.  This is the participant gate (the layer the MUST_FIX added).
	_, err = svc.ListNotes(ctx, oneonone.ListNotesInput{
		TenantID: f.tenantID, ActorID: f.nonParticipant, SessionID: session.ID,
		ViewerUserID: f.nonParticipant,
	})
	assert.ErrorIs(t, err, oneonone.ErrForbidden,
		"a non-participant must be denied even the shared note (participant gate)")
}

// TestNonParticipantDeniedOnReadPaths verifies the participant gate added for the
// ST-TM-03 security review: a same-tenant user who holds oneonone:read but is NOT
// the series' manager or member is denied (ErrForbidden) on every body/detail
// read path, while the actual participants succeed.  This is the regression that
// the original tests lacked (they only checked private vs shared, never the
// non-participant case).
func TestNonParticipantDeniedOnReadPaths(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "GATE1")
	session, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)
	_, err = svc.AddAgendaItem(ctx, oneonone.AddAgendaItemInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		Topic: "キャリア相談-合成", SortOrder: 1,
	})
	require.NoError(t, err)
	_, err = svc.AddNote(ctx, oneonone.AddNoteInput{
		TenantID: f.tenantID, ActorID: f.managerUser, SessionID: session.ID,
		AuthorUserID: f.managerUser, Visibility: oneonone.VisibilityShared, Body: "共有メモ-合成",
	})
	require.NoError(t, err)
	_, err = svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		AssigneeEmployeeID: f.member, Description: "宿題-合成",
	})
	require.NoError(t, err)

	// --- Non-participant: every gated read path must return ErrForbidden. ---
	np := f.nonParticipant

	_, err = svc.GetSeries(ctx, f.tenantID, np, f.series.ID)
	assert.ErrorIs(t, err, oneonone.ErrForbidden, "non-participant must not GetSeries detail")

	_, err = svc.ListSessions(ctx, f.tenantID, np, f.series.ID)
	assert.ErrorIs(t, err, oneonone.ErrForbidden, "non-participant must not list sessions")

	_, err = svc.ListAgendaItems(ctx, f.tenantID, np, session.ID)
	assert.ErrorIs(t, err, oneonone.ErrForbidden, "non-participant must not list agenda")

	_, err = svc.ListNotes(ctx, oneonone.ListNotesInput{
		TenantID: f.tenantID, ActorID: np, SessionID: session.ID, ViewerUserID: np,
	})
	assert.ErrorIs(t, err, oneonone.ErrForbidden, "non-participant must not list notes")

	_, err = svc.ListOpenActionsForSeries(ctx, f.tenantID, np, f.series.ID)
	assert.ErrorIs(t, err, oneonone.ErrForbidden, "non-participant must not list open actions")

	// --- Both participants: the same paths must succeed. ---
	for _, viewer := range []uuid.UUID{f.managerUser, f.memberUser} {
		_, err = svc.GetSeries(ctx, f.tenantID, viewer, f.series.ID)
		require.NoErrorf(t, err, "participant %s must GetSeries", viewer)

		sessions, err := svc.ListSessions(ctx, f.tenantID, viewer, f.series.ID)
		require.NoErrorf(t, err, "participant %s must list sessions", viewer)
		assert.Len(t, sessions, 1)

		agenda, err := svc.ListAgendaItems(ctx, f.tenantID, viewer, session.ID)
		require.NoErrorf(t, err, "participant %s must list agenda", viewer)
		assert.Len(t, agenda, 1)

		notes, err := svc.ListNotes(ctx, oneonone.ListNotesInput{
			TenantID: f.tenantID, ActorID: viewer, SessionID: session.ID, ViewerUserID: viewer,
		})
		require.NoErrorf(t, err, "participant %s must list notes", viewer)
		assert.Len(t, notes, 1, "both participants see the shared note")

		open, err := svc.ListOpenActionsForSeries(ctx, f.tenantID, viewer, f.series.ID)
		require.NoErrorf(t, err, "participant %s must list open actions", viewer)
		assert.Len(t, open, 1)
	}
}

// TestActorWithoutEmployeeLinkIsDenied verifies the fail-closed behaviour: an
// actor user with no users.employee_id link can never be a participant, so the
// gate denies it even though the row exists in the same tenant.
func TestActorWithoutEmployeeLinkIsDenied(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "NOLINK1")
	// A user with no linked employee (external admin-style account).
	unlinked := seedUser(t, h.AdminDB, f.tenantID, "NOLINK1-admin@example.com")

	_, err := svc.GetSeries(ctx, f.tenantID, unlinked, f.series.ID)
	assert.ErrorIs(t, err, oneonone.ErrForbidden,
		"an actor with no employee link must be denied (fail-closed)")
}

func TestSeriesMetadataExposesNoNoteBody(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "META1")
	session, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)
	_, err = svc.UpdateSessionStatus(ctx, oneonone.UpdateSessionStatusInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		Status: oneonone.SessionStatusDone,
	})
	require.NoError(t, err)

	bodyText := "人事には見せたくない機微な本文-合成"
	_, err = svc.AddNote(ctx, oneonone.AddNoteInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		AuthorUserID: f.actorID, Visibility: oneonone.VisibilityShared, Body: bodyText,
	})
	require.NoError(t, err)
	// An open action so the metadata count is meaningful.
	_, err = svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		AssigneeEmployeeID: f.member, Description: "資料を共有する-合成",
	})
	require.NoError(t, err)

	meta, err := svc.GetSeriesMetadata(ctx, f.tenantID, f.series.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), meta.SessionCount)
	assert.Equal(t, int64(1), meta.OpenActionCount)
	require.NotNil(t, meta.LastHeldAt, "last_held_at must reflect the done session")
	// The metadata struct has no body field at all; this is a compile-time
	// guarantee. The assertions above confirm only counts/timestamps surface.
}

// ---------------------------------------------------------------------------
// Actions — completion, open carry-over, overdue, FK
// ---------------------------------------------------------------------------

func TestActionLifecycleAndCarryOver(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "ACT1")
	s1, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)
	s2, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)

	overdueDate := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	a1, err := svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: s1.ID,
		AssigneeEmployeeID: f.member, Description: "前回の宿題-合成", DueDate: &overdueDate,
	})
	require.NoError(t, err)
	a2, err := svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: s2.ID,
		AssigneeEmployeeID: f.manager, Description: "今回の宿題-合成",
	})
	require.NoError(t, err)

	// Both open actions carried across the series.
	open, err := svc.ListOpenActionsForSeries(ctx, f.tenantID, f.actorID, f.series.ID)
	require.NoError(t, err)
	assert.Len(t, open, 2, "both open actions surface across the series")

	// Complete a1.
	completed, err := svc.UpdateActionStatus(ctx, oneonone.UpdateActionStatusInput{
		TenantID: f.tenantID, ActorID: f.actorID, ActionID: a1.ID, Status: oneonone.ActionStatusDone,
	})
	require.NoError(t, err)
	assert.Equal(t, oneonone.ActionStatusDone, completed.Status)
	require.NotNil(t, completed.CompletedAt, "completed_at set on done")

	// Now only a2 remains open.
	open, err = svc.ListOpenActionsForSeries(ctx, f.tenantID, f.actorID, f.series.ID)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, a2.ID, open[0].ID)

	// done → done must fail (terminal).
	_, err = svc.UpdateActionStatus(ctx, oneonone.UpdateActionStatusInput{
		TenantID: f.tenantID, ActorID: f.actorID, ActionID: a1.ID, Status: oneonone.ActionStatusDone,
	})
	assert.ErrorIs(t, err, oneonone.ErrInvalidTransition, "done is terminal")

	// Overdue extraction (asOf after a1's due date, but a1 is now done so not listed;
	// add a fresh overdue open action to verify the filter).
	a3Date := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	_, err = svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: s1.ID,
		AssigneeEmployeeID: f.member, Description: "期日超過の宿題-合成", DueDate: &a3Date,
	})
	require.NoError(t, err)
	overdue, err := svc.ListOverdueActions(ctx, f.tenantID, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	// a3 (open, due 2026-04-01) is overdue; a1 is done; a2 has no due date.
	require.Len(t, overdue, 1)
	assert.Equal(t, oneonone.ActionStatusOpen, overdue[0].Status)
}

func TestAddActionRejectsForeignTenantAssignee(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "ACT2")
	session, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)

	// Assignee from another tenant.
	tenantB := seedTenant(t, h.AdminDB)
	foreignEmp := seedEmployee(t, h.AdminDB, tenantB, "ACT2-B")

	_, err = svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		AssigneeEmployeeID: foreignEmp, Description: "x-合成",
	})
	assert.Error(t, err, "assignee from another tenant must be rejected")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	// Tenant A: full fixture + session + note + action.
	fa := newSeriesFixture(t, h, svc, ctx, "RLS-A")
	sessA, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: fa.tenantID, ActorID: fa.actorID, SeriesID: fa.series.ID,
	})
	require.NoError(t, err)
	_, err = svc.AddNote(ctx, oneonone.AddNoteInput{
		TenantID: fa.tenantID, ActorID: fa.actorID, SessionID: sessA.ID,
		AuthorUserID: fa.actorID, Visibility: oneonone.VisibilityShared, Body: "Aの共有メモ-合成",
	})
	require.NoError(t, err)
	actA, err := svc.AddAction(ctx, oneonone.AddActionInput{
		TenantID: fa.tenantID, ActorID: fa.actorID, SessionID: sessA.ID,
		AssigneeEmployeeID: fa.member, Description: "Aのアクション-合成",
	})
	require.NoError(t, err)

	// Tenant B context.
	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "RLS-B-actor@example.com")

	// B cannot read A's series.
	_, err = svc.GetSeries(ctx, tenantB, actorB, fa.series.ID)
	assert.ErrorIs(t, err, oneonone.ErrNotFound, "tenant B must not read tenant A's series")

	// B cannot list A's sessions (series load fails as not-found in B).
	_, err = svc.ListSessions(ctx, tenantB, actorB, fa.series.ID)
	assert.ErrorIs(t, err, oneonone.ErrNotFound)

	// B cannot read A's session notes.
	_, err = svc.ListNotes(ctx, oneonone.ListNotesInput{
		TenantID: tenantB, ActorID: actorB, SessionID: sessA.ID, ViewerUserID: actorB,
	})
	assert.ErrorIs(t, err, oneonone.ErrNotFound, "tenant B must not read tenant A's notes")

	// B cannot transition A's action.
	_, err = svc.UpdateActionStatus(ctx, oneonone.UpdateActionStatusInput{
		TenantID: tenantB, ActorID: actorB, ActionID: actA.ID, Status: oneonone.ActionStatusDone,
	})
	assert.ErrorIs(t, err, oneonone.ErrNotFound, "tenant B must not mutate tenant A's action")

	// B's series list is empty.
	list, err := svc.ListSeries(ctx, tenantB, uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, list, "tenant B must not see tenant A's series")

	// Confirm A's action was not modified by B's attempt.
	stillOpen, err := svc.ListOpenActionsForSeries(ctx, fa.tenantID, fa.actorID, fa.series.ID)
	require.NoError(t, err)
	require.Len(t, stillOpen, 1)
	assert.Equal(t, oneonone.ActionStatusOpen, stillOpen[0].Status)
}

// ---------------------------------------------------------------------------
// Audit log must contain meta only — no note body / sensitive PII
// ---------------------------------------------------------------------------

func TestAuditLogContainsNoNoteBody(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	f := newSeriesFixture(t, h, svc, ctx, "AUD1")
	session, err := svc.CreateSession(ctx, oneonone.CreateSessionInput{
		TenantID: f.tenantID, ActorID: f.actorID, SeriesID: f.series.ID,
	})
	require.NoError(t, err)

	secretBody := "監査に絶対残してはいけない機微本文-合成XYZ"
	_, err = svc.AddNote(ctx, oneonone.AddNoteInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID,
		AuthorUserID: f.actorID, Visibility: oneonone.VisibilityPrivate, Body: secretBody,
	})
	require.NoError(t, err)

	// Reading the notes also writes a meta audit entry; ensure it carries no body.
	_, err = svc.ListNotes(ctx, oneonone.ListNotesInput{
		TenantID: f.tenantID, ActorID: f.actorID, SessionID: session.ID, ViewerUserID: f.actorID,
	})
	require.NoError(t, err)

	// No audit_logs row may contain the note body fragment in any text column.
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ? OR resource_type LIKE ?`,
		"%機微本文%", "%機微本文%", "%機微本文%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount, "audit_logs must never contain the 1on1 note body")

	// resource_id values for note actions must be opaque UUIDs only.
	var ids []string
	require.NoError(t, h.AdminDB.Raw(
		`SELECT resource_id FROM audit_logs
		 WHERE tenant_id = ? AND action LIKE 'oneonone_note%' AND resource_id IS NOT NULL`,
		f.tenantID,
	).Scan(&ids).Error)
	require.NotEmpty(t, ids)
	for _, id := range ids {
		_, perr := uuid.Parse(id)
		assert.NoErrorf(t, perr, "audit resource_id must be an opaque UUID, got %q", id)
	}
}

// ---------------------------------------------------------------------------
// Settings — defaults minimal; upsert toggles disclosure
// ---------------------------------------------------------------------------

func TestSettingsDefaultsAndUpsert(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := oneonone.NewService(tdb)
	ctx := context.Background()
	t.Cleanup(func() { truncateAll(h) })

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "set-actor@example.com")

	// Default (unconfigured) → minimal access: disclosure false.
	st, err := svc.GetSettings(ctx, tenantID)
	require.NoError(t, err)
	assert.False(t, st.HRManagerBodyDisclosure, "default must be最小アクセス (false)")

	days := 1825
	upd, err := svc.UpsertSettings(ctx, oneonone.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID,
		HRManagerBodyDisclosure: true, NoteRetentionDays: &days,
	})
	require.NoError(t, err)
	assert.True(t, upd.HRManagerBodyDisclosure)
	require.NotNil(t, upd.NoteRetentionDays)
	assert.Equal(t, 1825, *upd.NoteRetentionDays)

	// Re-upsert (idempotent on tenant_id) toggling back.
	upd2, err := svc.UpsertSettings(ctx, oneonone.UpsertSettingsInput{
		TenantID: tenantID, ActorID: actorID, HRManagerBodyDisclosure: false,
	})
	require.NoError(t, err)
	assert.False(t, upd2.HRManagerBodyDisclosure)
}
