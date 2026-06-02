package approval

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/httpx"
)

var validate = validator.New()

// Handler exposes HTTP endpoints for the approval-workflow domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ---------------------------------------------------------------------------
// Response shapes
// ---------------------------------------------------------------------------

// RouteResponse is the JSON representation of an ApprovalRoute.
type RouteResponse struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	RequestType  string          `json:"request_type"`
	DepartmentID *uuid.UUID      `json:"department_id,omitempty"`
	Name         string          `json:"name"`
	Steps        json.RawMessage `json:"steps"`
	Active       bool            `json:"active"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func toRouteResponse(r *ApprovalRoute) RouteResponse {
	steps := json.RawMessage(r.StepsJSON)
	if len(steps) == 0 {
		steps = json.RawMessage(`[]`)
	}
	return RouteResponse{
		ID:           r.ID,
		TenantID:     r.TenantID,
		RequestType:  r.RequestType,
		DepartmentID: r.DepartmentID,
		Name:         r.Name,
		Steps:        steps,
		Active:       r.Active,
		CreatedAt:    r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// RequestResponse is the JSON representation of an ApprovalRequest.
type RequestResponse struct {
	ID                uuid.UUID       `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	RequestType       string          `json:"request_type"`
	SubjectRef        string          `json:"subject_ref"`
	RequestedByUserID uuid.UUID       `json:"requested_by_user_id"`
	RouteID           uuid.UUID       `json:"route_id"`
	CurrentStep       int             `json:"current_step"`
	Status            string          `json:"status"`
	Payload           json.RawMessage `json:"payload"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
}

func toRequestResponse(r *ApprovalRequest) RequestResponse {
	payload := json.RawMessage(r.PayloadJSON)
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return RequestResponse{
		ID:                r.ID,
		TenantID:          r.TenantID,
		RequestType:       r.RequestType,
		SubjectRef:        r.SubjectRef,
		RequestedByUserID: r.RequestedByUserID,
		RouteID:           r.RouteID,
		CurrentStep:       r.CurrentStep,
		Status:            r.Status,
		Payload:           payload,
		CreatedAt:         r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// StepResponse is the JSON representation of an ApprovalStep.
type StepResponse struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	RequestID       uuid.UUID  `json:"request_id"`
	StepIndex       int        `json:"step_index"`
	ApproverUserID  *uuid.UUID `json:"approver_user_id,omitempty"`
	DelegateUserID  *uuid.UUID `json:"delegate_user_id,omitempty"`
	Decision        string     `json:"decision"`
	DecidedByUserID *uuid.UUID `json:"decided_by_user_id,omitempty"`
	Comment         *string    `json:"comment,omitempty"`
	DecidedAt       *string    `json:"decided_at,omitempty"`
	CreatedAt       string     `json:"created_at"`
}

func toStepResponse(s *ApprovalStep) StepResponse {
	r := StepResponse{
		ID:              s.ID,
		TenantID:        s.TenantID,
		RequestID:       s.RequestID,
		StepIndex:       s.StepIndex,
		ApproverUserID:  s.ApproverUserID,
		DelegateUserID:  s.DelegateUserID,
		Decision:        s.Decision,
		DecidedByUserID: s.DecidedByUserID,
		Comment:         s.Comment,
		CreatedAt:       s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if s.DecidedAt != nil {
		ts := s.DecidedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.DecidedAt = &ts
	}
	return r
}

// ---------------------------------------------------------------------------
// Request shapes
// ---------------------------------------------------------------------------

type createRouteRequest struct {
	RequestType  string      `json:"request_type"  validate:"required,max=100"`
	DepartmentID *uuid.UUID  `json:"department_id"`
	Name         string      `json:"name"          validate:"required,max=200"`
	Steps        []routeStep `json:"steps"         validate:"required,min=1,dive"`
}

type routeStep struct {
	Step   int        `json:"step"    validate:"min=0"`
	Role   string     `json:"role"    validate:"max=100"`
	UserID *uuid.UUID `json:"user_id"`
}

type submitRequest struct {
	RequestType  string          `json:"request_type"   validate:"required,max=100"`
	SubjectRef   string          `json:"subject_ref"    validate:"max=200"`
	DepartmentID *uuid.UUID      `json:"department_id"`
	PayloadJSON  json.RawMessage `json:"payload"`
}

type decideRequest struct {
	Decision string  `json:"decision" validate:"required,oneof=approved rejected returned"`
	Comment  *string `json:"comment"`
}

type setDelegateRequest struct {
	StepIndex      int       `json:"step_index"       validate:"min=0"`
	DelegateUserID uuid.UUID `json:"delegate_user_id" validate:"required"`
}

// ---------------------------------------------------------------------------
// Validation helper
// ---------------------------------------------------------------------------

func validationMessage(err error) string {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		e := ve[0]
		return "validation failed on field '" + e.Field() + "' (" + e.Tag() + ")"
	}
	return "validation failed"
}

func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// ---------------------------------------------------------------------------
// Route administration handlers
// ---------------------------------------------------------------------------

// CreateRoute handles POST /approval/routes.
func (h *Handler) CreateRoute(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createRouteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	steps := make([]RouteStep, len(req.Steps))
	for i, s := range req.Steps {
		steps[i] = RouteStep{
			Step:   s.Step,
			Role:   s.Role,
			UserID: s.UserID,
		}
	}

	route, err := h.svc.CreateRoute(c.Request.Context(), CreateRouteInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		RequestType:  req.RequestType,
		DepartmentID: req.DepartmentID,
		Name:         req.Name,
		Steps:        steps,
		IP:           clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrRouteNotFound) {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "department not found in this tenant")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusCreated, toRouteResponse(route))
}

// ListRoutes handles GET /approval/routes.
func (h *Handler) ListRoutes(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	routes, err := h.svc.ListRoutes(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]RouteResponse, len(routes))
	for i := range routes {
		items[i] = toRouteResponse(&routes[i])
	}
	c.JSON(http.StatusOK, gin.H{"routes": items})
}

// ---------------------------------------------------------------------------
// Request handlers
// ---------------------------------------------------------------------------

// Submit handles POST /approval/requests.
func (h *Handler) Submit(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req submitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	payload := []byte(req.PayloadJSON)
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte(`{}`)
	}

	ar, err := h.svc.Submit(c.Request.Context(), SubmitInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		RequestType:  req.RequestType,
		SubjectRef:   req.SubjectRef,
		DepartmentID: req.DepartmentID,
		PayloadJSON:  payload,
		IP:           clientIP(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrRouteNotFound):
			httpx.RespondError(c, http.StatusBadRequest, "ROUTE_NOT_FOUND", "no active route found for this request type")
		case errors.Is(err, ErrRouteEmpty):
			httpx.RespondError(c, http.StatusBadRequest, "ROUTE_EMPTY", "route has no steps configured")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}

	c.JSON(http.StatusCreated, toRequestResponse(ar))
}

// GetRequest handles GET /approval/requests/:id.
func (h *Handler) GetRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	ar, err := h.svc.GetRequest(c.Request.Context(), tenantID, id, actorID)
	if err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "request not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toRequestResponse(ar))
}

// ListMyRequests handles GET /approval/requests/mine.
func (h *Handler) ListMyRequests(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reqs, err := h.svc.ListRequestsByUser(c.Request.Context(), tenantID, actorID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]RequestResponse, len(reqs))
	for i := range reqs {
		items[i] = toRequestResponse(&reqs[i])
	}
	c.JSON(http.StatusOK, gin.H{"requests": items})
}

// ListPendingForMe handles GET /approval/requests/pending.
func (h *Handler) ListPendingForMe(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reqs, err := h.svc.ListPendingForApprover(c.Request.Context(), tenantID, actorID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]RequestResponse, len(reqs))
	for i := range reqs {
		items[i] = toRequestResponse(&reqs[i])
	}
	c.JSON(http.StatusOK, gin.H{"requests": items})
}

// Decide handles POST /approval/requests/:id/decide.
func (h *Handler) Decide(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	var req decideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	ar, err := h.svc.Decide(c.Request.Context(), DecideInput{
		TenantID:  tenantID,
		RequestID: id,
		ActorID:   actorID,
		Decision:  req.Decision,
		Comment:   req.Comment,
		IP:        clientIP(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestNotFound):
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "request not found")
		case errors.Is(err, ErrForbidden):
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "not authorised to decide on this step")
		case errors.Is(err, ErrInvalidTransition):
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "decision not allowed in current state")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}

	c.JSON(http.StatusOK, toRequestResponse(ar))
}

// Cancel handles POST /approval/requests/:id/cancel.
func (h *Handler) Cancel(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	ar, err := h.svc.Cancel(c.Request.Context(), CancelInput{
		TenantID:  tenantID,
		RequestID: id,
		ActorID:   actorID,
		IP:        clientIP(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestNotFound):
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "request not found")
		case errors.Is(err, ErrForbidden):
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "only the requester may cancel")
		case errors.Is(err, ErrInvalidTransition):
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "request cannot be cancelled in current state")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}

	c.JSON(http.StatusOK, toRequestResponse(ar))
}

// ListSteps handles GET /approval/requests/:id/steps.
func (h *Handler) ListSteps(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	steps, err := h.svc.ListSteps(c.Request.Context(), tenantID, id, actorID)
	if err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "request not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	items := make([]StepResponse, len(steps))
	for i := range steps {
		items[i] = toStepResponse(&steps[i])
	}
	c.JSON(http.StatusOK, gin.H{"steps": items})
}

// SetDelegate handles PUT /approval/requests/:id/delegate.
func (h *Handler) SetDelegate(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	var req setDelegateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	if err := h.svc.SetDelegate(c.Request.Context(), tenantID, id, req.StepIndex, req.DelegateUserID, actorID, clientIP(c)); err != nil {
		switch {
		case errors.Is(err, ErrRequestNotFound):
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "request not found")
		case errors.Is(err, ErrForbidden):
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "not authorised to set delegate on this step")
		case errors.Is(err, ErrInvalidTransition):
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "request is not in a modifiable state")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "delegate set"})
}
