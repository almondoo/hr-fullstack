package goal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/httpx"
)

var validate = validator.New()

// Handler exposes HTTP endpoints for the goal-management domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts a validator.ValidationErrors into a safe message.
func validationMessage(err error) string {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		e := ve[0]
		return fmt.Sprintf("validation failed on field '%s' (%s)", e.Field(), e.Tag())
	}
	return "validation failed"
}

// clientIP extracts the client IP from the gin context.
func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// respondError maps a service error to an HTTP response.
func respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrCycleClosed):
		httpx.RespondError(c, http.StatusConflict, "CYCLE_CLOSED", "review cycle is closed (read-only)")
	case errors.Is(err, ErrCascadeCycle):
		httpx.RespondError(c, http.StatusConflict, "CASCADE_CYCLE", "parent link would create a cascade cycle")
	case errors.Is(err, ErrCascadeTooDeep):
		httpx.RespondError(c, http.StatusConflict, "CASCADE_TOO_DEEP", "cascade depth exceeds the configured maximum")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	case errors.Is(err, ErrInvalidInput):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid input")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Review cycle shapes / handlers
// ---------------------------------------------------------------------------

type createCycleRequest struct {
	Name             string  `json:"name"               validate:"required,max=200"`
	StartsOn         string  `json:"starts_on"          validate:"required"`
	EndsOn           string  `json:"ends_on"            validate:"required"`
	GoalDueOn        *string `json:"goal_due_on"`
	ReviewDueOn      *string `json:"review_due_on"`
	RequireWeight100 bool    `json:"require_weight_100"`
	ProgressMethod   string  `json:"progress_method"    validate:"omitempty,oneof=average weighted"`
	MaxCascadeDepth  int     `json:"max_cascade_depth"  validate:"omitempty,min=1,max=100"`
}

type updateCycleStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=draft active closed"`
}

// CycleResponse is the JSON representation of a review cycle.
type CycleResponse struct {
	ID               uuid.UUID `json:"id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	Name             string    `json:"name"`
	StartsOn         string    `json:"starts_on"`
	EndsOn           string    `json:"ends_on"`
	GoalDueOn        *string   `json:"goal_due_on,omitempty"`
	ReviewDueOn      *string   `json:"review_due_on,omitempty"`
	Status           string    `json:"status"`
	RequireWeight100 bool      `json:"require_weight_100"`
	ProgressMethod   string    `json:"progress_method"`
	MaxCascadeDepth  int       `json:"max_cascade_depth"`
	CreatedAt        string    `json:"created_at"`
	UpdatedAt        string    `json:"updated_at"`
}

func toCycleResponse(c *ReviewCycle) CycleResponse {
	r := CycleResponse{
		ID:               c.ID,
		TenantID:         c.TenantID,
		Name:             c.Name,
		StartsOn:         c.StartsOn.Format("2006-01-02"),
		EndsOn:           c.EndsOn.Format("2006-01-02"),
		Status:           c.Status,
		RequireWeight100: c.RequireWeight100,
		ProgressMethod:   c.ProgressMethod,
		MaxCascadeDepth:  c.MaxCascadeDepth,
		CreatedAt:        c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:        c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if c.GoalDueOn != nil {
		s := c.GoalDueOn.Format("2006-01-02")
		r.GoalDueOn = &s
	}
	if c.ReviewDueOn != nil {
		s := c.ReviewDueOn.Format("2006-01-02")
		r.ReviewDueOn = &s
	}
	return r
}

// CreateCycle handles POST /goal-cycles.
func (h *Handler) CreateCycle(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createCycleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if !validDate(req.StartsOn) || !validDate(req.EndsOn) {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "starts_on/ends_on must be YYYY-MM-DD")
		return
	}
	if req.GoalDueOn != nil && *req.GoalDueOn != "" && !validDate(*req.GoalDueOn) {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "goal_due_on must be YYYY-MM-DD")
		return
	}
	if req.ReviewDueOn != nil && *req.ReviewDueOn != "" && !validDate(*req.ReviewDueOn) {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "review_due_on must be YYYY-MM-DD")
		return
	}

	cycle, err := h.svc.CreateCycle(c.Request.Context(), CreateCycleInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		Name:             req.Name,
		StartsOn:         req.StartsOn,
		EndsOn:           req.EndsOn,
		GoalDueOn:        req.GoalDueOn,
		ReviewDueOn:      req.ReviewDueOn,
		RequireWeight100: req.RequireWeight100,
		ProgressMethod:   req.ProgressMethod,
		MaxCascadeDepth:  req.MaxCascadeDepth,
		IP:               clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toCycleResponse(cycle))
}

// ListCycles handles GET /goal-cycles.
func (h *Handler) ListCycles(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	cycles, err := h.svc.ListCycles(c.Request.Context(), tenantID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]CycleResponse, len(cycles))
	for i := range cycles {
		items[i] = toCycleResponse(&cycles[i])
	}
	c.JSON(http.StatusOK, gin.H{"cycles": items})
}

// GetCycle handles GET /goal-cycles/:cycle_id.
func (h *Handler) GetCycle(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	cycleID, err := uuid.Parse(c.Param("cycle_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid cycle id")
		return
	}
	cycle, err := h.svc.GetCycle(c.Request.Context(), tenantID, cycleID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toCycleResponse(cycle))
}

// UpdateCycleStatus handles PATCH /goal-cycles/:cycle_id/status.
func (h *Handler) UpdateCycleStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	cycleID, err := uuid.Parse(c.Param("cycle_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid cycle id")
		return
	}
	var req updateCycleStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	cycle, err := h.svc.UpdateCycleStatus(c.Request.Context(), UpdateCycleStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		CycleID:  cycleID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toCycleResponse(cycle))
}

// ---------------------------------------------------------------------------
// Goal shapes / handlers
// ---------------------------------------------------------------------------

type createGoalRequest struct {
	CycleID      string   `json:"cycle_id"       validate:"required"`
	EmployeeID   string   `json:"employee_id"    validate:"required"`
	ParentGoalID *string  `json:"parent_goal_id"`
	Method       string   `json:"method"         validate:"required,oneof=mbo okr"`
	Title        string   `json:"title"          validate:"required,max=300"`
	Description  string   `json:"description"    validate:"omitempty,max=4000"`
	Weight       *float64 `json:"weight"         validate:"omitempty,min=0,max=100"`
	SelfRating   *string  `json:"self_rating"    validate:"omitempty,max=100"`
}

// updateGoalRequest is the JSON body for PUT /goals/:goal_id.
//
// parent_goal_id semantics:
//   - key absent from JSON  → SetParentGoalID=false, existing link unchanged
//   - "parent_goal_id":null → SetParentGoalID=true, ParentGoalID=nil (unlink)
//   - "parent_goal_id":"<uuid>" → SetParentGoalID=true, ParentGoalID set
//
// We use a raw json.RawMessage to distinguish key-absent from null.
type updateGoalRequest struct {
	Title           string          `json:"title"          validate:"required,max=300"`
	Description     string          `json:"description"    validate:"omitempty,max=4000"`
	Weight          *float64        `json:"weight"         validate:"omitempty,min=0,max=100"`
	SelfRating      *string         `json:"self_rating"    validate:"omitempty,max=100"`
	ParentGoalIDRaw json.RawMessage `json:"parent_goal_id"` // absent=nil slice, null=[]byte("null"), value=[]byte(`"<uuid>"`)
}

type submitGoalRequest struct {
	DepartmentID *string `json:"department_id"`
}

type transitionGoalRequest struct {
	Status string `json:"status" validate:"required,oneof=approved draft in_progress achieved closed"`
}

type updateGoalProgressRequest struct {
	ProgressPct float64 `json:"progress_pct" validate:"min=0,max=100"`
	Comment     string  `json:"comment"      validate:"omitempty,max=2000"`
}

// GoalResponse is the JSON representation of a goal.
type GoalResponse struct { //nolint:revive // type name intentionally includes package prefix for external API clarity
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	CycleID           uuid.UUID  `json:"cycle_id"`
	EmployeeID        uuid.UUID  `json:"employee_id"`
	ParentGoalID      *uuid.UUID `json:"parent_goal_id,omitempty"`
	Method            string     `json:"method"`
	Title             string     `json:"title"`
	Description       string     `json:"description"`
	Weight            *float64   `json:"weight,omitempty"`
	Status            string     `json:"status"`
	SelfRating        *string    `json:"self_rating,omitempty"`
	ProgressPct       float64    `json:"progress_pct"`
	ApprovalRequestID *uuid.UUID `json:"approval_request_id,omitempty"`
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
}

func toGoalResponse(g *Goal) GoalResponse {
	return GoalResponse{
		ID:                g.ID,
		TenantID:          g.TenantID,
		CycleID:           g.CycleID,
		EmployeeID:        g.EmployeeID,
		ParentGoalID:      g.ParentGoalID,
		Method:            g.Method,
		Title:             g.Title,
		Description:       g.Description,
		Weight:            g.Weight,
		Status:            g.Status,
		SelfRating:        g.SelfRating,
		ProgressPct:       g.ProgressPct,
		ApprovalRequestID: g.ApprovalRequestID,
		CreatedAt:         g.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         g.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// CreateGoal handles POST /goals.
func (h *Handler) CreateGoal(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createGoalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	cycleID, err := uuid.Parse(req.CycleID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid cycle_id")
		return
	}
	employeeID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}
	parentGoalID, ok := parseOptionalUUID(c, req.ParentGoalID, "parent_goal_id")
	if !ok {
		return
	}

	desc := ""
	if req.Description != "" {
		desc = req.Description
	}

	actorEmpID := h.svc.ResolveActorEmployee(c.Request.Context(), tenantID, actorID)

	g, err := h.svc.CreateGoal(c.Request.Context(), CreateGoalInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ActorEmployeeID: actorEmpID,
		CycleID:         cycleID,
		EmployeeID:      employeeID,
		ParentGoalID:    parentGoalID,
		Method:          req.Method,
		Title:           req.Title,
		Description:     desc,
		Weight:          req.Weight,
		SelfRating:      req.SelfRating,
		IP:              clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toGoalResponse(g))
}

// ListGoals handles GET /goal-cycles/:cycle_id/goals.
func (h *Handler) ListGoals(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	cycleID, err := uuid.Parse(c.Param("cycle_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid cycle id")
		return
	}
	var employeeID *uuid.UUID
	if q := c.Query("employee_id"); q != "" {
		id, err := uuid.Parse(q)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
			return
		}
		employeeID = &id
	}
	goals, err := h.svc.ListGoals(c.Request.Context(), tenantID, cycleID, employeeID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]GoalResponse, len(goals))
	for i := range goals {
		items[i] = toGoalResponse(&goals[i])
	}
	c.JSON(http.StatusOK, gin.H{"goals": items})
}

// GetGoal handles GET /goals/:goal_id.
func (h *Handler) GetGoal(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	g, err := h.svc.GetGoal(c.Request.Context(), tenantID, goalID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toGoalResponse(g))
}

// UpdateGoal handles PUT /goals/:goal_id.
//
// parent_goal_id field semantics:
//   - key absent  → existing parent link unchanged (SetParentGoalID=false)
//   - null        → explicit unlink (SetParentGoalID=true, ParentGoalID=nil)
//   - "<uuid>"    → set new parent (SetParentGoalID=true, ParentGoalID=&id)
func (h *Handler) UpdateGoal(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	var req updateGoalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	// Resolve the 3-state parent_goal_id.
	var parentGoalID *uuid.UUID
	setParent := false
	if len(req.ParentGoalIDRaw) > 0 {
		setParent = true
		if string(req.ParentGoalIDRaw) != "null" {
			var s string
			if err := json.Unmarshal(req.ParentGoalIDRaw, &s); err != nil {
				httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid parent_goal_id")
				return
			}
			id, err := uuid.Parse(s)
			if err != nil {
				httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid parent_goal_id")
				return
			}
			parentGoalID = &id
		}
	}

	actorEmpID := h.svc.ResolveActorEmployee(c.Request.Context(), tenantID, actorID)

	g, err := h.svc.UpdateGoal(c.Request.Context(), UpdateGoalInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ActorEmployeeID: actorEmpID,
		GoalID:          goalID,
		Title:           req.Title,
		Description:     req.Description,
		Weight:          req.Weight,
		SelfRating:      req.SelfRating,
		ParentGoalID:    parentGoalID,
		SetParentGoalID: setParent,
		IP:              clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toGoalResponse(g))
}

// SubmitGoal handles POST /goals/:goal_id/submit.
func (h *Handler) SubmitGoal(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	var req submitGoalRequest
	// Body is optional; ignore bind errors for an empty body.
	_ = c.ShouldBindJSON(&req)
	departmentID, ok := parseOptionalUUID(c, req.DepartmentID, "department_id")
	if !ok {
		return
	}
	actorEmpID := h.svc.ResolveActorEmployee(c.Request.Context(), tenantID, actorID)
	g, err := h.svc.SubmitGoal(c.Request.Context(), SubmitGoalInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ActorEmployeeID: actorEmpID,
		GoalID:          goalID,
		DepartmentID:    departmentID,
		IP:              clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toGoalResponse(g))
}

// TransitionGoal handles PATCH /goals/:goal_id/status.
func (h *Handler) TransitionGoal(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	var req transitionGoalRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	g, err := h.svc.TransitionGoal(c.Request.Context(), TransitionGoalInput{
		TenantID: tenantID,
		ActorID:  actorID,
		GoalID:   goalID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toGoalResponse(g))
}

// UpdateGoalProgress handles POST /goals/:goal_id/progress.
func (h *Handler) UpdateGoalProgress(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	var req updateGoalProgressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	actorEmpID := h.svc.ResolveActorEmployee(c.Request.Context(), tenantID, actorID)
	g, err := h.svc.UpdateGoalProgress(c.Request.Context(), UpdateGoalProgressInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ActorEmployeeID: actorEmpID,
		GoalID:          goalID,
		ProgressPct:     req.ProgressPct,
		Comment:         req.Comment,
		IP:              clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toGoalResponse(g))
}

// GetCascadeTree handles GET /goals/:goal_id/cascade.
func (h *Handler) GetCascadeTree(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	node, err := h.svc.GetCascadeTree(c.Request.Context(), tenantID, goalID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toCascadeResponse(node))
}

// CascadeNodeResponse is the JSON representation of a cascade tree node.
type CascadeNodeResponse struct {
	Goal     GoalResponse           `json:"goal"`
	Children []*CascadeNodeResponse `json:"children"`
}

func toCascadeResponse(n *CascadeNode) *CascadeNodeResponse {
	if n == nil {
		return nil
	}
	r := &CascadeNodeResponse{Goal: toGoalResponse(&n.Goal)}
	r.Children = make([]*CascadeNodeResponse, 0, len(n.Children))
	for _, child := range n.Children {
		r.Children = append(r.Children, toCascadeResponse(child))
	}
	return r
}

// ---------------------------------------------------------------------------
// Key result shapes / handlers
// ---------------------------------------------------------------------------

type addKeyResultRequest struct {
	Title        string  `json:"title"         validate:"required,max=300"`
	MetricUnit   string  `json:"metric_unit"   validate:"omitempty,max=100"`
	StartValue   float64 `json:"start_value"`
	TargetValue  float64 `json:"target_value"`
	CurrentValue float64 `json:"current_value"`
}

type updateKeyResultProgressRequest struct {
	CurrentValue float64 `json:"current_value"`
	Comment      string  `json:"comment" validate:"omitempty,max=2000"`
}

// KeyResultResponse is the JSON representation of a KeyResult.
type KeyResultResponse struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	GoalID       uuid.UUID `json:"goal_id"`
	Title        string    `json:"title"`
	MetricUnit   string    `json:"metric_unit"`
	StartValue   float64   `json:"start_value"`
	TargetValue  float64   `json:"target_value"`
	CurrentValue float64   `json:"current_value"`
	ProgressPct  float64   `json:"progress_pct"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
}

func toKeyResultResponse(k *KeyResult) KeyResultResponse {
	return KeyResultResponse{
		ID:           k.ID,
		TenantID:     k.TenantID,
		GoalID:       k.GoalID,
		Title:        k.Title,
		MetricUnit:   k.MetricUnit,
		StartValue:   k.StartValue,
		TargetValue:  k.TargetValue,
		CurrentValue: k.CurrentValue,
		ProgressPct:  k.ProgressPct,
		CreatedAt:    k.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    k.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// AddKeyResult handles POST /goals/:goal_id/key-results.
func (h *Handler) AddKeyResult(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	var req addKeyResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	actorEmpID := h.svc.ResolveActorEmployee(c.Request.Context(), tenantID, actorID)
	kr, err := h.svc.AddKeyResult(c.Request.Context(), AddKeyResultInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ActorEmployeeID: actorEmpID,
		GoalID:          goalID,
		Title:           req.Title,
		MetricUnit:      req.MetricUnit,
		StartValue:      req.StartValue,
		TargetValue:     req.TargetValue,
		CurrentValue:    req.CurrentValue,
		IP:              clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toKeyResultResponse(kr))
}

// ListKeyResults handles GET /goals/:goal_id/key-results.
func (h *Handler) ListKeyResults(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	krs, err := h.svc.ListKeyResults(c.Request.Context(), tenantID, goalID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]KeyResultResponse, len(krs))
	for i := range krs {
		items[i] = toKeyResultResponse(&krs[i])
	}
	c.JSON(http.StatusOK, gin.H{"key_results": items})
}

// UpdateKeyResultProgress handles PATCH /key-results/:kr_id/progress.
func (h *Handler) UpdateKeyResultProgress(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	krID, err := uuid.Parse(c.Param("kr_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid key result id")
		return
	}
	var req updateKeyResultProgressRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	actorEmpID := h.svc.ResolveActorEmployee(c.Request.Context(), tenantID, actorID)
	kr, err := h.svc.UpdateKeyResultProgress(c.Request.Context(), UpdateKeyResultProgressInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ActorEmployeeID: actorEmpID,
		KeyResultID:     krID,
		CurrentValue:    req.CurrentValue,
		Comment:         req.Comment,
		IP:              clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toKeyResultResponse(kr))
}

// ---------------------------------------------------------------------------
// Progress log handlers
// ---------------------------------------------------------------------------

// ProgressLogResponse is the JSON representation of a progress log entry.
type ProgressLogResponse struct {
	ID              uuid.UUID  `json:"id"`
	TenantID        uuid.UUID  `json:"tenant_id"`
	GoalID          uuid.UUID  `json:"goal_id"`
	KeyResultID     *uuid.UUID `json:"key_result_id,omitempty"`
	ProgressPct     float64    `json:"progress_pct"`
	Comment         string     `json:"comment"`
	UpdatedByUserID *uuid.UUID `json:"updated_by_user_id,omitempty"`
	CreatedAt       string     `json:"created_at"`
}

func toProgressLogResponse(p *ProgressLog) ProgressLogResponse {
	return ProgressLogResponse{
		ID:              p.ID,
		TenantID:        p.TenantID,
		GoalID:          p.GoalID,
		KeyResultID:     p.KeyResultID,
		ProgressPct:     p.ProgressPct,
		Comment:         p.Comment,
		UpdatedByUserID: p.UpdatedByUserID,
		CreatedAt:       p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ListProgressLogs handles GET /goals/:goal_id/progress.
func (h *Handler) ListProgressLogs(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	goalID, err := uuid.Parse(c.Param("goal_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid goal id")
		return
	}
	logs, err := h.svc.ListProgressLogs(c.Request.Context(), tenantID, goalID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]ProgressLogResponse, len(logs))
	for i := range logs {
		items[i] = toProgressLogResponse(&logs[i])
	}
	c.JSON(http.StatusOK, gin.H{"progress_logs": items})
}

// ---------------------------------------------------------------------------
// Cross-cycle copy handler
// ---------------------------------------------------------------------------

type copyGoalsRequest struct {
	FromCycleID string `json:"from_cycle_id" validate:"required"`
	ToCycleID   string `json:"to_cycle_id"   validate:"required"`
	EmployeeID  string `json:"employee_id"   validate:"required"`
}

// CopyGoals handles POST /goals/copy.
func (h *Handler) CopyGoals(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	var req copyGoalsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	fromCycleID, err := uuid.Parse(req.FromCycleID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid from_cycle_id")
		return
	}
	toCycleID, err := uuid.Parse(req.ToCycleID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid to_cycle_id")
		return
	}
	employeeID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}
	goals, err := h.svc.CopyGoals(c.Request.Context(), CopyGoalsInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		FromCycleID: fromCycleID,
		ToCycleID:   toCycleID,
		EmployeeID:  employeeID,
		IP:          clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]GoalResponse, len(goals))
	for i := range goals {
		items[i] = toGoalResponse(&goals[i])
	}
	c.JSON(http.StatusCreated, gin.H{"goals": items})
}

// ---------------------------------------------------------------------------
// shared parse helpers
// ---------------------------------------------------------------------------

// validDate reports whether s is a YYYY-MM-DD date.
func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// parseOptionalUUID parses an optional *string UUID, responding 400 and
// returning ok=false when the value is present but invalid.  A nil/empty value
// yields (nil, true).
func parseOptionalUUID(c *gin.Context, s *string, field string) (*uuid.UUID, bool) {
	if s == nil || *s == "" {
		return nil, true
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid "+field)
		return nil, false
	}
	return &id, true
}
