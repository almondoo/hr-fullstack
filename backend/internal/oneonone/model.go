// Package oneonone implements the 1on1 (アジェンダ・記録・アクション管理) domain
// for ST-TM-03.
//
// Features:
//   - Series (上司↔部下の継続関係) CRUD, manager hand-over on reassignment.
//   - Sessions under a series with scheduled/held timestamps and status.
//   - Agenda items with carry-over (持ち越し) from prior sessions.
//   - Notes distinguishing shared (両参加者閲覧) and private (記入者本人のみ).
//   - Next actions with assignee / due date / completion, carried across
//     sessions, and notification hooks for due-date reminders.
//
// Privacy invariants (docs/05 §4, CMP-004 個人情報保護法 利用目的明示・最小アクセス):
//   - RLS guarantees the tenant boundary only.  Participant scope (manager /
//     member) and private-note author scope are enforced in the service layer
//     with explicit query conditions (defence-in-depth).
//   - 1on1 notes may contain sensitive dialogue; audit records hold meta only
//     (opaque UUID resource IDs), never the note body.  Notification payloads
//     never carry the body.
//
// Legal/config note: 1on1 record retention period and HR-manager body
// disclosure are社内規程/個人情報の利用目的依存のため設定化 (tm_settings).
// 法令値ではない。本実装は法的助言ではなく、設定で改正/方針変更に追従する前提。
package oneonone

import (
	"time"

	"github.com/google/uuid"
)

// Series status / cadence constants.
const (
	SeriesStatusActive = "active"
	SeriesStatusClosed = "closed"

	CadenceWeekly    = "weekly"
	CadenceBiweekly  = "biweekly"
	CadenceMonthly   = "monthly"
	CadenceQuarterly = "quarterly"
	CadenceAdhoc     = "adhoc"
)

// Session status constants.
const (
	SessionStatusScheduled = "scheduled"
	SessionStatusDone      = "done"
	SessionStatusCanceled  = "canceled"
)

// Note visibility constants.
const (
	VisibilityShared  = "shared"
	VisibilityPrivate = "private"
)

// Action status constants.
const (
	ActionStatusOpen     = "open"
	ActionStatusDone     = "done"
	ActionStatusCanceled = "canceled"
)

// Series is the GORM model for one_on_one_series.
type Series struct {
	ID                uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID `gorm:"column:tenant_id"`
	ManagerEmployeeID uuid.UUID `gorm:"column:manager_employee_id"`
	MemberEmployeeID  uuid.UUID `gorm:"column:member_employee_id"`
	Title             string    `gorm:"column:title"`
	Cadence           string    `gorm:"column:cadence"`
	Status            string    `gorm:"column:status"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

// TableName maps Series to one_on_one_series.
func (Series) TableName() string { return "one_on_one_series" }

// Session is the GORM model for one_on_one_sessions.
type Session struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	SeriesID    uuid.UUID  `gorm:"column:series_id"`
	ScheduledAt *time.Time `gorm:"column:scheduled_at"`
	HeldAt      *time.Time `gorm:"column:held_at"`
	Status      string     `gorm:"column:status"`
	Summary     string     `gorm:"column:summary"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps Session to one_on_one_sessions.
func (Session) TableName() string { return "one_on_one_sessions" }

// AgendaItem is the GORM model for one_on_one_agenda_items.
type AgendaItem struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	SessionID         uuid.UUID  `gorm:"column:session_id"`
	Topic             string     `gorm:"column:topic"`
	AuthorUserID      *uuid.UUID `gorm:"column:author_user_id"`
	SortOrder         int        `gorm:"column:sort_order"`
	CarriedOverFromID *uuid.UUID `gorm:"column:carried_over_from_id"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
}

// TableName maps AgendaItem to one_on_one_agenda_items.
func (AgendaItem) TableName() string { return "one_on_one_agenda_items" }

// Note is the GORM model for one_on_one_notes.
//
// Security note on Body and Visibility:
//   - Body may hold sensitive dialogue and is NEVER written to audit records,
//     logs, or notification payloads.
//   - Visibility "private" notes are readable only by AuthorUserID; the service
//     layer applies the visibility predicate to every read query
//     (visibility='shared' OR (visibility='private' AND author_user_id = :caller)).
type Note struct {
	ID           uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID `gorm:"column:tenant_id"`
	SessionID    uuid.UUID `gorm:"column:session_id"`
	AuthorUserID uuid.UUID `gorm:"column:author_user_id"`
	Visibility   string    `gorm:"column:visibility"`
	Body         string    `gorm:"column:body"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName maps Note to one_on_one_notes.
func (Note) TableName() string { return "one_on_one_notes" }

// Action is the GORM model for one_on_one_actions.
type Action struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	SessionID          uuid.UUID  `gorm:"column:session_id"`
	AssigneeEmployeeID uuid.UUID  `gorm:"column:assignee_employee_id"`
	Description        string     `gorm:"column:description"`
	DueDate            *time.Time `gorm:"column:due_date"`
	Status             string     `gorm:"column:status"`
	CompletedAt        *time.Time `gorm:"column:completed_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps Action to one_on_one_actions.
func (Action) TableName() string { return "one_on_one_actions" }

// Settings is the GORM model for tm_settings (per-tenant TM configuration).
//
// Legal/config note: values here are社内規程依存 (not 法令値).  They are
// configurable so policy / regulatory changes can be followed without code
// changes; this implementation is not legal advice.
type Settings struct {
	ID                      uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID                uuid.UUID `gorm:"column:tenant_id"`
	HRManagerBodyDisclosure bool      `gorm:"column:hr_manager_body_disclosure"`
	NoteRetentionDays       *int      `gorm:"column:note_retention_days"`
	CreatedAt               time.Time `gorm:"column:created_at"`
	UpdatedAt               time.Time `gorm:"column:updated_at"`
}

// TableName maps Settings to tm_settings.
func (Settings) TableName() string { return "tm_settings" }
