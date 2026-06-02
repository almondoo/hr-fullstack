package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("onboarding: not found")
	ErrInvalidTransition = errors.New("onboarding: invalid task status transition")
	ErrAlreadyExists     = errors.New("onboarding: record already exists")
	ErrForbidden         = errors.New("onboarding: permission denied")
)

// allowedTaskTransitions defines legal task status moves.
// Terminal states: done, skipped — no transitions out.
var allowedTaskTransitions = map[string]map[string]bool{
	"pending": {
		"in_progress": true,
		"skipped":     true,
	},
	"in_progress": {
		"done":    true,
		"skipped": true,
		"pending": true, // allow reset to pending
	},
}

// allowedEmployeeStatusTransitions defines legal employee status moves for
// the offboarding lifecycle (active → leaving → left).
// These extend the general employee status machine; only offboarding
// transitions are encoded here — the general ones live in employee/service.go.
var allowedEmployeeStatusTransitions = map[string]map[string]bool{
	"active": {
		"leaving": true,
	},
	"leaving": {
		"left": true,
	},
}

// isTaskTransitionAllowed reports whether moving a task from current → next is valid.
func isTaskTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedTaskTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// isEmployeeStatusTransitionAllowed reports whether the employee status move is allowed.
func isEmployeeStatusTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedEmployeeStatusTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// templateItem is the shape of each element in a checklist template's items_json.
type templateItem struct {
	Title         string `json:"title"`
	Category      string `json:"category"`
	DueOffsetDays int    `json:"due_offset_days"`
}

// Service provides business logic for onboarding and offboarding operations.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Checklist Templates
// ---------------------------------------------------------------------------

// CreateTemplateInput holds fields for creating a checklist template.
type CreateTemplateInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	Name      string
	Kind      string
	ItemsJSON []byte
	IP        *string
}

// CreateTemplate creates a new checklist template.
func (s *Service) CreateTemplate(ctx context.Context, in CreateTemplateInput) (*ChecklistTemplate, error) {
	tmpl := ChecklistTemplate{
		ID:        uuid.New(),
		TenantID:  in.TenantID,
		Name:      in.Name,
		Kind:      in.Kind,
		ItemsJSON: in.ItemsJSON,
		Active:    true,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO onboarding_checklist_templates
			   (id, tenant_id, name, kind, items_json, active)
			 VALUES (?, ?, ?, ?, ?::jsonb, ?)`,
			tmpl.ID, tmpl.TenantID, tmpl.Name, tmpl.Kind,
			tmpl.ItemsJSON, tmpl.Active,
		).Error; err != nil {
			return fmt.Errorf("onboarding: create template insert: %w", err)
		}
		idStr := tmpl.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "onboarding_template.created",
			ResourceType: "onboarding_checklist_template",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// GetTemplate fetches a checklist template by ID within the tenant.
func (s *Service) GetTemplate(ctx context.Context, tenantID, id uuid.UUID) (*ChecklistTemplate, error) {
	var tmpl ChecklistTemplate
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, name, kind, items_json, active, created_at, updated_at
			 FROM onboarding_checklist_templates
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&tmpl).Error
	})
	if err != nil {
		return nil, err
	}
	if tmpl.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &tmpl, nil
}

// ListTemplates returns all active templates for a tenant and kind.
func (s *Service) ListTemplates(ctx context.Context, tenantID uuid.UUID, kind string) ([]ChecklistTemplate, error) {
	var tmpls []ChecklistTemplate
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, name, kind, items_json, active, created_at, updated_at
			  FROM onboarding_checklist_templates
			  WHERE tenant_id = ? AND active = true`
		args := []any{tenantID}
		if kind != "" {
			q += ` AND kind = ?`
			args = append(args, kind)
		}
		q += ` ORDER BY name`
		return tx.Raw(q, args...).Scan(&tmpls).Error
	})
	if err != nil {
		return nil, err
	}
	return tmpls, nil
}

// ---------------------------------------------------------------------------
// Tasks — LM-001 (onboarding) / LM-004 (offboarding)
// ---------------------------------------------------------------------------

// GenerateTasksInput holds parameters for bulk task generation from a template.
type GenerateTasksInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	TemplateID uuid.UUID
	// BaseDate is the reference date for computing due dates from DueOffsetDays.
	// Typically the hire date (onboarding) or last working day (offboarding).
	BaseDate time.Time
	IP       *string
}

// GenerateTasks creates onboarding_tasks in bulk from a checklist template.
// All tasks are inserted in a single transaction together with the audit record.
// The employee must belong to the same tenant.
func (s *Service) GenerateTasks(ctx context.Context, in GenerateTasksInput) ([]Task, error) {
	var tasks []Task

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("onboarding: generate tasks verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Fetch the template.
		var tmpl ChecklistTemplate
		if err := tx.Raw(
			`SELECT id, tenant_id, kind, items_json FROM onboarding_checklist_templates
			 WHERE id = ? AND tenant_id = ? AND active = true LIMIT 1`,
			in.TemplateID, in.TenantID,
		).Scan(&tmpl).Error; err != nil {
			return fmt.Errorf("onboarding: generate tasks fetch template: %w", err)
		}
		if tmpl.ID == uuid.Nil {
			return ErrNotFound
		}

		var items []templateItem
		if err := json.Unmarshal(tmpl.ItemsJSON, &items); err != nil {
			return fmt.Errorf("onboarding: generate tasks parse items_json: %w", err)
		}

		for _, item := range items {
			t := Task{
				ID:         uuid.New(),
				TenantID:   in.TenantID,
				EmployeeID: in.EmployeeID,
				Kind:       tmpl.Kind,
				Title:      item.Title,
				Category:   item.Category,
				Status:     "pending",
			}
			if item.DueOffsetDays != 0 {
				d := in.BaseDate.AddDate(0, 0, item.DueOffsetDays)
				t.DueDate = &d
			}

			if err := tx.Exec(
				`INSERT INTO onboarding_tasks
				   (id, tenant_id, employee_id, kind, title, category, status, due_date)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, t.TenantID, t.EmployeeID, t.Kind, t.Title, t.Category, t.Status, t.DueDate,
			).Error; err != nil {
				return fmt.Errorf("onboarding: generate tasks insert: %w", err)
			}
			tasks = append(tasks, t)
		}

		idStr := in.TemplateID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "onboarding_tasks.generated",
			ResourceType: "onboarding_checklist_template",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// ListTasks returns tasks for an employee filtered by optional kind.
func (s *Service) ListTasks(ctx context.Context, tenantID, employeeID uuid.UUID, kind string) ([]Task, error) {
	var tasks []Task
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, employee_id, kind, title, category, status,
			         due_date, assignee_user_id, completed_at, created_at, updated_at
			  FROM onboarding_tasks
			  WHERE tenant_id = ? AND employee_id = ?`
		args := []any{tenantID, employeeID}
		if kind != "" {
			q += ` AND kind = ?`
			args = append(args, kind)
		}
		q += ` ORDER BY due_date NULLS LAST, created_at`
		return tx.Raw(q, args...).Scan(&tasks).Error
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetTask fetches a single task by ID within the tenant.
func (s *Service) GetTask(ctx context.Context, tenantID, id uuid.UUID) (*Task, error) {
	var task Task
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, kind, title, category, status,
			        due_date, assignee_user_id, completed_at, created_at, updated_at
			 FROM onboarding_tasks WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&task).Error
	})
	if err != nil {
		return nil, err
	}
	if task.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &task, nil
}

// UpdateTaskStatusInput holds fields for a task status transition.
type UpdateTaskStatusInput struct {
	TenantID uuid.UUID
	ID       uuid.UUID
	ActorID  uuid.UUID
	Status   string
	IP       *string
}

// UpdateTaskStatus transitions a task to the given status.
// Only allow-listed transitions are accepted (ErrInvalidTransition otherwise).
// Completing a task sets completed_at = now().
func (s *Service) UpdateTaskStatus(ctx context.Context, in UpdateTaskStatusInput) (*Task, error) {
	var task Task
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM onboarding_tasks WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("onboarding: task status read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isTaskTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		var res *gorm.DB
		if in.Status == "done" {
			res = tx.Exec(
				`UPDATE onboarding_tasks
				 SET status = ?, completed_at = now(), updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.ID, in.TenantID,
			)
		} else {
			res = tx.Exec(
				`UPDATE onboarding_tasks
				 SET status = ?, completed_at = NULL, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.Status, in.ID, in.TenantID,
			)
		}
		if res.Error != nil {
			return fmt.Errorf("onboarding: task status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, kind, title, category, status,
			        due_date, assignee_user_id, completed_at, created_at, updated_at
			 FROM onboarding_tasks WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&task).Error; err != nil {
			return fmt.Errorf("onboarding: task status re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "onboarding_task.status_updated",
			ResourceType: "onboarding_task",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// AssignTaskInput holds fields for assigning a task to a user.
type AssignTaskInput struct {
	TenantID       uuid.UUID
	ID             uuid.UUID
	ActorID        uuid.UUID
	AssigneeUserID *uuid.UUID
	IP             *string
}

// AssignTask sets the assignee for a task.
// The assignee user (when non-nil) must belong to the same tenant.
func (s *Service) AssignTask(ctx context.Context, in AssignTaskInput) (*Task, error) {
	var task Task
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify assignee belongs to the tenant when specified.
		if in.AssigneeUserID != nil {
			var userCount int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
				*in.AssigneeUserID, in.TenantID,
			).Scan(&userCount).Error; err != nil {
				return fmt.Errorf("onboarding: assign task verify user: %w", err)
			}
			if userCount == 0 {
				return ErrNotFound
			}
		}

		res := tx.Exec(
			`UPDATE onboarding_tasks
			 SET assignee_user_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.AssigneeUserID, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("onboarding: assign task update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, kind, title, category, status,
			        due_date, assignee_user_id, completed_at, created_at, updated_at
			 FROM onboarding_tasks WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&task).Error; err != nil {
			return fmt.Errorf("onboarding: assign task re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "onboarding_task.assigned",
			ResourceType: "onboarding_task",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// ---------------------------------------------------------------------------
// Intake Forms — LM-003
// ---------------------------------------------------------------------------

// SubmitIntakeFormInput holds fields for submitting an employee intake form.
//
// BankAccountPlaintext contains the bank account number in plaintext.
// It is encrypted with AES-256-GCM before storage; the plaintext is NEVER
// persisted to the database.
type SubmitIntakeFormInput struct {
	TenantID             uuid.UUID
	ActorID              uuid.UUID
	EmployeeID           uuid.UUID
	EmergencyContactJSON []byte
	CommuteJSON          []byte
	DependentsJSON       []byte
	// BankAccountPlaintext is the unencrypted bank account number.
	// This field MUST NOT be logged, written to audit records, or persisted
	// as plaintext.  It is encrypted immediately before the INSERT.
	BankAccountPlaintext []byte
	IP                   *string
}

// SubmitIntakeForm creates or updates an employee intake form.
// BankAccountPlaintext is encrypted before storage; the plaintext is never
// persisted.  Audit records contain only the form ID (opaque UUID), not any
// PII or decrypted values.
func (s *Service) SubmitIntakeForm(ctx context.Context, in SubmitIntakeFormInput) (*IntakeForm, error) {
	// Encrypt the bank account number BEFORE opening the transaction so that
	// any crypto error fails fast without acquiring DB resources.
	// Security: the plaintext value does not appear in any error message.
	var bankAccountEnc []byte
	if len(in.BankAccountPlaintext) > 0 {
		var err error
		bankAccountEnc, err = crypto.Encrypt(in.BankAccountPlaintext)
		if err != nil {
			return nil, fmt.Errorf("onboarding: intake form encrypt bank account: %w", err)
		}
	}

	form := IntakeForm{
		ID:                   uuid.New(),
		TenantID:             in.TenantID,
		EmployeeID:           in.EmployeeID,
		EmergencyContactJSON: in.EmergencyContactJSON,
		CommuteJSON:          in.CommuteJSON,
		DependentsJSON:       in.DependentsJSON,
		BankAccountEnc:       bankAccountEnc,
		Status:               "submitted",
		RetentionPolicy:      "7years",
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("onboarding: submit intake verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Upsert: ON CONFLICT (employee_id, tenant_id) DO UPDATE so that
		// re-submission updates the existing form rather than creating a duplicate.
		if err := tx.Exec(
			`INSERT INTO employee_intake_forms
			   (id, tenant_id, employee_id, emergency_contact_json, commute_json,
			    dependents_json, bank_account_enc, status)
			 VALUES (?, ?, ?, ?::jsonb, ?::jsonb, ?::jsonb, ?, ?)
			 ON CONFLICT (employee_id, tenant_id) DO UPDATE
			   SET emergency_contact_json = EXCLUDED.emergency_contact_json,
			       commute_json           = EXCLUDED.commute_json,
			       dependents_json        = EXCLUDED.dependents_json,
			       bank_account_enc       = EXCLUDED.bank_account_enc,
			       status                 = EXCLUDED.status,
			       updated_at             = now()
			 RETURNING id`,
			form.ID, form.TenantID, form.EmployeeID,
			form.EmergencyContactJSON, form.CommuteJSON, form.DependentsJSON,
			form.BankAccountEnc, form.Status,
		).Error; err != nil {
			return fmt.Errorf("onboarding: submit intake insert: %w", err)
		}

		// Re-read to get the actual persisted row (handles upsert case).
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, emergency_contact_json, commute_json,
			        dependents_json, bank_account_enc, status, retention_policy,
			        created_at, updated_at
			 FROM employee_intake_forms
			 WHERE employee_id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&form).Error; err != nil {
			return fmt.Errorf("onboarding: submit intake re-read: %w", err)
		}

		// Audit: record only the opaque form ID — never PII or decrypted values.
		idStr := form.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "intake_form.submitted",
			ResourceType: "employee_intake_form",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers of SubmitIntakeForm.
	form.BankAccountEnc = nil
	return &form, nil
}

// GetIntakeFormInput holds parameters for fetching an intake form.
// ReadSensitive must be true when the caller holds intake:read_sensitive;
// when false, BankAccountEnc is cleared from the returned form.
type GetIntakeFormInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	EmployeeID    uuid.UUID
	ReadSensitive bool
	IP            *string
}

// GetIntakeForm fetches the intake form for an employee.
//
// Multi-layer permission enforcement for ReadSensitive:
//   - Layer 1 (HTTP): the handler route requires intake:read_sensitive via
//     RequirePermission middleware before setting ReadSensitive=true.
//   - Layer 2 (Service, this function): when ReadSensitive is true, the
//     service re-validates intake:read_sensitive by calling
//     platformauth.LoadUserPermissions within the same transaction.  This
//     defence-in-depth ensures that future callers that bypass the HTTP layer
//     (e.g. internal service calls, batch jobs) cannot receive plaintext bank
//     account data without holding the required permission.
//
// The decrypted bank account value is returned only when both layers grant
// access; it is NEVER written to the audit log or any other log.
func (s *Service) GetIntakeForm(ctx context.Context, in GetIntakeFormInput) (*IntakeForm, []byte, error) {
	var form IntakeForm
	var permittedSensitive bool // resolved inside the transaction
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Service-layer permission check (multi-layer defence).
		// When the caller requests sensitive decryption, verify the actor
		// actually holds intake:read_sensitive — even if the HTTP middleware
		// already checked this.
		if in.ReadSensitive {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("onboarding: get intake form load permissions: %w", err)
			}
			if !platformauth.HasPermission(perms, "intake:read_sensitive") {
				return ErrForbidden
			}
			permittedSensitive = true
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, emergency_contact_json, commute_json,
			        dependents_json, bank_account_enc, status, retention_policy,
			        created_at, updated_at
			 FROM employee_intake_forms
			 WHERE employee_id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&form).Error; err != nil {
			return fmt.Errorf("onboarding: get intake form: %w", err)
		}
		if form.ID == uuid.Nil {
			return ErrNotFound
		}

		action := "intake_form.read"
		if permittedSensitive {
			action = "intake_form.read_sensitive"
		}
		// Audit: resource_id is opaque (UUID only) — no PII in audit record.
		idStr := form.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "employee_intake_form",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}

	// Decrypt the bank account number only when service-layer permission
	// verification succeeded.  The decrypted value is returned separately from
	// the form so callers cannot accidentally persist it.
	// Security: decrypted value is NEVER written to logs, audit records, or
	// returned as part of the IntakeForm struct.
	var bankAccountPlaintext []byte
	if permittedSensitive && len(form.BankAccountEnc) > 0 {
		plain, err := crypto.Decrypt(form.BankAccountEnc)
		if err != nil {
			return nil, nil, fmt.Errorf("onboarding: decrypt bank account: %w", err)
		}
		bankAccountPlaintext = plain
	}

	// Clear the ciphertext from the returned struct regardless — callers that
	// need the plaintext use the second return value.
	form.BankAccountEnc = nil

	return &form, bankAccountPlaintext, nil
}

// VerifyIntakeFormInput holds fields for verifying an intake form.
type VerifyIntakeFormInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	IP         *string
}

// VerifyIntakeForm moves the form status from submitted → verified.
func (s *Service) VerifyIntakeForm(ctx context.Context, in VerifyIntakeFormInput) (*IntakeForm, error) {
	var form IntakeForm
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE employee_intake_forms
			 SET status = 'verified', updated_at = now()
			 WHERE employee_id = ? AND tenant_id = ? AND status = 'submitted'`,
			in.EmployeeID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("onboarding: verify intake form update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, emergency_contact_json, commute_json,
			        dependents_json, bank_account_enc, status, retention_policy,
			        created_at, updated_at
			 FROM employee_intake_forms
			 WHERE employee_id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&form).Error; err != nil {
			return fmt.Errorf("onboarding: verify intake re-read: %w", err)
		}

		idStr := form.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "intake_form.verified",
			ResourceType: "employee_intake_form",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("onboarding: verify intake audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	form.BankAccountEnc = nil // never expose ciphertext via this path
	return &form, nil
}

// ---------------------------------------------------------------------------
// Offboarding — LM-004
// ---------------------------------------------------------------------------

// InitiateOffboardingInput holds fields for starting the offboarding process.
type InitiateOffboardingInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	EmployeeID      uuid.UUID
	TemplateID      *uuid.UUID // optional; tasks generated when provided
	LastWorkingDate *time.Time // used as BaseDate for task due offsets
	RetentionLabel  string
	ExpiresOn       *time.Time
	Notes           *string
	IP              *string
}

// InitiateOffboarding transitions an employee to "leaving", generates
// offboarding tasks from a template (when provided), and records the data
// retention policy.  All writes occur in a single transaction.
//
// Data deletion policy: physical deletion is NEVER performed.  The
// offboarding_policies row records when data may be logically expired.
func (s *Service) InitiateOffboarding(ctx context.Context, in InitiateOffboardingInput) ([]Task, *OffboardingPolicy, error) {
	var tasks []Task
	var policy OffboardingPolicy

	retentionLabel := in.RetentionLabel
	if retentionLabel == "" {
		retentionLabel = "7years"
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Read current employee status.
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM employees WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("onboarding: initiate offboarding read employee: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isEmployeeStatusTransitionAllowed(current.Status, "leaving") {
			return fmt.Errorf("%w: employee status %s → leaving", ErrInvalidTransition, current.Status)
		}

		// Transition employee status to "leaving".
		res := tx.Exec(
			`UPDATE employees SET status = 'leaving', updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("onboarding: initiate offboarding update employee: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		// Generate offboarding tasks when a template is provided.
		if in.TemplateID != nil {
			baseDate := time.Now()
			if in.LastWorkingDate != nil {
				baseDate = *in.LastWorkingDate
			}

			var tmpl ChecklistTemplate
			if err := tx.Raw(
				`SELECT id, tenant_id, kind, items_json FROM onboarding_checklist_templates
				 WHERE id = ? AND tenant_id = ? AND active = true AND kind = 'offboarding' LIMIT 1`,
				*in.TemplateID, in.TenantID,
			).Scan(&tmpl).Error; err != nil {
				return fmt.Errorf("onboarding: initiate offboarding fetch template: %w", err)
			}
			if tmpl.ID == uuid.Nil {
				return ErrNotFound
			}

			var items []templateItem
			if err := json.Unmarshal(tmpl.ItemsJSON, &items); err != nil {
				return fmt.Errorf("onboarding: initiate offboarding parse items_json: %w", err)
			}

			for _, item := range items {
				t := Task{
					ID:         uuid.New(),
					TenantID:   in.TenantID,
					EmployeeID: in.EmployeeID,
					Kind:       "offboarding",
					Title:      item.Title,
					Category:   item.Category,
					Status:     "pending",
				}
				if item.DueOffsetDays != 0 {
					d := baseDate.AddDate(0, 0, item.DueOffsetDays)
					t.DueDate = &d
				}
				if err := tx.Exec(
					`INSERT INTO onboarding_tasks
					   (id, tenant_id, employee_id, kind, title, category, status, due_date)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					t.ID, t.TenantID, t.EmployeeID, t.Kind, t.Title, t.Category, t.Status, t.DueDate,
				).Error; err != nil {
					return fmt.Errorf("onboarding: initiate offboarding insert task: %w", err)
				}
				tasks = append(tasks, t)
			}
		}

		// Record the data retention policy.
		// Physical deletion is NEVER performed — this row records the logical
		// expiry date after which data may be anonymised or access-restricted.
		policy = OffboardingPolicy{
			ID:             uuid.New(),
			TenantID:       in.TenantID,
			EmployeeID:     in.EmployeeID,
			RetentionLabel: retentionLabel,
			ExpiresOn:      in.ExpiresOn,
			RecordedBy:     &in.ActorID,
			Notes:          in.Notes,
		}
		if err := tx.Exec(
			`INSERT INTO offboarding_policies
			   (id, tenant_id, employee_id, retention_label, expires_on, recorded_by, notes)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (employee_id, tenant_id) DO UPDATE
			   SET retention_label = EXCLUDED.retention_label,
			       expires_on      = EXCLUDED.expires_on,
			       recorded_by     = EXCLUDED.recorded_by,
			       notes           = EXCLUDED.notes,
			       updated_at      = now()`,
			policy.ID, policy.TenantID, policy.EmployeeID,
			policy.RetentionLabel, policy.ExpiresOn, policy.RecordedBy, policy.Notes,
		).Error; err != nil {
			return fmt.Errorf("onboarding: initiate offboarding insert policy: %w", err)
		}

		idStr := in.EmployeeID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "offboarding.initiated",
			ResourceType: "employee",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return tasks, &policy, nil
}

// CompleteOffboardingInput holds fields for completing the offboarding process.
type CompleteOffboardingInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	IP         *string
}

// CompleteOffboarding transitions an employee from "leaving" → "left".
// All offboarding tasks must be in a terminal state (done/skipped) before
// the transition is allowed.  Data is never permanently deleted.
func (s *Service) CompleteOffboarding(ctx context.Context, in CompleteOffboardingInput) error {
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM employees WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.EmployeeID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("onboarding: complete offboarding read employee: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isEmployeeStatusTransitionAllowed(current.Status, "left") {
			return fmt.Errorf("%w: employee status %s → left", ErrInvalidTransition, current.Status)
		}

		// Verify all offboarding tasks are terminal.
		var pendingCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM onboarding_tasks
			 WHERE employee_id = ? AND tenant_id = ? AND kind = 'offboarding'
			   AND status NOT IN ('done', 'skipped')`,
			in.EmployeeID, in.TenantID,
		).Scan(&pendingCount).Error; err != nil {
			return fmt.Errorf("onboarding: complete offboarding check tasks: %w", err)
		}
		if pendingCount > 0 {
			return fmt.Errorf("%w: %d offboarding tasks are not yet complete", ErrInvalidTransition, pendingCount)
		}

		res := tx.Exec(
			`UPDATE employees SET status = 'left', updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("onboarding: complete offboarding update employee: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := in.EmployeeID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "offboarding.completed",
			ResourceType: "employee",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// GetOffboardingPolicy fetches the retention policy for an employee.
func (s *Service) GetOffboardingPolicy(ctx context.Context, tenantID, employeeID uuid.UUID) (*OffboardingPolicy, error) {
	var policy OffboardingPolicy
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, retention_label, expires_on,
			        recorded_by, notes, created_at, updated_at
			 FROM offboarding_policies
			 WHERE employee_id = ? AND tenant_id = ? LIMIT 1`,
			employeeID, tenantID,
		).Scan(&policy).Error
	})
	if err != nil {
		return nil, err
	}
	if policy.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &policy, nil
}
