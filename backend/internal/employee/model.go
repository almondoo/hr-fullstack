// Package employee implements the employee (従業員マスタ), assignment (発令履歴),
// and employment contract (雇用契約) domains.
package employee

import (
	"time"

	"github.com/google/uuid"
)

// Employee is the GORM model for the employees table.
type Employee struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeCode   string     `gorm:"column:employee_code"`
	LastName       string     `gorm:"column:last_name"`
	FirstName      string     `gorm:"column:first_name"`
	Email          *string    `gorm:"column:email"`
	DepartmentID   *uuid.UUID `gorm:"column:department_id"`
	EmploymentType string     `gorm:"column:employment_type"`
	Status         string     `gorm:"column:status"`
	HiredOn        *time.Time `gorm:"column:hired_on"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Employee to the employees table.
func (Employee) TableName() string { return "employees" }

// Assignment is the GORM model for employee_assignments (発令履歴).
type Assignment struct {
	ID            uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID      uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID    uuid.UUID  `gorm:"column:employee_id"`
	DepartmentID  *uuid.UUID `gorm:"column:department_id"`
	Position      *string    `gorm:"column:position"`
	Grade         *string    `gorm:"column:grade"`
	EffectiveFrom time.Time  `gorm:"column:effective_from"`
	EffectiveTo   *time.Time `gorm:"column:effective_to"`
	Reason        *string    `gorm:"column:reason"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
}

// TableName maps Assignment to the employee_assignments table.
func (Assignment) TableName() string { return "employee_assignments" }

// Contract is the GORM model for employment_contracts (雇用契約).
type Contract struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID        uuid.UUID  `gorm:"column:employee_id"`
	ContractType      string     `gorm:"column:contract_type"`
	StartDate         time.Time  `gorm:"column:start_date"`
	EndDate           *time.Time `gorm:"column:end_date"`
	WorkingConditions []byte     `gorm:"column:working_conditions;type:jsonb"`
	Status            string     `gorm:"column:status"`
	SignedAt          *time.Time `gorm:"column:signed_at"`
	DocumentRef       *string    `gorm:"column:document_ref"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps Contract to the employment_contracts table.
func (Contract) TableName() string { return "employment_contracts" }
