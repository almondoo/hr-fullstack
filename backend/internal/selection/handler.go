package selection

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

// Handler exposes HTTP endpoints for the selection-pipeline domain.
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

// parseDate parses an optional YYYY-MM-DD string pointer.
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

// clientIP extracts the client IP from the gin context.
func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// respondServiceError maps domain sentinel errors to HTTP responses.
func respondServiceError(c *gin.Context, err error, notFoundMsg string) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", notFoundMsg)
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "stage transition not allowed")
	case errors.Is(err, ErrReasonRequired):
		httpx.RespondError(c, http.StatusBadRequest, "REASON_REQUIRED", "a reason is required for this transition")
	case errors.Is(err, ErrAlreadyExists):
		httpx.RespondError(c, http.StatusConflict, "ALREADY_EXISTS", "record already exists")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	default:
		httpx.RespondInternalError(c)
	}
}

func tsUTC(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ---------------------------------------------------------------------------
// Stage template shapes / handlers
// ---------------------------------------------------------------------------

type createStageTemplateRequest struct {
	Name       string          `json:"name"        validate:"required,max=200"`
	StagesJSON json.RawMessage `json:"stages_json"`
}

// StageTemplateResponse is the JSON representation of a stage template.
type StageTemplateResponse struct {
	ID         uuid.UUID       `json:"id"`
	TenantID   uuid.UUID       `json:"tenant_id"`
	Name       string          `json:"name"`
	StagesJSON json.RawMessage `json:"stages_json"`
	Active     bool            `json:"active"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
}

func toStageTemplateResponse(t *StageTemplate) StageTemplateResponse {
	stages := json.RawMessage(t.StagesJSON)
	if len(stages) == 0 {
		stages = json.RawMessage(`[]`)
	}
	return StageTemplateResponse{
		ID:         t.ID,
		TenantID:   t.TenantID,
		Name:       t.Name,
		StagesJSON: stages,
		Active:     t.Active,
		CreatedAt:  tsUTC(t.CreatedAt),
		UpdatedAt:  tsUTC(t.UpdatedAt),
	}
}

// CreateStageTemplate handles POST /selection/stage-templates.
func (h *Handler) CreateStageTemplate(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createStageTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.StagesJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "stages_json: "+err.Error())
		return
	}

	stages := []byte(req.StagesJSON)
	if len(stages) == 0 || string(stages) == "null" {
		stages = []byte(`[]`)
	}

	tmpl, err := h.svc.CreateStageTemplate(c.Request.Context(), CreateStageTemplateInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		Name:       req.Name,
		StagesJSON: stages,
		IP:         clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "not found")
		return
	}
	c.JSON(http.StatusCreated, toStageTemplateResponse(tmpl))
}

// ListStageTemplates handles GET /selection/stage-templates.
func (h *Handler) ListStageTemplates(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	tmpls, err := h.svc.ListStageTemplates(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]StageTemplateResponse, len(tmpls))
	for i := range tmpls {
		items[i] = toStageTemplateResponse(&tmpls[i])
	}
	c.JSON(http.StatusOK, gin.H{"stage_templates": items})
}

// ---------------------------------------------------------------------------
// Stage shapes / handlers
// ---------------------------------------------------------------------------

type stageDefRequest struct {
	Name      string `json:"name"       validate:"required,max=200"`
	StageType string `json:"stage_type" validate:"required,oneof=screening interview offer hired rejected"`
	Position  int    `json:"position"   validate:"gte=0"`
}

type initStagesRequest struct {
	TemplateID *string           `json:"template_id"`
	Stages     []stageDefRequest `json:"stages" validate:"omitempty,dive"`
}

// StageResponse is the JSON representation of a stage.
type StageResponse struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	JobPostingID uuid.UUID `json:"job_posting_id"`
	Position     int       `json:"position"`
	Name         string    `json:"name"`
	StageType    string    `json:"stage_type"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
}

func toStageResponse(s *Stage) StageResponse {
	return StageResponse{
		ID:           s.ID,
		TenantID:     s.TenantID,
		JobPostingID: s.JobPostingID,
		Position:     s.Position,
		Name:         s.Name,
		StageType:    s.StageType,
		CreatedAt:    tsUTC(s.CreatedAt),
		UpdatedAt:    tsUTC(s.UpdatedAt),
	}
}

// InitStages handles POST /job-postings/:id/selection/stages.
func (h *Handler) InitStages(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	jpID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	var req initStagesRequest
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

	if templateID == nil && len(req.Stages) == 0 {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "either template_id or stages is required")
		return
	}

	defs := make([]StageDef, len(req.Stages))
	for i, s := range req.Stages {
		defs[i] = StageDef(s)
	}

	stages, err := h.svc.InitStages(c.Request.Context(), InitStagesInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		JobPostingID: jpID,
		TemplateID:   templateID,
		Stages:       defs,
		IP:           clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "job posting or template not found")
		return
	}

	items := make([]StageResponse, len(stages))
	for i := range stages {
		items[i] = toStageResponse(&stages[i])
	}
	c.JSON(http.StatusCreated, gin.H{"stages": items})
}

// ListStages handles GET /job-postings/:id/selection/stages.
func (h *Handler) ListStages(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	jpID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	stages, err := h.svc.ListStages(c.Request.Context(), tenantID, jpID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]StageResponse, len(stages))
	for i := range stages {
		items[i] = toStageResponse(&stages[i])
	}
	c.JSON(http.StatusOK, gin.H{"stages": items})
}

// GetKanban handles GET /job-postings/:id/selection/kanban.
func (h *Handler) GetKanban(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	jpID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job posting id")
		return
	}

	cols, err := h.svc.GetKanban(c.Request.Context(), tenantID, jpID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	type kanbanColumnResponse struct {
		Stage        StageResponse         `json:"stage"`
		Applications []ApplicationResponse `json:"applications"`
	}
	out := make([]kanbanColumnResponse, len(cols))
	for i := range cols {
		apps := make([]ApplicationResponse, len(cols[i].Applications))
		for j := range cols[i].Applications {
			apps[j] = toApplicationResponse(&cols[i].Applications[j])
		}
		out[i] = kanbanColumnResponse{
			Stage:        toStageResponse(&cols[i].Stage),
			Applications: apps,
		}
	}
	c.JSON(http.StatusOK, gin.H{"columns": out})
}

// ---------------------------------------------------------------------------
// Application shapes / handlers
// ---------------------------------------------------------------------------

type createApplicationRequest struct {
	JobPostingID string `json:"job_posting_id" validate:"required"`
	ApplicantID  string `json:"applicant_id"   validate:"required"`
}

// ApplicationResponse is the JSON representation of an application.
type ApplicationResponse struct {
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	JobPostingID       uuid.UUID  `json:"job_posting_id"`
	ApplicantID        uuid.UUID  `json:"applicant_id"`
	CurrentStageID     *uuid.UUID `json:"current_stage_id,omitempty"`
	Status             string     `json:"status"`
	RetentionLabel     string     `json:"retention_label"`
	RetentionExpiresOn *string    `json:"retention_expires_on,omitempty"`
	CreatedAt          string     `json:"created_at"`
	UpdatedAt          string     `json:"updated_at"`
}

func toApplicationResponse(a *Application) ApplicationResponse {
	r := ApplicationResponse{
		ID:             a.ID,
		TenantID:       a.TenantID,
		JobPostingID:   a.JobPostingID,
		ApplicantID:    a.ApplicantID,
		CurrentStageID: a.CurrentStageID,
		Status:         a.Status,
		RetentionLabel: a.RetentionLabel,
		CreatedAt:      tsUTC(a.CreatedAt),
		UpdatedAt:      tsUTC(a.UpdatedAt),
	}
	if a.RetentionExpiresOn != nil {
		s := a.RetentionExpiresOn.Format("2006-01-02")
		r.RetentionExpiresOn = &s
	}
	return r
}

// CreateApplication handles POST /selection/applications.
func (h *Handler) CreateApplication(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createApplicationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	jpID, err := uuid.Parse(req.JobPostingID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid job_posting_id")
		return
	}
	apID, err := uuid.Parse(req.ApplicantID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid applicant_id")
		return
	}

	app, err := h.svc.CreateApplication(c.Request.Context(), CreateApplicationInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		JobPostingID: jpID,
		ApplicantID:  apID,
		IP:           clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "job posting, applicant, or pipeline not found")
		return
	}
	c.JSON(http.StatusCreated, toApplicationResponse(app))
}

// GetApplication handles GET /selection/applications/:app_id.
func (h *Handler) GetApplication(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	appID, err := uuid.Parse(c.Param("app_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid application id")
		return
	}

	app, err := h.svc.GetApplication(c.Request.Context(), tenantID, appID)
	if err != nil {
		respondServiceError(c, err, "application not found")
		return
	}
	c.JSON(http.StatusOK, toApplicationResponse(app))
}

type moveStageRequest struct {
	TargetStageID      string  `json:"target_stage_id" validate:"required"`
	Reason             *string `json:"reason"          validate:"omitempty,max=2000"`
	ReasonRequired     bool    `json:"reason_required"`
	RetentionLabel     string  `json:"retention_label" validate:"omitempty,max=100"`
	RetentionExpiresOn *string `json:"retention_expires_on"`
}

// MoveStage handles POST /selection/applications/:app_id/move.
func (h *Handler) MoveStage(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	appID, err := uuid.Parse(c.Param("app_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid application id")
		return
	}

	var req moveStageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	targetStageID, err := uuid.Parse(req.TargetStageID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid target_stage_id")
		return
	}

	retentionExpiresOn, err := parseDate(req.RetentionExpiresOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	app, err := h.svc.MoveStage(c.Request.Context(), MoveStageInput{
		TenantID:                      tenantID,
		ActorID:                       actorID,
		ApplicationID:                 appID,
		TargetStageID:                 targetStageID,
		Reason:                        req.Reason,
		ReasonRequiredForBackOrReject: req.ReasonRequired,
		RetentionLabel:                req.RetentionLabel,
		RetentionExpiresOn:            retentionExpiresOn,
		IP:                            clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "application or target stage not found")
		return
	}
	c.JSON(http.StatusOK, toApplicationResponse(app))
}

// StageHistoryResponse is the JSON representation of one stage-history row.
type StageHistoryResponse struct {
	ID            uuid.UUID  `json:"id"`
	ApplicationID uuid.UUID  `json:"application_id"`
	FromStageID   *uuid.UUID `json:"from_stage_id,omitempty"`
	ToStageID     uuid.UUID  `json:"to_stage_id"`
	MovedBy       *uuid.UUID `json:"moved_by,omitempty"`
	MovedAt       string     `json:"moved_at"`
	Reason        *string    `json:"reason,omitempty"`
}

func toStageHistoryResponse(hh *StageHistory) StageHistoryResponse {
	return StageHistoryResponse{
		ID:            hh.ID,
		ApplicationID: hh.ApplicationID,
		FromStageID:   hh.FromStageID,
		ToStageID:     hh.ToStageID,
		MovedBy:       hh.MovedBy,
		MovedAt:       tsUTC(hh.MovedAt),
		Reason:        hh.Reason,
	}
}

// ListHistory handles GET /selection/applications/:app_id/history.
func (h *Handler) ListHistory(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	appID, err := uuid.Parse(c.Param("app_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid application id")
		return
	}

	hist, err := h.svc.ListHistory(c.Request.Context(), tenantID, appID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]StageHistoryResponse, len(hist))
	for i := range hist {
		items[i] = toStageHistoryResponse(&hist[i])
	}
	c.JSON(http.StatusOK, gin.H{"history": items})
}

// ---------------------------------------------------------------------------
// Candidate message template shapes / handlers
// ---------------------------------------------------------------------------

type createMessageTemplateRequest struct {
	StageType string `json:"stage_type" validate:"required,oneof=screening interview offer hired rejected"`
	Name      string `json:"name"       validate:"required,max=200"`
	Subject   string `json:"subject"    validate:"omitempty,max=500"`
	Body      string `json:"body"       validate:"omitempty,max=10000"`
}

// MessageTemplateResponse is the JSON representation of a candidate template.
type MessageTemplateResponse struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	StageType string    `json:"stage_type"`
	Name      string    `json:"name"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	Active    bool      `json:"active"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

func toMessageTemplateResponse(t *MessageTemplate) MessageTemplateResponse {
	return MessageTemplateResponse{
		ID:        t.ID,
		TenantID:  t.TenantID,
		StageType: t.StageType,
		Name:      t.Name,
		Subject:   t.Subject,
		Body:      t.Body,
		Active:    t.Active,
		CreatedAt: tsUTC(t.CreatedAt),
		UpdatedAt: tsUTC(t.UpdatedAt),
	}
}

// CreateMessageTemplate handles POST /selection/message-templates.
func (h *Handler) CreateMessageTemplate(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createMessageTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	tmpl, err := h.svc.CreateMessageTemplate(c.Request.Context(), UpsertMessageTemplateInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		StageType: req.StageType,
		Name:      req.Name,
		Subject:   req.Subject,
		Body:      req.Body,
		IP:        clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err, "not found")
		return
	}
	c.JSON(http.StatusCreated, toMessageTemplateResponse(tmpl))
}

// ListMessageTemplates handles GET /selection/message-templates.
func (h *Handler) ListMessageTemplates(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	tmpls, err := h.svc.ListMessageTemplates(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]MessageTemplateResponse, len(tmpls))
	for i := range tmpls {
		items[i] = toMessageTemplateResponse(&tmpls[i])
	}
	c.JSON(http.StatusOK, gin.H{"message_templates": items})
}
