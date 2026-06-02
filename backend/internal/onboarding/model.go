// Package onboarding implements the onboarding (入社手続き) and offboarding
// (退職手続き) domains.
//
// Features:
//   - LM-001: Onboarding task generation from templates, CRUD, status transitions.
//   - LM-003: Employee intake form (口座番号 = AES-256-GCM encrypted, JSON PII fields).
//   - LM-004: Offboarding task generation, employee status transitions, data
//     retention policy recording.  Physical deletion is never performed.
package onboarding

import (
	"time"

	"github.com/google/uuid"
)

// ChecklistTemplate is the GORM model for onboarding_checklist_templates.
type ChecklistTemplate struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID `gorm:"column:tenant_id"`
	Name      string    `gorm:"column:name"`
	Kind      string    `gorm:"column:kind"`
	ItemsJSON []byte    `gorm:"column:items_json;type:jsonb"`
	Active    bool      `gorm:"column:active"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps ChecklistTemplate to onboarding_checklist_templates.
func (ChecklistTemplate) TableName() string { return "onboarding_checklist_templates" }

// Task is the GORM model for onboarding_tasks.
type Task struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID     uuid.UUID  `gorm:"column:employee_id"`
	Kind           string     `gorm:"column:kind"`
	Title          string     `gorm:"column:title"`
	Category       string     `gorm:"column:category"`
	Status         string     `gorm:"column:status"`
	DueDate        *time.Time `gorm:"column:due_date"`
	AssigneeUserID *uuid.UUID `gorm:"column:assignee_user_id"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Task to onboarding_tasks.
func (Task) TableName() string { return "onboarding_tasks" }

// IntakeForm is the GORM model for employee_intake_forms.
//
// Security note on BankAccountEnc:
//   - This field holds the AES-256-GCM ciphertext of the bank account number.
//   - The plaintext is NEVER stored or returned to callers without the
//     intake:read_sensitive permission check.
//   - Callers that do not hold intake:read_sensitive receive a nil/omitted field.
type IntakeForm struct {
	ID                   uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID             uuid.UUID `gorm:"column:tenant_id"`
	EmployeeID           uuid.UUID `gorm:"column:employee_id"`
	EmergencyContactJSON []byte    `gorm:"column:emergency_contact_json;type:jsonb"`
	CommuteJSON          []byte    `gorm:"column:commute_json;type:jsonb"`
	DependentsJSON       []byte    `gorm:"column:dependents_json;type:jsonb"`
	// BankAccountEnc holds the encrypted bank account number ciphertext.
	// Use crypto.Decrypt to obtain plaintext; only do so when the caller
	// holds intake:read_sensitive permission.
	BankAccountEnc  []byte    `gorm:"column:bank_account_enc;type:bytea"`
	Status          string    `gorm:"column:status"`
	RetentionPolicy string    `gorm:"column:retention_policy"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps IntakeForm to employee_intake_forms.
func (IntakeForm) TableName() string { return "employee_intake_forms" }

// OffboardingPolicy is the GORM model for offboarding_policies.
type OffboardingPolicy struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID     uuid.UUID  `gorm:"column:employee_id"`
	RetentionLabel string     `gorm:"column:retention_label"`
	ExpiresOn      *time.Time `gorm:"column:expires_on"`
	RecordedBy     *uuid.UUID `gorm:"column:recorded_by"`
	Notes          *string    `gorm:"column:notes"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps OffboardingPolicy to offboarding_policies.
func (OffboardingPolicy) TableName() string { return "offboarding_policies" }
