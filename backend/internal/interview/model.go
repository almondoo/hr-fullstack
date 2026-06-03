// Package interview implements the interview scheduling and evaluation domain
// for the ATS (応募者追跡) feature set.
//
// Story ST-ATS-04 (docs/01 §C-2 ATS-012 / ATS-013, INT-005):
//   - Interviews are created against an application (ST-ATS-03).  Candidate
//     slots are proposed, one is confirmed, and the interview moves through
//     proposed → confirmed → completed/cancelled.
//   - Calendar linkage (Google/Outlook = INT-005) is loosely coupled: only an
//     opaque external_event_id is stored.  The MVP manages interviews
//     internally and remind notifications are delegated via a Reminder hook;
//     a sync/notification failure must never corrupt interview data.
//   - Panellists (面接官) score interviews against a tenant-defined evaluation
//     sheet.  Evaluation comments are selection-sensitive: reads are gated by
//     the ats:evaluation:read permission and recorded in the audit log.  A
//     per-tenant setting controls whether panellists may see each other's
//     evaluations (independent-evaluation bias control).
//
// Cross-story references (application_id, evaluator_user_id, panellist user_id)
// are bare uuid columns with service-layer tenant validation — no foreign keys
// to other stories' tables or to users (which lacks UNIQUE(id, tenant_id)).
package interview

import (
	"time"

	"github.com/google/uuid"
)

// Interview status values.
const (
	StatusProposed  = "proposed"
	StatusConfirmed = "confirmed"
	StatusCompleted = "completed"
	StatusCancelled = "cancelled"
)

// Interview format values.
const (
	FormatOnsite = "onsite"
	FormatOnline = "online"
	FormatPhone  = "phone"
)

// Panellist role values.
const (
	RoleInterviewer = "interviewer"
	RoleObserver    = "observer"
)

// Recommendation values.
const (
	RecStrongYes = "strong_yes"
	RecYes       = "yes"
	RecNeutral   = "neutral"
	RecNo        = "no"
	RecStrongNo  = "strong_no"
)

// Interview is the GORM model for interviews.
type Interview struct {
	ID uuid.UUID `gorm:"column:id;primaryKey"`
	// TenantID scopes the row; enforced by RLS and explicit WHERE.
	TenantID uuid.UUID `gorm:"column:tenant_id"`
	// ApplicationID is a logical reference to applications(id) (ST-ATS-03). No FK.
	ApplicationID uuid.UUID  `gorm:"column:application_id"`
	Status        string     `gorm:"column:status"`
	Format        string     `gorm:"column:format"`
	ScheduledAt   *time.Time `gorm:"column:scheduled_at"`
	OnlineURL     *string    `gorm:"column:online_url"`
	// ExternalEventID is the opaque calendar event id (INT-005). Best-effort.
	ExternalEventID *string   `gorm:"column:external_event_id"`
	Notes           *string   `gorm:"column:notes"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps Interview to interviews.
func (Interview) TableName() string { return "interviews" }

// Slot is the GORM model for interview_slots.
type Slot struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	InterviewID    uuid.UUID  `gorm:"column:interview_id"`
	CandidateStart time.Time  `gorm:"column:candidate_start"`
	CandidateEnd   *time.Time `gorm:"column:candidate_end"`
	Selected       bool       `gorm:"column:selected"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Slot to interview_slots.
func (Slot) TableName() string { return "interview_slots" }

// Panellist is the GORM model for interview_panellists. //nolint:misspell // DB table name is a schema contract; US spelling kept to match migration
type Panellist struct {
	ID          uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	InterviewID uuid.UUID `gorm:"column:interview_id"`
	// UserID is a logical reference to users(id). No composite FK.
	UserID    uuid.UUID `gorm:"column:user_id"`
	Role      string    `gorm:"column:role"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps Panellist to interview_panellists. //nolint:misspell // DB table name is a schema contract; US spelling kept to match migration
func (Panellist) TableName() string { return "interview_panelists" } //nolint:misspell // DB table name is schema contract; cannot change to UK spelling

// EvaluationSheet is the GORM model for evaluation_sheets.
type EvaluationSheet struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID `gorm:"column:tenant_id"`
	Name      string    `gorm:"column:name"`
	ItemsJSON []byte    `gorm:"column:items_json;type:jsonb"`
	Active    bool      `gorm:"column:active"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps EvaluationSheet to evaluation_sheets.
func (EvaluationSheet) TableName() string { return "evaluation_sheets" }

// Evaluation is the GORM model for interview_evaluations.
//
// Security note on Comment:
//   - Comment is selection-sensitive free text.  It is NOT 要配慮 PII (so it is
//     not encrypted), but reads are gated by ats:evaluation:read and recorded
//     in the audit log.  Comment / scores are masked for other panellists when
//     the tenant disables peer-evaluation visibility.
type Evaluation struct {
	ID          uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	InterviewID uuid.UUID `gorm:"column:interview_id"`
	// ApplicationID is a logical reference to applications(id). No FK.
	ApplicationID uuid.UUID `gorm:"column:application_id"`
	// EvaluatorUserID is a logical reference to users(id). No composite FK.
	EvaluatorUserID uuid.UUID `gorm:"column:evaluator_user_id"`
	SheetID         uuid.UUID `gorm:"column:sheet_id"`
	ScoresJSON      []byte    `gorm:"column:scores_json;type:jsonb"`
	OverallScore    *float64  `gorm:"column:overall_score"`
	Recommendation  string    `gorm:"column:recommendation"`
	Comment         string    `gorm:"column:comment"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps Evaluation to interview_evaluations.
func (Evaluation) TableName() string { return "interview_evaluations" }

// TenantSettings is the GORM model for tenant_interview_settings.
type TenantSettings struct {
	TenantID        uuid.UUID `gorm:"column:tenant_id;primaryKey"`
	PeerEvalVisible bool      `gorm:"column:peer_eval_visible"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps TenantSettings to tenant_interview_settings.
func (TenantSettings) TableName() string { return "tenant_interview_settings" }
