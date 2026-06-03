// Package selection implements the ST-ATS-03 selection-pipeline domain.
//
// Features:
//   - Per-job-posting ordered selection stages, initialised from a tenant
//     standard template (selection_stage_templates → selection_stages).
//   - applications (求人 × 応募者 の選考エンティティ) with a current stage and a
//     status lifecycle (in_progress / rejected / withdrawn / hired).
//   - Stage transitions (advance / move-back / reject / hire) validated in the
//     service layer with an allow-list state machine, recorded both in
//     application_stage_history and the platform audit log.
//   - Kanban aggregation per job posting (stage × applications), tenant-scoped.
//   - Candidate notification hook: on stage arrival the matching
//     candidate_message_templates row is resolved and handed to a pluggable
//     CandidateNotifier (実送信は通知基盤 ST-FND-09 へ委譲)。テンプレ未設定は
//     スキップし、通知失敗は選考遷移をブロックしない。
//
// Legal note: rejection-reason recording policy, retention horizons and any
// required-field flags are configuration-driven (not hardcoded) and assume
// review by a 社労士/弁護士 per CMP-004 (募集差別禁止) and ST-ATS-02 consent.
// This implementation is not legal advice.
package selection

import (
	"time"

	"github.com/google/uuid"
)

// Stage type values. The terminal types finalise application.status on arrival.
const (
	StageTypeScreening = "screening"
	StageTypeInterview = "interview"
	StageTypeOffer     = "offer"
	StageTypeHired     = "hired"    // terminal
	StageTypeRejected  = "rejected" // terminal
)

// Application status values.
const (
	AppStatusInProgress = "in_progress"
	AppStatusRejected   = "rejected"
	AppStatusWithdrawn  = "withdrawn"
	AppStatusHired      = "hired"
)

// StageTemplate is the GORM model for selection_stage_templates.
type StageTemplate struct {
	ID         uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID   uuid.UUID `gorm:"column:tenant_id"`
	Name       string    `gorm:"column:name"`
	StagesJSON []byte    `gorm:"column:stages_json;type:jsonb"`
	Active     bool      `gorm:"column:active"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName maps StageTemplate to selection_stage_templates.
func (StageTemplate) TableName() string { return "selection_stage_templates" }

// Stage is the GORM model for selection_stages.
type Stage struct {
	ID           uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID `gorm:"column:tenant_id"`
	JobPostingID uuid.UUID `gorm:"column:job_posting_id"`
	Position     int       `gorm:"column:position"`
	Name         string    `gorm:"column:name"`
	StageType    string    `gorm:"column:stage_type"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName maps Stage to selection_stages.
func (Stage) TableName() string { return "selection_stages" }

// Application is the GORM model for applications.
type Application struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	JobPostingID       uuid.UUID  `gorm:"column:job_posting_id"`
	ApplicantID        uuid.UUID  `gorm:"column:applicant_id"`
	CurrentStageID     *uuid.UUID `gorm:"column:current_stage_id"`
	Status             string     `gorm:"column:status"`
	RetentionLabel     string     `gorm:"column:retention_label"`
	RetentionExpiresOn *time.Time `gorm:"column:retention_expires_on"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps Application to applications.
func (Application) TableName() string { return "applications" }

// StageHistory is the GORM model for application_stage_history.
type StageHistory struct {
	ID            uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID      uuid.UUID  `gorm:"column:tenant_id"`
	ApplicationID uuid.UUID  `gorm:"column:application_id"`
	FromStageID   *uuid.UUID `gorm:"column:from_stage_id"`
	ToStageID     uuid.UUID  `gorm:"column:to_stage_id"`
	MovedBy       *uuid.UUID `gorm:"column:moved_by"`
	MovedAt       time.Time  `gorm:"column:moved_at"`
	Reason        *string    `gorm:"column:reason"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

// TableName maps StageHistory to application_stage_history.
func (StageHistory) TableName() string { return "application_stage_history" }

// MessageTemplate is the GORM model for candidate_message_templates.
type MessageTemplate struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID `gorm:"column:tenant_id"`
	StageType string    `gorm:"column:stage_type"`
	Name      string    `gorm:"column:name"`
	Subject   string    `gorm:"column:subject"`
	Body      string    `gorm:"column:body"`
	Active    bool      `gorm:"column:active"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps MessageTemplate to candidate_message_templates.
func (MessageTemplate) TableName() string { return "candidate_message_templates" }
