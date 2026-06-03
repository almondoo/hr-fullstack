// Package evaluation implements the talent-management performance review
// workflow (ST-TM-02).
//
// Features:
//   - TM-002: Multi-stage review workflow (self → primary → secondary →
//     calibrated → confirmed) on a review template, integrated with the
//     approval engine for submission / approval / return.
//   - TM-003: 360-degree review (rater invitations, anonymous aggregation) and
//     competency / grade linkage via template items_json.
//   - TM-004: Calibration (評価会議) — adjusted ratings recorded while the
//     original final_rating is preserved immutably.
//
// 制度 / legal note: evaluation scales, item weights, and grade mappings are
// company-specific and stored in JSONB (rating_scale_json / items_json) — never
// hardcoded.  Reflecting results into compensation requires社労士 review and
// 就業規則 alignment.  This implementation is not legal advice.
package evaluation

import (
	"time"

	"github.com/google/uuid"
)

// Stage values for review_entries.stage and template stage definitions.
const (
	StageSelf      = "self"
	StagePrimary   = "primary"
	StageSecondary = "secondary"
	Stage360       = "360"
)

// Review status values (the FSM state for a review header).
const (
	StatusNotStarted         = "not_started"
	StatusSelfSubmitted      = "self_submitted"
	StatusPrimarySubmitted   = "primary_submitted"
	StatusSecondarySubmitted = "secondary_submitted"
	StatusCalibrated         = "calibrated"
	StatusConfirmed          = "confirmed"
)

// 360 request status values.
const (
	Req360Pending   = "pending"
	Req360Submitted = "submitted"
	Req360Declined  = "declined"
)

// Calibration session status values.
const (
	CalibrationOpen   = "open"
	CalibrationClosed = "closed"
)

// Template is the GORM model for review_templates.
type Template struct {
	ID              uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID `gorm:"column:tenant_id"`
	Name            string    `gorm:"column:name"`
	StagesJSON      []byte    `gorm:"column:stages_json;type:jsonb"`
	ItemsJSON       []byte    `gorm:"column:items_json;type:jsonb"`
	RatingScaleJSON []byte    `gorm:"column:rating_scale_json;type:jsonb"`
	Active          bool      `gorm:"column:active"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps Template to review_templates.
func (Template) TableName() string { return "review_templates" }

// Review is the GORM model for reviews (the evaluation header).
//
// final_rating is the confirmed weighted score; it is immutable once set.
// adjusted_rating holds the calibration-adjusted value (nullable) while the
// original final_rating is preserved.
type Review struct {
	ID                  uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id"`
	CycleID             uuid.UUID  `gorm:"column:cycle_id"`
	TemplateID          uuid.UUID  `gorm:"column:template_id"`
	EmployeeID          uuid.UUID  `gorm:"column:employee_id"`
	PrimaryReviewerID   *uuid.UUID `gorm:"column:primary_reviewer_id"`
	SecondaryReviewerID *uuid.UUID `gorm:"column:secondary_reviewer_id"`
	Status              string     `gorm:"column:status"`
	FinalRating         *float64   `gorm:"column:final_rating"`
	AdjustedRating      *float64   `gorm:"column:adjusted_rating"`
	ConfirmedAt         *time.Time `gorm:"column:confirmed_at"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

// TableName maps Review to reviews.
func (Review) TableName() string { return "reviews" }

// Entry is the GORM model for review_entries (a per-item answer).
//
// Security note on Comment:
//   - The comment field carries potentially sensitive free text and is NEVER
//     written to audit logs or notification payloads.
//   - ReviewerUserID is suppressed from aggregation responses for anonymous 360
//     raters (see Service.Aggregate360).
type Entry struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	ReviewID       uuid.UUID  `gorm:"column:review_id"`
	Stage          string     `gorm:"column:stage"`
	ReviewerUserID *uuid.UUID `gorm:"column:reviewer_user_id"`
	ItemKey        string     `gorm:"column:item_key"`
	Score          *float64   `gorm:"column:score"`
	Comment        *string    `gorm:"column:comment"`
	SubmittedAt    *time.Time `gorm:"column:submitted_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Entry to review_entries.
func (Entry) TableName() string { return "review_entries" }

// Request360 is the GORM model for review_360_requests.
//
// When Anonymous is true, RaterEmployeeID must NOT be exposed in any
// aggregation API, audit log, or JSON response.
type Request360 struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	ReviewID        uuid.UUID  `gorm:"column:review_id"`
	RaterEmployeeID uuid.UUID  `gorm:"column:rater_employee_id"`
	Relationship    string     `gorm:"column:relationship"`
	Anonymous       bool       `gorm:"column:anonymous"`
	Status          string     `gorm:"column:status"`
	RespondedAt     *time.Time `gorm:"column:responded_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps Request360 to review_360_requests.
func (Request360) TableName() string { return "review_360_requests" }

// CalibrationSession is the GORM model for calibration_sessions.
type CalibrationSession struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	CycleID           uuid.UUID  `gorm:"column:cycle_id"`
	Name              string     `gorm:"column:name"`
	FacilitatorUserID *uuid.UUID `gorm:"column:facilitator_user_id"`
	Status            string     `gorm:"column:status"`
	DecisionsJSON     []byte     `gorm:"column:decisions_json;type:jsonb"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps CalibrationSession to calibration_sessions.
func (CalibrationSession) TableName() string { return "calibration_sessions" }
