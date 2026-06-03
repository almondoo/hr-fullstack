package interview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("interview: not found")
	ErrInvalidTransition = errors.New("interview: invalid status transition")
	ErrForbidden         = errors.New("interview: permission denied")
	ErrInvalidInput      = errors.New("interview: invalid input")
	ErrAlreadyExists     = errors.New("interview: record already exists")
)

// Permission strings used for service-layer (multi-layer) RBAC re-checks.
const (
	permEvaluationRead = "ats:evaluation:read"
)

// allowedInterviewTransitions defines legal interview status moves.
// Terminal states: completed, cancelled — no transitions out.
var allowedInterviewTransitions = map[string]map[string]bool{
	StatusProposed: {
		StatusConfirmed: true,
		StatusCancelled: true,
	},
	StatusConfirmed: {
		StatusCompleted: true,
		StatusCancelled: true,
	},
}

// isInterviewTransitionAllowed reports whether moving current → next is valid.
func isInterviewTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedInterviewTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// Reminder is the remind-notification hook for confirmed interviews.
//
// Calendar linkage (INT-005) and remind delivery are delegated to the
// notification platform.  This interface decouples the interview domain from
// that platform so the MVP can run with a log-only implementation; a failure
// to enqueue a reminder MUST NOT block interview confirmation (best-effort).
type Reminder interface {
	// Remind is called after an interview is confirmed.  Implementations must
	// be side-effect-only; the returned error is logged but does not roll back
	// the interview transaction.
	Remind(ctx context.Context, ev ReminderEvent) error
}

// ReminderEvent carries only opaque identifiers — never candidate PII.
type ReminderEvent struct {
	TenantID    uuid.UUID
	InterviewID uuid.UUID
	ScheduledAt time.Time
}

// logReminder is the default log-only Reminder used when none is injected.
// It never carries PII (only opaque interview / tenant ids).
type logReminder struct{}

// Remind logs the reminder event without sending external notifications.
func (logReminder) Remind(_ context.Context, ev ReminderEvent) error {
	slog.Info("interview.reminder.enqueued",
		"tenant_id", ev.TenantID.String(),
		"interview_id", ev.InterviewID.String(),
		"scheduled_at", ev.ScheduledAt.UTC().Format(time.RFC3339),
	)
	return nil
}

// Service provides business logic for interview scheduling and evaluation.
type Service struct {
	tdb      *tenantdb.TenantDB
	reminder Reminder
}

// NewService constructs a Service with the default log-only reminder.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb, reminder: logReminder{}}
}

// WithReminder returns a copy of the service using the supplied reminder hook.
// Used by tests and by wiring that injects the real notification adapter.
func (s *Service) WithReminder(r Reminder) *Service {
	if r == nil {
		r = logReminder{}
	}
	return &Service{tdb: s.tdb, reminder: r}
}

// ---------------------------------------------------------------------------
// Interviews
// ---------------------------------------------------------------------------

// CreateInterviewInput holds fields for creating an interview.
type CreateInterviewInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	ApplicationID uuid.UUID
	Format        string
	OnlineURL     *string
	Notes         *string
	IP            *string
}

// CreateInterview creates a new interview in the 'proposed' state.
func (s *Service) CreateInterview(ctx context.Context, in CreateInterviewInput) (*Interview, error) {
	if in.Format == "" {
		in.Format = FormatOnsite
	}
	if in.Format != FormatOnsite && in.Format != FormatOnline && in.Format != FormatPhone {
		return nil, fmt.Errorf("%w: format %q", ErrInvalidInput, in.Format)
	}

	iv := Interview{
		ID:            uuid.New(),
		TenantID:      in.TenantID,
		ApplicationID: in.ApplicationID,
		Status:        StatusProposed,
		Format:        in.Format,
		OnlineURL:     in.OnlineURL,
		Notes:         in.Notes,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO interviews
			   (id, tenant_id, application_id, status, format, online_url, notes)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			iv.ID, iv.TenantID, iv.ApplicationID, iv.Status, iv.Format, iv.OnlineURL, iv.Notes,
		).Error; err != nil {
			return fmt.Errorf("interview: create insert: %w", err)
		}
		idStr := iv.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview.created",
			ResourceType: "interview",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &iv, nil
}

// GetInterview fetches an interview by ID within the tenant.
func (s *Service) GetInterview(ctx context.Context, tenantID, id uuid.UUID) (*Interview, error) {
	var iv Interview
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, application_id, status, format, scheduled_at,
			        online_url, external_event_id, notes, created_at, updated_at
			 FROM interviews WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&iv).Error
	})
	if err != nil {
		return nil, err
	}
	if iv.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &iv, nil
}

// ListInterviews returns interviews for an application (or all in the tenant
// when applicationID is uuid.Nil).
func (s *Service) ListInterviews(ctx context.Context, tenantID, applicationID uuid.UUID) ([]Interview, error) {
	var list []Interview
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, application_id, status, format, scheduled_at,
		             online_url, external_event_id, notes, created_at, updated_at
		      FROM interviews WHERE tenant_id = ?`
		args := []any{tenantID}
		if applicationID != uuid.Nil {
			q += ` AND application_id = ?`
			args = append(args, applicationID)
		}
		q += ` ORDER BY created_at`
		return tx.Raw(q, args...).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// AddSlotInput holds fields for proposing a candidate slot.
type AddSlotInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	InterviewID    uuid.UUID
	CandidateStart time.Time
	CandidateEnd   *time.Time
	IP             *string
}

// AddSlot proposes a candidate slot for an interview.  Only interviews in the
// 'proposed' state may receive new slots.
func (s *Service) AddSlot(ctx context.Context, in AddSlotInput) (*Slot, error) {
	slot := Slot{
		ID:             uuid.New(),
		TenantID:       in.TenantID,
		InterviewID:    in.InterviewID,
		CandidateStart: in.CandidateStart,
		CandidateEnd:   in.CandidateEnd,
		Selected:       false,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the interview exists and is in 'proposed' state.
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM interviews WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.InterviewID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("interview: add slot read interview: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if current.Status != StatusProposed {
			return fmt.Errorf("%w: slots can only be added while proposed (status=%s)",
				ErrInvalidTransition, current.Status)
		}

		if err := tx.Exec(
			`INSERT INTO interview_slots
			   (id, tenant_id, interview_id, candidate_start, candidate_end, selected)
			 VALUES (?, ?, ?, ?, ?, false)`,
			slot.ID, slot.TenantID, slot.InterviewID, slot.CandidateStart, slot.CandidateEnd,
		).Error; err != nil {
			return fmt.Errorf("interview: add slot insert: %w", err)
		}
		idStr := slot.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview_slot.proposed",
			ResourceType: "interview_slot",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &slot, nil
}

// ListSlots returns the candidate slots of an interview.
func (s *Service) ListSlots(ctx context.Context, tenantID, interviewID uuid.UUID) ([]Slot, error) {
	var list []Slot
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, interview_id, candidate_start, candidate_end,
			        selected, created_at, updated_at
			 FROM interview_slots
			 WHERE tenant_id = ? AND interview_id = ?
			 ORDER BY candidate_start`,
			tenantID, interviewID,
		).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// AddPanelistInput holds fields for assigning a panellist.
type AddPanelistInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	InterviewID uuid.UUID
	UserID      uuid.UUID
	Role        string
	IP          *string
}

// AddPanelist assigns a panellist (interviewer/observer) to an interview.
// The user must belong to the same tenant (verified in the service layer
// because users has no UNIQUE(id, tenant_id) for a composite FK).
func (s *Service) AddPanelist(ctx context.Context, in AddPanelistInput) (*Panellist, error) {
	if in.Role == "" {
		in.Role = RoleInterviewer
	}
	if in.Role != RoleInterviewer && in.Role != RoleObserver {
		return nil, fmt.Errorf("%w: role %q", ErrInvalidInput, in.Role)
	}

	p := Panellist{
		ID:          uuid.New(),
		TenantID:    in.TenantID,
		InterviewID: in.InterviewID,
		UserID:      in.UserID,
		Role:        in.Role,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the interview exists in this tenant.
		var ivCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM interviews WHERE id = ? AND tenant_id = ?`,
			in.InterviewID, in.TenantID,
		).Scan(&ivCount).Error; err != nil {
			return fmt.Errorf("interview: add panellist verify interview: %w", err)
		}
		if ivCount == 0 {
			return ErrNotFound
		}

		// Verify the panellist user belongs to this tenant.
		var userCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
			in.UserID, in.TenantID,
		).Scan(&userCount).Error; err != nil {
			return fmt.Errorf("interview: add panellist verify user: %w", err)
		}
		if userCount == 0 {
			return ErrNotFound
		}

		const sqlInsertPanelist = "INSERT INTO interview_panelists" + //nolint:misspell // DB table name is schema contract; US spelling kept to match migration
			" (id, tenant_id, interview_id, user_id, role) VALUES (?, ?, ?, ?, ?)" +
			" ON CONFLICT (tenant_id, interview_id, user_id) DO NOTHING"
		res := tx.Exec(sqlInsertPanelist,
			p.ID, p.TenantID, p.InterviewID, p.UserID, p.Role,
		)
		if res.Error != nil {
			return fmt.Errorf("interview: add panellist insert: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrAlreadyExists
		}

		idStr := p.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview_panelist.assigned", //nolint:misspell // audit action name is an external API contract; cannot change to UK spelling
			ResourceType: "interview_panelist",          //nolint:misspell // audit resource type is an external API contract; cannot change to UK spelling
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPanelists returns the panellists assigned to an interview.
func (s *Service) ListPanelists(ctx context.Context, tenantID, interviewID uuid.UUID) ([]Panellist, error) {
	var list []Panellist
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		const sqlListPanelists = "SELECT id, tenant_id, interview_id, user_id, role, created_at, updated_at" +
			" FROM interview_panelists WHERE tenant_id = ? AND interview_id = ? ORDER BY created_at" //nolint:misspell // DB table name is schema contract; US spelling kept to match migration
		return tx.Raw(sqlListPanelists, tenantID, interviewID).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// ConfirmInterviewInput holds fields for confirming an interview.
type ConfirmInterviewInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	InterviewID uuid.UUID
	SlotID      uuid.UUID
	IP          *string
}

// ConfirmInterview confirms an interview by selecting one of its candidate
// slots.  The selected slot's start time is written to interviews.scheduled_at
// in the SAME transaction so that the confirmed slot and scheduled_at stay
// consistent.  The remind hook is invoked best-effort AFTER commit; a remind
// failure does not roll back the confirmation.
func (s *Service) ConfirmInterview(ctx context.Context, in ConfirmInterviewInput) (*Interview, error) {
	var iv Interview
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Read & lock the interview row to avoid TOCTOU on the status check.
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM interviews
			 WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.InterviewID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("interview: confirm read interview: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isInterviewTransitionAllowed(current.Status, StatusConfirmed) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, StatusConfirmed)
		}

		// Fetch the selected slot (must belong to this interview/tenant).
		var slot Slot
		if err := tx.Raw(
			`SELECT id, tenant_id, interview_id, candidate_start, candidate_end, selected
			 FROM interview_slots
			 WHERE id = ? AND interview_id = ? AND tenant_id = ? LIMIT 1`,
			in.SlotID, in.InterviewID, in.TenantID,
		).Scan(&slot).Error; err != nil {
			return fmt.Errorf("interview: confirm read slot: %w", err)
		}
		if slot.ID == uuid.Nil {
			return ErrNotFound
		}

		// Mark all slots unselected, then select the chosen one (single tx).
		if err := tx.Exec(
			`UPDATE interview_slots SET selected = false, updated_at = now()
			 WHERE interview_id = ? AND tenant_id = ?`,
			in.InterviewID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("interview: confirm clear slots: %w", err)
		}
		if err := tx.Exec(
			`UPDATE interview_slots SET selected = true, updated_at = now()
			 WHERE id = ? AND interview_id = ? AND tenant_id = ?`,
			in.SlotID, in.InterviewID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("interview: confirm select slot: %w", err)
		}

		// Sync scheduled_at with the selected slot's start time.
		res := tx.Exec(
			`UPDATE interviews
			 SET status = ?, scheduled_at = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			StatusConfirmed, slot.CandidateStart, in.InterviewID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("interview: confirm update interview: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, format, scheduled_at,
			        online_url, external_event_id, notes, created_at, updated_at
			 FROM interviews WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.InterviewID, in.TenantID,
		).Scan(&iv).Error; err != nil {
			return fmt.Errorf("interview: confirm re-read: %w", err)
		}

		idStr := in.InterviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview.confirmed",
			ResourceType: "interview",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}

	// Best-effort remind hook: a failure here must NOT undo the confirmation.
	if iv.ScheduledAt != nil {
		if rerr := s.reminder.Remind(ctx, ReminderEvent{
			TenantID:    iv.TenantID,
			InterviewID: iv.ID,
			ScheduledAt: *iv.ScheduledAt,
		}); rerr != nil {
			slog.Warn("interview.reminder.failed",
				"tenant_id", iv.TenantID.String(),
				"interview_id", iv.ID.String(),
				"error", rerr.Error(),
			)
		}
	}
	return &iv, nil
}

// TransitionInterviewInput holds fields for a non-confirm status transition
// (complete / cancel).
type TransitionInterviewInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	InterviewID uuid.UUID
	Status      string
	IP          *string
}

// TransitionInterview moves an interview to 'completed' or 'cancelled'.
// 'confirmed' is reached only via ConfirmInterview (slot selection required).
func (s *Service) TransitionInterview(ctx context.Context, in TransitionInterviewInput) (*Interview, error) {
	if in.Status != StatusCompleted && in.Status != StatusCancelled {
		return nil, fmt.Errorf("%w: use ConfirmInterview for confirmed; got %q", ErrInvalidInput, in.Status)
	}
	var iv Interview
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM interviews
			 WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.InterviewID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("interview: transition read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isInterviewTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		res := tx.Exec(
			`UPDATE interviews SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.InterviewID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("interview: transition update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, format, scheduled_at,
			        online_url, external_event_id, notes, created_at, updated_at
			 FROM interviews WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.InterviewID, in.TenantID,
		).Scan(&iv).Error; err != nil {
			return fmt.Errorf("interview: transition re-read: %w", err)
		}

		idStr := in.InterviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview." + in.Status,
			ResourceType: "interview",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &iv, nil
}

// SetExternalEventInput holds fields for recording a calendar event id.
type SetExternalEventInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	InterviewID     uuid.UUID
	ExternalEventID *string
	IP              *string
}

// SetExternalEvent records the opaque external calendar event id (INT-005).
// This is a best-effort calendar-sync side effect: it only updates the opaque
// column and never blocks other interview operations.  Passing a nil/empty id
// clears the link (e.g. after a failed sync that must be retried).
func (s *Service) SetExternalEvent(ctx context.Context, in SetExternalEventInput) (*Interview, error) {
	var iv Interview
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE interviews SET external_event_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.ExternalEventID, in.InterviewID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("interview: set external event update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, format, scheduled_at,
			        online_url, external_event_id, notes, created_at, updated_at
			 FROM interviews WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.InterviewID, in.TenantID,
		).Scan(&iv).Error; err != nil {
			return fmt.Errorf("interview: set external event re-read: %w", err)
		}

		idStr := in.InterviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview.external_event_set",
			ResourceType: "interview",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &iv, nil
}

// ---------------------------------------------------------------------------
// Evaluation sheets & tenant settings
// ---------------------------------------------------------------------------

// CreateSheetInput holds fields for creating an evaluation sheet.
type CreateSheetInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	Name      string
	ItemsJSON []byte
	IP        *string
}

// CreateSheet creates a tenant evaluation sheet template.
func (s *Service) CreateSheet(ctx context.Context, in CreateSheetInput) (*EvaluationSheet, error) {
	if len(in.ItemsJSON) == 0 {
		in.ItemsJSON = []byte(`[]`)
	}
	sheet := EvaluationSheet{
		ID:        uuid.New(),
		TenantID:  in.TenantID,
		Name:      in.Name,
		ItemsJSON: in.ItemsJSON,
		Active:    true,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO evaluation_sheets (id, tenant_id, name, items_json, active)
			 VALUES (?, ?, ?, ?::jsonb, true)`,
			sheet.ID, sheet.TenantID, sheet.Name, sheet.ItemsJSON,
		).Error; err != nil {
			return fmt.Errorf("interview: create sheet insert: %w", err)
		}
		idStr := sheet.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "evaluation_sheet.created",
			ResourceType: "evaluation_sheet",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sheet, nil
}

// ListSheets returns the active evaluation sheets for a tenant.
func (s *Service) ListSheets(ctx context.Context, tenantID uuid.UUID) ([]EvaluationSheet, error) {
	var list []EvaluationSheet
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, name, items_json, active, created_at, updated_at
			 FROM evaluation_sheets
			 WHERE tenant_id = ? AND active = true
			 ORDER BY name`,
			tenantID,
		).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// SetPeerEvalVisibilityInput toggles the peer-evaluation visibility setting.
type SetPeerEvalVisibilityInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	Visible  bool
	IP       *string
}

// SetPeerEvalVisibility upserts the per-tenant peer-evaluation visibility flag.
func (s *Service) SetPeerEvalVisibility(ctx context.Context, in SetPeerEvalVisibilityInput) (*TenantSettings, error) {
	var settings TenantSettings
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO tenant_interview_settings (tenant_id, peer_eval_visible)
			 VALUES (?, ?)
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET peer_eval_visible = EXCLUDED.peer_eval_visible,
			       updated_at        = now()`,
			in.TenantID, in.Visible,
		).Error; err != nil {
			return fmt.Errorf("interview: set peer eval visibility upsert: %w", err)
		}
		if err := tx.Raw(
			`SELECT tenant_id, peer_eval_visible, created_at, updated_at
			 FROM tenant_interview_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&settings).Error; err != nil {
			return fmt.Errorf("interview: set peer eval visibility re-read: %w", err)
		}
		idStr := in.TenantID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview_settings.peer_eval_visibility_set",
			ResourceType: "tenant_interview_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

// peerEvalVisible reads the tenant's peer-evaluation visibility flag within tx.
// Defaults to false (independent evaluation) when no settings row exists.
func peerEvalVisible(tx *gorm.DB, tenantID uuid.UUID) (bool, error) {
	var row struct {
		PeerEvalVisible *bool `gorm:"column:peer_eval_visible"`
	}
	if err := tx.Raw(
		`SELECT peer_eval_visible FROM tenant_interview_settings
		 WHERE tenant_id = ? LIMIT 1`,
		tenantID,
	).Scan(&row).Error; err != nil {
		return false, fmt.Errorf("interview: read peer eval setting: %w", err)
	}
	if row.PeerEvalVisible == nil {
		return false, nil
	}
	return *row.PeerEvalVisible, nil
}

// ---------------------------------------------------------------------------
// Evaluations
// ---------------------------------------------------------------------------

// SubmitEvaluationInput holds fields for submitting/updating an evaluation.
type SubmitEvaluationInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	InterviewID     uuid.UUID
	EvaluatorUserID uuid.UUID
	SheetID         uuid.UUID
	ScoresJSON      []byte
	OverallScore    *float64
	Recommendation  string
	Comment         string
	IP              *string
}

var validRecommendations = map[string]bool{
	RecStrongYes: true, RecYes: true, RecNeutral: true, RecNo: true, RecStrongNo: true,
}

// SubmitEvaluation records a panelist's evaluation for an interview.
// One evaluation per (interview, evaluator) is enforced; re-submission by the
// same evaluator updates the existing row (upsert).  The application_id is
// derived from the interview within the same transaction so it stays
// consistent with the interview's application.
func (s *Service) SubmitEvaluation(ctx context.Context, in SubmitEvaluationInput) (*Evaluation, error) {
	if in.Recommendation == "" {
		in.Recommendation = RecNeutral
	}
	if !validRecommendations[in.Recommendation] {
		return nil, fmt.Errorf("%w: recommendation %q", ErrInvalidInput, in.Recommendation)
	}
	if len(in.ScoresJSON) == 0 {
		in.ScoresJSON = []byte(`{}`)
	}

	var ev Evaluation
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Fetch interview to derive application_id and verify tenant scope.
		var iv struct {
			ApplicationID uuid.UUID `gorm:"column:application_id"`
		}
		if err := tx.Raw(
			`SELECT application_id FROM interviews
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.InterviewID, in.TenantID,
		).Scan(&iv).Error; err != nil {
			return fmt.Errorf("interview: submit eval read interview: %w", err)
		}
		if iv.ApplicationID == uuid.Nil {
			return ErrNotFound
		}

		// Verify the evaluator belongs to this tenant.
		var userCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
			in.EvaluatorUserID, in.TenantID,
		).Scan(&userCount).Error; err != nil {
			return fmt.Errorf("interview: submit eval verify user: %w", err)
		}
		if userCount == 0 {
			return ErrNotFound
		}

		// Verify the sheet exists in this tenant (also enforced by composite FK).
		var sheetCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM evaluation_sheets WHERE id = ? AND tenant_id = ?`,
			in.SheetID, in.TenantID,
		).Scan(&sheetCount).Error; err != nil {
			return fmt.Errorf("interview: submit eval verify sheet: %w", err)
		}
		if sheetCount == 0 {
			return ErrNotFound
		}

		newID := uuid.New()
		if err := tx.Exec(
			`INSERT INTO interview_evaluations
			   (id, tenant_id, interview_id, application_id, evaluator_user_id,
			    sheet_id, scores_json, overall_score, recommendation, comment)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?)
			 ON CONFLICT (tenant_id, interview_id, evaluator_user_id) DO UPDATE
			   SET sheet_id       = EXCLUDED.sheet_id,
			       scores_json    = EXCLUDED.scores_json,
			       overall_score  = EXCLUDED.overall_score,
			       recommendation = EXCLUDED.recommendation,
			       comment        = EXCLUDED.comment,
			       updated_at     = now()`,
			newID, in.TenantID, in.InterviewID, iv.ApplicationID, in.EvaluatorUserID,
			in.SheetID, in.ScoresJSON, in.OverallScore, in.Recommendation, in.Comment,
		).Error; err != nil {
			return fmt.Errorf("interview: submit eval upsert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, interview_id, application_id, evaluator_user_id,
			        sheet_id, scores_json, overall_score, recommendation, comment,
			        created_at, updated_at
			 FROM interview_evaluations
			 WHERE tenant_id = ? AND interview_id = ? AND evaluator_user_id = ? LIMIT 1`,
			in.TenantID, in.InterviewID, in.EvaluatorUserID,
		).Scan(&ev).Error; err != nil {
			return fmt.Errorf("interview: submit eval re-read: %w", err)
		}

		// Audit records only the opaque evaluation id — never the comment text.
		idStr := ev.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "interview_evaluation.submitted",
			ResourceType: "interview_evaluation",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// ListEvaluationsInput holds parameters for listing evaluations of an interview.
//
// Masking rules (independent-evaluation bias control):
//   - The actor always sees their OWN evaluation in full.
//   - For OTHER panelists' evaluations: when the tenant's peer_eval_visible is
//     false, the comment is masked (empty) and scores are hidden.  When true,
//     all evaluations are returned in full.
//   - Reading other panelists' sensitive comments requires the
//     ats:evaluation:read permission AND peer_eval_visible=true.  The service
//     re-validates this permission (multi-layer defence) before unmasking.
type ListEvaluationsInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	InterviewID uuid.UUID
	// CanReadEvaluations is set by the HTTP layer (RequirePermission for
	// ats:evaluation:read).  The service re-validates it for defence-in-depth.
	CanReadEvaluations bool
	IP                 *string
}

// ListEvaluations returns the evaluations for an interview with peer-visibility
// masking applied.  The sensitive read (other panelists' comments/scores) is
// recorded in the audit log when the actor is granted unmasked access.
func (s *Service) ListEvaluations(ctx context.Context, in ListEvaluationsInput) ([]Evaluation, error) {
	var out []Evaluation
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the interview exists in this tenant.
		var ivCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM interviews WHERE id = ? AND tenant_id = ?`,
			in.InterviewID, in.TenantID,
		).Scan(&ivCount).Error; err != nil {
			return fmt.Errorf("interview: list eval verify interview: %w", err)
		}
		if ivCount == 0 {
			return ErrNotFound
		}

		// Resolve effective unmasking: tenant must allow peer visibility AND the
		// actor must hold ats:evaluation:read (re-validated here for defence in
		// depth even when the HTTP middleware already checked it).
		peerVisible, err := peerEvalVisible(tx, in.TenantID)
		if err != nil {
			return err
		}
		unmaskOthers := false
		if peerVisible && in.CanReadEvaluations {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("interview: list eval load permissions: %w", err)
			}
			if platformauth.HasPermission(perms, permEvaluationRead) {
				unmaskOthers = true
			}
		}

		var rows []Evaluation
		if err := tx.Raw(
			`SELECT id, tenant_id, interview_id, application_id, evaluator_user_id,
			        sheet_id, scores_json, overall_score, recommendation, comment,
			        created_at, updated_at
			 FROM interview_evaluations
			 WHERE tenant_id = ? AND interview_id = ?
			 ORDER BY created_at`,
			in.TenantID, in.InterviewID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("interview: list eval query: %w", err)
		}

		exposedOther := false
		for i := range rows {
			own := rows[i].EvaluatorUserID == in.ActorID
			if !own && !unmaskOthers {
				// Mask other panelists' sensitive fields.
				rows[i].Comment = ""
				rows[i].ScoresJSON = []byte(`{}`)
				rows[i].OverallScore = nil
			} else if !own {
				exposedOther = true
			}
			out = append(out, rows[i])
		}

		// Audit the access. Record the sensitive variant only when another
		// panelist's evaluation was actually exposed to the actor.
		action := "interview_evaluation.read"
		if exposedOther {
			action = "interview_evaluation.read_peer"
		}
		idStr := in.InterviewID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "interview",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EvaluationSummary aggregates evaluations for an application.
type EvaluationSummary struct {
	ApplicationID     uuid.UUID
	Count             int
	AverageScore      *float64
	StrongYesCount    int
	YesCount          int
	NeutralCount      int
	NoCount           int
	StrongNoCount     int
	RecommendRatioYes float64 // (strong_yes + yes) / count
}

// SummarizeApplication aggregates all evaluations for an application across its
// interviews (average score, recommendation distribution / ratio).  A single
// indexed query avoids N+1 fan-out across evaluations.
func (s *Service) SummarizeApplication(ctx context.Context, tenantID, applicationID uuid.UUID) (*EvaluationSummary, error) {
	summary := EvaluationSummary{ApplicationID: applicationID}
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var agg struct {
			Cnt       int64    `gorm:"column:cnt"`
			Avg       *float64 `gorm:"column:avg_score"`
			StrongYes int64    `gorm:"column:strong_yes"`
			Yes       int64    `gorm:"column:yes"`
			Neutral   int64    `gorm:"column:neutral"`
			No        int64    `gorm:"column:no"`
			StrongNo  int64    `gorm:"column:strong_no"`
		}
		// Single aggregate query (uses idx_interview_evaluations_application).
		if err := tx.Raw(
			`SELECT
			    COUNT(1)                                                    AS cnt,
			    AVG(overall_score)                                          AS avg_score,
			    COUNT(1) FILTER (WHERE recommendation = 'strong_yes')       AS strong_yes,
			    COUNT(1) FILTER (WHERE recommendation = 'yes')              AS yes,
			    COUNT(1) FILTER (WHERE recommendation = 'neutral')          AS neutral,
			    COUNT(1) FILTER (WHERE recommendation = 'no')               AS no,
			    COUNT(1) FILTER (WHERE recommendation = 'strong_no')        AS strong_no
			 FROM interview_evaluations
			 WHERE tenant_id = ? AND application_id = ?`,
			tenantID, applicationID,
		).Scan(&agg).Error; err != nil {
			return fmt.Errorf("interview: summarise query: %w", err)
		}
		summary.Count = int(agg.Cnt)
		summary.AverageScore = agg.Avg
		summary.StrongYesCount = int(agg.StrongYes)
		summary.YesCount = int(agg.Yes)
		summary.NeutralCount = int(agg.Neutral)
		summary.NoCount = int(agg.No)
		summary.StrongNoCount = int(agg.StrongNo)
		if agg.Cnt > 0 {
			summary.RecommendRatioYes = float64(agg.StrongYes+agg.Yes) / float64(agg.Cnt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &summary, nil
}
