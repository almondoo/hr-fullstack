// Package hiring implements ST-ATS-06: onboarding linkage between the ATS
// (採用) domain and the LM (労務) domain.
//
// Features:
//   - ATS-020: Candidate → employee master generation (idempotent conversion)
//     with provenance trace in applicant_employee_links.
//   - ATS-021: New-hire onboarding header (new_hire_onboardings) plus bulk
//     generation of onboarding_tasks from a per-department checklist template
//     (reuses the existing onboarding assets — see internal/onboarding) in the
//     SAME transaction (孤児防止 / single-tx atomicity).
//   - ATS-022: Preboarding IT requests (preboarding_requests).
//   - ATS-023: Post-hire follow-up surveys / early-attrition alerting
//     (onboarding_surveys) — a minimal scheduling stub; survey answer bodies
//     and predictive analytics are intentionally Future work.
//
// Cross-story note: applicant (ST-ATS-02) and offer (ST-ATS-05) are referenced
// by bare uuid columns only (no FK / no import); employees, departments and
// onboarding_checklist_templates are existing stable assets referenced via
// composite FKs.
package hiring

import (
	"time"

	"github.com/google/uuid"
)

// New-hire onboarding lifecycle statuses (new_hire_onboardings.status).
const (
	OnboardingStatusOfferAccepted = "offer_accepted"
	OnboardingStatusPreboarding   = "preboarding"
	OnboardingStatusOnboarding    = "onboarding"
	OnboardingStatusCompleted     = "completed"
)

// Preboarding request types and statuses.
const (
	RequestTypeAccount   = "account"
	RequestTypeEquipment = "equipment"
	RequestTypeAccess    = "access"
	RequestTypeOther     = "other"

	RequestStatusRequested  = "requested"
	RequestStatusInProgress = "in_progress"
	RequestStatusCompleted  = "completed"
	RequestStatusCancelled  = "cancelled"
)

// Onboarding survey types and statuses.
const (
	SurveyTypeOnboarding30d  = "onboarding_30d"
	SurveyTypeOnboarding90d  = "onboarding_90d"
	SurveyTypeEarlyAttrition = "early_attrition"

	SurveyStatusScheduled = "scheduled"
	SurveyStatusSent      = "sent"
	SurveyStatusResponded = "responded"
	SurveyStatusCancelled = "cancelled"
)

// Link is the GORM model for applicant_employee_links.
//
// It records the provenance of a candidate→employee conversion and enforces
// idempotency (UNIQUE(tenant_id, applicant_id) / UNIQUE(tenant_id, employee_id))
// so that the same candidate can never generate two employees.
type Link struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	ApplicantID uuid.UUID  `gorm:"column:applicant_id"`
	OfferID     *uuid.UUID `gorm:"column:offer_id"`
	EmployeeID  uuid.UUID  `gorm:"column:employee_id"`
	ConvertedAt time.Time  `gorm:"column:converted_at"`
	ConvertedBy *uuid.UUID `gorm:"column:converted_by"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps Link to applicant_employee_links.
func (Link) TableName() string { return "applicant_employee_links" }

// NewHireOnboarding is the GORM model for new_hire_onboardings.
type NewHireOnboarding struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID        uuid.UUID  `gorm:"column:employee_id"`
	ApplicantID       uuid.UUID  `gorm:"column:applicant_id"`
	DepartmentID      *uuid.UUID `gorm:"column:department_id"`
	TemplateID        *uuid.UUID `gorm:"column:template_id"`
	Status            string     `gorm:"column:status"`
	ExpectedStartDate *time.Time `gorm:"column:expected_start_date"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps NewHireOnboarding to new_hire_onboardings.
func (NewHireOnboarding) TableName() string { return "new_hire_onboardings" }

// PreboardingRequest is the GORM model for preboarding_requests.
type PreboardingRequest struct {
	ID                  uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id"`
	NewHireOnboardingID uuid.UUID  `gorm:"column:new_hire_onboarding_id"`
	RequestType         string     `gorm:"column:request_type"`
	Status              string     `gorm:"column:status"`
	AssigneeUserID      *uuid.UUID `gorm:"column:assignee_user_id"`
	Notes               *string    `gorm:"column:notes"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

// TableName maps PreboardingRequest to preboarding_requests.
func (PreboardingRequest) TableName() string { return "preboarding_requests" }

// Survey is the GORM model for onboarding_surveys.
//
// MVP stub: holds only a schedule slot + status.  No survey answer body or PII
// is stored here; predictive early-attrition analytics is Future work.
type Survey struct {
	ID                  uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id"`
	NewHireOnboardingID uuid.UUID  `gorm:"column:new_hire_onboarding_id"`
	EmployeeID          uuid.UUID  `gorm:"column:employee_id"`
	SurveyType          string     `gorm:"column:survey_type"`
	ScheduledOn         *time.Time `gorm:"column:scheduled_on"`
	Status              string     `gorm:"column:status"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

// TableName maps Survey to onboarding_surveys.
func (Survey) TableName() string { return "onboarding_surveys" }
