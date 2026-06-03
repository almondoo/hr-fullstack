package hiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/notification"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("hiring: not found")
	ErrInvalidTransition = errors.New("hiring: invalid status transition")
	ErrAlreadyConverted  = errors.New("hiring: applicant already converted to employee")
	ErrForbidden         = errors.New("hiring: permission denied")
)

// allowedOnboardingTransitions defines legal new_hire_onboardings.status moves.
//
// Lifecycle: offer_accepted → preboarding → onboarding → completed.
// completed is terminal.  Skipping forward is intentionally NOT allowed so the
// LM-side employee lifecycle stays in sync (employee is activated only at
// completion).  Legal/config note: which collected documents are mandatory per
// employment type is governed by the existing LM-001/LM-003 settings tables,
// not hard-coded here.
var allowedOnboardingTransitions = map[string]map[string]bool{
	OnboardingStatusOfferAccepted: {
		OnboardingStatusPreboarding: true,
	},
	OnboardingStatusPreboarding: {
		OnboardingStatusOnboarding: true,
	},
	OnboardingStatusOnboarding: {
		OnboardingStatusCompleted: true,
	},
}

// isOnboardingTransitionAllowed reports whether moving from current → next is valid.
func isOnboardingTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedOnboardingTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// templateItem is the shape of each element in a checklist template's items_json.
// This mirrors the format used by internal/onboarding so the reused
// onboarding_checklist_templates / onboarding_tasks assets stay compatible.
type templateItem struct {
	Title         string `json:"title"`
	Category      string `json:"category"`
	DueOffsetDays int    `json:"due_offset_days"`
}

// Service provides business logic for the hiring (onboarding linkage) domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Candidate → employee conversion (ATS-020 / ATS-021)
// ---------------------------------------------------------------------------

// ConvertApplicantInput holds the fields required to convert an offer-accepted
// candidate into an employee and bootstrap their onboarding.
//
// The candidate's basic info (氏名/連絡先/雇用区分/入社予定日) is passed in by the
// caller (the ST-ATS-05 acceptance trigger).  The applicants table lives in a
// separately-built slice (ST-ATS-02), so this package never reads it directly;
// it only stores the applicant_id as a provenance pointer.
//
// PII minimisation: only the fields needed to seed the employee master are
// accepted.  Contact PII beyond what employee/intake require must NOT be
// duplicated here; the ATS-side candidate record retains its own
// consent/retention policy (ST-ATS-02).  No contact PII is written to the
// audit log.
type ConvertApplicantInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	ApplicantID uuid.UUID
	OfferID     *uuid.UUID
	// EmployeeCode must be unique within the tenant (employees.employee_code).
	EmployeeCode string
	LastName     string
	FirstName    string
	// Email is optional; mapped to employees.email.  Treated as minimised
	// contact PII transferred for 雇用管理目的.
	Email *string
	// EmploymentType maps to employees.employment_type (e.g. full_time).
	EmploymentType string
	// DepartmentID selects the per-department onboarding template; also written
	// to employees.department_id.  Optional.
	DepartmentID *uuid.UUID
	// TemplateID is the onboarding_checklist_templates row used to generate
	// onboarding_tasks (reused asset).  Optional; when nil no tasks are created.
	TemplateID *uuid.UUID
	// ExpectedStartDate is the planned hire date; used as the base date for
	// task due offsets and stored on the onboarding header.  Optional.
	ExpectedStartDate *time.Time
	IP                *string
}

// ConvertResult bundles the rows produced by a conversion.
type ConvertResult struct {
	Link       *Link
	Onboarding *NewHireOnboarding
	EmployeeID uuid.UUID
	Tasks      []taskRow
}

// taskRow is a lightweight view of a generated onboarding_tasks row.
type taskRow struct {
	ID       uuid.UUID  `gorm:"column:id"`
	Title    string     `gorm:"column:title"`
	Category string     `gorm:"column:category"`
	Status   string     `gorm:"column:status"`
	DueDate  *time.Time `gorm:"column:due_date"`
}

// ConvertApplicant idempotently converts an offer-accepted candidate into an
// employee, records the provenance link, creates the new-hire onboarding
// header, and (when a template is provided) generates onboarding_tasks — ALL
// in a single transaction so partial state / orphan tasks cannot occur.
//
// Idempotency: if a link already exists for (tenant_id, applicant_id) the
// existing link/onboarding is returned as a successful no-op so a re-fired
// acceptance trigger never creates a second employee.  Double conversion is
// also blocked at the DB layer by UNIQUE(tenant_id, applicant_id) (which would
// surface as a constraint error on a concurrent racing insert).  ErrAlready
// converted is exported for callers that prefer to treat a re-conversion as an
// error rather than a no-op.
//
// The created employee starts in status 'inactive' (pre-start); it is activated
// only when CompleteOnboarding runs, keeping the LM employee lifecycle in sync.
func (s *Service) ConvertApplicant(ctx context.Context, in ConvertApplicantInput) (*ConvertResult, error) {
	result := &ConvertResult{}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// --- Idempotency: return the existing conversion if present. ---
		var existing Link
		if err := tx.Raw(
			`SELECT id, tenant_id, applicant_id, offer_id, employee_id,
			        converted_at, converted_by, created_at, updated_at
			 FROM applicant_employee_links
			 WHERE applicant_id = ? AND tenant_id = ? LIMIT 1`,
			in.ApplicantID, in.TenantID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("hiring: convert lookup existing link: %w", err)
		}
		if existing.ID != uuid.Nil {
			// Already converted — return existing rows, no new writes.
			result.Link = &existing
			result.EmployeeID = existing.EmployeeID
			var ob NewHireOnboarding
			if err := tx.Raw(
				`SELECT id, tenant_id, employee_id, applicant_id, department_id,
				        template_id, status, expected_start_date, created_at, updated_at
				 FROM new_hire_onboardings
				 WHERE employee_id = ? AND tenant_id = ? LIMIT 1`,
				existing.EmployeeID, in.TenantID,
			).Scan(&ob).Error; err != nil {
				return fmt.Errorf("hiring: convert lookup existing onboarding: %w", err)
			}
			if ob.ID != uuid.Nil {
				result.Onboarding = &ob
			}
			return nil
		}

		// --- Validate department / template belong to this tenant. ---
		if in.DepartmentID != nil {
			var cnt int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM departments WHERE id = ? AND tenant_id = ?`,
				*in.DepartmentID, in.TenantID,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("hiring: convert verify department: %w", err)
			}
			if cnt == 0 {
				return ErrNotFound
			}
		}
		if in.TemplateID != nil {
			var cnt int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM onboarding_checklist_templates
				 WHERE id = ? AND tenant_id = ? AND active = true`,
				*in.TemplateID, in.TenantID,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("hiring: convert verify template: %w", err)
			}
			if cnt == 0 {
				return ErrNotFound
			}
		}

		// --- Create the employee master row (status 'inactive' = pre-start). ---
		empID := uuid.New()
		var hiredOn *time.Time // hired_on set at completion, not at conversion
		if err := tx.Exec(
			`INSERT INTO employees
			   (id, tenant_id, employee_code, last_name, first_name,
			    department_id, status, hired_on, email, employment_type)
			 VALUES (?, ?, ?, ?, ?, ?, 'inactive', ?, ?, ?)`,
			empID, in.TenantID, in.EmployeeCode, in.LastName, in.FirstName,
			in.DepartmentID, hiredOn, in.Email, in.EmploymentType,
		).Error; err != nil {
			return fmt.Errorf("hiring: convert insert employee: %w", err)
		}

		// --- Record the provenance link (idempotency enforced by UNIQUE). ---
		link := Link{
			ID:          uuid.New(),
			TenantID:    in.TenantID,
			ApplicantID: in.ApplicantID,
			OfferID:     in.OfferID,
			EmployeeID:  empID,
			ConvertedBy: &in.ActorID,
		}
		if err := tx.Exec(
			`INSERT INTO applicant_employee_links
			   (id, tenant_id, applicant_id, offer_id, employee_id, converted_by)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			link.ID, link.TenantID, link.ApplicantID, link.OfferID,
			link.EmployeeID, link.ConvertedBy,
		).Error; err != nil {
			return fmt.Errorf("hiring: convert insert link: %w", err)
		}

		// --- Create the new-hire onboarding header. ---
		ob := NewHireOnboarding{
			ID:                uuid.New(),
			TenantID:          in.TenantID,
			EmployeeID:        empID,
			ApplicantID:       in.ApplicantID,
			DepartmentID:      in.DepartmentID,
			TemplateID:        in.TemplateID,
			Status:            OnboardingStatusOfferAccepted,
			ExpectedStartDate: in.ExpectedStartDate,
		}
		if err := tx.Exec(
			`INSERT INTO new_hire_onboardings
			   (id, tenant_id, employee_id, applicant_id, department_id,
			    template_id, status, expected_start_date)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ob.ID, ob.TenantID, ob.EmployeeID, ob.ApplicantID, ob.DepartmentID,
			ob.TemplateID, ob.Status, ob.ExpectedStartDate,
		).Error; err != nil {
			return fmt.Errorf("hiring: convert insert onboarding: %w", err)
		}

		// --- Generate onboarding_tasks from the template in the SAME tx. ---
		// Reuses the existing onboarding assets (onboarding_checklist_templates
		// / onboarding_tasks) so we do not duplicate the LM-001 implementation,
		// and keeps task creation atomic with employee/link/header creation.
		if in.TemplateID != nil {
			tasks, err := generateOnboardingTasks(tx, in.TenantID, empID, *in.TemplateID, in.ExpectedStartDate)
			if err != nil {
				return err
			}
			result.Tasks = tasks
		}

		result.Link = &link
		result.Onboarding = &ob
		result.EmployeeID = empID

		// Audit: opaque IDs only — never name/email/contact PII.
		idStr := link.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "hiring.applicant_converted",
			ResourceType: "applicant_employee_link",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return err
		}

		// Outbox hook: notify the HR actor that an applicant was converted to employee.
		// The employee is 'inactive' (pre-start); no employee user ID exists yet.
		// Notify the actor (HR person who performed the conversion).
		return notification.InsertOutbox(tx, notification.InsertOutboxEntry{
			TenantID:        in.TenantID,
			EventType:       "hiring.applicant_converted",
			ActorUserID:     &in.ActorID,
			RecipientUserID: in.ActorID,
			ResourceType:    "applicant_employee_link",
			ResourceID:      &link.ID,
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// generateOnboardingTasks inserts onboarding_tasks rows from the given template
// within the provided (already tenant-scoped) transaction.  baseDate (typically
// the expected start date) drives due-date offsets; nil ⇒ now().
func generateOnboardingTasks(tx *gorm.DB, tenantID, employeeID, templateID uuid.UUID, baseDate *time.Time) ([]taskRow, error) {
	var tmpl struct {
		ID        uuid.UUID `gorm:"column:id"`
		Kind      string    `gorm:"column:kind"`
		ItemsJSON []byte    `gorm:"column:items_json"`
	}
	if err := tx.Raw(
		`SELECT id, kind, items_json FROM onboarding_checklist_templates
		 WHERE id = ? AND tenant_id = ? AND active = true LIMIT 1`,
		templateID, tenantID,
	).Scan(&tmpl).Error; err != nil {
		return nil, fmt.Errorf("hiring: generate tasks fetch template: %w", err)
	}
	if tmpl.ID == uuid.Nil {
		return nil, ErrNotFound
	}

	var items []templateItem
	if err := json.Unmarshal(tmpl.ItemsJSON, &items); err != nil {
		return nil, fmt.Errorf("hiring: generate tasks parse items_json: %w", err)
	}

	base := time.Now()
	if baseDate != nil {
		base = *baseDate
	}
	// onboarding_tasks.kind is constrained to 'onboarding' | 'offboarding';
	// these are always onboarding tasks regardless of the template's own kind.
	const taskKind = "onboarding"

	var out []taskRow
	for _, item := range items {
		id := uuid.New()
		var due *time.Time
		if item.DueOffsetDays != 0 {
			d := base.AddDate(0, 0, item.DueOffsetDays)
			due = &d
		}
		if err := tx.Exec(
			`INSERT INTO onboarding_tasks
			   (id, tenant_id, employee_id, kind, title, category, status, due_date)
			 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
			id, tenantID, employeeID, taskKind, item.Title, item.Category, due,
		).Error; err != nil {
			return nil, fmt.Errorf("hiring: generate tasks insert: %w", err)
		}
		out = append(out, taskRow{
			ID: id, Title: item.Title, Category: item.Category,
			Status: "pending", DueDate: due,
		})
	}
	return out, nil
}

// GetOnboarding fetches a new-hire onboarding header by ID within the tenant.
func (s *Service) GetOnboarding(ctx context.Context, tenantID, id uuid.UUID) (*NewHireOnboarding, error) {
	var ob NewHireOnboarding
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, applicant_id, department_id,
			        template_id, status, expected_start_date, created_at, updated_at
			 FROM new_hire_onboardings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&ob).Error
	})
	if err != nil {
		return nil, err
	}
	if ob.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &ob, nil
}

// ListOnboardings returns new-hire onboarding headers for the tenant, optionally
// filtered by status.
func (s *Service) ListOnboardings(ctx context.Context, tenantID uuid.UUID, status string) ([]NewHireOnboarding, error) {
	var obs []NewHireOnboarding
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, employee_id, applicant_id, department_id,
		             template_id, status, expected_start_date, created_at, updated_at
		      FROM new_hire_onboardings
		      WHERE tenant_id = ?`
		args := []any{tenantID}
		if status != "" {
			q += ` AND status = ?`
			args = append(args, status)
		}
		q += ` ORDER BY created_at`
		return tx.Raw(q, args...).Scan(&obs).Error
	})
	if err != nil {
		return nil, err
	}
	return obs, nil
}

// AdvanceOnboardingInput holds fields for an onboarding status transition.
type AdvanceOnboardingInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	Status   string
	IP       *string
}

// AdvanceOnboarding transitions a new-hire onboarding header to the given
// status (offer_accepted → preboarding → onboarding).  The terminal
// 'completed' transition is handled by CompleteOnboarding so the employee can
// be activated atomically.  Invalid moves return ErrInvalidTransition.
func (s *Service) AdvanceOnboarding(ctx context.Context, in AdvanceOnboardingInput) (*NewHireOnboarding, error) {
	if in.Status == OnboardingStatusCompleted {
		// Completion must go through CompleteOnboarding (activates employee).
		return nil, fmt.Errorf("%w: use CompleteOnboarding to complete", ErrInvalidTransition)
	}

	var ob NewHireOnboarding
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM new_hire_onboardings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("hiring: advance read status: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isOnboardingTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		res := tx.Exec(
			`UPDATE new_hire_onboardings
			 SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("hiring: advance update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, applicant_id, department_id,
			        template_id, status, expected_start_date, created_at, updated_at
			 FROM new_hire_onboardings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&ob).Error; err != nil {
			return fmt.Errorf("hiring: advance re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "hiring.onboarding_advanced",
			ResourceType: "new_hire_onboarding",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &ob, nil
}

// CompleteOnboardingInput holds fields for completing a new-hire onboarding.
type CompleteOnboardingInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	// HiredOn is the actual hire date written to employees.hired_on on
	// activation.  Optional; nil leaves hired_on unchanged.
	HiredOn *time.Time
	IP      *string
}

// CompleteOnboarding transitions a new-hire onboarding from 'onboarding' →
// 'completed' and activates the linked employee (status → 'active') in the same
// transaction, keeping the ATS and LM lifecycles consistent.  TOCTOU is avoided
// by locking the header row FOR UPDATE before the transition check.
func (s *Service) CompleteOnboarding(ctx context.Context, in CompleteOnboardingInput) (*NewHireOnboarding, error) {
	var ob NewHireOnboarding
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var row struct {
			Status     string    `gorm:"column:status"`
			EmployeeID uuid.UUID `gorm:"column:employee_id"`
		}
		if err := tx.Raw(
			`SELECT status, employee_id FROM new_hire_onboardings
			 WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&row).Error; err != nil {
			return fmt.Errorf("hiring: complete read status: %w", err)
		}
		if row.Status == "" {
			return ErrNotFound
		}
		if !isOnboardingTransitionAllowed(row.Status, OnboardingStatusCompleted) {
			return fmt.Errorf("%w: %s → completed", ErrInvalidTransition, row.Status)
		}

		// Activate the employee (pre-start 'inactive' → 'active').
		if in.HiredOn != nil {
			if err := tx.Exec(
				`UPDATE employees SET status = 'active', hired_on = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				in.HiredOn, row.EmployeeID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("hiring: complete activate employee: %w", err)
			}
		} else {
			if err := tx.Exec(
				`UPDATE employees SET status = 'active', updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				row.EmployeeID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("hiring: complete activate employee: %w", err)
			}
		}

		res := tx.Exec(
			`UPDATE new_hire_onboardings SET status = 'completed', updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("hiring: complete update onboarding: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, applicant_id, department_id,
			        template_id, status, expected_start_date, created_at, updated_at
			 FROM new_hire_onboardings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&ob).Error; err != nil {
			return fmt.Errorf("hiring: complete re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "hiring.onboarding_completed",
			ResourceType: "new_hire_onboarding",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &ob, nil
}

// ---------------------------------------------------------------------------
// Preboarding requests (ATS-022)
// ---------------------------------------------------------------------------

// CreatePreboardingRequestInput holds fields for creating a preboarding request.
type CreatePreboardingRequestInput struct {
	TenantID            uuid.UUID
	ActorID             uuid.UUID
	NewHireOnboardingID uuid.UUID
	RequestType         string
	AssigneeUserID      *uuid.UUID
	Notes               *string
	IP                  *string
}

// CreatePreboardingRequest records an IT/account/equipment request tied to a
// new-hire onboarding.  The parent onboarding header must belong to the tenant
// (verified via COUNT + enforced by the composite FK).
func (s *Service) CreatePreboardingRequest(ctx context.Context, in CreatePreboardingRequestInput) (*PreboardingRequest, error) {
	req := PreboardingRequest{
		ID:                  uuid.New(),
		TenantID:            in.TenantID,
		NewHireOnboardingID: in.NewHireOnboardingID,
		RequestType:         in.RequestType,
		Status:              RequestStatusRequested,
		AssigneeUserID:      in.AssigneeUserID,
		Notes:               in.Notes,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM new_hire_onboardings WHERE id = ? AND tenant_id = ?`,
			in.NewHireOnboardingID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("hiring: create preboarding verify onboarding: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO preboarding_requests
			   (id, tenant_id, new_hire_onboarding_id, request_type, status,
			    assignee_user_id, notes)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			req.ID, req.TenantID, req.NewHireOnboardingID, req.RequestType,
			req.Status, req.AssigneeUserID, req.Notes,
		).Error; err != nil {
			return fmt.Errorf("hiring: create preboarding insert: %w", err)
		}

		idStr := req.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "hiring.preboarding_request_created",
			ResourceType: "preboarding_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// UpdatePreboardingRequestStatusInput holds fields for a request status change.
type UpdatePreboardingRequestStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	Status   string
	IP       *string
}

// allowedRequestTransitions defines legal preboarding_requests.status moves.
var allowedRequestTransitions = map[string]map[string]bool{
	RequestStatusRequested: {
		RequestStatusInProgress: true,
		RequestStatusCompleted:  true,
		RequestStatusCancelled:  true,
	},
	RequestStatusInProgress: {
		RequestStatusCompleted: true,
		RequestStatusCancelled: true,
	},
}

func isRequestTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedRequestTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// UpdatePreboardingRequestStatus transitions a preboarding request.
func (s *Service) UpdatePreboardingRequestStatus(ctx context.Context, in UpdatePreboardingRequestStatusInput) (*PreboardingRequest, error) {
	var req PreboardingRequest
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM preboarding_requests WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("hiring: update preboarding read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isRequestTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		res := tx.Exec(
			`UPDATE preboarding_requests SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("hiring: update preboarding: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, new_hire_onboarding_id, request_type, status,
			        assignee_user_id, notes, created_at, updated_at
			 FROM preboarding_requests WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("hiring: update preboarding re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "hiring.preboarding_request_status_updated",
			ResourceType: "preboarding_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// ListPreboardingRequests returns the preboarding requests for an onboarding.
func (s *Service) ListPreboardingRequests(ctx context.Context, tenantID, onboardingID uuid.UUID) ([]PreboardingRequest, error) {
	var reqs []PreboardingRequest
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, new_hire_onboarding_id, request_type, status,
			        assignee_user_id, notes, created_at, updated_at
			 FROM preboarding_requests
			 WHERE tenant_id = ? AND new_hire_onboarding_id = ?
			 ORDER BY created_at`,
			tenantID, onboardingID,
		).Scan(&reqs).Error
	})
	if err != nil {
		return nil, err
	}
	return reqs, nil
}

// ---------------------------------------------------------------------------
// Post-hire follow-up surveys (ATS-023, Could — minimal stub)
// ---------------------------------------------------------------------------

// ScheduleSurveyInput holds fields for scheduling a follow-up survey.
//
// MVP scope: only a schedule slot + status is recorded.  No survey answer body
// or PII is accepted/stored; predictive early-attrition analytics (CMP-005) is
// Future work and intentionally out of scope here.
type ScheduleSurveyInput struct {
	TenantID            uuid.UUID
	ActorID             uuid.UUID
	NewHireOnboardingID uuid.UUID
	SurveyType          string
	ScheduledOn         *time.Time
	IP                  *string
}

// ScheduleSurvey creates a scheduled follow-up survey slot for a new-hire
// onboarding.  The employee_id is resolved from the parent onboarding header so
// the survey is tied to the correct (employee, tenant) pair.
func (s *Service) ScheduleSurvey(ctx context.Context, in ScheduleSurveyInput) (*Survey, error) {
	var survey Survey
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var ob struct {
			EmployeeID uuid.UUID `gorm:"column:employee_id"`
		}
		if err := tx.Raw(
			`SELECT employee_id FROM new_hire_onboardings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.NewHireOnboardingID, in.TenantID,
		).Scan(&ob).Error; err != nil {
			return fmt.Errorf("hiring: schedule survey read onboarding: %w", err)
		}
		if ob.EmployeeID == uuid.Nil {
			return ErrNotFound
		}

		survey = Survey{
			ID:                  uuid.New(),
			TenantID:            in.TenantID,
			NewHireOnboardingID: in.NewHireOnboardingID,
			EmployeeID:          ob.EmployeeID,
			SurveyType:          in.SurveyType,
			ScheduledOn:         in.ScheduledOn,
			Status:              SurveyStatusScheduled,
		}
		if err := tx.Exec(
			`INSERT INTO onboarding_surveys
			   (id, tenant_id, new_hire_onboarding_id, employee_id, survey_type,
			    scheduled_on, status)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			survey.ID, survey.TenantID, survey.NewHireOnboardingID, survey.EmployeeID,
			survey.SurveyType, survey.ScheduledOn, survey.Status,
		).Error; err != nil {
			return fmt.Errorf("hiring: schedule survey insert: %w", err)
		}

		idStr := survey.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "hiring.survey_scheduled",
			ResourceType: "onboarding_survey",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &survey, nil
}

// ListSurveys returns the surveys scheduled for a new-hire onboarding.
func (s *Service) ListSurveys(ctx context.Context, tenantID, onboardingID uuid.UUID) ([]Survey, error) {
	var surveys []Survey
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, new_hire_onboarding_id, employee_id, survey_type,
			        scheduled_on, status, created_at, updated_at
			 FROM onboarding_surveys
			 WHERE tenant_id = ? AND new_hire_onboarding_id = ?
			 ORDER BY scheduled_on NULLS LAST, created_at`,
			tenantID, onboardingID,
		).Scan(&surveys).Error
	})
	if err != nil {
		return nil, err
	}
	return surveys, nil
}
