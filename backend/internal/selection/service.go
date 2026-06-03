package selection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("selection: not found")
	ErrInvalidTransition = errors.New("selection: invalid stage transition")
	ErrAlreadyExists     = errors.New("selection: record already exists")
	ErrForbidden         = errors.New("selection: permission denied")
	ErrReasonRequired    = errors.New("selection: reason required for this transition")
)

// terminalStageTypes are stage types from which no forward progression is
// allowed; arriving at one finalises application.status.
var terminalStageTypes = map[string]bool{
	StageTypeHired:    true,
	StageTypeRejected: true,
}

// isTerminalStageType reports whether the stage type ends the pipeline.
func isTerminalStageType(stageType string) bool { return terminalStageTypes[stageType] }

// stageTypeToAppStatus maps a terminal stage type to the application status it
// confirms. Non-terminal stage types keep the application in_progress.
func stageTypeToAppStatus(stageType string) string {
	switch stageType {
	case StageTypeHired:
		return AppStatusHired
	case StageTypeRejected:
		return AppStatusRejected
	default:
		return AppStatusInProgress
	}
}

// CandidateNotifier is the hook used to deliver candidate status notifications.
// 実送信は通知基盤 ST-FND-09 へ委譲する。本パッケージは解決済みテンプレと宛先参照
// (応募ID等の不透明ID)のみを渡す。氏名/メール等の実 PII はここに載せない
// (通知基盤側で application_id から解決する)。
//
// Notify は選考遷移をブロックしてはならない: 呼び出し側はエラーを握りつぶす。
type CandidateNotifier interface {
	Notify(ctx context.Context, ev CandidateNotification) error
}

// CandidateNotification is the payload handed to the notifier on stage arrival.
// Contains only opaque references and the resolved template id — never PII.
type CandidateNotification struct {
	TenantID      uuid.UUID
	ApplicationID uuid.UUID
	StageType     string
	TemplateID    uuid.UUID
}

// logNotifier is the default CandidateNotifier. It records that a notification
// would be enqueued (opaque ids only) and performs no external delivery.
// Replace with a real ST-FND-09-backed implementation via WithNotifier.
type logNotifier struct{}

// Notify logs that a candidate notification would be enqueued (stub implementation).
func (logNotifier) Notify(_ context.Context, ev CandidateNotification) error {
	slog.Info("selection: candidate notification enqueued (stub)",
		"tenant_id", ev.TenantID.String(),
		"application_id", ev.ApplicationID.String(),
		"stage_type", ev.StageType,
		"template_id", ev.TemplateID.String(),
	)
	return nil
}

// Service provides business logic for the selection pipeline.
type Service struct {
	tdb      *tenantdb.TenantDB
	notifier CandidateNotifier
}

// NewService constructs a Service with the default (stub) candidate notifier.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb, notifier: logNotifier{}}
}

// WithNotifier returns a copy of the service using the given notifier.
// Wiring code (or tests) can inject an ST-FND-09-backed notifier here.
func (s *Service) WithNotifier(n CandidateNotifier) *Service {
	if n == nil {
		n = logNotifier{}
	}
	return &Service{tdb: s.tdb, notifier: n}
}

// templateStage is the shape of each element in a stage template's stages_json.
type templateStage struct {
	Name      string `json:"name"`
	StageType string `json:"stage_type"`
	Position  int    `json:"position"`
}

// ---------------------------------------------------------------------------
// Stage templates
// ---------------------------------------------------------------------------

// CreateStageTemplateInput holds fields for creating a stage template.
type CreateStageTemplateInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	Name       string
	StagesJSON []byte
	IP         *string
}

// CreateStageTemplate creates a tenant standard selection-stage template.
func (s *Service) CreateStageTemplate(ctx context.Context, in CreateStageTemplateInput) (*StageTemplate, error) {
	tmpl := StageTemplate{
		ID:         uuid.New(),
		TenantID:   in.TenantID,
		Name:       in.Name,
		StagesJSON: in.StagesJSON,
		Active:     true,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO selection_stage_templates
			   (id, tenant_id, name, stages_json, active)
			 VALUES (?, ?, ?, ?::jsonb, ?)`,
			tmpl.ID, tmpl.TenantID, tmpl.Name, tmpl.StagesJSON, tmpl.Active,
		).Error; err != nil {
			return fmt.Errorf("selection: create stage template insert: %w", err)
		}
		idStr := tmpl.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "selection_stage_template.created",
			ResourceType: "selection_stage_template",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// ListStageTemplates returns active stage templates for a tenant.
func (s *Service) ListStageTemplates(ctx context.Context, tenantID uuid.UUID) ([]StageTemplate, error) {
	var tmpls []StageTemplate
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, name, stages_json, active, created_at, updated_at
			 FROM selection_stage_templates
			 WHERE tenant_id = ? AND active = true
			 ORDER BY name`,
			tenantID,
		).Scan(&tmpls).Error
	})
	if err != nil {
		return nil, err
	}
	return tmpls, nil
}

// ---------------------------------------------------------------------------
// Stages (per job posting)
// ---------------------------------------------------------------------------

// InitStagesInput holds parameters for initialising a job posting's stages.
type InitStagesInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	JobPostingID uuid.UUID
	// TemplateID, when set, copies the template's stages_json. Otherwise Stages
	// must be supplied directly.
	TemplateID *uuid.UUID
	// Stages is the explicit stage list used when TemplateID is nil.
	Stages []StageDef
	IP     *string
}

// StageDef describes one stage to create.
type StageDef struct {
	Name      string
	StageType string
	Position  int
}

// InitStages creates the ordered stage list for a job posting, either from a
// tenant standard template or from an explicit list. Existing stages for the
// job posting (if any) cause ErrAlreadyExists — re-initialisation is rejected
// to keep history references stable.
func (s *Service) InitStages(ctx context.Context, in InitStagesInput) ([]Stage, error) {
	var stages []Stage
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the job posting belongs to this tenant (logical FK; verified
		// in-service because job_postings is another story's table).
		var jpCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM job_postings WHERE id = ? AND tenant_id = ?`,
			in.JobPostingID, in.TenantID,
		).Scan(&jpCount).Error; err != nil {
			return fmt.Errorf("selection: init stages verify job posting: %w", err)
		}
		if jpCount == 0 {
			return ErrNotFound
		}

		// Reject re-initialisation if stages already exist for this job posting.
		var existing int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM selection_stages WHERE job_posting_id = ? AND tenant_id = ?`,
			in.JobPostingID, in.TenantID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("selection: init stages check existing: %w", err)
		}
		if existing > 0 {
			return ErrAlreadyExists
		}

		defs := in.Stages
		if in.TemplateID != nil {
			var tmpl StageTemplate
			if err := tx.Raw(
				`SELECT id, tenant_id, stages_json FROM selection_stage_templates
				 WHERE id = ? AND tenant_id = ? AND active = true LIMIT 1`,
				*in.TemplateID, in.TenantID,
			).Scan(&tmpl).Error; err != nil {
				return fmt.Errorf("selection: init stages fetch template: %w", err)
			}
			if tmpl.ID == uuid.Nil {
				return ErrNotFound
			}
			var ts []templateStage
			if err := json.Unmarshal(tmpl.StagesJSON, &ts); err != nil {
				return fmt.Errorf("selection: init stages parse stages_json: %w", err)
			}
			defs = defs[:0]
			for _, t := range ts {
				defs = append(defs, StageDef(t))
			}
		}
		if len(defs) == 0 {
			return ErrNotFound
		}

		for _, d := range defs {
			st := Stage{
				ID:           uuid.New(),
				TenantID:     in.TenantID,
				JobPostingID: in.JobPostingID,
				Position:     d.Position,
				Name:         d.Name,
				StageType:    d.StageType,
			}
			if err := tx.Exec(
				`INSERT INTO selection_stages
				   (id, tenant_id, job_posting_id, position, name, stage_type)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				st.ID, st.TenantID, st.JobPostingID, st.Position, st.Name, st.StageType,
			).Error; err != nil {
				return fmt.Errorf("selection: init stages insert: %w", err)
			}
			stages = append(stages, st)
		}

		idStr := in.JobPostingID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "selection_stages.initialized",
			ResourceType: "job_posting",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return stages, nil
}

// ListStages returns the ordered stages for a job posting.
func (s *Service) ListStages(ctx context.Context, tenantID, jobPostingID uuid.UUID) ([]Stage, error) {
	var stages []Stage
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, job_posting_id, position, name, stage_type,
			        created_at, updated_at
			 FROM selection_stages
			 WHERE tenant_id = ? AND job_posting_id = ?
			 ORDER BY position`,
			tenantID, jobPostingID,
		).Scan(&stages).Error
	})
	if err != nil {
		return nil, err
	}
	return stages, nil
}

// ---------------------------------------------------------------------------
// Applications
// ---------------------------------------------------------------------------

// CreateApplicationInput holds fields for creating an application.
type CreateApplicationInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	JobPostingID uuid.UUID
	ApplicantID  uuid.UUID
	IP           *string
}

// CreateApplication creates an application for an applicant against a job
// posting, placing it at the first stage (lowest position) of the job's
// pipeline. Duplicate applications (same job + applicant) are rejected.
func (s *Service) CreateApplication(ctx context.Context, in CreateApplicationInput) (*Application, error) {
	app := Application{
		ID:             uuid.New(),
		TenantID:       in.TenantID,
		JobPostingID:   in.JobPostingID,
		ApplicantID:    in.ApplicantID,
		Status:         AppStatusInProgress,
		RetentionLabel: "unset",
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify job posting belongs to this tenant (logical FK).
		var jpCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM job_postings WHERE id = ? AND tenant_id = ?`,
			in.JobPostingID, in.TenantID,
		).Scan(&jpCount).Error; err != nil {
			return fmt.Errorf("selection: create application verify job posting: %w", err)
		}
		if jpCount == 0 {
			return ErrNotFound
		}

		// Verify applicant belongs to this tenant (logical FK).
		var apCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM applicants WHERE id = ? AND tenant_id = ?`,
			in.ApplicantID, in.TenantID,
		).Scan(&apCount).Error; err != nil {
			return fmt.Errorf("selection: create application verify applicant: %w", err)
		}
		if apCount == 0 {
			return ErrNotFound
		}

		// Reject duplicate application (same job + applicant).
		var dup int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM applications
			 WHERE tenant_id = ? AND job_posting_id = ? AND applicant_id = ?`,
			in.TenantID, in.JobPostingID, in.ApplicantID,
		).Scan(&dup).Error; err != nil {
			return fmt.Errorf("selection: create application dup check: %w", err)
		}
		if dup > 0 {
			return ErrAlreadyExists
		}

		// Resolve the first stage (lowest position) of the pipeline.
		var firstStage Stage
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, position, name, stage_type
			 FROM selection_stages
			 WHERE tenant_id = ? AND job_posting_id = ?
			 ORDER BY position ASC LIMIT 1`,
			in.TenantID, in.JobPostingID,
		).Scan(&firstStage).Error; err != nil {
			return fmt.Errorf("selection: create application resolve first stage: %w", err)
		}
		if firstStage.ID == uuid.Nil {
			// No stages defined for this job posting yet.
			return ErrNotFound
		}
		app.CurrentStageID = &firstStage.ID

		if err := tx.Exec(
			`INSERT INTO applications
			   (id, tenant_id, job_posting_id, applicant_id, current_stage_id, status, retention_label)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			app.ID, app.TenantID, app.JobPostingID, app.ApplicantID,
			app.CurrentStageID, app.Status, app.RetentionLabel,
		).Error; err != nil {
			return fmt.Errorf("selection: create application insert: %w", err)
		}

		// Record the entry transition (from NULL → first stage).
		if err := s.insertHistory(tx, app.TenantID, app.ID, nil, firstStage.ID, &in.ActorID, nil); err != nil {
			return err
		}

		idStr := app.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "application.created",
			ResourceType: "application",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// GetApplication fetches a single application by ID within the tenant.
func (s *Service) GetApplication(ctx context.Context, tenantID, id uuid.UUID) (*Application, error) {
	var app Application
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, job_posting_id, applicant_id, current_stage_id,
			        status, retention_label, retention_expires_on, created_at, updated_at
			 FROM applications WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&app).Error
	})
	if err != nil {
		return nil, err
	}
	if app.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &app, nil
}

// ListHistory returns the stage transition history for an application,
// ordered chronologically.
func (s *Service) ListHistory(ctx context.Context, tenantID, applicationID uuid.UUID) ([]StageHistory, error) {
	var hist []StageHistory
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, application_id, from_stage_id, to_stage_id,
			        moved_by, moved_at, reason, created_at, updated_at
			 FROM application_stage_history
			 WHERE tenant_id = ? AND application_id = ?
			 ORDER BY moved_at ASC, created_at ASC`,
			tenantID, applicationID,
		).Scan(&hist).Error
	})
	if err != nil {
		return nil, err
	}
	return hist, nil
}

// ---------------------------------------------------------------------------
// Stage transitions (the selection state machine)
// ---------------------------------------------------------------------------

// MoveStageInput holds fields for moving an application to a target stage.
//
// The target stage must belong to the same job posting as the application.
// Allowed moves:
//   - advance:   target.position == current.position + 1
//   - move-back: target.position  < current.position (差戻し)
//   - reject:    target.stage_type == "rejected"  (any position)
//   - hire:      target.stage_type == "hired"     (only from the offer stage)
//
// Forward progression out of a terminal stage (hired/rejected) is rejected.
// ReasonRequired enforces that move-back / reject moves carry a reason.
type MoveStageInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	ApplicationID uuid.UUID
	TargetStageID uuid.UUID
	Reason        *string
	// ReasonRequiredForBackOrReject is configuration-driven (CMP-004): when true,
	// move-back and reject transitions must carry a non-empty reason. The caller
	// resolves this from tenant settings — it is never hardcoded here.
	ReasonRequiredForBackOrReject bool
	// RetentionLabel / RetentionExpiresOn are applied to the application when the
	// target is a "rejected" stage, per ST-ATS-02 retention/consent policy. Both
	// are configuration-derived (no hardcoded horizon).
	RetentionLabel     string
	RetentionExpiresOn *time.Time
	IP                 *string
}

// MoveStage transitions an application to TargetStageID and, on a terminal
// stage, finalises application.status. The move is validated against the
// allow-list state machine, recorded in application_stage_history and the audit
// log within one transaction. On success a candidate notification is attempted
// for the arrived stage type (best-effort; never blocks the transition).
func (s *Service) MoveStage(ctx context.Context, in MoveStageInput) (*Application, error) {
	var app Application
	var arrivedStageType string
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock the application row to avoid concurrent conflicting transitions.
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, applicant_id, current_stage_id,
			        status, retention_label, retention_expires_on, created_at, updated_at
			 FROM applications
			 WHERE id = ? AND tenant_id = ? LIMIT 1
			 FOR UPDATE`,
			in.ApplicationID, in.TenantID,
		).Scan(&app).Error; err != nil {
			return fmt.Errorf("selection: move stage read application: %w", err)
		}
		if app.ID == uuid.Nil {
			return ErrNotFound
		}

		// Resolve the target stage and confirm it belongs to the same job posting.
		var target Stage
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, position, name, stage_type
			 FROM selection_stages
			 WHERE id = ? AND tenant_id = ? AND job_posting_id = ? LIMIT 1`,
			in.TargetStageID, in.TenantID, app.JobPostingID,
		).Scan(&target).Error; err != nil {
			return fmt.Errorf("selection: move stage read target: %w", err)
		}
		if target.ID == uuid.Nil {
			return ErrNotFound
		}

		// Resolve the current stage (nil only for a freshly-created application
		// whose stage was somehow cleared — treat as entry).
		var current Stage
		if app.CurrentStageID != nil {
			if err := tx.Raw(
				`SELECT id, tenant_id, job_posting_id, position, name, stage_type
				 FROM selection_stages
				 WHERE id = ? AND tenant_id = ? LIMIT 1`,
				*app.CurrentStageID, in.TenantID,
			).Scan(&current).Error; err != nil {
				return fmt.Errorf("selection: move stage read current: %w", err)
			}
		}

		// An application already in a finalised status cannot move further.
		if app.Status != AppStatusInProgress {
			return fmt.Errorf("%w: application status is %s", ErrInvalidTransition, app.Status)
		}

		// Validate the move against the state machine.
		moveKind, err := classifyMove(current, target)
		if err != nil {
			return err
		}

		// Reason enforcement for move-back / reject (configuration-driven).
		if in.ReasonRequiredForBackOrReject &&
			(moveKind == moveBack || target.StageType == StageTypeRejected) {
			if in.Reason == nil || *in.Reason == "" {
				return ErrReasonRequired
			}
		}

		newStatus := stageTypeToAppStatus(target.StageType)

		// Apply retention policy on rejection (ST-ATS-02). Values are caller-supplied
		// from tenant settings; defaults remain unset when not provided.
		retentionLabel := app.RetentionLabel
		retentionExpiresOn := app.RetentionExpiresOn
		if target.StageType == StageTypeRejected {
			if in.RetentionLabel != "" {
				retentionLabel = in.RetentionLabel
			}
			if in.RetentionExpiresOn != nil {
				retentionExpiresOn = in.RetentionExpiresOn
			}
		}

		res := tx.Exec(
			`UPDATE applications
			 SET current_stage_id = ?, status = ?,
			     retention_label = ?, retention_expires_on = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			target.ID, newStatus, retentionLabel, retentionExpiresOn,
			in.ApplicationID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("selection: move stage update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		var fromStageID *uuid.UUID
		if app.CurrentStageID != nil {
			fromStageID = app.CurrentStageID
		}
		if err := s.insertHistory(tx, in.TenantID, in.ApplicationID, fromStageID, target.ID, &in.ActorID, in.Reason); err != nil {
			return err
		}

		// Re-read for the up-to-date returned struct.
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, applicant_id, current_stage_id,
			        status, retention_label, retention_expires_on, created_at, updated_at
			 FROM applications WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ApplicationID, in.TenantID,
		).Scan(&app).Error; err != nil {
			return fmt.Errorf("selection: move stage re-read: %w", err)
		}

		arrivedStageType = target.StageType

		idStr := in.ApplicationID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "application.stage_moved",
			ResourceType: "application",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}

	// Candidate notification hook — best effort, must NOT block the transition.
	// Runs after commit so a notifier failure cannot roll back the move.
	s.maybeNotifyCandidate(ctx, in.TenantID, in.ApplicationID, arrivedStageType)

	return &app, nil
}

// move kinds classified for the state machine.
type moveKind int

const (
	moveAdvance moveKind = iota
	moveBack
	moveReject
	moveHire
)

// classifyMove validates a transition from current → target and returns its kind.
// current.ID == uuid.Nil means "no current stage" (entry).
func classifyMove(current, target Stage) (moveKind, error) {
	// Reject is allowed from any non-terminal current stage.
	if target.StageType == StageTypeRejected {
		if current.ID != uuid.Nil && isTerminalStageType(current.StageType) {
			return 0, fmt.Errorf("%w: cannot move from terminal stage %q", ErrInvalidTransition, current.StageType)
		}
		return moveReject, nil
	}

	// Forward progression out of a terminal current stage is never allowed.
	if current.ID != uuid.Nil && isTerminalStageType(current.StageType) {
		return 0, fmt.Errorf("%w: cannot move from terminal stage %q", ErrInvalidTransition, current.StageType)
	}

	// Hire is allowed only when advancing into the hired stage from the
	// immediately-preceding stage (typically the offer stage).
	if target.StageType == StageTypeHired {
		if current.ID == uuid.Nil || target.Position != current.Position+1 {
			return 0, fmt.Errorf("%w: hire must advance from the preceding stage", ErrInvalidTransition)
		}
		return moveHire, nil
	}

	// No current stage (entry) may only set the very first reachable stage; we
	// treat that as advance (CreateApplication uses insertHistory directly, so
	// this path is defensive).
	if current.ID == uuid.Nil {
		return moveAdvance, nil
	}

	switch {
	case target.Position == current.Position+1:
		return moveAdvance, nil
	case target.Position < current.Position:
		return moveBack, nil
	default:
		// Skipping ahead (>+1) or staying in place is not allowed.
		return 0, fmt.Errorf("%w: %d → %d is not a single-step advance or a move-back",
			ErrInvalidTransition, current.Position, target.Position)
	}
}

// insertHistory writes one stage-history row inside the caller's transaction.
func (s *Service) insertHistory(
	tx *gorm.DB,
	tenantID, applicationID uuid.UUID,
	fromStageID *uuid.UUID,
	toStageID uuid.UUID,
	movedBy *uuid.UUID,
	reason *string,
) error {
	if err := tx.Exec(
		`INSERT INTO application_stage_history
		   (id, tenant_id, application_id, from_stage_id, to_stage_id, moved_by, moved_at, reason)
		 VALUES (?, ?, ?, ?, ?, ?, now(), ?)`,
		uuid.New(), tenantID, applicationID, fromStageID, toStageID, movedBy, reason,
	).Error; err != nil {
		return fmt.Errorf("selection: insert stage history: %w", err)
	}
	return nil
}

// maybeNotifyCandidate resolves the active candidate message template for the
// arrived stage type and hands it to the notifier. Missing template → skip.
// Any failure (resolution or notifier) is logged and swallowed: it must never
// block or undo the already-committed stage transition.
func (s *Service) maybeNotifyCandidate(ctx context.Context, tenantID, applicationID uuid.UUID, stageType string) {
	if stageType == "" {
		return
	}
	var tmpl MessageTemplate
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, stage_type, name, subject, body, active
			 FROM candidate_message_templates
			 WHERE tenant_id = ? AND stage_type = ? AND active = true
			 ORDER BY updated_at DESC LIMIT 1`,
			tenantID, stageType,
		).Scan(&tmpl).Error
	})
	if err != nil {
		slog.Warn("selection: resolve candidate template failed (skipping notification)",
			"tenant_id", tenantID.String(),
			"application_id", applicationID.String(),
			"stage_type", stageType,
			"err", err,
		)
		return
	}
	if tmpl.ID == uuid.Nil {
		// No template configured for this stage type — skip silently.
		return
	}
	if err := s.notifier.Notify(ctx, CandidateNotification{
		TenantID:      tenantID,
		ApplicationID: applicationID,
		StageType:     stageType,
		TemplateID:    tmpl.ID,
	}); err != nil {
		slog.Warn("selection: candidate notification failed (transition not blocked)",
			"tenant_id", tenantID.String(),
			"application_id", applicationID.String(),
			"stage_type", stageType,
			"err", err,
		)
	}
}

// ---------------------------------------------------------------------------
// Candidate message templates
// ---------------------------------------------------------------------------

// UpsertMessageTemplateInput holds fields for creating/updating a template.
type UpsertMessageTemplateInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	StageType string
	Name      string
	Subject   string
	Body      string
	IP        *string
}

// CreateMessageTemplate creates a candidate notification template. A partial
// unique index enforces at most one active template per (tenant, stage_type),
// so a conflicting active template surfaces as ErrAlreadyExists.
func (s *Service) CreateMessageTemplate(ctx context.Context, in UpsertMessageTemplateInput) (*MessageTemplate, error) {
	tmpl := MessageTemplate{
		ID:        uuid.New(),
		TenantID:  in.TenantID,
		StageType: in.StageType,
		Name:      in.Name,
		Subject:   in.Subject,
		Body:      in.Body,
		Active:    true,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Pre-check for an existing active template (clearer error than a raw
		// unique-violation; the partial index remains the source of truth).
		var existing int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM candidate_message_templates
			 WHERE tenant_id = ? AND stage_type = ? AND active = true`,
			in.TenantID, in.StageType,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("selection: create message template check: %w", err)
		}
		if existing > 0 {
			return ErrAlreadyExists
		}

		if err := tx.Exec(
			`INSERT INTO candidate_message_templates
			   (id, tenant_id, stage_type, name, subject, body, active)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			tmpl.ID, tmpl.TenantID, tmpl.StageType, tmpl.Name, tmpl.Subject, tmpl.Body, tmpl.Active,
		).Error; err != nil {
			return fmt.Errorf("selection: create message template insert: %w", err)
		}

		idStr := tmpl.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "candidate_message_template.created",
			ResourceType: "candidate_message_template",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// ListMessageTemplates returns the active candidate templates for a tenant.
func (s *Service) ListMessageTemplates(ctx context.Context, tenantID uuid.UUID) ([]MessageTemplate, error) {
	var tmpls []MessageTemplate
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, stage_type, name, subject, body, active,
			        created_at, updated_at
			 FROM candidate_message_templates
			 WHERE tenant_id = ? AND active = true
			 ORDER BY stage_type, name`,
			tenantID,
		).Scan(&tmpls).Error
	})
	if err != nil {
		return nil, err
	}
	return tmpls, nil
}

// ---------------------------------------------------------------------------
// Kanban aggregation
// ---------------------------------------------------------------------------

// KanbanColumn is one stage column with its applications, for the board view.
type KanbanColumn struct {
	Stage        Stage
	Applications []Application
}

// GetKanban returns the per-stage application grouping for a job posting,
// tenant-scoped. Applications are fetched with a single indexed query
// (idx_applications_kanban) and grouped in memory to avoid N+1.
func (s *Service) GetKanban(ctx context.Context, tenantID, jobPostingID uuid.UUID) ([]KanbanColumn, error) {
	var stages []Stage
	var apps []Application
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, position, name, stage_type,
			        created_at, updated_at
			 FROM selection_stages
			 WHERE tenant_id = ? AND job_posting_id = ?
			 ORDER BY position`,
			tenantID, jobPostingID,
		).Scan(&stages).Error; err != nil {
			return fmt.Errorf("selection: kanban read stages: %w", err)
		}
		// Single query for all in-flight applications of this job posting.
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, applicant_id, current_stage_id,
			        status, retention_label, retention_expires_on, created_at, updated_at
			 FROM applications
			 WHERE tenant_id = ? AND job_posting_id = ?
			 ORDER BY created_at`,
			tenantID, jobPostingID,
		).Scan(&apps).Error; err != nil {
			return fmt.Errorf("selection: kanban read applications: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Group applications by current_stage_id in memory.
	byStage := make(map[uuid.UUID][]Application, len(stages))
	for _, a := range apps {
		if a.CurrentStageID == nil {
			continue
		}
		byStage[*a.CurrentStageID] = append(byStage[*a.CurrentStageID], a)
	}
	cols := make([]KanbanColumn, len(stages))
	for i := range stages {
		cols[i] = KanbanColumn{
			Stage:        stages[i],
			Applications: byStage[stages[i].ID],
		}
	}
	return cols, nil
}
