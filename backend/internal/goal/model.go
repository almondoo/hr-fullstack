// Package goal implements the goal management (目標管理 MBO/OKR) and goal
// cascade domain for ST-TM-01.
//
// Features:
//   - Review cycles (評価サイクル/期) — tenant-configurable, status managed
//     (draft/active/closed).  Policy switches (require_weight_100, progress
//     method, cascade depth) are stored per-cycle, never hardcoded.
//   - Goals — MBO (title/weight/criteria/self-rating) and OKR (Objective +
//     KeyResults).  Status FSM (draft→submitted→approved→in_progress→
//     achieved/closed) with approval-engine integration for submit/approve.
//   - Cascade — parent_goal_id self-reference with cycle detection (direct
//     and multi-level ancestor traversal) and tree retrieval.
//   - Progress logs — append-only history of progress updates.
//
// Legal/policy note: evaluation systems differ per company.  Weight-100%
// enforcement, progress calculation method, and cascade depth are tenant
// settings (review_cycles), not hardcoded.  This implementation is not legal
// advice; the validity of any evaluation scheme is the responsibility of each
// company's HR / certified social insurance labour consultant.
package goal

import (
	"time"

	"github.com/google/uuid"
)

// Method / status / progress-method literals.
const (
	MethodMBO = "mbo"
	MethodOKR = "okr"

	CycleStatusDraft  = "draft"
	CycleStatusActive = "active"
	CycleStatusClosed = "closed"

	GoalStatusDraft      = "draft"
	GoalStatusSubmitted  = "submitted"
	GoalStatusApproved   = "approved"
	GoalStatusInProgress = "in_progress"
	GoalStatusAchieved   = "achieved"
	GoalStatusClosed     = "closed"

	ProgressMethodAverage  = "average"
	ProgressMethodWeighted = "weighted"
)

// ReviewCycle is the GORM model for review_cycles (評価サイクル / 期).
type ReviewCycle struct {
	ID               uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id"`
	Name             string     `gorm:"column:name"`
	StartsOn         time.Time  `gorm:"column:starts_on"`
	EndsOn           time.Time  `gorm:"column:ends_on"`
	GoalDueOn        *time.Time `gorm:"column:goal_due_on"`
	ReviewDueOn      *time.Time `gorm:"column:review_due_on"`
	Status           string     `gorm:"column:status"`
	RequireWeight100 bool       `gorm:"column:require_weight_100"`
	ProgressMethod   string     `gorm:"column:progress_method"`
	MaxCascadeDepth  int        `gorm:"column:max_cascade_depth"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName maps ReviewCycle to review_cycles.
func (ReviewCycle) TableName() string { return "review_cycles" }

// Goal is the GORM model for goals (MBO / OKR 目標).
type Goal struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	CycleID           uuid.UUID  `gorm:"column:cycle_id"`
	EmployeeID        uuid.UUID  `gorm:"column:employee_id"`
	ParentGoalID      *uuid.UUID `gorm:"column:parent_goal_id"`
	Method            string     `gorm:"column:method"`
	Title             string     `gorm:"column:title"`
	Description       string     `gorm:"column:description"`
	Weight            *float64   `gorm:"column:weight"`
	Status            string     `gorm:"column:status"`
	SelfRating        *string    `gorm:"column:self_rating"`
	ProgressPct       float64    `gorm:"column:progress_pct"`
	ApprovalRequestID *uuid.UUID `gorm:"column:approval_request_id"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps Goal to goals.
func (Goal) TableName() string { return "goals" }

// KeyResult is the GORM model for key_results (OKR の KeyResult).
type KeyResult struct {
	ID           uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID `gorm:"column:tenant_id"`
	GoalID       uuid.UUID `gorm:"column:goal_id"`
	Title        string    `gorm:"column:title"`
	MetricUnit   string    `gorm:"column:metric_unit"`
	StartValue   float64   `gorm:"column:start_value"`
	TargetValue  float64   `gorm:"column:target_value"`
	CurrentValue float64   `gorm:"column:current_value"`
	ProgressPct  float64   `gorm:"column:progress_pct"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName maps KeyResult to key_results.
func (KeyResult) TableName() string { return "key_results" }

// ProgressLog is the GORM model for goal_progress_logs (進捗更新履歴, 追記専用).
type ProgressLog struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	GoalID          uuid.UUID  `gorm:"column:goal_id"`
	KeyResultID     *uuid.UUID `gorm:"column:key_result_id"`
	ProgressPct     float64    `gorm:"column:progress_pct"`
	Comment         string     `gorm:"column:comment"`
	UpdatedByUserID *uuid.UUID `gorm:"column:updated_by_user_id"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

// TableName maps ProgressLog to goal_progress_logs.
func (ProgressLog) TableName() string { return "goal_progress_logs" }
