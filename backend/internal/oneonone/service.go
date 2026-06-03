package oneonone

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("oneonone: not found")
	ErrInvalidTransition = errors.New("oneonone: invalid status transition")
	ErrForbidden         = errors.New("oneonone: permission denied")
)

// allowedSessionTransitions defines legal session status moves.
// Terminal states: done, canceled — no transitions out.
var allowedSessionTransitions = map[string]map[string]bool{
	"scheduled": {
		"done":     true,
		"canceled": true,
	},
}

// allowedActionTransitions defines legal action status moves.
// Terminal states: done, canceled — no transitions out.
var allowedActionTransitions = map[string]map[string]bool{
	"open": {
		"done":     true,
		"canceled": true,
	},
}

// isSessionTransitionAllowed reports whether a session may move current → next.
func isSessionTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedSessionTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// isActionTransitionAllowed reports whether an action may move current → next.
func isActionTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedActionTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// Service provides business logic for the 1on1 domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Internal helpers — participant / scope verification (defence-in-depth)
// ---------------------------------------------------------------------------

// loadSeriesTx reads a series row within an open tenant transaction.
// Returns ErrNotFound when the series does not exist in this tenant.
func loadSeriesTx(tx *gorm.DB, tenantID, seriesID uuid.UUID) (*Series, error) {
	var s Series
	if err := tx.Raw(
		`SELECT id, tenant_id, manager_employee_id, member_employee_id, title,
		        cadence, status, created_at, updated_at
		 FROM one_on_one_series
		 WHERE id = ? AND tenant_id = ? LIMIT 1`,
		seriesID, tenantID,
	).Scan(&s).Error; err != nil {
		return nil, fmt.Errorf("oneonone: load series: %w", err)
	}
	if s.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &s, nil
}

// loadSessionTx reads a session row (with its series_id) within a tenant tx.
func loadSessionTx(tx *gorm.DB, tenantID, sessionID uuid.UUID) (*Session, error) {
	var s Session
	if err := tx.Raw(
		`SELECT id, tenant_id, series_id, scheduled_at, held_at, status, summary,
		        created_at, updated_at
		 FROM one_on_one_sessions
		 WHERE id = ? AND tenant_id = ? LIMIT 1`,
		sessionID, tenantID,
	).Scan(&s).Error; err != nil {
		return nil, fmt.Errorf("oneonone: load session: %w", err)
	}
	if s.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &s, nil
}

// verifyEmployeeInTenant ensures an employee belongs to the tenant.
func verifyEmployeeInTenant(tx *gorm.DB, tenantID, employeeID uuid.UUID) error {
	var n int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
		employeeID, tenantID,
	).Scan(&n).Error; err != nil {
		return fmt.Errorf("oneonone: verify employee: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// resolveActorEmployeeID resolves the actor user's linked employee_id within the
// tenant (users.employee_id → employees.id).  Returns uuid.Nil when the actor
// has no linked employee (e.g. an external admin user without an employee
// record) so the caller can deny participant access fail-closed.
func resolveActorEmployeeID(tx *gorm.DB, tenantID, actorUserID uuid.UUID) (uuid.UUID, error) {
	if actorUserID == uuid.Nil {
		return uuid.Nil, nil
	}
	var row struct {
		EmployeeID *uuid.UUID `gorm:"column:employee_id"`
	}
	if err := tx.Raw(
		`SELECT employee_id FROM users WHERE id = ? AND tenant_id = ? LIMIT 1`,
		actorUserID, tenantID,
	).Scan(&row).Error; err != nil {
		return uuid.Nil, fmt.Errorf("oneonone: resolve actor employee: %w", err)
	}
	if row.EmployeeID == nil {
		return uuid.Nil, nil
	}
	return *row.EmployeeID, nil
}

// verifySeriesParticipant enforces the spec's participant scope (上司・部下のみ閲覧可):
// the actor user must map (via users.employee_id) to either the series'
// manager_employee_id or member_employee_id.  This is the application-layer
// participant gate that complements RLS (tenant boundary) and the private-note
// author scope — the missing "participant" layer of defence-in-depth required by
// docs/05 §4 / spec domainLogic.
//
// It is mandatory on every body/detail read path (sessions, agenda, notes,
// actions, series detail).  Non-participants — including unrelated members of the
// same tenant who merely hold oneonone:read — are rejected with ErrForbidden.
// The HR-manager metadata view (GetSeriesMetadata) intentionally does NOT call
// this gate: it exposes meta only (counts / timestamps), never bodies/details.
func verifySeriesParticipant(tx *gorm.DB, ser *Series, actorUserID uuid.UUID) error {
	actorEmp, err := resolveActorEmployeeID(tx, ser.TenantID, actorUserID)
	if err != nil {
		return err
	}
	// Fail-closed: an actor with no linked employee can never be a participant.
	if actorEmp == uuid.Nil {
		return ErrForbidden
	}
	if actorEmp == ser.ManagerEmployeeID || actorEmp == ser.MemberEmployeeID {
		return nil
	}
	return ErrForbidden
}

// ---------------------------------------------------------------------------
// Series
// ---------------------------------------------------------------------------

// CreateSeriesInput holds fields for creating a 1on1 series.
type CreateSeriesInput struct {
	TenantID          uuid.UUID
	ActorID           uuid.UUID
	ManagerEmployeeID uuid.UUID
	MemberEmployeeID  uuid.UUID
	Title             string
	Cadence           string
	IP                *string
}

// CreateSeries creates a new 1on1 series.  Both manager and member must be
// employees of the same tenant (also enforced by the composite FK at the DB
// layer).
func (s *Service) CreateSeries(ctx context.Context, in CreateSeriesInput) (*Series, error) {
	cadence := in.Cadence
	if cadence == "" {
		cadence = CadenceBiweekly
	}
	series := Series{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		ManagerEmployeeID: in.ManagerEmployeeID,
		MemberEmployeeID:  in.MemberEmployeeID,
		Title:             in.Title,
		Cadence:           cadence,
		Status:            SeriesStatusActive,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify both participants belong to this tenant (defence-in-depth;
		// composite FK also enforces this at the DB layer).
		if err := verifyEmployeeInTenant(tx, in.TenantID, in.ManagerEmployeeID); err != nil {
			return err
		}
		if err := verifyEmployeeInTenant(tx, in.TenantID, in.MemberEmployeeID); err != nil {
			return err
		}

		if err := tx.Exec(
			`INSERT INTO one_on_one_series
			   (id, tenant_id, manager_employee_id, member_employee_id, title, cadence, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			series.ID, series.TenantID, series.ManagerEmployeeID, series.MemberEmployeeID,
			series.Title, series.Cadence, series.Status,
		).Error; err != nil {
			return fmt.Errorf("oneonone: create series insert: %w", err)
		}

		idStr := series.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_series.created",
			ResourceType: "one_on_one_series",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &series, nil
}

// ListSeries returns series for a tenant, optionally filtered to those where the
// given employee is the manager or member.  When employeeFilter is uuid.Nil all
// series in the tenant are returned (HR-manager / admin view).
func (s *Service) ListSeries(ctx context.Context, tenantID, employeeFilter uuid.UUID) ([]Series, error) {
	var list []Series
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, manager_employee_id, member_employee_id, title,
		             cadence, status, created_at, updated_at
		      FROM one_on_one_series
		      WHERE tenant_id = ?`
		args := []any{tenantID}
		if employeeFilter != uuid.Nil {
			q += ` AND (manager_employee_id = ? OR member_employee_id = ?)`
			args = append(args, employeeFilter, employeeFilter)
		}
		q += ` ORDER BY created_at DESC`
		return tx.Raw(q, args...).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// GetSeries fetches a single series by ID within the tenant.  Only the series'
// participants (上司・部下) may read it; non-participants get ErrForbidden.
func (s *Service) GetSeries(ctx context.Context, tenantID, actorID, seriesID uuid.UUID) (*Series, error) {
	var out *Series
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ser, err := loadSeriesTx(tx, tenantID, seriesID)
		if err != nil {
			return err
		}
		if err := verifySeriesParticipant(tx, ser, actorID); err != nil {
			return err
		}
		out = ser
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateSeriesManagerInput holds fields for the manager hand-over.
type UpdateSeriesManagerInput struct {
	TenantID             uuid.UUID
	ActorID              uuid.UUID
	SeriesID             uuid.UUID
	NewManagerEmployeeID uuid.UUID
	IP                   *string
}

// UpdateSeriesManager re-points an existing series to a new manager (異動による
// 引き継ぎ).  The series is preserved (its sessions/notes/actions carry over);
// only manager_employee_id changes.  A meta-only audit record is written.
func (s *Service) UpdateSeriesManager(ctx context.Context, in UpdateSeriesManagerInput) (*Series, error) {
	var out Series
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadSeriesTx(tx, in.TenantID, in.SeriesID); err != nil {
			return err
		}
		if err := verifyEmployeeInTenant(tx, in.TenantID, in.NewManagerEmployeeID); err != nil {
			return err
		}

		res := tx.Exec(
			`UPDATE one_on_one_series
			 SET manager_employee_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.NewManagerEmployeeID, in.SeriesID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("oneonone: update series manager: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		ser, err := loadSeriesTx(tx, in.TenantID, in.SeriesID)
		if err != nil {
			return err
		}
		out = *ser

		idStr := in.SeriesID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_series.manager_updated",
			ResourceType: "one_on_one_series",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseSeriesInput holds fields for closing a series.
type CloseSeriesInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	SeriesID uuid.UUID
	IP       *string
}

// CloseSeries marks a series as closed (e.g. relationship ended).
func (s *Service) CloseSeries(ctx context.Context, in CloseSeriesInput) (*Series, error) {
	var out Series
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadSeriesTx(tx, in.TenantID, in.SeriesID); err != nil {
			return err
		}
		res := tx.Exec(
			`UPDATE one_on_one_series
			 SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			SeriesStatusClosed, in.SeriesID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("oneonone: close series: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		ser, err := loadSeriesTx(tx, in.TenantID, in.SeriesID)
		if err != nil {
			return err
		}
		out = *ser

		idStr := in.SeriesID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_series.closed",
			ResourceType: "one_on_one_series",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSessionInput holds fields for creating a session under a series.
type CreateSessionInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	SeriesID    uuid.UUID
	ScheduledAt *time.Time
	HeldAt      *time.Time
	Summary     string
	IP          *string
}

// CreateSession creates a session under a series.  The series must exist in the
// tenant (also enforced by composite FK).
func (s *Service) CreateSession(ctx context.Context, in CreateSessionInput) (*Session, error) {
	session := Session{
		ID:          uuid.New(),
		TenantID:    in.TenantID,
		SeriesID:    in.SeriesID,
		ScheduledAt: in.ScheduledAt,
		HeldAt:      in.HeldAt,
		Status:      SessionStatusScheduled,
		Summary:     in.Summary,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadSeriesTx(tx, in.TenantID, in.SeriesID); err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO one_on_one_sessions
			   (id, tenant_id, series_id, scheduled_at, held_at, status, summary)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			session.ID, session.TenantID, session.SeriesID, session.ScheduledAt,
			session.HeldAt, session.Status, session.Summary,
		).Error; err != nil {
			return fmt.Errorf("oneonone: create session insert: %w", err)
		}

		idStr := session.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_session.created",
			ResourceType: "one_on_one_session",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// ListSessions returns sessions for a series, newest first.  Only the series'
// participants (上司・部下) may list its sessions; non-participants get
// ErrForbidden.
func (s *Service) ListSessions(ctx context.Context, tenantID, actorID, seriesID uuid.UUID) ([]Session, error) {
	var list []Session
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Confirm the series exists in this tenant before listing (returns empty
		// for a series owned by another tenant — RLS already prevents the rows).
		ser, err := loadSeriesTx(tx, tenantID, seriesID)
		if err != nil {
			return err
		}
		// Participant gate: the actor must be the series' manager or member.
		if err := verifySeriesParticipant(tx, ser, actorID); err != nil {
			return err
		}
		return tx.Raw(
			`SELECT id, tenant_id, series_id, scheduled_at, held_at, status, summary,
			        created_at, updated_at
			 FROM one_on_one_sessions
			 WHERE tenant_id = ? AND series_id = ?
			 ORDER BY COALESCE(held_at, scheduled_at, created_at) DESC, created_at DESC`,
			tenantID, seriesID,
		).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// UpdateSessionStatusInput holds fields for a session status transition.
type UpdateSessionStatusInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	SessionID uuid.UUID
	Status    string
	HeldAt    *time.Time // optional; set when marking done
	IP        *string
}

// UpdateSessionStatus transitions a session (scheduled → done / canceled).
// When moving to done and HeldAt is provided it is recorded; when HeldAt is nil
// and the session has no held_at yet, now() is used.
func (s *Service) UpdateSessionStatus(ctx context.Context, in UpdateSessionStatusInput) (*Session, error) {
	var out Session
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		cur, err := loadSessionTx(tx, in.TenantID, in.SessionID)
		if err != nil {
			return err
		}
		if !isSessionTransitionAllowed(cur.Status, in.Status) {
			return fmt.Errorf("%w: session %s → %s", ErrInvalidTransition, cur.Status, in.Status)
		}

		var res *gorm.DB
		if in.Status == SessionStatusDone {
			res = tx.Exec(
				`UPDATE one_on_one_sessions
				 SET status = ?, held_at = COALESCE(?, held_at, now()), updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.HeldAt, in.SessionID, in.TenantID,
			)
		} else {
			res = tx.Exec(
				`UPDATE one_on_one_sessions
				 SET status = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.SessionID, in.TenantID,
			)
		}
		if res.Error != nil {
			return fmt.Errorf("oneonone: update session status: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		updated, err := loadSessionTx(tx, in.TenantID, in.SessionID)
		if err != nil {
			return err
		}
		out = *updated

		idStr := in.SessionID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_session.status_updated",
			ResourceType: "one_on_one_session",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Agenda items
// ---------------------------------------------------------------------------

// AddAgendaItemInput holds fields for adding an agenda item.
type AddAgendaItemInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	SessionID    uuid.UUID
	Topic        string
	AuthorUserID *uuid.UUID
	SortOrder    int
	IP           *string
}

// AddAgendaItem appends an agenda item to a session.
func (s *Service) AddAgendaItem(ctx context.Context, in AddAgendaItemInput) (*AgendaItem, error) {
	item := AgendaItem{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		SessionID:    in.SessionID,
		Topic:        in.Topic,
		AuthorUserID: in.AuthorUserID,
		SortOrder:    in.SortOrder,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadSessionTx(tx, in.TenantID, in.SessionID); err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO one_on_one_agenda_items
			   (id, tenant_id, session_id, topic, author_user_id, sort_order)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			item.ID, item.TenantID, item.SessionID, item.Topic, item.AuthorUserID, item.SortOrder,
		).Error; err != nil {
			return fmt.Errorf("oneonone: add agenda item insert: %w", err)
		}

		idStr := item.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_agenda_item.created",
			ResourceType: "one_on_one_agenda_item",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// ListAgendaItems returns the agenda items of a session ordered by sort_order.
// Only the parent series' participants (上司・部下) may read them; non-participants
// get ErrForbidden.
func (s *Service) ListAgendaItems(ctx context.Context, tenantID, actorID, sessionID uuid.UUID) ([]AgendaItem, error) {
	var list []AgendaItem
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		sess, err := loadSessionTx(tx, tenantID, sessionID)
		if err != nil {
			return err
		}
		// Participant gate via the session's parent series.
		ser, err := loadSeriesTx(tx, tenantID, sess.SeriesID)
		if err != nil {
			return err
		}
		if err := verifySeriesParticipant(tx, ser, actorID); err != nil {
			return err
		}
		return tx.Raw(
			`SELECT id, tenant_id, session_id, topic, author_user_id, sort_order,
			        carried_over_from_id, created_at
			 FROM one_on_one_agenda_items
			 WHERE tenant_id = ? AND session_id = ?
			 ORDER BY sort_order, created_at`,
			tenantID, sessionID,
		).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// CarryOverAgendaInput holds fields for carrying agenda items into a session.
type CarryOverAgendaInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	FromSessionID uuid.UUID
	ToSessionID   uuid.UUID
	IP            *string
}

// CarryOverAgenda copies every agenda item from FromSessionID into ToSessionID,
// tagging each copy with carried_over_from_id pointing at its source.  Both
// sessions must belong to the same series (継続関係) and tenant.
func (s *Service) CarryOverAgenda(ctx context.Context, in CarryOverAgendaInput) ([]AgendaItem, error) {
	var created []AgendaItem
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		from, err := loadSessionTx(tx, in.TenantID, in.FromSessionID)
		if err != nil {
			return err
		}
		to, err := loadSessionTx(tx, in.TenantID, in.ToSessionID)
		if err != nil {
			return err
		}
		// Carry-over only within the same series (継続関係の一貫性).
		if from.SeriesID != to.SeriesID {
			return fmt.Errorf("%w: sessions belong to different series", ErrInvalidTransition)
		}

		var src []AgendaItem
		if err := tx.Raw(
			`SELECT id, tenant_id, session_id, topic, author_user_id, sort_order,
			        carried_over_from_id, created_at
			 FROM one_on_one_agenda_items
			 WHERE tenant_id = ? AND session_id = ?
			 ORDER BY sort_order, created_at`,
			in.TenantID, in.FromSessionID,
		).Scan(&src).Error; err != nil {
			return fmt.Errorf("oneonone: carry over read source: %w", err)
		}

		for _, item := range src {
			srcID := item.ID
			n := AgendaItem{
				ID:                uuid.New(),
				TenantID:          in.TenantID,
				SessionID:         in.ToSessionID,
				Topic:             item.Topic,
				AuthorUserID:      item.AuthorUserID,
				SortOrder:         item.SortOrder,
				CarriedOverFromID: &srcID,
			}
			if err := tx.Exec(
				`INSERT INTO one_on_one_agenda_items
				   (id, tenant_id, session_id, topic, author_user_id, sort_order, carried_over_from_id)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				n.ID, n.TenantID, n.SessionID, n.Topic, n.AuthorUserID, n.SortOrder, n.CarriedOverFromID,
			).Error; err != nil {
				return fmt.Errorf("oneonone: carry over insert: %w", err)
			}
			created = append(created, n)
		}

		idStr := in.ToSessionID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_agenda.carried_over",
			ResourceType: "one_on_one_session",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ---------------------------------------------------------------------------
// Notes (shared / private visibility)
// ---------------------------------------------------------------------------

// AddNoteInput holds fields for adding a session note.
type AddNoteInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	SessionID    uuid.UUID
	AuthorUserID uuid.UUID
	Visibility   string
	Body         string
	IP           *string
}

// AddNote stores a note for a session.  Visibility must be shared or private.
// The audit record carries only the opaque note ID — never the body.
func (s *Service) AddNote(ctx context.Context, in AddNoteInput) (*Note, error) {
	visibility := in.Visibility
	if visibility == "" {
		visibility = VisibilityShared
	}
	if visibility != VisibilityShared && visibility != VisibilityPrivate {
		return nil, fmt.Errorf("%w: unknown visibility %q", ErrInvalidTransition, visibility)
	}
	note := Note{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		SessionID:    in.SessionID,
		AuthorUserID: in.AuthorUserID,
		Visibility:   visibility,
		Body:         in.Body,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadSessionTx(tx, in.TenantID, in.SessionID); err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO one_on_one_notes
			   (id, tenant_id, session_id, author_user_id, visibility, body)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			note.ID, note.TenantID, note.SessionID, note.AuthorUserID, note.Visibility, note.Body,
		).Error; err != nil {
			return fmt.Errorf("oneonone: add note insert: %w", err)
		}

		// Audit: meta only — never the note body (sensitive dialogue).
		idStr := note.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_note.created",
			ResourceType: "one_on_one_note",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &note, nil
}

// ListNotesInput holds fields for reading session notes with visibility scope.
type ListNotesInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	SessionID uuid.UUID
	// ViewerUserID is the user whose private notes may be returned.  Shared
	// notes are returned for any caller; private notes are returned only when
	// author_user_id = ViewerUserID.
	ViewerUserID uuid.UUID
	IP           *string
}

// ListNotes returns notes for a session enforcing the visibility scope at the
// query layer (defence-in-depth): shared notes for everyone, private notes only
// for their author (ViewerUserID).  This guarantees a private note body is never
// returned to the other participant or to an HR manager via this path.
func (s *Service) ListNotes(ctx context.Context, in ListNotesInput) ([]Note, error) {
	var list []Note
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		sess, err := loadSessionTx(tx, in.TenantID, in.SessionID)
		if err != nil {
			return err
		}
		// Participant gate: only the parent series' manager/member may read its
		// notes at all.  Combined with the visibility predicate below, a private
		// note remains visible to its author alone; a shared note is visible to
		// both participants but never to a non-participant. ViewerUserID is the
		// identity whose access is being authorized (handler sets it to the
		// authenticated user).
		ser, err := loadSeriesTx(tx, in.TenantID, sess.SeriesID)
		if err != nil {
			return err
		}
		if err := verifySeriesParticipant(tx, ser, in.ViewerUserID); err != nil {
			return err
		}
		// Mandatory visibility predicate — never relaxed.
		if err := tx.Raw(
			`SELECT id, tenant_id, session_id, author_user_id, visibility, body,
			        created_at, updated_at
			 FROM one_on_one_notes
			 WHERE tenant_id = ? AND session_id = ?
			   AND (visibility = 'shared'
			        OR (visibility = 'private' AND author_user_id = ?))
			 ORDER BY created_at`,
			in.TenantID, in.SessionID, in.ViewerUserID,
		).Scan(&list).Error; err != nil {
			return fmt.Errorf("oneonone: list notes: %w", err)
		}

		// Audit: meta only — record that the viewer accessed this session's
		// notes; never store any body.
		idStr := in.SessionID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_note.read",
			ResourceType: "one_on_one_session",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// ---------------------------------------------------------------------------
// Actions (next actions / follow-ups)
// ---------------------------------------------------------------------------

// AddActionInput holds fields for adding a next action.
type AddActionInput struct {
	TenantID           uuid.UUID
	ActorID            uuid.UUID
	SessionID          uuid.UUID
	AssigneeEmployeeID uuid.UUID
	Description        string
	DueDate            *time.Time
	IP                 *string
}

// AddAction creates a next action under a session.  The assignee must be an
// employee of the tenant (also enforced by composite FK).
func (s *Service) AddAction(ctx context.Context, in AddActionInput) (*Action, error) {
	action := Action{
		ID:                 uuid.New(),
		TenantID:           in.TenantID,
		SessionID:          in.SessionID,
		AssigneeEmployeeID: in.AssigneeEmployeeID,
		Description:        in.Description,
		DueDate:            in.DueDate,
		Status:             ActionStatusOpen,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if _, err := loadSessionTx(tx, in.TenantID, in.SessionID); err != nil {
			return err
		}
		if err := verifyEmployeeInTenant(tx, in.TenantID, in.AssigneeEmployeeID); err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO one_on_one_actions
			   (id, tenant_id, session_id, assignee_employee_id, description, due_date, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			action.ID, action.TenantID, action.SessionID, action.AssigneeEmployeeID,
			action.Description, action.DueDate, action.Status,
		).Error; err != nil {
			return fmt.Errorf("oneonone: add action insert: %w", err)
		}

		idStr := action.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_action.created",
			ResourceType: "one_on_one_action",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &action, nil
}

// UpdateActionStatusInput holds fields for an action status transition.
type UpdateActionStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ActionID uuid.UUID
	Status   string
	IP       *string
}

// UpdateActionStatus transitions an action (open → done / canceled).  Moving to
// done sets completed_at = now().
func (s *Service) UpdateActionStatus(ctx context.Context, in UpdateActionStatusInput) (*Action, error) {
	var out Action
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cur struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM one_on_one_actions WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ActionID, in.TenantID,
		).Scan(&cur).Error; err != nil {
			return fmt.Errorf("oneonone: action status read: %w", err)
		}
		if cur.Status == "" {
			return ErrNotFound
		}
		if !isActionTransitionAllowed(cur.Status, in.Status) {
			return fmt.Errorf("%w: action %s → %s", ErrInvalidTransition, cur.Status, in.Status)
		}

		var res *gorm.DB
		if in.Status == ActionStatusDone {
			res = tx.Exec(
				`UPDATE one_on_one_actions
				 SET status = ?, completed_at = now(), updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.ActionID, in.TenantID,
			)
		} else {
			res = tx.Exec(
				`UPDATE one_on_one_actions
				 SET status = ?, completed_at = NULL, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.ActionID, in.TenantID,
			)
		}
		if res.Error != nil {
			return fmt.Errorf("oneonone: action status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, session_id, assignee_employee_id, description,
			        due_date, status, completed_at, created_at, updated_at
			 FROM one_on_one_actions WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ActionID, in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("oneonone: action status re-read: %w", err)
		}

		idStr := in.ActionID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "oneonone_action.status_updated",
			ResourceType: "one_on_one_action",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListOpenActionsForSeries returns all open actions across every session of a
// series (carried forward to the latest session view).  This drives the
// "未完了アクションの引き継ぎ" display.  Only the series' participants (上司・部下)
// may read it; non-participants get ErrForbidden.
func (s *Service) ListOpenActionsForSeries(ctx context.Context, tenantID, actorID, seriesID uuid.UUID) ([]Action, error) {
	var list []Action
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ser, err := loadSeriesTx(tx, tenantID, seriesID)
		if err != nil {
			return err
		}
		// Participant gate: only the series' manager/member see its open actions.
		if err := verifySeriesParticipant(tx, ser, actorID); err != nil {
			return err
		}
		return tx.Raw(
			`SELECT a.id, a.tenant_id, a.session_id, a.assignee_employee_id, a.description,
			        a.due_date, a.status, a.completed_at, a.created_at, a.updated_at
			 FROM one_on_one_actions a
			 JOIN one_on_one_sessions s
			   ON s.id = a.session_id AND s.tenant_id = a.tenant_id
			 WHERE a.tenant_id = ? AND s.series_id = ? AND a.status = 'open'
			 ORDER BY a.due_date NULLS LAST, a.created_at`,
			tenantID, seriesID,
		).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// ListOverdueActions returns open actions whose due_date is on or before asOf,
// across the whole tenant.  This drives due-date reminder alerts (the
// notification trigger is a hook; ST-FND-09 未実装のためフックのみ).
func (s *Service) ListOverdueActions(ctx context.Context, tenantID uuid.UUID, asOf time.Time) ([]Action, error) {
	var list []Action
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, session_id, assignee_employee_id, description,
			        due_date, status, completed_at, created_at, updated_at
			 FROM one_on_one_actions
			 WHERE tenant_id = ? AND status = 'open'
			   AND due_date IS NOT NULL AND due_date <= ?
			 ORDER BY due_date, created_at`,
			tenantID, asOf,
		).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// ---------------------------------------------------------------------------
// HR-manager metadata view (meta only — never note bodies)
// ---------------------------------------------------------------------------

// SeriesMetadata aggregates non-body meta for a series for the HR-manager view.
// It exposes実施有無/頻度などのメタ情報 only; bodies are never read here.
type SeriesMetadata struct {
	Series          Series
	SessionCount    int64
	LastHeldAt      *time.Time
	OpenActionCount int64
}

// GetSeriesMetadata returns aggregate meta (session count, last held date, open
// action count) for a series WITHOUT reading any note body.  This is the
// HR-manager metadata view: it lets HR see実施頻度/未完了件数 without exposing
// shared or private note content.
//
// Note: even when tm_settings.hr_manager_body_disclosure is true, private notes
// are NEVER disclosed; only shared note disclosure may be enabled by config, and
// that disclosure path is intentionally NOT part of this meta view.
func (s *Service) GetSeriesMetadata(ctx context.Context, tenantID, seriesID uuid.UUID) (*SeriesMetadata, error) {
	var meta SeriesMetadata
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ser, err := loadSeriesTx(tx, tenantID, seriesID)
		if err != nil {
			return err
		}
		meta.Series = *ser

		var agg struct {
			SessionCount int64      `gorm:"column:session_count"`
			LastHeldAt   *time.Time `gorm:"column:last_held_at"`
		}
		if err := tx.Raw(
			`SELECT COUNT(1) AS session_count, MAX(held_at) AS last_held_at
			 FROM one_on_one_sessions
			 WHERE tenant_id = ? AND series_id = ?`,
			tenantID, seriesID,
		).Scan(&agg).Error; err != nil {
			return fmt.Errorf("oneonone: series metadata sessions: %w", err)
		}
		meta.SessionCount = agg.SessionCount
		meta.LastHeldAt = agg.LastHeldAt

		var openCount int64
		if err := tx.Raw(
			`SELECT COUNT(1)
			 FROM one_on_one_actions a
			 JOIN one_on_one_sessions s
			   ON s.id = a.session_id AND s.tenant_id = a.tenant_id
			 WHERE a.tenant_id = ? AND s.series_id = ? AND a.status = 'open'`,
			tenantID, seriesID,
		).Scan(&openCount).Error; err != nil {
			return fmt.Errorf("oneonone: series metadata open actions: %w", err)
		}
		meta.OpenActionCount = openCount
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

// ---------------------------------------------------------------------------
// TM settings (configurable disclosure / retention)
// ---------------------------------------------------------------------------

// GetSettings returns the tenant's TM settings, or a default (all-minimal)
// settings object when none has been configured yet.
//
// Defaults follow最小アクセス (CMP-004): hr_manager_body_disclosure = false.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*Settings, error) {
	var st Settings
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, hr_manager_body_disclosure, note_retention_days,
			        created_at, updated_at
			 FROM tm_settings WHERE tenant_id = ? LIMIT 1`,
			tenantID,
		).Scan(&st).Error
	})
	if err != nil {
		return nil, err
	}
	if st.ID == uuid.Nil {
		// Not configured yet — return safe defaults (minimal access).
		return &Settings{
			TenantID:                tenantID,
			HRManagerBodyDisclosure: false,
		}, nil
	}
	return &st, nil
}

// UpsertSettingsInput holds fields for configuring TM settings.
type UpsertSettingsInput struct {
	TenantID                uuid.UUID
	ActorID                 uuid.UUID
	HRManagerBodyDisclosure bool
	NoteRetentionDays       *int
	IP                      *string
}

// UpsertSettings creates or updates the tenant's TM settings.
//
// Legal/config note: values here are社内規程依存 (not 法令値).  hr_manager_body_
// disclosure governs whether shared note bodies may be disclosed to HR managers;
// private notes are never disclosed regardless.  Confirm policy with社労士/弁護士;
// this implementation is not legal advice.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*Settings, error) {
	var out Settings
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO tm_settings
			   (id, tenant_id, hr_manager_body_disclosure, note_retention_days)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET hr_manager_body_disclosure = EXCLUDED.hr_manager_body_disclosure,
			       note_retention_days        = EXCLUDED.note_retention_days,
			       updated_at                 = now()`,
			id, in.TenantID, in.HRManagerBodyDisclosure, in.NoteRetentionDays,
		).Error; err != nil {
			return fmt.Errorf("oneonone: upsert settings: %w", err)
		}
		if err := tx.Raw(
			`SELECT id, tenant_id, hr_manager_body_disclosure, note_retention_days,
			        created_at, updated_at
			 FROM tm_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("oneonone: upsert settings re-read: %w", err)
		}

		idStr := out.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "tm_settings.updated",
			ResourceType: "tm_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
