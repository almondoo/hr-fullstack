package employee

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

// maxWorkingConditionsBytes is the maximum permitted size of the
// working_conditions JSON field. Requests exceeding this limit receive 400.
const maxWorkingConditionsBytes = 64 * 1024 // 64 KB

// Handler exposes HTTP endpoints for the employee domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ---------------------------------------------------------------------------
// Employee request / response shapes
// ---------------------------------------------------------------------------

// EmployeeResponse is the JSON representation of an employee.
type EmployeeResponse struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	EmployeeCode   string     `json:"employee_code"`
	LastName       string     `json:"last_name"`
	FirstName      string     `json:"first_name"`
	Email          *string    `json:"email,omitempty"`
	DepartmentID   *uuid.UUID `json:"department_id,omitempty"`
	EmploymentType string     `json:"employment_type"`
	Status         string     `json:"status"`
	HiredOn        *string    `json:"hired_on,omitempty"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
}

func toEmployeeResponse(e *Employee) EmployeeResponse {
	r := EmployeeResponse{
		ID:             e.ID,
		TenantID:       e.TenantID,
		EmployeeCode:   e.EmployeeCode,
		LastName:       e.LastName,
		FirstName:      e.FirstName,
		Email:          e.Email,
		DepartmentID:   e.DepartmentID,
		EmploymentType: e.EmploymentType,
		Status:         e.Status,
		CreatedAt:      e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      e.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if e.HiredOn != nil {
		s := e.HiredOn.Format("2006-01-02")
		r.HiredOn = &s
	}
	return r
}

type createEmployeeRequest struct {
	EmployeeCode   string     `json:"employee_code"   validate:"required,max=50"`
	LastName       string     `json:"last_name"       validate:"required,max=100"`
	FirstName      string     `json:"first_name"      validate:"required,max=100"`
	Email          *string    `json:"email"           validate:"omitempty,email"`
	DepartmentID   *uuid.UUID `json:"department_id"`
	EmploymentType string     `json:"employment_type" validate:"required,oneof=full_time part_time contract temporary"`
	Status         string     `json:"status"          validate:"required,oneof=active inactive terminated"`
	HiredOn        *string    `json:"hired_on"        validate:"omitempty"`
}

type updateEmployeeRequest struct {
	EmployeeCode   string     `json:"employee_code"   validate:"required,max=50"`
	LastName       string     `json:"last_name"       validate:"required,max=100"`
	FirstName      string     `json:"first_name"      validate:"required,max=100"`
	Email          *string    `json:"email"           validate:"omitempty,email"`
	DepartmentID   *uuid.UUID `json:"department_id"`
	EmploymentType string     `json:"employment_type" validate:"required,oneof=full_time part_time contract temporary"`
	Status         string     `json:"status"          validate:"required,oneof=active inactive terminated"`
	HiredOn        *string    `json:"hired_on"        validate:"omitempty"`
}

// parseDate parses an optional YYYY-MM-DD string pointer.
//
// [Security: Imp 3] Empty / nil input is treated as "not set" and returns
// (nil, nil). A non-empty string that fails parsing returns a non-nil error
// instead of silently returning nil — callers must return 400 on error.
func parseDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q: must be YYYY-MM-DD", *s)
	}
	return &t, nil
}

// validateWorkingConditions checks that wc is either empty/null or valid JSON
// within the allowed size limit.
//
// [Security: Imp 4] Rules:
//   - nil / zero-length → treated as unset, returns nil error.
//   - The literal "null" (4 bytes) → treated as unset, returns nil error.
//   - Exceeds maxWorkingConditionsBytes → returns error (too large).
//   - Non-empty, not null → must be valid JSON, otherwise returns error.
func validateWorkingConditions(wc json.RawMessage) error {
	if len(wc) == 0 {
		return nil
	}
	if string(wc) == "null" {
		return nil
	}
	if len(wc) > maxWorkingConditionsBytes {
		return fmt.Errorf("working_conditions exceeds maximum size of %d bytes", maxWorkingConditionsBytes)
	}
	if !json.Valid(wc) {
		return fmt.Errorf("working_conditions is not valid JSON")
	}
	return nil
}

// validationMessage converts a validator.ValidationErrors into a safe,
// non-leaking error message.
//
// [Security: Imp 5] The raw err.Error() from go-playground/validator exposes
// struct field names and constraint details (e.g. "Key: 'Struct.Field'
// Error:Field validation for 'Field' failed on the 'required' tag"). This
// helper returns only the field name and failed tag — no Go type internals.
func validationMessage(err error) string {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		// Return only the first failure to avoid enumerating all fields.
		e := ve[0]
		return fmt.Sprintf("validation failed on field '%s' (%s)", e.Field(), e.Tag())
	}
	return "validation failed"
}

// ---------------------------------------------------------------------------
// Employee handlers
// ---------------------------------------------------------------------------

// CreateEmployee handles POST /employees.
func (h *Handler) CreateEmployee(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createEmployeeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	hiredOn, err := parseDate(req.HiredOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	emp, err := h.svc.CreateEmployee(c.Request.Context(), CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeCode:   req.EmployeeCode,
		LastName:       req.LastName,
		FirstName:      req.FirstName,
		Email:          req.Email,
		DepartmentID:   req.DepartmentID,
		EmploymentType: req.EmploymentType,
		Status:         req.Status,
		HiredOn:        hiredOn,
		IP:             clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "department not found in this tenant")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusCreated, toEmployeeResponse(emp))
}

// GetEmployee handles GET /employees/:id.
func (h *Handler) GetEmployee(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	emp, err := h.svc.GetEmployee(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toEmployeeResponse(emp))
}

// ListEmployees handles GET /employees.
func (h *Handler) ListEmployees(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	emps, err := h.svc.ListEmployees(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]EmployeeResponse, len(emps))
	for i := range emps {
		items[i] = toEmployeeResponse(&emps[i])
	}
	c.JSON(http.StatusOK, gin.H{"employees": items})
}

// UpdateEmployee handles PUT /employees/:id.
func (h *Handler) UpdateEmployee(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req updateEmployeeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	hiredOn, err := parseDate(req.HiredOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	emp, err := h.svc.UpdateEmployee(c.Request.Context(), UpdateEmployeeInput{
		TenantID:       tenantID,
		ID:             id,
		ActorID:        actorID,
		EmployeeCode:   req.EmployeeCode,
		LastName:       req.LastName,
		FirstName:      req.FirstName,
		Email:          req.Email,
		DepartmentID:   req.DepartmentID,
		EmploymentType: req.EmploymentType,
		Status:         req.Status,
		HiredOn:        hiredOn,
		IP:             clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toEmployeeResponse(emp))
}

// DeleteEmployee handles DELETE /employees/:id.
func (h *Handler) DeleteEmployee(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	if err := h.svc.DeleteEmployee(c.Request.Context(), tenantID, id, actorID, clientIP(c)); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// ---------------------------------------------------------------------------
// Assignment request / response shapes
// ---------------------------------------------------------------------------

// AssignmentResponse is the JSON representation of an assignment.
type AssignmentResponse struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      uuid.UUID  `json:"tenant_id"`
	EmployeeID    uuid.UUID  `json:"employee_id"`
	DepartmentID  *uuid.UUID `json:"department_id,omitempty"`
	Position      *string    `json:"position,omitempty"`
	Grade         *string    `json:"grade,omitempty"`
	EffectiveFrom string     `json:"effective_from"`
	EffectiveTo   *string    `json:"effective_to,omitempty"`
	Reason        *string    `json:"reason,omitempty"`
	CreatedAt     string     `json:"created_at"`
}

func toAssignmentResponse(a *Assignment) AssignmentResponse {
	r := AssignmentResponse{
		ID:            a.ID,
		TenantID:      a.TenantID,
		EmployeeID:    a.EmployeeID,
		DepartmentID:  a.DepartmentID,
		Position:      a.Position,
		Grade:         a.Grade,
		EffectiveFrom: a.EffectiveFrom.Format("2006-01-02"),
		Reason:        a.Reason,
		CreatedAt:     a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if a.EffectiveTo != nil {
		s := a.EffectiveTo.Format("2006-01-02")
		r.EffectiveTo = &s
	}
	return r
}

type createAssignmentRequest struct {
	DepartmentID  *uuid.UUID `json:"department_id"`
	Position      *string    `json:"position"`
	Grade         *string    `json:"grade"`
	EffectiveFrom string     `json:"effective_from" validate:"required"`
	EffectiveTo   *string    `json:"effective_to"`
	Reason        *string    `json:"reason"`
}

// ---------------------------------------------------------------------------
// Assignment handlers
// ---------------------------------------------------------------------------

// CreateAssignment handles POST /employees/:id/assignments.
func (h *Handler) CreateAssignment(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req createAssignmentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	effectiveFrom, err := time.Parse("2006-01-02", req.EffectiveFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_from must be YYYY-MM-DD")
		return
	}

	effectiveTo, err := parseDate(req.EffectiveTo)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	asgn, err := h.svc.CreateAssignment(c.Request.Context(), CreateAssignmentInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		DepartmentID:  req.DepartmentID,
		Position:      req.Position,
		Grade:         req.Grade,
		EffectiveFrom: effectiveFrom,
		EffectiveTo:   effectiveTo,
		Reason:        req.Reason,
		IP:            clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee or department not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusCreated, toAssignmentResponse(asgn))
}

// ListAssignments handles GET /employees/:id/assignments.
func (h *Handler) ListAssignments(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	asgns, err := h.svc.ListAssignments(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]AssignmentResponse, len(asgns))
	for i := range asgns {
		items[i] = toAssignmentResponse(&asgns[i])
	}
	c.JSON(http.StatusOK, gin.H{"assignments": items})
}

// ---------------------------------------------------------------------------
// Contract request / response shapes
// ---------------------------------------------------------------------------

// ContractResponse is the JSON representation of an employment contract.
type ContractResponse struct {
	ID                uuid.UUID       `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	EmployeeID        uuid.UUID       `json:"employee_id"`
	ContractType      string          `json:"contract_type"`
	StartDate         string          `json:"start_date"`
	EndDate           *string         `json:"end_date,omitempty"`
	WorkingConditions json.RawMessage `json:"working_conditions"`
	Status            string          `json:"status"`
	SignedAt          *string         `json:"signed_at,omitempty"`
	DocumentRef       *string         `json:"document_ref,omitempty"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
}

func toContractResponse(c *Contract) ContractResponse {
	wc := json.RawMessage(c.WorkingConditions)
	if len(wc) == 0 {
		wc = json.RawMessage(`{}`)
	}
	r := ContractResponse{
		ID:                c.ID,
		TenantID:          c.TenantID,
		EmployeeID:        c.EmployeeID,
		ContractType:      c.ContractType,
		StartDate:         c.StartDate.Format("2006-01-02"),
		WorkingConditions: wc,
		Status:            c.Status,
		DocumentRef:       c.DocumentRef,
		CreatedAt:         c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if c.EndDate != nil {
		s := c.EndDate.Format("2006-01-02")
		r.EndDate = &s
	}
	if c.SignedAt != nil {
		s := c.SignedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.SignedAt = &s
	}
	return r
}

type createContractRequest struct {
	ContractType      string          `json:"contract_type"       validate:"required,max=100"`
	StartDate         string          `json:"start_date"          validate:"required"`
	EndDate           *string         `json:"end_date"`
	WorkingConditions json.RawMessage `json:"working_conditions"`
	DocumentRef       *string         `json:"document_ref"`
}

type updateContractStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=draft active expired terminated"`
}

// ---------------------------------------------------------------------------
// Contract handlers
// ---------------------------------------------------------------------------

// CreateContract handles POST /employees/:id/contracts.
func (h *Handler) CreateContract(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req createContractRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	startDate, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "start_date must be YYYY-MM-DD")
		return
	}

	endDate, err := parseDate(req.EndDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	// [Security: Imp 4] Validate working_conditions JSON size and structure.
	if err := validateWorkingConditions(req.WorkingConditions); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	wc := []byte(req.WorkingConditions)
	// Treat null literal and empty as unset — use default empty object.
	if len(wc) == 0 || string(wc) == "null" {
		wc = []byte(`{}`)
	}

	ctr, err := h.svc.CreateContract(c.Request.Context(), CreateContractInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		EmployeeID:        empID,
		ContractType:      req.ContractType,
		StartDate:         startDate,
		EndDate:           endDate,
		WorkingConditions: wc,
		DocumentRef:       req.DocumentRef,
		IP:                clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusCreated, toContractResponse(ctr))
}

// GetContract handles GET /contracts/:id.
func (h *Handler) GetContract(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid contract id")
		return
	}

	ctr, err := h.svc.GetContract(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrContractNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "contract not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toContractResponse(ctr))
}

// ListContracts handles GET /employees/:id/contracts.
func (h *Handler) ListContracts(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	ctrs, err := h.svc.ListContracts(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]ContractResponse, len(ctrs))
	for i := range ctrs {
		items[i] = toContractResponse(&ctrs[i])
	}
	c.JSON(http.StatusOK, gin.H{"contracts": items})
}

// UpdateContractStatus handles PATCH /contracts/:id/status.
func (h *Handler) UpdateContractStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid contract id")
		return
	}

	var req updateContractStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	ctr, err := h.svc.UpdateContractStatus(c.Request.Context(), UpdateContractStatusInput{
		TenantID: tenantID,
		ID:       id,
		ActorID:  actorID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrContractNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "contract not found")
			return
		}
		if errors.Is(err, ErrInvalidTransition) {
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toContractResponse(ctr))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}
