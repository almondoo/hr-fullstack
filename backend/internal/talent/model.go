// Package talent implements the talent-management domain (ST-TM-04):
//
//   - TM-020 人材DB: skill master (skills), employee skills (employee_skills),
//     certifications (employee_certifications), and an aggregated integrated
//     profile view.
//   - TM-021 配置/異動シミュレーション: non-destructive placement drafts
//     (placement_simulations / placement_simulation_items) that are mapped to
//     employee_assignments (発令履歴) only on explicit apply.
//   - TM-022 エンゲージメント/パルスサーベイ: pulse surveys and responses with
//     anonymity and minimum-disclosure thresholds.  The free-text answer is
//     AES-256-GCM column-encrypted (要配慮個人情報に準じた厳格管理).
//
// Legal/privacy note: skill level definitions, minimum-disclosure thresholds,
// and certification expiry alert windows are tenant configuration, not
// hard-coded constants — they must follow the latest privacy policy and be
// confirmed by appropriate experts (社労士/弁護士/プライバシー担当).  This
// implementation is not legal advice.
package talent

import (
	"time"

	"github.com/google/uuid"
)

// Skill status / placement / survey constants.
const (
	// Placement simulation statuses.
	SimStatusDraft     = "draft"
	SimStatusApplied   = "applied"
	SimStatusDiscarded = "discarded"

	// Pulse survey statuses.
	SurveyStatusDraft  = "draft"
	SurveyStatusOpen   = "open"
	SurveyStatusClosed = "closed"
)

// Skill is the GORM model for skills (skill master).
type Skill struct {
	ID         uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID   uuid.UUID `gorm:"column:tenant_id"`
	Category   string    `gorm:"column:category"`
	Name       string    `gorm:"column:name"`
	LevelsJSON []byte    `gorm:"column:levels_json;type:jsonb"`
	Active     bool      `gorm:"column:active"`
	CreatedAt  time.Time `gorm:"column:created_at"`
	UpdatedAt  time.Time `gorm:"column:updated_at"`
}

// TableName maps Skill to the skills table.
func (Skill) TableName() string { return "skills" }

// EmployeeSkill is the GORM model for employee_skills (the skill-map entity).
type EmployeeSkill struct {
	ID         uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID   uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID uuid.UUID  `gorm:"column:employee_id"`
	SkillID    uuid.UUID  `gorm:"column:skill_id"`
	Level      int        `gorm:"column:level"`
	AcquiredOn *time.Time `gorm:"column:acquired_on"`
	ExpiresOn  *time.Time `gorm:"column:expires_on"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
}

// TableName maps EmployeeSkill to the employee_skills table.
func (EmployeeSkill) TableName() string { return "employee_skills" }

// Certification is the GORM model for employee_certifications.
type Certification struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID      uuid.UUID  `gorm:"column:employee_id"`
	Name            string     `gorm:"column:name"`
	Issuer          string     `gorm:"column:issuer"`
	AcquiredOn      *time.Time `gorm:"column:acquired_on"`
	ExpiresOn       *time.Time `gorm:"column:expires_on"`
	RenewalRequired bool       `gorm:"column:renewal_required"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps Certification to the employee_certifications table.
func (Certification) TableName() string { return "employee_certifications" }

// PlacementSimulation is the GORM model for placement_simulations (draft header).
type PlacementSimulation struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	Name            string     `gorm:"column:name"`
	Status          string     `gorm:"column:status"`
	CreatedByUserID *uuid.UUID `gorm:"column:created_by_user_id"`
	AppliedAt       *time.Time `gorm:"column:applied_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps PlacementSimulation to placement_simulations.
func (PlacementSimulation) TableName() string { return "placement_simulations" }

// PlacementSimulationItem is the GORM model for placement_simulation_items.
type PlacementSimulationItem struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	SimulationID       uuid.UUID  `gorm:"column:simulation_id"`
	EmployeeID         uuid.UUID  `gorm:"column:employee_id"`
	TargetDepartmentID *uuid.UUID `gorm:"column:target_department_id"`
	TargetPosition     *string    `gorm:"column:target_position"`
	TargetGrade        *string    `gorm:"column:target_grade"`
	EffectiveFrom      time.Time  `gorm:"column:effective_from"`
	Reason             *string    `gorm:"column:reason"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
}

// TableName maps PlacementSimulationItem to placement_simulation_items.
func (PlacementSimulationItem) TableName() string { return "placement_simulation_items" }

// PulseSurvey is the GORM model for pulse_surveys.
type PulseSurvey struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	Title              string     `gorm:"column:title"`
	QuestionsJSON      []byte     `gorm:"column:questions_json;type:jsonb"`
	Anonymous          bool       `gorm:"column:anonymous"`
	MinResponsesToShow int        `gorm:"column:min_responses_to_show"`
	StartsOn           *time.Time `gorm:"column:starts_on"`
	EndsOn             *time.Time `gorm:"column:ends_on"`
	Status             string     `gorm:"column:status"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps PulseSurvey to pulse_surveys.
func (PulseSurvey) TableName() string { return "pulse_surveys" }

// PulseSurveyResponse is the GORM model for pulse_survey_responses.
//
// Security note on FreeText:
//   - FreeText holds the AES-256-GCM ciphertext of the free-text answer.
//   - The plaintext is NEVER stored or returned to callers without the
//     survey:read_freetext permission (verified again in the service layer).
//   - RespondentEmployeeID is NULL for anonymous surveys so that responses can
//     never be reverse-linked to an individual.
type PulseSurveyResponse struct {
	ID                   uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID             uuid.UUID  `gorm:"column:tenant_id"`
	SurveyID             uuid.UUID  `gorm:"column:survey_id"`
	RespondentEmployeeID *uuid.UUID `gorm:"column:respondent_employee_id"`
	AnswersJSON          []byte     `gorm:"column:answers_json;type:jsonb"`
	// FreeText holds the encrypted free-text answer ciphertext.  Use
	// crypto.Decrypt only when the caller holds survey:read_freetext.
	FreeText    []byte    `gorm:"column:free_text;type:bytea"`
	SubmittedAt time.Time `gorm:"column:submitted_at"`
}

// TableName maps PulseSurveyResponse to pulse_survey_responses.
func (PulseSurveyResponse) TableName() string { return "pulse_survey_responses" }

// ---------------------------------------------------------------------------
// Aggregated view shapes (not GORM-mapped tables)
// ---------------------------------------------------------------------------

// AssignmentSummary is one發令履歴 row used in the integrated profile.
type AssignmentSummary struct {
	ID            uuid.UUID  `gorm:"column:id"`
	DepartmentID  *uuid.UUID `gorm:"column:department_id"`
	Position      *string    `gorm:"column:position"`
	Grade         *string    `gorm:"column:grade"`
	EffectiveFrom time.Time  `gorm:"column:effective_from"`
	EffectiveTo   *time.Time `gorm:"column:effective_to"`
}

// IntegratedProfile aggregates an employee's basic info, assignment history,
// and held skills.  Sensitive/compensation-related fields (grade) are masked
// when the viewer lacks talent:read_sensitive.
type IntegratedProfile struct {
	EmployeeID   uuid.UUID
	EmployeeCode string
	LastName     string
	FirstName    string
	Status       string
	DepartmentID *uuid.UUID
	Assignments  []AssignmentSummary
	Skills       []EmployeeSkill
	// SensitiveMasked reports whether compensation/grade fields were masked
	// because the viewer lacked talent:read_sensitive.
	SensitiveMasked bool
}

// OrgNode is a node in the department org tree (組織図ビュー).
type OrgNode struct {
	DepartmentID  uuid.UUID  `json:"department_id"`
	ParentID      *uuid.UUID `json:"parent_id,omitempty"`
	Name          string     `json:"name"`
	Code          string     `json:"code"`
	EmployeeCount int        `json:"employee_count"`
	Children      []*OrgNode `json:"children"`
}

// SkillMatrixCell is one (department, skill) aggregation cell.
type SkillMatrixCell struct {
	DepartmentID *uuid.UUID `gorm:"column:department_id"`
	SkillID      uuid.UUID  `gorm:"column:skill_id"`
	SkillName    string     `gorm:"column:skill_name"`
	HolderCount  int        `gorm:"column:holder_count"`
	AvgLevel     float64    `gorm:"column:avg_level"`
}

// SkillHolder is one employee returned by a skill-holder search.
type SkillHolder struct {
	EmployeeID   uuid.UUID `gorm:"column:employee_id"`
	EmployeeCode string    `gorm:"column:employee_code"`
	Level        int       `gorm:"column:level"`
}

// SurveyAggregate is the aggregation result for a survey, honouring the
// minimum-disclosure threshold.
type SurveyAggregate struct {
	SurveyID      uuid.UUID
	ResponseCount int
	// Suppressed is true when ResponseCount < min_responses_to_show, in which
	// case AnswerSummary is withheld for privacy.
	Suppressed bool
	// AnswerSummary is a per-question numeric aggregation (only populated when
	// not suppressed).  Free-text answers are never aggregated here.
	AnswerSummary map[string]float64
}
