package hiring

import (
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

// Handler exposes HTTP endpoints for the hiring domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts validator.ValidationErrors into a safe message.
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

// parseOptionalDate parses an optional YYYY-MM-DD string pointer.
// Returns (nil, nil) for empty input; (nil, error) for an unparseable string.
func parseOptionalDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil, fmt.Errorf("invalid date %q: must be YYYY-MM-DD", *s)
	}
	return &t, nil
}

// parseOptionalUUID parses an optional UUID string pointer.
func parseOptionalUUID(s *string) (*uuid.UUID, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, fmt.Errorf("invalid uuid %q", *s)
	}
	return &id, nil
}

// respondServiceError maps service sentinel errors to HTTP responses.
func respondServiceError(c *gin.Context, err error, notFoundMsg string) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", notFoundMsg)
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrAlreadyConverted):
		httpx.RespondError(c, http.StatusConflict, "ALREADY_CONVERTED", "applicant already converted")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Conversion request / response shapes
// ---------------------------------------------------------------------------

type convertApplicantRequest struct {
	ApplicantID       string  `json:"applicant_id"       validate:"required,uuid"`
	OfferID           *string `json:"offer_id"`
	EmployeeCode      string  `json:"employee_code"      validate:"required,max=100"`
	LastName          string  `json:"last_name"          validate:"required,max=200"`
	FirstName         string  `json:"first_name"         validate:"required,max=200"`
	Email             *string `json:"email"              validate:"omitempty,email,max=320"`
	EmploymentType    string  `json:"employment_type"    validate:"required,max=50"`
	DepartmentID      *string `json:"department_id"`
	TemplateID        *string `json:"template_id"`
	ExpectedStartDate *string `json:"expected_start_date"`
}

// OnboardingResponse is the JSON representation of a new-hire onboarding header.
type OnboardingResponse struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	EmployeeID        uuid.UUID  `json:"employee_id"`
	ApplicantID       uuid.UUID  `json:"applicant_id"`
	DepartmentID      *uuid.UUID `json:"department_id,omitempty"`
	TemplateID        *uuid.UUID `json:"template_id,omitempty"`
	Status            string     `json:"status"`
	ExpectedStartDate *string    `json:"expected_start_date,omitempty"`
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
}

func toOnboardingResponse(o *NewHireOnboarding) OnboardingResponse {
	r := OnboardingResponse{
		ID:           o.ID,
		TenantID:     o.TenantID,
		EmployeeID:   o.EmployeeID,
		ApplicantID:  o.ApplicantID,
		DepartmentID: o.DepartmentID,
		TemplateID:   o.TemplateID,
		Status:       o.Status,
		CreatedAt:    o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    o.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if o.ExpectedStartDate != nil {
		s := o.ExpectedStartDate.Format("2006-01-02")
		r.ExpectedStartDate = &s
	}
	return r
}

// LinkResponse is the JSON representation of an applicant→employee link.
type LinkResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	ApplicantID uuid.UUID  `json:"applicant_id"`
	OfferID     *uuid.UUID `json:"offer_id,omitempty"`
	EmployeeID  uuid.UUID  `json:"employee_id"`
}

func toLinkResponse(l *Link) LinkResponse {
	return LinkResponse{
		ID:          l.ID,
		TenantID:    l.TenantID,
		ApplicantID: l.ApplicantID,
		OfferID:     l.OfferID,
		EmployeeID:  l.EmployeeID,
	}
}

// ConvertApplicant handles POST /hiring/conversions.
func (h *Handler) ConvertApplicant(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req convertApplicantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	applicantID, err := uuid.Parse(req.ApplicantID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid applicant_id")
		return
	}
	offerID, err := parseOptionalUUID(req.OfferID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid offer_id")
		return
	}
	departmentID, err := parseOptionalUUID(req.DepartmentID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid department_id")
		return
	}
	templateID, err := parseOptionalUUID(req.TemplateID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid template_id")
		return
	}
	expectedStart, err := parseOptionalDate(req.ExpectedStartDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	result, err := h.svc.ConvertApplicant(c.Request.Context(), ConvertApplicantInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		ApplicantID:       applicantID,
		OfferID:           offerID,
		EmployeeCode:      req.EmployeeCode,
		LastName:          req.LastName,
		FirstName:         req.FirstName,
		Email:             req.Email,
		EmploymentType:    req.EmploymentType,
		DepartmentID:      departmentID,
		TemplateID:        templateID,
		ExpectedStartDate: expectedStart,
		IP:                clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "department or template not found")
		return
	}

	resp := gin.H{
		"link":        toLinkResponse(result.Link),
		"employee_id": result.EmployeeID,
		"task_count":  len(result.Tasks),
	}
	if result.Onboarding != nil {
		resp["onboarding"] = toOnboardingResponse(result.Onboarding)
	}
	c.JSON(http.StatusCreated, resp)
}

// ---------------------------------------------------------------------------
// Onboarding header handlers
// ---------------------------------------------------------------------------

// GetOnboarding handles GET /hiring/onboardings/:id.
func (h *Handler) GetOnboarding(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid onboarding id")
		return
	}

	ob, err := h.svc.GetOnboarding(c.Request.Context(), tenantID, id)
	if err != nil {
		respondServiceError(c, err, "onboarding not found")
		return
	}
	c.JSON(http.StatusOK, toOnboardingResponse(ob))
}

// ListOnboardings handles GET /hiring/onboardings.
func (h *Handler) ListOnboardings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	status := c.Query("status")

	obs, err := h.svc.ListOnboardings(c.Request.Context(), tenantID, status)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]OnboardingResponse, len(obs))
	for i := range obs {
		items[i] = toOnboardingResponse(&obs[i])
	}
	c.JSON(http.StatusOK, gin.H{"onboardings": items})
}

type advanceOnboardingRequest struct {
	Status string `json:"status" validate:"required,oneof=preboarding onboarding"`
}

// AdvanceOnboarding handles PATCH /hiring/onboardings/:id/status.
func (h *Handler) AdvanceOnboarding(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid onboarding id")
		return
	}

	var req advanceOnboardingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	ob, err := h.svc.AdvanceOnboarding(c.Request.Context(), AdvanceOnboardingInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "onboarding not found")
		return
	}
	c.JSON(http.StatusOK, toOnboardingResponse(ob))
}

type completeOnboardingRequest struct {
	HiredOn *string `json:"hired_on"`
}

// CompleteOnboarding handles POST /hiring/onboardings/:id/complete.
func (h *Handler) CompleteOnboarding(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid onboarding id")
		return
	}

	var req completeOnboardingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	hiredOn, err := parseOptionalDate(req.HiredOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	ob, err := h.svc.CompleteOnboarding(c.Request.Context(), CompleteOnboardingInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		HiredOn:  hiredOn,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "onboarding not found")
		return
	}
	c.JSON(http.StatusOK, toOnboardingResponse(ob))
}

// ---------------------------------------------------------------------------
// Preboarding request handlers
// ---------------------------------------------------------------------------

type createPreboardingRequest struct {
	NewHireOnboardingID string  `json:"new_hire_onboarding_id" validate:"required,uuid"`
	RequestType         string  `json:"request_type"           validate:"required,oneof=account equipment access other"`
	AssigneeUserID      *string `json:"assignee_user_id"`
	Notes               *string `json:"notes" validate:"omitempty,max=2000"`
}

// PreboardingRequestResponse is the JSON representation of a preboarding request.
type PreboardingRequestResponse struct {
	ID                  uuid.UUID  `json:"id"`
	TenantID            uuid.UUID  `json:"tenant_id"`
	NewHireOnboardingID uuid.UUID  `json:"new_hire_onboarding_id"`
	RequestType         string     `json:"request_type"`
	Status              string     `json:"status"`
	AssigneeUserID      *uuid.UUID `json:"assignee_user_id,omitempty"`
	Notes               *string    `json:"notes,omitempty"`
	CreatedAt           string     `json:"created_at"`
	UpdatedAt           string     `json:"updated_at"`
}

func toPreboardingRequestResponse(r *PreboardingRequest) PreboardingRequestResponse {
	return PreboardingRequestResponse{
		ID:                  r.ID,
		TenantID:            r.TenantID,
		NewHireOnboardingID: r.NewHireOnboardingID,
		RequestType:         r.RequestType,
		Status:              r.Status,
		AssigneeUserID:      r.AssigneeUserID,
		Notes:               r.Notes,
		CreatedAt:           r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:           r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// CreatePreboardingRequest handles POST /hiring/preboarding-requests.
func (h *Handler) CreatePreboardingRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createPreboardingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	onboardingID, err := uuid.Parse(req.NewHireOnboardingID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid new_hire_onboarding_id")
		return
	}
	assigneeID, err := parseOptionalUUID(req.AssigneeUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid assignee_user_id")
		return
	}

	created, err := h.svc.CreatePreboardingRequest(c.Request.Context(), CreatePreboardingRequestInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		NewHireOnboardingID: onboardingID,
		RequestType:         req.RequestType,
		AssigneeUserID:      assigneeID,
		Notes:               req.Notes,
		IP:                  clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "onboarding not found")
		return
	}
	c.JSON(http.StatusCreated, toPreboardingRequestResponse(created))
}

type updatePreboardingStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=in_progress completed cancelled"`
}

// UpdatePreboardingRequestStatus handles PATCH /hiring/preboarding-requests/:id/status.
func (h *Handler) UpdatePreboardingRequestStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	var req updatePreboardingStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	updated, err := h.svc.UpdatePreboardingRequestStatus(c.Request.Context(), UpdatePreboardingRequestStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "preboarding request not found")
		return
	}
	c.JSON(http.StatusOK, toPreboardingRequestResponse(updated))
}

// ListPreboardingRequests handles GET /hiring/onboardings/:id/preboarding-requests.
func (h *Handler) ListPreboardingRequests(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	onboardingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid onboarding id")
		return
	}

	reqs, err := h.svc.ListPreboardingRequests(c.Request.Context(), tenantID, onboardingID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]PreboardingRequestResponse, len(reqs))
	for i := range reqs {
		items[i] = toPreboardingRequestResponse(&reqs[i])
	}
	c.JSON(http.StatusOK, gin.H{"preboarding_requests": items})
}

// ---------------------------------------------------------------------------
// Survey handlers (ATS-023 minimal stub)
// ---------------------------------------------------------------------------

type scheduleSurveyRequest struct {
	NewHireOnboardingID string  `json:"new_hire_onboarding_id" validate:"required,uuid"`
	SurveyType          string  `json:"survey_type"            validate:"required,oneof=onboarding_30d onboarding_90d early_attrition"`
	ScheduledOn         *string `json:"scheduled_on"`
}

// SurveyResponse is the JSON representation of an onboarding survey.
type SurveyResponse struct {
	ID                  uuid.UUID `json:"id"`
	TenantID            uuid.UUID `json:"tenant_id"`
	NewHireOnboardingID uuid.UUID `json:"new_hire_onboarding_id"`
	EmployeeID          uuid.UUID `json:"employee_id"`
	SurveyType          string    `json:"survey_type"`
	ScheduledOn         *string   `json:"scheduled_on,omitempty"`
	Status              string    `json:"status"`
	CreatedAt           string    `json:"created_at"`
	UpdatedAt           string    `json:"updated_at"`
}

func toSurveyResponse(s *Survey) SurveyResponse {
	r := SurveyResponse{
		ID:                  s.ID,
		TenantID:            s.TenantID,
		NewHireOnboardingID: s.NewHireOnboardingID,
		EmployeeID:          s.EmployeeID,
		SurveyType:          s.SurveyType,
		Status:              s.Status,
		CreatedAt:           s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:           s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if s.ScheduledOn != nil {
		v := s.ScheduledOn.Format("2006-01-02")
		r.ScheduledOn = &v
	}
	return r
}

// ScheduleSurvey handles POST /hiring/surveys.
func (h *Handler) ScheduleSurvey(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req scheduleSurveyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	onboardingID, err := uuid.Parse(req.NewHireOnboardingID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid new_hire_onboarding_id")
		return
	}
	scheduledOn, err := parseOptionalDate(req.ScheduledOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	survey, err := h.svc.ScheduleSurvey(c.Request.Context(), ScheduleSurveyInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		NewHireOnboardingID: onboardingID,
		SurveyType:          req.SurveyType,
		ScheduledOn:         scheduledOn,
		IP:                  clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "onboarding not found")
		return
	}
	c.JSON(http.StatusCreated, toSurveyResponse(survey))
}

// ListSurveys handles GET /hiring/onboardings/:id/surveys.
func (h *Handler) ListSurveys(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	onboardingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid onboarding id")
		return
	}

	surveys, err := h.svc.ListSurveys(c.Request.Context(), tenantID, onboardingID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]SurveyResponse, len(surveys))
	for i := range surveys {
		items[i] = toSurveyResponse(&surveys[i])
	}
	c.JSON(http.StatusOK, gin.H{"surveys": items})
}
