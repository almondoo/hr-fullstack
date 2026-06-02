// Package department implements the department (組織マスタ) domain.
// It provides CRUD for departments with hierarchical parent_id support.
package department

import (
	"time"

	"github.com/google/uuid"
)

// Department is the GORM model for the departments table.
type Department struct {
	ID        uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID  `gorm:"column:tenant_id"`
	ParentID  *uuid.UUID `gorm:"column:parent_id"`
	Name      string     `gorm:"column:name"`
	Code      string     `gorm:"column:code"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

// TableName maps Department to the departments table.
func (Department) TableName() string { return "departments" }
