package onboarding

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

// maxJSONBytes is the maximum size for any JSON field in request bodies.
const maxJSONBytes = 64 * 1024 // 64 KB

// Handler exposes HTTP endpoints for the onboarding domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts a validator.ValidationErrors into a safe message.
// This mirrors the pattern in the employee package to avoid exposing struct internals.
func validationMessage(err error) string {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		e := ve[0]
		return fmt.Sprintf("validation failed on field '%s' (%s)", e.Field(), e.Tag())
	}
	return "validation failed"
}

// parseDate parses an optional YYYY-MM-DD string pointer.
// Returns (nil, nil) for empty input; (nil, error) for an unparseable string.
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

// validateJSON checks that raw is either empty or valid JSON within maxJSONBytes.
func validateJSON(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if len(raw) > maxJSONBytes {
		return fmt.Errorf("JSON field exceeds maximum size of %d bytes", maxJSONBytes)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

// clientIP extracts the client IP from the gin context.
func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// ---------------------------------------------------------------------------
// Template request / response shapes
// ---------------------------------------------------------------------------

type createTemplateRequest struct {
	Name      string          `json:"name"       validate:"required,max=200"`
	Kind      string          `json:"kind"       validate:"required,oneof=onboarding offboarding"`
	ItemsJSON json.RawMessage `json:"items_json"`
}

// TemplateResponse is the JSON representation of a checklist template.
type TemplateResponse struct {
	ID        uuid.UUID       `json:"id"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	ItemsJSON json.RawMessage `json:"items_json"`
	Active    bool            `json:"active"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func toTemplateResponse(t *ChecklistTemplate) TemplateResponse {
	items := json.RawMessage(t.ItemsJSON)
	if len(items) == 0 {
		items = json.RawMessage(`[]`)
	}
	return TemplateResponse{
		ID:        t.ID,
		TenantID:  t.TenantID,
		Name:      t.Name,
		Kind:      t.Kind,
		ItemsJSON: items,
		Active:    t.Active,
		CreatedAt: t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ---------------------------------------------------------------------------
// Template handlers
// ---------------------------------------------------------------------------

// CreateTemplate handles POST /onboarding/templates.
func (h *Handler) CreateTemplate(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.ItemsJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "items_json: "+err.Error())
		return
	}

	items := []byte(req.ItemsJSON)
	if len(items) == 0 || string(items) == "null" {
		items = []byte(`[]`)
	}

	tmpl, err := h.svc.CreateTemplate(c.Request.Context(), CreateTemplateInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		Name:      req.Name,
		Kind:      req.Kind,
		ItemsJSON: items,
		IP:        clientIP(c),
	})
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusCreated, toTemplateResponse(tmpl))
}

// ListTemplates handles GET /onboarding/templates.
func (h *Handler) ListTemplates(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	kind := c.Query("kind")

	tmpls, err := h.svc.ListTemplates(c.Request.Context(), tenantID, kind)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]TemplateResponse, len(tmpls))
	for i := range tmpls {
		items[i] = toTemplateResponse(&tmpls[i])
	}
	c.JSON(http.StatusOK, gin.H{"templates": items})
}

// ---------------------------------------------------------------------------
// Task request / response shapes
// ---------------------------------------------------------------------------

type generateTasksRequest struct {
	TemplateID string  `json:"template_id" validate:"required"`
	BaseDate   *string `json:"base_date"`
}

type updateTaskStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=pending in_progress done skipped"`
}

type assignTaskRequest struct {
	AssigneeUserID *uuid.UUID `json:"assignee_user_id"`
}

// TaskResponse is the JSON representation of a task.
type TaskResponse struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	EmployeeID     uuid.UUID  `json:"employee_id"`
	Kind           string     `json:"kind"`
	Title          string     `json:"title"`
	Category       string     `json:"category"`
	Status         string     `json:"status"`
	DueDate        *string    `json:"due_date,omitempty"`
	AssigneeUserID *uuid.UUID `json:"assignee_user_id,omitempty"`
	CompletedAt    *string    `json:"completed_at,omitempty"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
}

func toTaskResponse(t *Task) TaskResponse {
	r := TaskResponse{
		ID:             t.ID,
		TenantID:       t.TenantID,
		EmployeeID:     t.EmployeeID,
		Kind:           t.Kind,
		Title:          t.Title,
		Category:       t.Category,
		Status:         t.Status,
		AssigneeUserID: t.AssigneeUserID,
		CreatedAt:      t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if t.DueDate != nil {
		s := t.DueDate.Format("2006-01-02")
		r.DueDate = &s
	}
	if t.CompletedAt != nil {
		s := t.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.CompletedAt = &s
	}
	return r
}

// ---------------------------------------------------------------------------
// Task handlers
// ---------------------------------------------------------------------------

// GenerateTasks handles POST /employees/:id/onboarding/tasks/generate.
func (h *Handler) GenerateTasks(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req generateTasksRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	templateID, err := uuid.Parse(req.TemplateID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid template_id")
		return
	}

	baseDate := time.Now()
	if req.BaseDate != nil && *req.BaseDate != "" {
		bd, err := time.Parse("2006-01-02", *req.BaseDate)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "base_date must be YYYY-MM-DD")
			return
		}
		baseDate = bd
	}

	tasks, err := h.svc.GenerateTasks(c.Request.Context(), GenerateTasksInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TemplateID: templateID,
		BaseDate:   baseDate,
		IP:         clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee or template not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	items := make([]TaskResponse, len(tasks))
	for i := range tasks {
		items[i] = toTaskResponse(&tasks[i])
	}
	c.JSON(http.StatusCreated, gin.H{"tasks": items})
}

// ListTasks handles GET /employees/:id/onboarding/tasks.
func (h *Handler) ListTasks(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	kind := c.Query("kind")

	tasks, err := h.svc.ListTasks(c.Request.Context(), tenantID, empID, kind)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]TaskResponse, len(tasks))
	for i := range tasks {
		items[i] = toTaskResponse(&tasks[i])
	}
	c.JSON(http.StatusOK, gin.H{"tasks": items})
}

// UpdateTaskStatus handles PATCH /onboarding/tasks/:task_id/status.
func (h *Handler) UpdateTaskStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	taskID, err := uuid.Parse(c.Param("task_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid task id")
		return
	}

	var req updateTaskStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	task, err := h.svc.UpdateTaskStatus(c.Request.Context(), UpdateTaskStatusInput{
		TenantID: tenantID,
		ID:       taskID,
		ActorID:  actorID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "task not found")
			return
		}
		if errors.Is(err, ErrInvalidTransition) {
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "task status transition not allowed")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toTaskResponse(task))
}

// AssignTask handles PATCH /onboarding/tasks/:task_id/assign.
func (h *Handler) AssignTask(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	taskID, err := uuid.Parse(c.Param("task_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid task id")
		return
	}

	var req assignTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}

	task, err := h.svc.AssignTask(c.Request.Context(), AssignTaskInput{
		TenantID:       tenantID,
		ID:             taskID,
		ActorID:        actorID,
		AssigneeUserID: req.AssigneeUserID,
		IP:             clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "task or assignee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toTaskResponse(task))
}

// ---------------------------------------------------------------------------
// Intake Form request / response shapes
// ---------------------------------------------------------------------------

type submitIntakeFormRequest struct {
	EmergencyContact json.RawMessage `json:"emergency_contact"`
	Commute          json.RawMessage `json:"commute"`
	Dependents       json.RawMessage `json:"dependents"`
	// BankAccount is the bank account number (口座番号) in plaintext.
	// It is encrypted before storage and NEVER persisted as plaintext.
	// Maximum 100 characters to limit ciphertext size.
	BankAccount string `json:"bank_account" validate:"omitempty,max=100"`
}

// IntakeFormResponse is the JSON representation of an intake form.
// BankAccountMasked is always "****" for normal reads.
// BankAccount is populated only for callers with intake:read_sensitive.
type IntakeFormResponse struct {
	ID               uuid.UUID       `json:"id"`
	TenantID         uuid.UUID       `json:"tenant_id"`
	EmployeeID       uuid.UUID       `json:"employee_id"`
	EmergencyContact json.RawMessage `json:"emergency_contact"`
	Commute          json.RawMessage `json:"commute"`
	Dependents       json.RawMessage `json:"dependents"`
	// BankAccount is only populated when the caller holds intake:read_sensitive.
	// For all other callers this field is omitted entirely.
	BankAccount *string `json:"bank_account,omitempty"`
	// BankAccountMasked indicates whether the bank account field is populated.
	// Always "****" when no sensitive access; omitted when sensitive access granted.
	BankAccountMasked *string `json:"bank_account_masked,omitempty"`
	Status            string  `json:"status"`
	RetentionPolicy   string  `json:"retention_policy"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

func toIntakeFormResponse(f *IntakeForm, bankAccountPlaintext []byte) IntakeFormResponse {
	ec := json.RawMessage(f.EmergencyContactJSON)
	if len(ec) == 0 {
		ec = json.RawMessage(`{}`)
	}
	commute := json.RawMessage(f.CommuteJSON)
	if len(commute) == 0 {
		commute = json.RawMessage(`{}`)
	}
	deps := json.RawMessage(f.DependentsJSON)
	if len(deps) == 0 {
		deps = json.RawMessage(`[]`)
	}

	r := IntakeFormResponse{
		ID:               f.ID,
		TenantID:         f.TenantID,
		EmployeeID:       f.EmployeeID,
		EmergencyContact: ec,
		Commute:          commute,
		Dependents:       deps,
		Status:           f.Status,
		RetentionPolicy:  f.RetentionPolicy,
		CreatedAt:        f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:        f.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}

	if len(bankAccountPlaintext) > 0 {
		s := string(bankAccountPlaintext)
		r.BankAccount = &s
	} else {
		masked := "****"
		r.BankAccountMasked = &masked
	}
	return r
}

// ---------------------------------------------------------------------------
// Intake Form handlers
// ---------------------------------------------------------------------------

// SubmitIntakeForm handles POST /employees/:id/intake.
func (h *Handler) SubmitIntakeForm(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req submitIntakeFormRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	// Validate JSON fields individually.
	for name, raw := range map[string]json.RawMessage{
		"emergency_contact": req.EmergencyContact,
		"commute":           req.Commute,
		"dependents":        req.Dependents,
	} {
		if err := validateJSON(raw); err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", name+": "+err.Error())
			return
		}
	}

	ec := []byte(req.EmergencyContact)
	if len(ec) == 0 || string(ec) == "null" {
		ec = []byte(`{}`)
	}
	commute := []byte(req.Commute)
	if len(commute) == 0 || string(commute) == "null" {
		commute = []byte(`{}`)
	}
	deps := []byte(req.Dependents)
	if len(deps) == 0 || string(deps) == "null" {
		deps = []byte(`[]`)
	}

	form, err := h.svc.SubmitIntakeForm(c.Request.Context(), SubmitIntakeFormInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		EmployeeID:           empID,
		EmergencyContactJSON: ec,
		CommuteJSON:          commute,
		DependentsJSON:       deps,
		BankAccountPlaintext: []byte(req.BankAccount),
		IP:                   clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusCreated, toIntakeFormResponse(form, nil))
}

// GetIntakeForm handles GET /employees/:id/intake.
// The handler checks for intake:read_sensitive permission by reading the
// query parameter "sensitive=true" AND validating RBAC at the route layer.
// The sensitive flag only takes effect when the route-level RequirePermission
// for intake:read_sensitive passed (i.e. the middleware already verified it).
// See routes.go for the dual-route setup.
func (h *Handler) GetIntakeForm(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	// readSensitive is set by the route that has RequirePermission(intake:read_sensitive).
	// See routes.go: the sensitive route sets the gin context key before calling this handler.
	readSensitive := false
	if v, ok := c.Get("intake_read_sensitive"); ok {
		if b, ok := v.(bool); ok {
			readSensitive = b
		}
	}

	form, bankPlaintext, err := h.svc.GetIntakeForm(c.Request.Context(), GetIntakeFormInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		ReadSensitive: readSensitive,
		IP:            clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "intake form not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toIntakeFormResponse(form, bankPlaintext))
}

// GetIntakeFormSensitive handles GET /employees/:id/intake/sensitive.
// This route requires intake:read_sensitive permission (enforced in routes.go).
func (h *Handler) GetIntakeFormSensitive(c *gin.Context) {
	c.Set("intake_read_sensitive", true)
	h.GetIntakeForm(c)
}

// VerifyIntakeForm handles POST /employees/:id/intake/verify.
func (h *Handler) VerifyIntakeForm(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	form, err := h.svc.VerifyIntakeForm(c.Request.Context(), VerifyIntakeFormInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		IP:         clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "intake form not found or already verified")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toIntakeFormResponse(form, nil))
}

// ---------------------------------------------------------------------------
// Offboarding request / response shapes
// ---------------------------------------------------------------------------

type initiateOffboardingRequest struct {
	TemplateID      *string `json:"template_id"`
	LastWorkingDate *string `json:"last_working_date"`
	RetentionLabel  string  `json:"retention_label"  validate:"omitempty,oneof=7years 5years indefinite"`
	ExpiresOn       *string `json:"expires_on"`
	Notes           *string `json:"notes"`
}

// OffboardingPolicyResponse is the JSON representation of an offboarding policy.
type OffboardingPolicyResponse struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	EmployeeID     uuid.UUID  `json:"employee_id"`
	RetentionLabel string     `json:"retention_label"`
	ExpiresOn      *string    `json:"expires_on,omitempty"`
	RecordedBy     *uuid.UUID `json:"recorded_by,omitempty"`
	Notes          *string    `json:"notes,omitempty"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
}

func toOffboardingPolicyResponse(p *OffboardingPolicy) OffboardingPolicyResponse {
	r := OffboardingPolicyResponse{
		ID:             p.ID,
		TenantID:       p.TenantID,
		EmployeeID:     p.EmployeeID,
		RetentionLabel: p.RetentionLabel,
		RecordedBy:     p.RecordedBy,
		Notes:          p.Notes,
		CreatedAt:      p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if p.ExpiresOn != nil {
		s := p.ExpiresOn.Format("2006-01-02")
		r.ExpiresOn = &s
	}
	return r
}

// ---------------------------------------------------------------------------
// Offboarding handlers
// ---------------------------------------------------------------------------

// InitiateOffboarding handles POST /employees/:id/offboarding.
func (h *Handler) InitiateOffboarding(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req initiateOffboardingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	var templateID *uuid.UUID
	if req.TemplateID != nil && *req.TemplateID != "" {
		id, err := uuid.Parse(*req.TemplateID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid template_id")
			return
		}
		templateID = &id
	}

	lastWorkingDate, err := parseDate(req.LastWorkingDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	expiresOn, err := parseDate(req.ExpiresOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	tasks, policy, err := h.svc.InitiateOffboarding(c.Request.Context(), InitiateOffboardingInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		TemplateID:      templateID,
		LastWorkingDate: lastWorkingDate,
		RetentionLabel:  req.RetentionLabel,
		ExpiresOn:       expiresOn,
		Notes:           req.Notes,
		IP:              clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee or template not found")
			return
		}
		if errors.Is(err, ErrInvalidTransition) {
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "employee status transition not allowed")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	taskItems := make([]TaskResponse, len(tasks))
	for i := range tasks {
		taskItems[i] = toTaskResponse(&tasks[i])
	}
	c.JSON(http.StatusCreated, gin.H{
		"tasks":  taskItems,
		"policy": toOffboardingPolicyResponse(policy),
	})
}

// CompleteOffboarding handles POST /employees/:id/offboarding/complete.
func (h *Handler) CompleteOffboarding(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	if err := h.svc.CompleteOffboarding(c.Request.Context(), CompleteOffboardingInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		IP:         clientIP(c),
	}); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		if errors.Is(err, ErrInvalidTransition) {
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", err.Error())
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "offboarding completed"})
}

// GetOffboardingPolicy handles GET /employees/:id/offboarding/policy.
func (h *Handler) GetOffboardingPolicy(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	policy, err := h.svc.GetOffboardingPolicy(c.Request.Context(), tenantID, empID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "offboarding policy not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toOffboardingPolicyResponse(policy))
}
