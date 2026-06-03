// Package approval implements the generic multi-step approval-workflow engine.
//
// Architecture overview:
//
//   - ApprovalRoute   defines the ordered approval steps for a request_type,
//     optionally scoped to a department.
//   - ApprovalRequest represents a single submitted request instance.
//   - ApprovalStep    records the per-step decision state and history.
//
// Other domains (leave, transfer, contract, etc.) call the public engine API:
//
//	Submit(ctx, SubmitInput) (*ApprovalRequest, error)
//	Decide(ctx, DecideInput) (*ApprovalRequest, error)
//	Cancel(ctx, CancelInput) (*ApprovalRequest, error)
package approval

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Status constants
// ---------------------------------------------------------------------------

// Request status values.
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusReturned  = "returned"
	StatusCancelled = "cancelled"
)

// Step decision values.
const (
	DecisionPending  = "pending"
	DecisionApproved = "approved"
	DecisionRejected = "rejected"
	DecisionReturned = "returned"
)

// ---------------------------------------------------------------------------
// GORM models
// ---------------------------------------------------------------------------

// RouteStep is the decoded representation of one element in steps_json.
// Either RoleRequired or UserID must be non-empty; both may be set.
type RouteStep struct {
	Step   int        `json:"step"`
	Role   string     `json:"role,omitempty"`
	UserID *uuid.UUID `json:"user_id,omitempty"`
}

// ApprovalRoute is the GORM model for the approval_routes table.
type ApprovalRoute struct { //nolint:revive // name is intentional for cross-package clarity
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	RequestType  string     `gorm:"column:request_type"`
	DepartmentID *uuid.UUID `gorm:"column:department_id"`
	Name         string     `gorm:"column:name"`
	StepsJSON    []byte     `gorm:"column:steps_json;type:jsonb"`
	Active       bool       `gorm:"column:active"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at"`
}

// TableName maps ApprovalRoute to the approval_routes table.
func (ApprovalRoute) TableName() string { return "approval_routes" }

// ApprovalRequest is the GORM model for the approval_requests table.
type ApprovalRequest struct { //nolint:revive // name is intentional for cross-package clarity
	ID                uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID `gorm:"column:tenant_id"`
	RequestType       string    `gorm:"column:request_type"`
	SubjectRef        string    `gorm:"column:subject_ref"`
	RequestedByUserID uuid.UUID `gorm:"column:requested_by_user_id"`
	RouteID           uuid.UUID `gorm:"column:route_id"`
	CurrentStep       int       `gorm:"column:current_step"`
	Status            string    `gorm:"column:status"`
	PayloadJSON       []byte    `gorm:"column:payload_json;type:jsonb"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

// TableName maps ApprovalRequest to the approval_requests table.
func (ApprovalRequest) TableName() string { return "approval_requests" }

// ApprovalStep is the GORM model for the approval_steps table.
type ApprovalStep struct { //nolint:revive // name is intentional for cross-package clarity
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	RequestID       uuid.UUID  `gorm:"column:request_id"`
	StepIndex       int        `gorm:"column:step_index"`
	ApproverUserID  *uuid.UUID `gorm:"column:approver_user_id"`
	DelegateUserID  *uuid.UUID `gorm:"column:delegate_user_id"`
	Decision        string     `gorm:"column:decision"`
	DecidedByUserID *uuid.UUID `gorm:"column:decided_by_user_id"`
	Comment         *string    `gorm:"column:comment"`
	DecidedAt       *time.Time `gorm:"column:decided_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
}

// TableName maps ApprovalStep to the approval_steps table.
func (ApprovalStep) TableName() string { return "approval_steps" }
