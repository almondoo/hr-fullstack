package approval

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
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	// ErrRouteNotFound is returned when no matching active route exists for the
	// given request_type (and optional department).
	ErrRouteNotFound = errors.New("approval: route not found")

	// ErrRequestNotFound is returned when a request does not exist or belongs
	// to a different tenant.
	ErrRequestNotFound = errors.New("approval: request not found")

	// ErrForbidden is returned when the actor is not authorised to decide on
	// the current step (not the assigned approver and not a delegate), or is not
	// permitted to perform the requested operation (e.g. SetDelegate by a
	// non-owner, non-approver, non-admin).
	ErrForbidden = errors.New("approval: not authorised to decide on this step")

	// ErrInvalidTransition is returned when the requested decision or status
	// move is not permitted from the current state.
	ErrInvalidTransition = errors.New("approval: invalid state transition")

	// ErrRouteEmpty is returned when the resolved route has no steps defined.
	ErrRouteEmpty = errors.New("approval: route has no steps")
)

// maxPayloadBytes caps the size of payload_json.
const maxPayloadBytes = 64 * 1024 // 64 KB

// ---------------------------------------------------------------------------
// Public engine API — input types
// ---------------------------------------------------------------------------

// SubmitInput is the parameter bag for Submit.
// PayloadJSON should contain reference IDs only; PII must not be stored.
type SubmitInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	RequestType  string
	SubjectRef   string
	DepartmentID *uuid.UUID // used to resolve the most-specific route
	PayloadJSON  []byte     // reference IDs only, no PII; validated by caller
	IP           *string
}

// DecideInput is the parameter bag for Decide.
// Decision must be one of "approved", "rejected", or "returned".
type DecideInput struct {
	TenantID  uuid.UUID
	RequestID uuid.UUID
	ActorID   uuid.UUID
	Decision  string
	Comment   *string
	IP        *string
}

// CancelInput is the parameter bag for Cancel.
// Only the original requester may cancel a pending request.
type CancelInput struct {
	TenantID  uuid.UUID
	RequestID uuid.UUID
	ActorID   uuid.UUID
	IP        *string
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service provides the approval-workflow engine.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Submit — create a new approval request
// ---------------------------------------------------------------------------

// Submit resolves the best-match active route for the given request_type
// (department-scoped route preferred over tenant-wide fallback), creates an
// ApprovalRequest with status=pending, and initialises all step rows.
//
// Route resolution priority:
//  1. Active route with request_type match AND department_id match (if departmentID given).
//  2. Active route with request_type match AND department_id IS NULL (tenant-wide fallback).
//
// All operations execute within a single WithinTenant transaction so the
// request rows and the audit entry are atomic.
func (s *Service) Submit(ctx context.Context, in SubmitInput) (*ApprovalRequest, error) {
	if err := validatePayload(in.PayloadJSON); err != nil {
		return nil, err
	}

	payload := in.PayloadJSON
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(`{}`)
	}

	req := ApprovalRequest{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		RequestType:       in.RequestType,
		SubjectRef:        in.SubjectRef,
		RequestedByUserID: in.ActorID,
		Status:            StatusPending,
		PayloadJSON:       payload,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		return submitInTx(tx, in.TenantID, in.ActorID, in.IP, in.DepartmentID, &req)
	}); err != nil {
		return nil, err
	}

	return &req, nil
}

// SubmitTx performs the same route-resolution and approval_request/step INSERT
// as Submit, but operates on the provided *gorm.DB transaction rather than
// opening a new WithinTenant transaction.  The caller is responsible for
// ensuring that tx is already scoped to in.TenantID (i.e. app.tenant_id is
// already set via SET LOCAL).
//
// This variant exists so that other domains (e.g. leave) can include approval
// submission atomically within their own transaction, preventing orphaned
// approval_request rows when the outer transaction rolls back.
func (s *Service) SubmitTx(tx *gorm.DB, in SubmitInput) (*ApprovalRequest, error) {
	if err := validatePayload(in.PayloadJSON); err != nil {
		return nil, err
	}

	payload := in.PayloadJSON
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(`{}`)
	}

	req := ApprovalRequest{
		ID:                uuid.New(),
		TenantID:          in.TenantID,
		RequestType:       in.RequestType,
		SubjectRef:        in.SubjectRef,
		RequestedByUserID: in.ActorID,
		Status:            StatusPending,
		PayloadJSON:       payload,
	}

	if err := submitInTx(tx, in.TenantID, in.ActorID, in.IP, in.DepartmentID, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// submitInTx is the shared implementation used by both Submit and SubmitTx.
// It resolves the approval route and inserts approval_request + approval_step
// rows using the provided tx.  req must be pre-populated with ID, TenantID,
// RequestType, SubjectRef, RequestedByUserID, Status, and PayloadJSON.
// departmentID is used for route resolution (department-scoped first, tenant-wide fallback).
func submitInTx(tx *gorm.DB, tenantID, actorID uuid.UUID, ip *string, departmentID *uuid.UUID, req *ApprovalRequest) error {
	// Resolve route: prefer department-scoped, fall back to tenant-wide.
	route, steps, err := resolveRoute(tx, tenantID, req.RequestType, departmentID)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return ErrRouteEmpty
	}

	req.RouteID = route.ID
	req.CurrentStep = 0

	if err := tx.Exec(
		`INSERT INTO approval_requests
		   (id, tenant_id, request_type, subject_ref, requested_by_user_id,
		    route_id, current_step, status, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?::jsonb)`,
		req.ID, req.TenantID, req.RequestType, req.SubjectRef,
		req.RequestedByUserID, req.RouteID, req.CurrentStep,
		req.Status, req.PayloadJSON,
	).Error; err != nil {
		return fmt.Errorf("approval: submit insert request: %w", err)
	}

	// Insert all step rows up-front.
	for _, step := range steps {
		stepID := uuid.New()
		var approverUserID *uuid.UUID
		if step.UserID != nil {
			approverUserID = step.UserID
		}
		if err := tx.Exec(
			`INSERT INTO approval_steps
			   (id, tenant_id, request_id, step_index, approver_user_id, decision)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			stepID, tenantID, req.ID, step.Step, approverUserID, DecisionPending,
		).Error; err != nil {
			return fmt.Errorf("approval: submit insert step %d: %w", step.Step, err)
		}
	}

	reqIDStr := req.ID.String()
	if err := audit.Record(tx, audit.Entry{
		TenantID:     tenantID,
		UserID:       &actorID,
		Action:       "approval.submitted",
		ResourceType: "approval_request",
		ResourceID:   &reqIDStr,
		IP:           ip,
	}); err != nil {
		return fmt.Errorf("approval: submit audit: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Decide — submit a decision on the current step
// ---------------------------------------------------------------------------

// Decide records a decision (approved / rejected / returned) on the current
// step of an existing pending request.
//
// Authorisation: the actor must be either the assigned approver_user_id for
// the current step, or the delegate_user_id on that step, or a user whose
// role matches the role requirement defined in the route step definition.
// If the step defines neither a user nor a role, the step is treated as
// undecidable (deny) — open-approver steps are not permitted.
//
// State transitions:
//   - approved:  advance to next step; if no more steps → request status=approved.
//   - rejected:  request status=rejected (terminal).
//   - returned:  step back to previous step (or step 0); request stays pending.
//     When returned from step 0, the request status=returned (requester must
//     re-submit or cancel).
//
// Only requests with status=pending may receive decisions.
// Attempting to decide on a non-pending request returns ErrInvalidTransition.
func (s *Service) Decide(ctx context.Context, in DecideInput) (*ApprovalRequest, error) {
	if err := validateDecision(in.Decision); err != nil {
		return nil, err
	}

	var updated ApprovalRequest

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Load request (must be in this tenant and pending).
		var req ApprovalRequest
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("approval: decide load request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrRequestNotFound
		}
		if req.Status != StatusPending {
			return fmt.Errorf("%w: request status is %q (not pending)", ErrInvalidTransition, req.Status)
		}

		// Load the current step row.
		var step ApprovalStep
		if err := tx.Raw(
			`SELECT id, tenant_id, request_id, step_index, approver_user_id,
			        delegate_user_id, decision, decided_by_user_id, comment,
			        decided_at, created_at
			 FROM approval_steps
			 WHERE request_id = ? AND tenant_id = ? AND step_index = ?
			 LIMIT 1`,
			req.ID, in.TenantID, req.CurrentStep,
		).Scan(&step).Error; err != nil {
			return fmt.Errorf("approval: decide load step: %w", err)
		}
		if step.ID == uuid.Nil {
			return fmt.Errorf("approval: step %d not found for request %s", req.CurrentStep, req.ID)
		}
		if step.Decision != DecisionPending {
			return fmt.Errorf("%w: step %d already has decision %q", ErrInvalidTransition, req.CurrentStep, step.Decision)
		}

		// Authorisation: actor must be the assigned approver, delegate, or a
		// user whose role matches the step's role requirement.
		authorised, err := checkDecideAuthorisation(tx, in.TenantID, req, step, in.ActorID)
		if err != nil {
			return fmt.Errorf("approval: decide auth check: %w", err)
		}
		if !authorised {
			return ErrForbidden
		}

		// Count total steps for this route.
		totalSteps, err := countSteps(tx, in.TenantID, req.ID)
		if err != nil {
			return err
		}

		now := time.Now().UTC()

		// Record the decision on the current step.
		if err := tx.Exec(
			`UPDATE approval_steps
			 SET decision = ?, decided_by_user_id = ?, comment = ?, decided_at = ?
			 WHERE id = ? AND tenant_id = ?`,
			in.Decision, in.ActorID, in.Comment, now, step.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("approval: decide update step: %w", err)
		}

		// Determine new request state.
		var newStatus string
		var newStep int

		switch in.Decision {
		case DecisionApproved:
			nextStep := req.CurrentStep + 1
			if nextStep >= totalSteps {
				// Final step approved — request is fully approved.
				newStatus = StatusApproved
				newStep = req.CurrentStep
			} else {
				// Advance to next step.
				newStatus = StatusPending
				newStep = nextStep
			}

		case DecisionRejected:
			newStatus = StatusRejected
			newStep = req.CurrentStep

		case DecisionReturned:
			prevStep := req.CurrentStep - 1
			if prevStep < 0 {
				// Returned from first step — back to requester.
				newStatus = StatusReturned
				newStep = 0
			} else {
				// Return to previous step: reset that step's decision to pending.
				if err := tx.Exec(
					`UPDATE approval_steps
					 SET decision = ?, decided_by_user_id = NULL, comment = NULL, decided_at = NULL
					 WHERE request_id = ? AND tenant_id = ? AND step_index = ?`,
					DecisionPending, req.ID, in.TenantID, prevStep,
				).Error; err != nil {
					return fmt.Errorf("approval: decide reset prev step: %w", err)
				}
				newStatus = StatusPending
				newStep = prevStep
			}
		}

		if err := tx.Exec(
			`UPDATE approval_requests
			 SET status = ?, current_step = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			newStatus, newStep, req.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("approval: decide update request: %w", err)
		}

		// Re-read updated request.
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			req.ID, in.TenantID,
		).Scan(&updated).Error; err != nil {
			return fmt.Errorf("approval: decide re-read request: %w", err)
		}

		reqIDStr := req.ID.String()
		// [Imp 6] Use an explicit switch for audit action rather than string
		// concatenation, to prevent unexpected action names if Decision values
		// were ever extended without updating this code path.
		var auditAction string
		switch in.Decision {
		case DecisionApproved:
			auditAction = "approval.approved"
		case DecisionRejected:
			auditAction = "approval.rejected"
		case DecisionReturned:
			auditAction = "approval.returned"
		default:
			// validateDecision above ensures this branch is unreachable.
			auditAction = "approval.decided"
		}
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       auditAction,
			ResourceType: "approval_request",
			ResourceID:   &reqIDStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("approval: decide audit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &updated, nil
}

// ---------------------------------------------------------------------------
// Cancel — requester cancels a pending request
// ---------------------------------------------------------------------------

// Cancel allows the original requester to withdraw a pending request.
// Requests that are already in a terminal state (approved/rejected/returned/
// cancelled) cannot be cancelled.
func (s *Service) Cancel(ctx context.Context, in CancelInput) (*ApprovalRequest, error) {
	var updated ApprovalRequest

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var req ApprovalRequest
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("approval: cancel load request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrRequestNotFound
		}

		// Only the requester may cancel.
		if req.RequestedByUserID != in.ActorID {
			return ErrForbidden
		}

		// Only pending or returned requests can be cancelled.
		if req.Status != StatusPending && req.Status != StatusReturned {
			return fmt.Errorf("%w: request status is %q", ErrInvalidTransition, req.Status)
		}

		if err := tx.Exec(
			`UPDATE approval_requests
			 SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			StatusCancelled, req.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("approval: cancel update: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			req.ID, in.TenantID,
		).Scan(&updated).Error; err != nil {
			return fmt.Errorf("approval: cancel re-read: %w", err)
		}

		reqIDStr := req.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "approval.cancelled",
			ResourceType: "approval_request",
			ResourceID:   &reqIDStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("approval: cancel audit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &updated, nil
}

// ---------------------------------------------------------------------------
// Route administration
// ---------------------------------------------------------------------------

// CreateRouteInput holds validated fields for a new approval route.
type CreateRouteInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	RequestType  string
	DepartmentID *uuid.UUID
	Name         string
	Steps        []RouteStep
	IP           *string
}

// CreateRoute inserts a new active approval route.
func (s *Service) CreateRoute(ctx context.Context, in CreateRouteInput) (*ApprovalRoute, error) {
	stepsJSON, err := json.Marshal(in.Steps)
	if err != nil {
		return nil, fmt.Errorf("approval: marshal steps: %w", err)
	}

	route := ApprovalRoute{
		ID:           uuid.New(),
		TenantID:     in.TenantID,
		RequestType:  in.RequestType,
		DepartmentID: in.DepartmentID,
		Name:         in.Name,
		StepsJSON:    stepsJSON,
		Active:       true,
	}

	if err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Validate optional department FK.
		if in.DepartmentID != nil {
			var cnt int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM departments WHERE id = ? AND tenant_id = ?`,
				*in.DepartmentID, in.TenantID,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("approval: create route verify department: %w", err)
			}
			if cnt == 0 {
				return ErrRouteNotFound
			}
		}

		if err := tx.Exec(
			`INSERT INTO approval_routes
			   (id, tenant_id, request_type, department_id, name, steps_json, active)
			 VALUES (?, ?, ?, ?, ?, ?::jsonb, ?)`,
			route.ID, route.TenantID, route.RequestType, route.DepartmentID,
			route.Name, route.StepsJSON, route.Active,
		).Error; err != nil {
			return fmt.Errorf("approval: create route insert: %w", err)
		}

		idStr := route.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "approval_route.created",
			ResourceType: "approval_route",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("approval: create route audit: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return &route, nil
}

// GetRequest fetches a single approval request by ID within the tenant.
//
// [Imp 4] Visibility is restricted to: the requester, any step approver or
// delegate on the request, and users with approval:admin.  This prevents an
// approval:read-only third party from browsing all requests and leaking HR
// sensitive data (subject_ref, payload_json).  The handler still requires the
// approval:read RBAC permission at the routing layer, so the combined check is:
// has approval:read AND (is owner OR is involved approver/delegate OR has approval:admin).
func (s *Service) GetRequest(ctx context.Context, tenantID, id uuid.UUID, actorID uuid.UUID) (*ApprovalRequest, error) {
	var req ApprovalRequest
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("approval: get request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrRequestNotFound
		}

		// Check visibility.
		visible, err := isRequestVisible(tx, tenantID, req, actorID)
		if err != nil {
			return fmt.Errorf("approval: get request visibility check: %w", err)
		}
		if !visible {
			// Return not-found rather than forbidden to avoid leaking existence.
			return ErrRequestNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// ListRequestsByUser lists all approval requests submitted by a user.
func (s *Service) ListRequestsByUser(ctx context.Context, tenantID, userID uuid.UUID) ([]ApprovalRequest, error) {
	var reqs []ApprovalRequest
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE tenant_id = ? AND requested_by_user_id = ?
			 ORDER BY created_at DESC`,
			tenantID, userID,
		).Scan(&reqs).Error
	})
	if err != nil {
		return nil, err
	}
	return reqs, nil
}

// ListPendingForApprover returns all pending requests where the given user is
// the assigned approver (or delegate) on the current step.
func (s *Service) ListPendingForApprover(ctx context.Context, tenantID, approverID uuid.UUID) ([]ApprovalRequest, error) {
	var reqs []ApprovalRequest
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT ar.id, ar.tenant_id, ar.request_type, ar.subject_ref,
			        ar.requested_by_user_id, ar.route_id, ar.current_step,
			        ar.status, ar.payload_json, ar.created_at, ar.updated_at
			 FROM approval_requests ar
			 JOIN approval_steps s
			   ON s.request_id = ar.id
			  AND s.tenant_id  = ar.tenant_id
			  AND s.step_index = ar.current_step
			 WHERE ar.tenant_id = ?
			   AND ar.status    = 'pending'
			   AND (s.approver_user_id = ? OR s.delegate_user_id = ?)
			 ORDER BY ar.created_at ASC`,
			tenantID, approverID, approverID,
		).Scan(&reqs).Error
	})
	if err != nil {
		return nil, err
	}
	return reqs, nil
}

// ListSteps returns all step rows for a given request.
//
// [Imp 4] Visibility is restricted identically to GetRequest: requester,
// involved approver/delegate on any step, or approval:admin.  Returning step
// details (approver_user_id, decided_by_user_id, comment) to unauthorised
// third parties would leak HR-sensitive workflow data.
func (s *Service) ListSteps(ctx context.Context, tenantID, requestID uuid.UUID, actorID uuid.UUID) ([]ApprovalStep, error) {
	var steps []ApprovalStep
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// First verify the request exists and the actor can see it.
		var req ApprovalRequest
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, subject_ref, requested_by_user_id,
			        route_id, current_step, status, payload_json, created_at, updated_at
			 FROM approval_requests
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			requestID, tenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("approval: list steps load request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrRequestNotFound
		}

		visible, err := isRequestVisible(tx, tenantID, req, actorID)
		if err != nil {
			return fmt.Errorf("approval: list steps visibility check: %w", err)
		}
		if !visible {
			return ErrRequestNotFound
		}

		return tx.Raw(
			`SELECT id, tenant_id, request_id, step_index, approver_user_id,
			        delegate_user_id, decision, decided_by_user_id, comment,
			        decided_at, created_at
			 FROM approval_steps
			 WHERE tenant_id = ? AND request_id = ?
			 ORDER BY step_index ASC`,
			tenantID, requestID,
		).Scan(&steps).Error
	})
	if err != nil {
		return nil, err
	}
	return steps, nil
}

// SetDelegate assigns or replaces the delegate_user_id on a specific step.
//
// [MUSTFIX 2] delegateUserID is verified to belong to the same tenant before
// the update, preventing cross-tenant delegate assignment.
//
// [MUSTFIX 3] The actor must be: the original requester, the assigned approver
// on the target step, or a user with approval:admin permission.  Any
// authenticated user with approval:write could otherwise replace the approver
// on any pending step.
func (s *Service) SetDelegate(ctx context.Context, tenantID, requestID uuid.UUID, stepIndex int, delegateUserID uuid.UUID, actorID uuid.UUID, ip *string) error {
	return s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// [MUSTFIX 2] Verify the delegate user belongs to this tenant.
		var delegateCnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
			delegateUserID, tenantID,
		).Scan(&delegateCnt).Error; err != nil {
			return fmt.Errorf("approval: set_delegate verify delegate tenant: %w", err)
		}
		if delegateCnt == 0 {
			return fmt.Errorf("approval: delegate user not found in this tenant: %w", ErrForbidden)
		}

		// Verify request belongs to this tenant and is still pending.
		var req ApprovalRequest
		if err := tx.Raw(
			`SELECT id, tenant_id, requested_by_user_id, status
			 FROM approval_requests WHERE id = ? AND tenant_id = ? LIMIT 1`,
			requestID, tenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("approval: set_delegate load request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrRequestNotFound
		}
		if req.Status != StatusPending {
			return fmt.Errorf("%w: request status is %q", ErrInvalidTransition, req.Status)
		}

		// Load the target step to check the assigned approver.
		var step ApprovalStep
		if err := tx.Raw(
			`SELECT id, tenant_id, request_id, step_index, approver_user_id,
			        delegate_user_id, decision, decided_by_user_id, comment,
			        decided_at, created_at
			 FROM approval_steps
			 WHERE request_id = ? AND tenant_id = ? AND step_index = ? AND decision = 'pending'
			 LIMIT 1`,
			requestID, tenantID, stepIndex,
		).Scan(&step).Error; err != nil {
			return fmt.Errorf("approval: set_delegate load step: %w", err)
		}
		if step.ID == uuid.Nil {
			return fmt.Errorf("approval: step not found or not pending")
		}

		// [MUSTFIX 3] Actor must be the requester, the step's assigned approver,
		// or hold approval:admin.
		isRequester := req.RequestedByUserID == actorID
		isApprover := step.ApproverUserID != nil && *step.ApproverUserID == actorID
		if !isRequester && !isApprover {
			perms, err := platformauth.LoadUserPermissions(tx, tenantID, actorID)
			if err != nil {
				return fmt.Errorf("approval: set_delegate load actor perms: %w", err)
			}
			if !platformauth.HasPermission(perms, "approval:admin") {
				return fmt.Errorf("approval: actor not authorised to set delegate: %w", ErrForbidden)
			}
		}

		res := tx.Exec(
			`UPDATE approval_steps
			 SET delegate_user_id = ?
			 WHERE request_id = ? AND tenant_id = ? AND step_index = ? AND decision = 'pending'`,
			delegateUserID, requestID, tenantID, stepIndex,
		)
		if res.Error != nil {
			return fmt.Errorf("approval: set_delegate update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("approval: step not found or not pending")
		}

		reqIDStr := requestID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &actorID,
			Action:       "approval.delegate_set",
			ResourceType: "approval_request",
			ResourceID:   &reqIDStr,
			IP:           ip,
		}); err != nil {
			return fmt.Errorf("approval: set_delegate audit: %w", err)
		}
		return nil
	})
}

// ListRoutes returns all routes for a tenant, ordered by request_type.
func (s *Service) ListRoutes(ctx context.Context, tenantID uuid.UUID) ([]ApprovalRoute, error) {
	var routes []ApprovalRoute
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, request_type, department_id, name,
			        steps_json, active, created_at, updated_at
			 FROM approval_routes
			 WHERE tenant_id = ?
			 ORDER BY request_type, name`,
			tenantID,
		).Scan(&routes).Error
	})
	if err != nil {
		return nil, err
	}
	return routes, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// resolveRoute selects the best-match active route for (tenantID, requestType,
// departmentID). Department-scoped routes take priority over tenant-wide ones.
func resolveRoute(tx *gorm.DB, tenantID uuid.UUID, requestType string, departmentID *uuid.UUID) (*ApprovalRoute, []RouteStep, error) {
	var route ApprovalRoute

	if departmentID != nil {
		// Try department-scoped route first.
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, department_id, name,
			        steps_json, active, created_at, updated_at
			 FROM approval_routes
			 WHERE tenant_id = ? AND request_type = ? AND department_id = ? AND active = true
			 ORDER BY created_at DESC
			 LIMIT 1`,
			tenantID, requestType, *departmentID,
		).Scan(&route).Error; err != nil {
			return nil, nil, fmt.Errorf("approval: resolve route (dept): %w", err)
		}
	}

	if route.ID == uuid.Nil {
		// Fall back to tenant-wide route.
		if err := tx.Raw(
			`SELECT id, tenant_id, request_type, department_id, name,
			        steps_json, active, created_at, updated_at
			 FROM approval_routes
			 WHERE tenant_id = ? AND request_type = ? AND department_id IS NULL AND active = true
			 ORDER BY created_at DESC
			 LIMIT 1`,
			tenantID, requestType,
		).Scan(&route).Error; err != nil {
			return nil, nil, fmt.Errorf("approval: resolve route (tenant-wide): %w", err)
		}
	}

	if route.ID == uuid.Nil {
		return nil, nil, ErrRouteNotFound
	}

	steps, err := decodeSteps(route.StepsJSON)
	if err != nil {
		return nil, nil, err
	}

	return &route, steps, nil
}

// decodeSteps unmarshals the steps_json JSONB column into []RouteStep.
func decodeSteps(raw []byte) ([]RouteStep, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		return nil, nil
	}
	var steps []RouteStep
	if err := json.Unmarshal(raw, &steps); err != nil {
		return nil, fmt.Errorf("approval: decode steps_json: %w", err)
	}
	return steps, nil
}

// countSteps returns the total number of step rows for a request.
func countSteps(tx *gorm.DB, tenantID, requestID uuid.UUID) (int, error) {
	var cnt int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM approval_steps WHERE request_id = ? AND tenant_id = ?`,
		requestID, tenantID,
	).Scan(&cnt).Error; err != nil {
		return 0, fmt.Errorf("approval: count steps: %w", err)
	}
	return int(cnt), nil
}

// checkDecideAuthorisation verifies that actorID is permitted to decide on
// the current step of req.
//
// [MUSTFIX 1] Authorization rules (evaluated in order):
//  1. If approver_user_id matches actorID → allowed.
//  2. If delegate_user_id matches actorID → allowed.
//  3. If the route step defines a required role AND the actor's role (looked up
//     within the same tenant via users→roles) matches → allowed.
//  4. If both approver_user_id and delegate_user_id are NULL and no role is
//     required by the step definition → DENIED (not allowed).
//     This removes the previous open-approver fallback that permitted any
//     approval:write user to decide on unassigned steps.
//
// Tenant-crossing prevention: role lookup uses the WithinTenant tx, which has
// app.tenant_id set; the explicit tenant_id WHERE clause is an additional layer.
func checkDecideAuthorisation(tx *gorm.DB, tenantID uuid.UUID, req ApprovalRequest, step ApprovalStep, actorID uuid.UUID) (bool, error) {
	// Rule 1: directly assigned approver.
	if step.ApproverUserID != nil && *step.ApproverUserID == actorID {
		return true, nil
	}
	// Rule 2: delegate.
	if step.DelegateUserID != nil && *step.DelegateUserID == actorID {
		return true, nil
	}

	// Rule 3: role-based check — fetch the required role from the route step
	// definition (steps_json), then compare to the actor's role name.
	requiredRole, err := loadRequiredRoleForStep(tx, tenantID, req.RouteID, step.StepIndex)
	if err != nil {
		return false, fmt.Errorf("approval: load required role: %w", err)
	}
	if requiredRole != "" {
		actorRoleName, err := loadUserRoleName(tx, tenantID, actorID)
		if err != nil {
			return false, fmt.Errorf("approval: load actor role name: %w", err)
		}
		if actorRoleName == requiredRole {
			return true, nil
		}
	}

	// Rule 4: neither user nor role matched — deny.
	// The previous fallback (both NULL → allow any actor) is intentionally
	// removed: a step with no user and no role assignment is undecidable.
	return false, nil
}

// routeStepsRow is the minimal shape scanned when fetching steps_json from
// approval_routes.  Using a struct field with type []byte avoids the GORM
// scalar-scan issue where Raw(...).Scan(&[]byte) misinterprets JSONB columns.
type routeStepsRow struct {
	StepsJSON []byte `gorm:"column:steps_json;type:jsonb"`
}

// loadRequiredRoleForStep fetches the route identified by routeID (within the
// tenant) and returns the role name required for the step at stepIndex.
// Returns "" when the step defines no role requirement.
func loadRequiredRoleForStep(tx *gorm.DB, tenantID uuid.UUID, routeID uuid.UUID, stepIndex int) (string, error) {
	var row routeStepsRow
	if err := tx.Raw(
		`SELECT steps_json FROM approval_routes WHERE id = ? AND tenant_id = ? LIMIT 1`,
		routeID, tenantID,
	).Scan(&row).Error; err != nil {
		return "", fmt.Errorf("approval: load route steps_json: %w", err)
	}
	if len(row.StepsJSON) == 0 {
		return "", nil
	}
	steps, err := decodeSteps(row.StepsJSON)
	if err != nil {
		return "", err
	}
	for _, s := range steps {
		if s.Step == stepIndex {
			return s.Role, nil
		}
	}
	return "", nil
}

// loadUserRoleName fetches the role name for a user within a tenant.
// Returns "" when the user has no role assigned or the role does not exist.
// The lookup is scoped to the tenant via both RLS (app.tenant_id) and explicit
// WHERE clause to prevent cross-tenant role references.
func loadUserRoleName(tx *gorm.DB, tenantID, userID uuid.UUID) (string, error) {
	var roleName string
	if err := tx.Raw(
		`SELECT r.name
		 FROM users u
		 JOIN roles r ON r.id = u.role_id AND r.tenant_id = u.tenant_id
		 WHERE u.id = ? AND u.tenant_id = ?
		 LIMIT 1`,
		userID, tenantID,
	).Scan(&roleName).Error; err != nil {
		return "", fmt.Errorf("approval: load user role name: %w", err)
	}
	return roleName, nil
}

// isRequestVisible reports whether actorID is permitted to view the given
// request.  An actor may view a request when they are:
//   - the original requester (requested_by_user_id = actorID), OR
//   - an approver or delegate on any step of the request, OR
//   - a user holding the approval:admin permission.
//
// This helper must be called inside a WithinTenant transaction.
func isRequestVisible(tx *gorm.DB, tenantID uuid.UUID, req ApprovalRequest, actorID uuid.UUID) (bool, error) {
	// Owner check.
	if req.RequestedByUserID == actorID {
		return true, nil
	}

	// Approver/delegate on any step of this request.
	var involvementCnt int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM approval_steps
		 WHERE request_id = ? AND tenant_id = ?
		   AND (approver_user_id = ? OR delegate_user_id = ?)`,
		req.ID, tenantID, actorID, actorID,
	).Scan(&involvementCnt).Error; err != nil {
		return false, fmt.Errorf("approval: visibility involvement check: %w", err)
	}
	if involvementCnt > 0 {
		return true, nil
	}

	// Admin permission check.
	perms, err := platformauth.LoadUserPermissions(tx, tenantID, actorID)
	if err != nil {
		return false, fmt.Errorf("approval: visibility perms check: %w", err)
	}
	if platformauth.HasPermission(perms, "approval:admin") {
		return true, nil
	}

	return false, nil
}

// validateDecision checks that the decision value is one of the allowed literals.
func validateDecision(d string) error {
	switch d {
	case DecisionApproved, DecisionRejected, DecisionReturned:
		return nil
	default:
		return fmt.Errorf("%w: decision must be 'approved', 'rejected', or 'returned'", ErrInvalidTransition)
	}
}

// validatePayload validates that payload JSON is either empty or valid JSON
// within the size limit. PII must not be present — callers are responsible for
// this invariant at the domain boundary.
func validatePayload(p []byte) error {
	if len(p) == 0 || string(p) == "null" {
		return nil
	}
	if len(p) > maxPayloadBytes {
		return fmt.Errorf("approval: payload_json exceeds maximum size of %d bytes", maxPayloadBytes)
	}
	if !json.Valid(p) {
		return fmt.Errorf("approval: payload_json is not valid JSON")
	}
	return nil
}
