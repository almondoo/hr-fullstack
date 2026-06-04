package employee

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// employeeTracer is the OTel tracer for domain-level employee operations.
// No PII (names, emails, employee codes) is recorded in span attributes.
var employeeTracer = otel.Tracer("github.com/your-org/hr-saas/internal/employee")

// Sentinel errors.
var (
	ErrNotFound         = errors.New("employee: not found")
	ErrContractNotFound = errors.New("employee: contract not found")
	// ErrInvalidTransition is returned when a requested contract status
	// transition is not permitted by the allowed-transitions table.
	ErrInvalidTransition = errors.New("employee: invalid contract status transition")
)

// allowedTransitions defines the set of legal contract status moves.
//
// Permitted transitions:
//   - draft      → active      (first signing)
//   - draft      → terminated  (cancel before activation)
//   - active     → expired     (natural end-of-term)
//   - active     → terminated  (early termination)
//
// Prohibited (rollback / idempotent):
//   - terminated → * (terminal state, no rollback)
//   - expired    → * (terminal state, no rollback)
//   - active     → active (re-signing not allowed; create a new contract)
//   - active     → draft (rollback not allowed)
var allowedTransitions = map[string]map[string]bool{
	"draft": {
		"active":     true,
		"terminated": true,
	},
	"active": {
		"expired":    true,
		"terminated": true,
	},
}

// isTransitionAllowed returns true when moving from currentStatus to nextStatus
// is listed in allowedTransitions.
func isTransitionAllowed(currentStatus, nextStatus string) bool {
	if next, ok := allowedTransitions[currentStatus]; ok {
		return next[nextStatus]
	}
	return false
}

// Service provides business logic for employee, assignment, and contract operations.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Employee CRUD
// ---------------------------------------------------------------------------

// CreateEmployeeInput holds validated fields for a new employee.
type CreateEmployeeInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	EmployeeCode   string
	LastName       string
	FirstName      string
	Email          *string
	DepartmentID   *uuid.UUID
	EmploymentType string
	Status         string
	HiredOn        *time.Time
	IP             *string
}

// CreateEmployee inserts a new employee and records an audit event.
// [Security: MUSTFIX 1] When DepartmentID is provided, it verifies that the
// department belongs to the same tenant before inserting — defence-in-depth
// on top of the composite FK constraint.
func (s *Service) CreateEmployee(ctx context.Context, in CreateEmployeeInput) (*Employee, error) {
	ctx, span := employeeTracer.Start(ctx, "employee.CreateEmployee")
	defer span.End()

	emp := Employee{
		ID:             uuid.New(),
		TenantID:       in.TenantID,
		EmployeeCode:   in.EmployeeCode,
		LastName:       in.LastName,
		FirstName:      in.FirstName,
		Email:          in.Email,
		DepartmentID:   in.DepartmentID,
		EmploymentType: in.EmploymentType,
		Status:         in.Status,
		HiredOn:        in.HiredOn,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// [Security: MUSTFIX 1] Verify department belongs to this tenant when
		// a department_id is supplied.
		if in.DepartmentID != nil {
			var deptCount int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM departments WHERE id = ? AND tenant_id = ?`,
				*in.DepartmentID, in.TenantID,
			).Scan(&deptCount).Error; err != nil {
				return fmt.Errorf("employee: create verify department: %w", err)
			}
			if deptCount == 0 {
				return ErrNotFound
			}
		}

		if err := tx.Exec(
			`INSERT INTO employees
			   (id, tenant_id, employee_code, last_name, first_name, email,
			    department_id, employment_type, status, hired_on)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			emp.ID, emp.TenantID, emp.EmployeeCode, emp.LastName, emp.FirstName,
			emp.Email, emp.DepartmentID, emp.EmploymentType, emp.Status, emp.HiredOn,
		).Error; err != nil {
			return fmt.Errorf("employee: create insert: %w", err)
		}

		idStr := emp.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "employee.created",
			ResourceType: "employee",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("employee: create audit: %w", err)
		}
		return nil
	}); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return &emp, nil
}

// GetEmployee fetches a single employee by ID within the tenant.
func (s *Service) GetEmployee(ctx context.Context, tenantID, id uuid.UUID) (*Employee, error) {
	var emp Employee
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_code, last_name, first_name, email,
			        department_id, employment_type, status, hired_on, created_at, updated_at
			 FROM employees
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&emp).Error
	})
	if err != nil {
		return nil, err
	}
	if emp.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &emp, nil
}

// ListEmployees returns all employees for a tenant.
func (s *Service) ListEmployees(ctx context.Context, tenantID uuid.UUID) ([]Employee, error) {
	var emps []Employee
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_code, last_name, first_name, email,
			        department_id, employment_type, status, hired_on, created_at, updated_at
			 FROM employees
			 WHERE tenant_id = ?
			 ORDER BY last_name, first_name`,
			tenantID,
		).Scan(&emps).Error
	})
	if err != nil {
		return nil, err
	}
	return emps, nil
}

// UpdateEmployeeInput holds validated fields for updating an employee.
type UpdateEmployeeInput struct {
	TenantID       uuid.UUID
	ID             uuid.UUID
	ActorID        uuid.UUID
	EmployeeCode   string
	LastName       string
	FirstName      string
	Email          *string
	DepartmentID   *uuid.UUID
	EmploymentType string
	Status         string
	HiredOn        *time.Time
	IP             *string
}

// UpdateEmployee modifies an existing employee and records an audit event.
func (s *Service) UpdateEmployee(ctx context.Context, in UpdateEmployeeInput) (*Employee, error) {
	var emp Employee
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE employees
			 SET employee_code = ?, last_name = ?, first_name = ?, email = ?,
			     department_id = ?, employment_type = ?, status = ?, hired_on = ?,
			     updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.EmployeeCode, in.LastName, in.FirstName, in.Email,
			in.DepartmentID, in.EmploymentType, in.Status, in.HiredOn,
			in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("employee: update exec: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_code, last_name, first_name, email,
			        department_id, employment_type, status, hired_on, created_at, updated_at
			 FROM employees WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&emp).Error; err != nil {
			return fmt.Errorf("employee: update re-read: %w", err)
		}

		idStr := in.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "employee.updated",
			ResourceType: "employee",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("employee: update audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &emp, nil
}

// DeleteEmployee removes an employee and records an audit event.
func (s *Service) DeleteEmployee(ctx context.Context, tenantID, id, actorID uuid.UUID, ip *string) error {
	return s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`DELETE FROM employees WHERE id = ? AND tenant_id = ?`,
			id, tenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("employee: delete exec: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := id.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &actorID,
			Action:       "employee.deleted",
			ResourceType: "employee",
			ResourceID:   &idStr,
			IP:           ip,
		}); err != nil {
			return fmt.Errorf("employee: delete audit: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Assignments (発令履歴)
// ---------------------------------------------------------------------------

// CreateAssignmentInput holds validated fields for a new assignment.
type CreateAssignmentInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	EmployeeID    uuid.UUID
	DepartmentID  *uuid.UUID
	Position      *string
	Grade         *string
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	Reason        *string
	IP            *string
}

// CreateAssignment inserts a new発令 record and records an audit event.
// [Security: MUSTFIX 1] Verifies both employee and (optional) department
// belong to the same tenant before inserting — defence-in-depth on top of
// the composite FK constraints.
func (s *Service) CreateAssignment(ctx context.Context, in CreateAssignmentInput) (*Assignment, error) {
	asgn := Assignment{
		ID:            uuid.New(),
		TenantID:      in.TenantID,
		EmployeeID:    in.EmployeeID,
		DepartmentID:  in.DepartmentID,
		Position:      in.Position,
		Grade:         in.Grade,
		EffectiveFrom: in.EffectiveFrom,
		EffectiveTo:   in.EffectiveTo,
		Reason:        in.Reason,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var count int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&count).Error; err != nil {
			return fmt.Errorf("employee: assignment verify employee: %w", err)
		}
		if count == 0 {
			return ErrNotFound
		}

		// [Security: MUSTFIX 1] Verify department belongs to this tenant when
		// a department_id is supplied.
		if in.DepartmentID != nil {
			var deptCount int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM departments WHERE id = ? AND tenant_id = ?`,
				*in.DepartmentID, in.TenantID,
			).Scan(&deptCount).Error; err != nil {
				return fmt.Errorf("employee: assignment verify department: %w", err)
			}
			if deptCount == 0 {
				return ErrNotFound
			}
		}

		if err := tx.Exec(
			`INSERT INTO employee_assignments
			   (id, tenant_id, employee_id, department_id, position, grade,
			    effective_from, effective_to, reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			asgn.ID, asgn.TenantID, asgn.EmployeeID, asgn.DepartmentID,
			asgn.Position, asgn.Grade, asgn.EffectiveFrom, asgn.EffectiveTo, asgn.Reason,
		).Error; err != nil {
			return fmt.Errorf("employee: assignment insert: %w", err)
		}

		idStr := asgn.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "assignment.created",
			ResourceType: "assignment",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("employee: assignment audit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &asgn, nil
}

// ListAssignments returns all assignments for an employee ordered by effective_from DESC.
func (s *Service) ListAssignments(ctx context.Context, tenantID, employeeID uuid.UUID) ([]Assignment, error) {
	var asgns []Assignment
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, department_id, position, grade,
			        effective_from, effective_to, reason, created_at
			 FROM employee_assignments
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY effective_from DESC`,
			tenantID, employeeID,
		).Scan(&asgns).Error
	})
	if err != nil {
		return nil, err
	}
	return asgns, nil
}

// ---------------------------------------------------------------------------
// Contracts (雇用契約)
// ---------------------------------------------------------------------------

// CreateContractInput holds validated fields for a new employment contract.
type CreateContractInput struct {
	TenantID          uuid.UUID
	ActorID           uuid.UUID
	EmployeeID        uuid.UUID
	ContractType      string
	StartDate         time.Time
	EndDate           *time.Time
	WorkingConditions []byte
	DocumentRef       *string
	IP                *string
}

// CreateContract inserts a new employment contract with status=draft.
func (s *Service) CreateContract(ctx context.Context, in CreateContractInput) (*Contract, error) {
	ctr := Contract{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		EmployeeID:        in.EmployeeID,
		ContractType:      in.ContractType,
		StartDate:         in.StartDate,
		EndDate:           in.EndDate,
		WorkingConditions: in.WorkingConditions,
		Status:            "draft",
		DocumentRef:       in.DocumentRef,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var count int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&count).Error; err != nil {
			return fmt.Errorf("employee: contract verify employee: %w", err)
		}
		if count == 0 {
			return ErrNotFound
		}

		wc := ctr.WorkingConditions
		if len(wc) == 0 {
			wc = []byte(`{}`)
		}

		if err := tx.Exec(
			`INSERT INTO employment_contracts
			   (id, tenant_id, employee_id, contract_type, start_date, end_date,
			    working_conditions, status, document_ref)
			 VALUES (?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?)`,
			ctr.ID, ctr.TenantID, ctr.EmployeeID, ctr.ContractType,
			ctr.StartDate, ctr.EndDate, wc, ctr.Status, ctr.DocumentRef,
		).Error; err != nil {
			return fmt.Errorf("employee: contract insert: %w", err)
		}

		idStr := ctr.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "contract.created",
			ResourceType: "contract",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("employee: contract audit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &ctr, nil
}

// GetContract fetches a single contract by ID within the tenant.
func (s *Service) GetContract(ctx context.Context, tenantID, id uuid.UUID) (*Contract, error) {
	var ctr Contract
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, contract_type, start_date, end_date,
			        working_conditions, status, signed_at, document_ref, created_at, updated_at
			 FROM employment_contracts
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&ctr).Error
	})
	if err != nil {
		return nil, err
	}
	if ctr.ID == uuid.Nil {
		return nil, ErrContractNotFound
	}
	return &ctr, nil
}

// ListContracts returns all contracts for an employee within the tenant.
func (s *Service) ListContracts(ctx context.Context, tenantID, employeeID uuid.UUID) ([]Contract, error) {
	var ctrs []Contract
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, contract_type, start_date, end_date,
			        working_conditions, status, signed_at, document_ref, created_at, updated_at
			 FROM employment_contracts
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY start_date DESC`,
			tenantID, employeeID,
		).Scan(&ctrs).Error
	})
	if err != nil {
		return nil, err
	}
	return ctrs, nil
}

// UpdateContractStatusInput holds validated fields for a contract status change.
type UpdateContractStatusInput struct {
	TenantID uuid.UUID
	ID       uuid.UUID
	ActorID  uuid.UUID
	Status   string
	IP       *string
}

// UpdateContractStatus changes the status of a contract with transition validation.
//
// [Security: MUSTFIX 2] Rules:
//   - Only transitions listed in allowedTransitions are accepted (409 otherwise).
//   - signed_at is set to now() only on the first draft→active transition.
//     If signed_at is already populated it is never overwritten (signature
//     date integrity).
//   - Terminal states (terminated, expired) cannot be rolled back.
func (s *Service) UpdateContractStatus(ctx context.Context, in UpdateContractStatusInput) (*Contract, error) {
	var ctr Contract
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Read the current contract to validate the transition.
		var current struct {
			Status   string     `gorm:"column:status"`
			SignedAt *time.Time `gorm:"column:signed_at"`
		}
		if err := tx.Raw(
			`SELECT status, signed_at FROM employment_contracts
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("employee: contract status read current: %w", err)
		}
		if current.Status == "" {
			// No row returned — treat as not found.
			return ErrContractNotFound
		}

		// Validate the transition.
		if !isTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		// signed_at: set only on the first draft→active transition.
		// If signed_at is already set, preserve it (do not overwrite).
		var res *gorm.DB
		if in.Status == "active" && current.SignedAt == nil {
			res = tx.Exec(
				`UPDATE employment_contracts
				 SET status = ?, signed_at = now(), updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.ID, in.TenantID,
			)
		} else {
			res = tx.Exec(
				`UPDATE employment_contracts
				 SET status = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.ID, in.TenantID,
			)
		}
		if res.Error != nil {
			return fmt.Errorf("employee: contract status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrContractNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, contract_type, start_date, end_date,
			        working_conditions, status, signed_at, document_ref, created_at, updated_at
			 FROM employment_contracts WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&ctr).Error; err != nil {
			return fmt.Errorf("employee: contract status re-read: %w", err)
		}

		idStr := in.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "contract.status_updated",
			ResourceType: "contract",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("employee: contract status audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ctr, nil
}
