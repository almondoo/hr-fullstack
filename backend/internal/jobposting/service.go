package jobposting

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("jobposting: not found")
	ErrInvalidTransition = errors.New("jobposting: invalid status transition")
	ErrForbidden         = errors.New("jobposting: permission denied")
	ErrValidation        = errors.New("jobposting: validation failed")
)

// allowedStatusTransitions defines legal job posting status moves.
//
// State machine: draft → open → on_hold ↔ open → closed.
// 'closed' is terminal (no transitions out).  These are enforced both here
// (service layer allow-list) and by the chk_job_postings_status CHECK
// constraint (value domain) for defence-in-depth.
var allowedStatusTransitions = map[string]map[string]bool{
	StatusDraft: {
		StatusOpen:   true,
		StatusClosed: true,
	},
	StatusOpen: {
		StatusOnHold: true,
		StatusClosed: true,
	},
	StatusOnHold: {
		StatusOpen:   true,
		StatusClosed: true,
	},
	// StatusClosed is terminal — no entry here.
}

// isStatusTransitionAllowed reports whether moving current → next is valid.
func isStatusTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedStatusTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// budgetReadPermission is the item-level permission required to view the
// salary range and hiring budget fields.  Managers and above hold it.
const budgetReadPermission = "ats:read_budget"

// Service provides business logic for job postings and ATS foundation.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// generatePublicSlug returns an opaque, non-sequential slug derived from a
// fresh UUID.  This avoids leaking a sequential / guessable identifier in the
// public job listing URL (連番非露出の opaque 値).
func generatePublicSlug() string {
	return "job-" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

// verifyDepartment confirms (departmentID, tenantID) exists in departments.
// The composite FK already enforces this at write time, but verifying first
// yields a clean ErrNotFound instead of a constraint-violation error.
func verifyDepartment(tx *gorm.DB, tenantID, departmentID uuid.UUID) error {
	var count int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM departments WHERE id = ? AND tenant_id = ?`,
		departmentID, tenantID,
	).Scan(&count).Error; err != nil {
		return fmt.Errorf("jobposting: verify department: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: department not found in tenant", ErrNotFound)
	}
	return nil
}

// verifyUser confirms (userID, tenantID) exists in users.
//
// [Security] users has no UNIQUE(id, tenant_id), so a composite FK is not
// possible.  This service-layer check (combined with RLS) is the defence
// against cross-tenant user assignment for recruiter / hiring manager /
// interviewer references.
func verifyUser(tx *gorm.DB, tenantID, userID uuid.UUID) error {
	var count int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
		userID, tenantID,
	).Scan(&count).Error; err != nil {
		return fmt.Errorf("jobposting: verify user: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("%w: user not found in tenant", ErrNotFound)
	}
	return nil
}

// countUndecidedApplicants returns the number of in-progress applicants for a
// posting at close time.
//
// The applications table is owned by ST-ATS-02 (future story) and is referenced
// here only logically (by job_posting_id, no FK).  Until that migration exists
// the count is 0.  Existence is checked in a separate statement first because
// PostgreSQL plans every branch of a CASE/subquery eagerly and would raise
// "relation does not exist" even inside a guarded branch otherwise.
func countUndecidedApplicants(tx *gorm.DB, tenantID, jobPostingID uuid.UUID) (int64, error) {
	var exists bool
	if err := tx.Raw(
		`SELECT to_regclass('public.applications') IS NOT NULL`,
	).Scan(&exists).Error; err != nil {
		return 0, fmt.Errorf("jobposting: applications table existence check: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var undecided int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM applications
		 WHERE job_posting_id = ? AND tenant_id = ?
		   AND status NOT IN ('hired', 'rejected', 'withdrawn')`,
		jobPostingID, tenantID,
	).Scan(&undecided).Error; err != nil {
		return 0, fmt.Errorf("jobposting: close undecided applicants check: %w", err)
	}
	return undecided, nil
}

// ---------------------------------------------------------------------------
// Create
// ---------------------------------------------------------------------------

// CreateJobPostingInput holds fields for creating a job posting.
type CreateJobPostingInput struct {
	TenantID            uuid.UUID
	ActorID             uuid.UUID
	Title               string
	EmploymentType      string
	DepartmentID        uuid.UUID
	RecruiterUserID     *uuid.UUID
	HiringManagerUserID *uuid.UUID
	RequirementsJSON    []byte
	SalaryRangeMin      *int64
	SalaryRangeMax      *int64
	HiringBudget        *int64
	// RetentionLabel is the (configurable, non-statutory) retention label.
	// Defaults to "unset" when empty — callers should source it from tenant
	// configuration rather than hardcoding a legal value.
	RetentionLabel string
	IP             *string
}

// CreateJobPosting creates a new job posting in 'draft' status.
//
// The posting always starts in draft; publication (open) is a separate
// transition that re-validates the requirements.  A non-sequential opaque
// public_slug is generated up front so downstream public listing has a stable
// identifier, but public_published stays false until open.
func (s *Service) CreateJobPosting(ctx context.Context, in CreateJobPostingInput) (*JobPosting, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("%w: title is required", ErrValidation)
	}
	if strings.TrimSpace(in.EmploymentType) == "" {
		return nil, fmt.Errorf("%w: employment_type is required", ErrValidation)
	}
	if in.DepartmentID == uuid.Nil {
		return nil, fmt.Errorf("%w: department_id is required", ErrValidation)
	}

	requirements := in.RequirementsJSON
	if len(requirements) == 0 {
		requirements = []byte(`{}`)
	}
	retentionLabel := in.RetentionLabel
	if retentionLabel == "" {
		retentionLabel = "unset"
	}

	jp := JobPosting{
		ID:                  uuid.New(),
		TenantID:            in.TenantID,
		Title:               in.Title,
		Status:              StatusDraft,
		EmploymentType:      in.EmploymentType,
		DepartmentID:        in.DepartmentID,
		RecruiterUserID:     in.RecruiterUserID,
		HiringManagerUserID: in.HiringManagerUserID,
		RequirementsJSON:    requirements,
		SalaryRangeMin:      in.SalaryRangeMin,
		SalaryRangeMax:      in.SalaryRangeMax,
		HiringBudget:        in.HiringBudget,
		RetentionLabel:      retentionLabel,
		PublicPublished:     false,
		PublicSlug:          generatePublicSlug(),
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the department belongs to this tenant (also enforced by FK).
		if err := verifyDepartment(tx, in.TenantID, in.DepartmentID); err != nil {
			return err
		}
		// Verify recruiter / hiring manager belong to this tenant when set.
		if in.RecruiterUserID != nil {
			if err := verifyUser(tx, in.TenantID, *in.RecruiterUserID); err != nil {
				return err
			}
		}
		if in.HiringManagerUserID != nil {
			if err := verifyUser(tx, in.TenantID, *in.HiringManagerUserID); err != nil {
				return err
			}
		}

		if err := tx.Exec(
			`INSERT INTO job_postings
			   (id, tenant_id, title, status, employment_type, department_id,
			    recruiter_user_id, hiring_manager_user_id, requirements_json,
			    salary_range_min, salary_range_max, hiring_budget,
			    retention_label, public_published, public_slug)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?, ?, ?, ?, ?, ?)`,
			jp.ID, jp.TenantID, jp.Title, jp.Status, jp.EmploymentType, jp.DepartmentID,
			jp.RecruiterUserID, jp.HiringManagerUserID, jp.RequirementsJSON,
			jp.SalaryRangeMin, jp.SalaryRangeMax, jp.HiringBudget,
			jp.RetentionLabel, jp.PublicPublished, jp.PublicSlug,
		).Error; err != nil {
			return fmt.Errorf("jobposting: create insert: %w", err)
		}

		idStr := jp.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "job_posting.created",
			ResourceType: "job_posting",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &jp, nil
}

// ---------------------------------------------------------------------------
// Read
// ---------------------------------------------------------------------------

// GetJobPostingInput holds parameters for fetching a single posting.
//
// ReadBudget must be set true only when the caller holds ats:read_budget.
// When false (or the service-layer re-check fails) the salary range and
// hiring budget fields are cleared from the returned struct.
type GetJobPostingInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	ID         uuid.UUID
	ReadBudget bool
}

// GetJobPosting fetches a posting by ID within the tenant.
//
// Item-level permission (multi-layer defence): when ReadBudget is requested,
// the service re-validates ats:read_budget via LoadUserPermissions inside the
// transaction.  If the actor lacks it, the budget fields are silently cleared
// (read is still permitted; only the restricted fields are withheld).  This
// ensures internal callers that bypass the HTTP middleware cannot read budget
// without the permission.
func (s *Service) GetJobPosting(ctx context.Context, in GetJobPostingInput) (*JobPosting, error) {
	var jp JobPosting
	var budgetPermitted bool
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if in.ReadBudget {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("jobposting: get load permissions: %w", err)
			}
			budgetPermitted = platformauth.HasPermission(perms, budgetReadPermission)
		}
		return tx.Raw(
			`SELECT id, tenant_id, title, status, employment_type, department_id,
			        recruiter_user_id, hiring_manager_user_id, requirements_json,
			        salary_range_min, salary_range_max, hiring_budget,
			        retention_label, public_published, public_slug,
			        created_at, updated_at
			 FROM job_postings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&jp).Error
	})
	if err != nil {
		return nil, err
	}
	if jp.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	if !budgetPermitted {
		clearBudgetFields(&jp)
	}
	return &jp, nil
}

// clearBudgetFields removes the budget-restricted fields from a posting.
func clearBudgetFields(jp *JobPosting) {
	jp.SalaryRangeMin = nil
	jp.SalaryRangeMax = nil
	jp.HiringBudget = nil
}

// ListJobPostingsInput holds filter parameters for listing postings.
type ListJobPostingsInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	Status       string     // optional filter
	DepartmentID *uuid.UUID // optional filter
	ReadBudget   bool
}

// ListJobPostings returns postings for a tenant, optionally filtered by status
// and department.  Budget fields are cleared unless the actor holds
// ats:read_budget (re-validated in the same transaction).
func (s *Service) ListJobPostings(ctx context.Context, in ListJobPostingsInput) ([]JobPosting, error) {
	var postings []JobPosting
	var budgetPermitted bool
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if in.ReadBudget {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("jobposting: list load permissions: %w", err)
			}
			budgetPermitted = platformauth.HasPermission(perms, budgetReadPermission)
		}
		q := `SELECT id, tenant_id, title, status, employment_type, department_id,
		             recruiter_user_id, hiring_manager_user_id, requirements_json,
		             salary_range_min, salary_range_max, hiring_budget,
		             retention_label, public_published, public_slug,
		             created_at, updated_at
		      FROM job_postings
		      WHERE tenant_id = ?`
		args := []any{in.TenantID}
		if in.Status != "" {
			q += ` AND status = ?`
			args = append(args, in.Status)
		}
		if in.DepartmentID != nil {
			q += ` AND department_id = ?`
			args = append(args, *in.DepartmentID)
		}
		q += ` ORDER BY created_at DESC`
		return tx.Raw(q, args...).Scan(&postings).Error
	})
	if err != nil {
		return nil, err
	}
	if !budgetPermitted {
		for i := range postings {
			clearBudgetFields(&postings[i])
		}
	}
	return postings, nil
}

// ---------------------------------------------------------------------------
// Update (non-status fields)
// ---------------------------------------------------------------------------

// UpdateJobPostingInput holds editable fields for an existing posting.
// Status is NOT updated here — use UpdateStatus / Publish / Close instead.
type UpdateJobPostingInput struct {
	TenantID            uuid.UUID
	ActorID             uuid.UUID
	ID                  uuid.UUID
	Title               string
	EmploymentType      string
	DepartmentID        uuid.UUID
	RecruiterUserID     *uuid.UUID
	HiringManagerUserID *uuid.UUID
	RequirementsJSON    []byte
	SalaryRangeMin      *int64
	SalaryRangeMax      *int64
	HiringBudget        *int64
	RetentionLabel      string
	IP                  *string
}

// UpdateJobPosting updates the editable fields of a posting.
func (s *Service) UpdateJobPosting(ctx context.Context, in UpdateJobPostingInput) (*JobPosting, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("%w: title is required", ErrValidation)
	}
	if strings.TrimSpace(in.EmploymentType) == "" {
		return nil, fmt.Errorf("%w: employment_type is required", ErrValidation)
	}
	if in.DepartmentID == uuid.Nil {
		return nil, fmt.Errorf("%w: department_id is required", ErrValidation)
	}

	requirements := in.RequirementsJSON
	if len(requirements) == 0 {
		requirements = []byte(`{}`)
	}
	retentionLabel := in.RetentionLabel
	if retentionLabel == "" {
		retentionLabel = "unset"
	}

	var jp JobPosting
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Confirm the posting exists in this tenant first (clean ErrNotFound).
		var exists int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM job_postings WHERE id = ? AND tenant_id = ?`,
			in.ID, in.TenantID,
		).Scan(&exists).Error; err != nil {
			return fmt.Errorf("jobposting: update existence check: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}

		if err := verifyDepartment(tx, in.TenantID, in.DepartmentID); err != nil {
			return err
		}
		if in.RecruiterUserID != nil {
			if err := verifyUser(tx, in.TenantID, *in.RecruiterUserID); err != nil {
				return err
			}
		}
		if in.HiringManagerUserID != nil {
			if err := verifyUser(tx, in.TenantID, *in.HiringManagerUserID); err != nil {
				return err
			}
		}

		res := tx.Exec(
			`UPDATE job_postings
			 SET title = ?, employment_type = ?, department_id = ?,
			     recruiter_user_id = ?, hiring_manager_user_id = ?,
			     requirements_json = ?::jsonb,
			     salary_range_min = ?, salary_range_max = ?, hiring_budget = ?,
			     retention_label = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Title, in.EmploymentType, in.DepartmentID,
			in.RecruiterUserID, in.HiringManagerUserID, requirements,
			in.SalaryRangeMin, in.SalaryRangeMax, in.HiringBudget,
			retentionLabel, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("jobposting: update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, title, status, employment_type, department_id,
			        recruiter_user_id, hiring_manager_user_id, requirements_json,
			        salary_range_min, salary_range_max, hiring_budget,
			        retention_label, public_published, public_slug,
			        created_at, updated_at
			 FROM job_postings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&jp).Error; err != nil {
			return fmt.Errorf("jobposting: update re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "job_posting.updated",
			ResourceType: "job_posting",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &jp, nil
}

// ---------------------------------------------------------------------------
// Status transitions
// ---------------------------------------------------------------------------

// publicationCheck reports the missing-requirement error (if any) when a
// posting transitions to 'open'.  Publication requires the core requisition
// fields to be present.  The exact required set is law-dependent and should be
// driven by tenant configuration in future (legalConfigPoints); for ST-ATS-01
// we enforce the minimal MVP set: title + employment_type + department_id.
func publicationReadyError(jp *JobPosting) error {
	var missing []string
	if strings.TrimSpace(jp.Title) == "" {
		missing = append(missing, "title")
	}
	if strings.TrimSpace(jp.EmploymentType) == "" {
		missing = append(missing, "employment_type")
	}
	if jp.DepartmentID == uuid.Nil {
		missing = append(missing, "department_id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: cannot open posting; missing required fields: %s",
			ErrValidation, strings.Join(missing, ", "))
	}
	return nil
}

// UpdateStatusInput holds fields for a status transition.
type UpdateStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	Status   string
	IP       *string
}

// CloseWarning communicates a non-blocking warning at close time.
type CloseWarning struct {
	// UndecidedApplicants is the count of in-progress applicants at close time.
	// ST-ATS-01 does not auto-reject candidates; the data is retained
	// (ATS-017).  The count is surfaced as a warning only.
	UndecidedApplicants int64
}

// UpdateStatus transitions a posting between statuses (excluding the publish
// side-effects which are handled by their callers).  Only allow-listed
// transitions are accepted (ErrInvalidTransition otherwise).
//
// Concurrency: the current row is locked FOR UPDATE so concurrent transitions
// cannot both observe a stale source status and double-apply (TOCTOU-safe).
//
// When transitioning to 'open', publication readiness is re-validated and
// public_published is set true.  Any other transition leaves public_published
// unchanged except 'closed' which un-publishes (the listing is withdrawn).
func (s *Service) UpdateStatus(ctx context.Context, in UpdateStatusInput) (*JobPosting, *CloseWarning, error) {
	var jp JobPosting
	var warning *CloseWarning
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock the row to serialise concurrent transitions (TOCTOU-safe).
		var current JobPosting
		if err := tx.Raw(
			`SELECT id, tenant_id, title, status, employment_type, department_id,
			        recruiter_user_id, hiring_manager_user_id, requirements_json,
			        salary_range_min, salary_range_max, hiring_budget,
			        retention_label, public_published, public_slug,
			        created_at, updated_at
			 FROM job_postings
			 WHERE id = ? AND tenant_id = ?
			 FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("jobposting: status read for update: %w", err)
		}
		if current.ID == uuid.Nil {
			return ErrNotFound
		}

		// No-op transition to the same status is treated as an invalid move
		// (the allow-list has no self-loops).
		if !isStatusTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		// Determine publication flag and audit action for the target status.
		var publicPublished bool
		var action string
		switch in.Status {
		case StatusOpen:
			if err := publicationReadyError(&current); err != nil {
				return err
			}
			publicPublished = true
			action = "job_posting.opened"
		case StatusOnHold:
			// On hold: keep the listing published flag as-is (still visible as
			// the requisition exists, but recruiting is paused).
			publicPublished = current.PublicPublished
			action = "job_posting.on_hold"
		case StatusClosed:
			// Closing withdraws the public listing.
			publicPublished = false
			action = "job_posting.closed"
			// Surface a warning if undecided applicants exist.  ST-ATS-02 owns
			// the applications table; it is referenced here only by opaque
			// (job_posting_id) without an FK (logical reference).  Until that
			// table exists the count is skipped — we must check existence in a
			// SEPARATE statement first, because PostgreSQL parses/plans every
			// branch of a CASE eagerly and errors on a missing relation even
			// inside a guarded branch.
			undecided, err := countUndecidedApplicants(tx, in.TenantID, in.ID)
			if err != nil {
				return err
			}
			if undecided > 0 {
				warning = &CloseWarning{UndecidedApplicants: undecided}
			}
		default:
			// draft is never a transition target in the allow-list.
			return fmt.Errorf("%w: unsupported target status %s", ErrInvalidTransition, in.Status)
		}

		res := tx.Exec(
			`UPDATE job_postings
			 SET status = ?, public_published = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, publicPublished, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("jobposting: status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, title, status, employment_type, department_id,
			        recruiter_user_id, hiring_manager_user_id, requirements_json,
			        salary_range_min, salary_range_max, hiring_budget,
			        retention_label, public_published, public_slug,
			        created_at, updated_at
			 FROM job_postings WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&jp).Error; err != nil {
			return fmt.Errorf("jobposting: status re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "job_posting",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return &jp, warning, nil
}

// ---------------------------------------------------------------------------
// Interviewer assignment
// ---------------------------------------------------------------------------

// AssignInterviewerInput holds fields for assigning an interviewer.
type AssignInterviewerInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	JobPostingID uuid.UUID
	UserID       uuid.UUID
	IP           *string
}

// AssignInterviewer adds an interviewer (a tenant user) to a posting.
//
// The interviewer user and the posting are both verified to belong to the
// tenant (service-layer + the posting composite FK + RLS).  Duplicate
// assignment is idempotent via ON CONFLICT (UNIQUE(tenant_id, job_posting_id,
// user_id)).
func (s *Service) AssignInterviewer(ctx context.Context, in AssignInterviewerInput) (*Interviewer, error) {
	iv := Interviewer{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		JobPostingID: in.JobPostingID,
		UserID:       in.UserID,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the posting exists in this tenant.
		var postingCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM job_postings WHERE id = ? AND tenant_id = ?`,
			in.JobPostingID, in.TenantID,
		).Scan(&postingCount).Error; err != nil {
			return fmt.Errorf("jobposting: assign interviewer verify posting: %w", err)
		}
		if postingCount == 0 {
			return fmt.Errorf("%w: job posting not found in tenant", ErrNotFound)
		}
		// Verify the interviewer user belongs to this tenant.
		if err := verifyUser(tx, in.TenantID, in.UserID); err != nil {
			return err
		}

		if err := tx.Exec(
			`INSERT INTO job_posting_interviewers
			   (id, tenant_id, job_posting_id, user_id)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (tenant_id, job_posting_id, user_id) DO NOTHING`,
			iv.ID, iv.TenantID, iv.JobPostingID, iv.UserID,
		).Error; err != nil {
			return fmt.Errorf("jobposting: assign interviewer insert: %w", err)
		}

		// Re-read to obtain the canonical row (handles the conflict / no-op).
		if err := tx.Raw(
			`SELECT id, tenant_id, job_posting_id, user_id, created_at, updated_at
			 FROM job_posting_interviewers
			 WHERE tenant_id = ? AND job_posting_id = ? AND user_id = ? LIMIT 1`,
			in.TenantID, in.JobPostingID, in.UserID,
		).Scan(&iv).Error; err != nil {
			return fmt.Errorf("jobposting: assign interviewer re-read: %w", err)
		}

		idStr := iv.JobPostingID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "job_posting.interviewer_assigned",
			ResourceType: "job_posting",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &iv, nil
}

// RemoveInterviewerInput holds fields for removing an interviewer.
type RemoveInterviewerInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	JobPostingID uuid.UUID
	UserID       uuid.UUID
	IP           *string
}

// RemoveInterviewer removes an interviewer from a posting.
func (s *Service) RemoveInterviewer(ctx context.Context, in RemoveInterviewerInput) error {
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`DELETE FROM job_posting_interviewers
			 WHERE tenant_id = ? AND job_posting_id = ? AND user_id = ?`,
			in.TenantID, in.JobPostingID, in.UserID,
		)
		if res.Error != nil {
			return fmt.Errorf("jobposting: remove interviewer: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := in.JobPostingID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "job_posting.interviewer_removed",
			ResourceType: "job_posting",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// ListInterviewers returns the interviewers assigned to a posting.
func (s *Service) ListInterviewers(ctx context.Context, tenantID, jobPostingID uuid.UUID) ([]Interviewer, error) {
	var interviewers []Interviewer
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, job_posting_id, user_id, created_at, updated_at
			 FROM job_posting_interviewers
			 WHERE tenant_id = ? AND job_posting_id = ?
			 ORDER BY created_at`,
			tenantID, jobPostingID,
		).Scan(&interviewers).Error
	})
	if err != nil {
		return nil, err
	}
	return interviewers, nil
}
