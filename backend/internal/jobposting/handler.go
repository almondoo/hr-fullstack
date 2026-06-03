package jobposting

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/httpx"
)

var validate = validator.New()

// maxJSONBytes is the maximum size for any JSON field in request bodies.
const maxJSONBytes = 64 * 1024 // 64 KB

// Handler exposes HTTP endpoints for the job posting domain.
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

// mapServiceError converts a service error into an HTTP response.  Returns true
// when an error was handled (response already written).
func mapServiceError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "job posting or referenced resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "job posting status transition not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	case errors.Is(err, ErrValidation):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
	default:
		httpx.RespondInternalError(c)
	}
	return true
}

// ---------------------------------------------------------------------------
// Request / response shapes
// ---------------------------------------------------------------------------

type createJobPostingRequest struct {
	Title               string          `json:"title"                  validate:"required,max=300"`
	EmploymentType      string          `json:"employment_type"        validate:"required,max=100"`
	DepartmentID        string          `json:"department_id"          validate:"required,uuid"`
	RecruiterUserID     *string         `json:"recruiter_user_id"      validate:"omitempty,uuid"`
	HiringManagerUserID *string         `json:"hiring_manager_user_id" validate:"omitempty,uuid"`
	Requirements        json.RawMessage `json:"requirements"`
	SalaryRangeMin      *int64          `json:"salary_range_min"`
	SalaryRangeMax      *int64          `json:"salary_range_max"`
	HiringBudget        *int64          `json:"hiring_budget"`
	RetentionLabel      string          `json:"retention_label"        validate:"omitempty,max=100"`
}

type updateJobPostingRequest struct {
	Title               string          `json:"title"                  validate:"required,max=300"`
	EmploymentType      string          `json:"employment_type"        validate:"required,max=100"`
	DepartmentID        string          `json:"department_id"          validate:"required,uuid"`
	RecruiterUserID     *string         `json:"recruiter_user_id"      validate:"omitempty,uuid"`
	HiringManagerUserID *string         `json:"hiring_manager_user_id" validate:"omitempty,uuid"`
	Requirements        json.RawMessage `json:"requirements"`
	SalaryRangeMin      *int64          `json:"salary_range_min"`
	SalaryRangeMax      *int64          `json:"salary_range_max"`
	HiringBudget        *int64          `json:"hiring_budget"`
	RetentionLabel      string          `json:"retention_label"        validate:"omitempty,max=100"`
}

type updateStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=open on_hold closed"`
}

type assignInterviewerRequest struct {
	UserID string `json:"user_id" validate:"required,uuid"`
}

// JobPostingResponse is the JSON representation of a job posting.
//
// SalaryRangeMin/Max and HiringBudget are only populated for callers holding
// ats:read_budget; the service clears them otherwise (so they serialise as
// null / omitted).
type JobPostingResponse struct { //nolint:revive // type name intentionally includes package prefix for external API clarity
	ID                  uuid.UUID       `json:"id"`
	TenantID            uuid.UUID       `json:"tenant_id"`
	Title               string          `json:"title"`
	Status              string          `json:"status"`
	EmploymentType      string          `json:"employment_type"`
	DepartmentID        uuid.UUID       `json:"department_id"`
	RecruiterUserID     *uuid.UUID      `json:"recruiter_user_id,omitempty"`
	HiringManagerUserID *uuid.UUID      `json:"hiring_manager_user_id,omitempty"`
	Requirements        json.RawMessage `json:"requirements"`
	SalaryRangeMin      *int64          `json:"salary_range_min,omitempty"`
	SalaryRangeMax      *int64          `json:"salary_range_max,omitempty"`
	HiringBudget        *int64          `json:"hiring_budget,omitempty"`
	RetentionLabel      string          `json:"retention_label"`
	PublicPublished     bool            `json:"public_published"`
	PublicSlug          string          `json:"public_slug"`
	CreatedAt           string          `json:"created_at"`
	UpdatedAt           string          `json:"updated_at"`
}

func toJobPostingResponse(jp *JobPosting) JobPostingResponse {
	req := json.RawMessage(jp.RequirementsJSON)
	if len(req) == 0 {
		req = json.RawMessage(`{}`)
	}
	return JobPostingResponse{
		ID:                  jp.ID,
		TenantID:            jp.TenantID,
		Title:               jp.Title,
		Status:              jp.Status,
		EmploymentType:      jp.EmploymentType,
		DepartmentID:        jp.DepartmentID,
		RecruiterUserID:     jp.RecruiterUserID,
		HiringManagerUserID: jp.HiringManagerUserID,
		Requirements:        req,
		SalaryRangeMin:      jp.SalaryRangeMin,
		SalaryRangeMax:      jp.SalaryRangeMax,
		HiringBudget:        jp.HiringBudget,
		RetentionLabel:      jp.RetentionLabel,
		PublicPublished:     jp.PublicPublished,
		PublicSlug:          jp.PublicSlug,
		CreatedAt:           jp.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:           jp.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// InterviewerResponse is the JSON representation of an interviewer assignment.
type InterviewerResponse struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	JobPostingID uuid.UUID `json:"job_posting_id"`
	UserID       uuid.UUID `json:"user_id"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
}

func toInterviewerResponse(iv *Interviewer) InterviewerResponse {
	return InterviewerResponse{
		ID:           iv.ID,
		TenantID:     iv.TenantID,
		JobPostingID: iv.JobPostingID,
		UserID:       iv.UserID,
		CreatedAt:    iv.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    iv.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// parseOptionalUUID parses an optional UUID string pointer into a *uuid.UUID.
func parseOptionalUUID(s *string) (*uuid.UUID, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(*s)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// CreateJobPosting handles POST /job-postings.
func (h *Handler) CreateJobPosting(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createJobPostingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Requirements); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "requirements: "+err.Error())
		return
	}

	departmentID, err := uuid.Parse(req.DepartmentID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid department_id")
		return
	}
	recruiterID, err := parseOptionalUUID(req.RecruiterUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid recruiter_user_id")
		return
	}
	hiringManagerID, err := parseOptionalUUID(req.HiringManagerUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid hiring_manager_user_id")
		return
	}

	requirements := []byte(req.Requirements)
	if len(requirements) == 0 || string(requirements) == "null" {
		requirements = []byte(`{}`)
	}

	jp, err := h.svc.CreateJobPosting(c.Request.Context(), CreateJobPostingInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		Title:               req.Title,
		EmploymentType:      req.EmploymentType,
		DepartmentID:        departmentID,
		RecruiterUserID:     recruiterID,
		HiringManagerUserID: hiringManagerID,
		RequirementsJSON:    requirements,
		SalaryRangeMin:      req.SalaryRangeMin,
		SalaryRangeMax:      req.SalaryRangeMax,
		HiringBudget:        req.HiringBudget,
		RetentionLabel:      req.RetentionLabel,
		IP:                  clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toJobPostingResponse(jp))
}

// GetJobPosting handles GET /job-postings/:id.
func (h *Handler) GetJobPosting(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	// ReadBudget is always requested; the service re-validates ats:read_budget
	// and clears the budget fields when the actor lacks the permission.
	jp, err := h.svc.GetJobPosting(c.Request.Context(), GetJobPostingInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		ID:         id,
		ReadBudget: true,
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toJobPostingResponse(jp))
}

// ListJobPostings handles GET /job-postings.
func (h *Handler) ListJobPostings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	// ReadBudget is always requested; the service re-validates ats:read_budget
	// and clears the budget fields when the actor lacks the permission.
	in := ListJobPostingsInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		Status:     c.Query("status"),
		ReadBudget: true,
	}
	if deptStr := c.Query("department_id"); deptStr != "" {
		deptID, err := uuid.Parse(deptStr)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid department_id")
			return
		}
		in.DepartmentID = &deptID
	}

	postings, err := h.svc.ListJobPostings(c.Request.Context(), in)
	if mapServiceError(c, err) {
		return
	}

	items := make([]JobPostingResponse, len(postings))
	for i := range postings {
		items[i] = toJobPostingResponse(&postings[i])
	}
	c.JSON(http.StatusOK, gin.H{"job_postings": items})
}

// UpdateJobPosting handles PUT /job-postings/:id.
func (h *Handler) UpdateJobPosting(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	var req updateJobPostingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Requirements); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "requirements: "+err.Error())
		return
	}

	departmentID, err := uuid.Parse(req.DepartmentID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid department_id")
		return
	}
	recruiterID, err := parseOptionalUUID(req.RecruiterUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid recruiter_user_id")
		return
	}
	hiringManagerID, err := parseOptionalUUID(req.HiringManagerUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid hiring_manager_user_id")
		return
	}

	requirements := []byte(req.Requirements)
	if len(requirements) == 0 || string(requirements) == "null" {
		requirements = []byte(`{}`)
	}

	jp, err := h.svc.UpdateJobPosting(c.Request.Context(), UpdateJobPostingInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		ID:                  id,
		Title:               req.Title,
		EmploymentType:      req.EmploymentType,
		DepartmentID:        departmentID,
		RecruiterUserID:     recruiterID,
		HiringManagerUserID: hiringManagerID,
		RequirementsJSON:    requirements,
		SalaryRangeMin:      req.SalaryRangeMin,
		SalaryRangeMax:      req.SalaryRangeMax,
		HiringBudget:        req.HiringBudget,
		RetentionLabel:      req.RetentionLabel,
		IP:                  clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toJobPostingResponse(jp))
}

// UpdateStatus handles PATCH /job-postings/:id/status.
func (h *Handler) UpdateStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	var req updateStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	jp, warning, err := h.svc.UpdateStatus(c.Request.Context(), UpdateStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}

	resp := gin.H{"job_posting": toJobPostingResponse(jp)}
	if warning != nil {
		resp["warning"] = gin.H{
			"code":                 "UNDECIDED_APPLICANTS",
			"message":              "job posting closed with undecided applicants; candidate data is retained",
			"undecided_applicants": warning.UndecidedApplicants,
		}
	}
	c.JSON(http.StatusOK, resp)
}

// AssignInterviewer handles POST /job-postings/:id/interviewers.
func (h *Handler) AssignInterviewer(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	postingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	var req assignInterviewerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid user_id")
		return
	}

	iv, err := h.svc.AssignInterviewer(c.Request.Context(), AssignInterviewerInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		JobPostingID: postingID,
		UserID:       userID,
		IP:           clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toInterviewerResponse(iv))
}

// ListInterviewers handles GET /job-postings/:id/interviewers.
func (h *Handler) ListInterviewers(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	postingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	interviewers, err := h.svc.ListInterviewers(c.Request.Context(), tenantID, postingID)
	if mapServiceError(c, err) {
		return
	}

	items := make([]InterviewerResponse, len(interviewers))
	for i := range interviewers {
		items[i] = toInterviewerResponse(&interviewers[i])
	}
	c.JSON(http.StatusOK, gin.H{"interviewers": items})
}

// RemoveInterviewer handles DELETE /job-postings/:id/interviewers/:user_id.
func (h *Handler) RemoveInterviewer(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	postingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}
	userID, err := uuid.Parse(c.Param("user_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid user id")
		return
	}

	if err := h.svc.RemoveInterviewer(c.Request.Context(), RemoveInterviewerInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		JobPostingID: postingID,
		UserID:       userID,
		IP:           clientIP(c),
	}); mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "interviewer removed"})
}
