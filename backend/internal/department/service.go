package department

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// ErrNotFound is returned when a requested department does not exist within the
// caller's tenant.
var ErrNotFound = errors.New("department: not found")

// Service provides business logic for department operations.
// All DB access is routed through tdb.WithinTenant to enforce RLS.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateInput holds validated fields for a new department.
type CreateInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ParentID *uuid.UUID
	Name     string
	Code     string
	IP       *string
}

// Create inserts a new department and records an audit event.
func (s *Service) Create(ctx context.Context, in CreateInput) (*Department, error) {
	dept := Department{
		ID:       uuid.New(),
		TenantID: in.TenantID,
		ParentID: in.ParentID,
		Name:     in.Name,
		Code:     in.Code,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO departments (id, tenant_id, parent_id, name, code)
			 VALUES (?, ?, ?, ?, ?)`,
			dept.ID, dept.TenantID, dept.ParentID, dept.Name, dept.Code,
		).Error; err != nil {
			return fmt.Errorf("department: create insert: %w", err)
		}

		idStr := dept.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "department.created",
			ResourceType: "department",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("department: create audit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &dept, nil
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

// Get fetches a single department by ID within the tenant.
func (s *Service) Get(ctx context.Context, tenantID, id uuid.UUID) (*Department, error) {
	var dept Department
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, parent_id, name, code, created_at, updated_at
			 FROM departments
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&dept).Error
	})
	if err != nil {
		return nil, err
	}
	if dept.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &dept, nil
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

// List returns all departments for a tenant.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID) ([]Department, error) {
	var depts []Department
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, parent_id, name, code, created_at, updated_at
			 FROM departments
			 WHERE tenant_id = ?
			 ORDER BY name`,
			tenantID,
		).Scan(&depts).Error
	})
	if err != nil {
		return nil, err
	}
	return depts, nil
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

// UpdateInput holds validated fields for updating a department.
type UpdateInput struct {
	TenantID uuid.UUID
	ID       uuid.UUID
	ActorID  uuid.UUID
	ParentID *uuid.UUID
	Name     string
	Code     string
	IP       *string
}

// Update modifies an existing department and records an audit event.
func (s *Service) Update(ctx context.Context, in UpdateInput) (*Department, error) {
	var dept Department
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE departments
			 SET parent_id = ?, name = ?, code = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.ParentID, in.Name, in.Code, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("department: update exec: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, parent_id, name, code, created_at, updated_at
			 FROM departments WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&dept).Error; err != nil {
			return fmt.Errorf("department: update re-read: %w", err)
		}

		idStr := in.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "department.updated",
			ResourceType: "department",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("department: update audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &dept, nil
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

// Delete removes a department and records an audit event.
func (s *Service) Delete(ctx context.Context, tenantID, id, actorID uuid.UUID, ip *string) error {
	return s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`DELETE FROM departments WHERE id = ? AND tenant_id = ?`,
			id, tenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("department: delete exec: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := id.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &actorID,
			Action:       "department.deleted",
			ResourceType: "department",
			ResourceID:   &idStr,
			IP:           ip,
		}); err != nil {
			return fmt.Errorf("department: delete audit: %w", err)
		}
		return nil
	})
}
