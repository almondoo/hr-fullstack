package interview

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

// Handler exposes HTTP endpoints for the interview domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts validator errors into a safe message.
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

// respondServiceErr maps service sentinel errors to HTTP responses.
func respondServiceErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	case errors.Is(err, ErrAlreadyExists):
		httpx.RespondError(c, http.StatusConflict, "ALREADY_EXISTS", "resource already exists")
	case errors.Is(err, ErrInvalidInput):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid input")
	default:
		httpx.RespondInternalError(c)
	}
}

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := fmtTime(*t)
	return &s
}

// ---------------------------------------------------------------------------
// Interview shapes
// ---------------------------------------------------------------------------

type createInterviewRequest struct {
	ApplicationID string  `json:"application_id" validate:"required,uuid"`
	Format        string  `json:"format"         validate:"omitempty,oneof=onsite online phone"`
	OnlineURL     *string `json:"online_url"     validate:"omitempty,max=500"`
	Notes         *string `json:"notes"          validate:"omitempty,max=2000"`
}

// interviewResponse is the JSON representation of an interview.
type interviewResponse struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	ApplicationID   uuid.UUID `json:"application_id"`
	Status          string    `json:"status"`
	Format          string    `json:"format"`
	ScheduledAt     *string   `json:"scheduled_at,omitempty"`
	OnlineURL       *string   `json:"online_url,omitempty"`
	ExternalEventID *string   `json:"external_event_id,omitempty"`
	Notes           *string   `json:"notes,omitempty"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
}

func tointerviewResponse(iv *Interview) interviewResponse {
	return interviewResponse{
		ID:              iv.ID,
		TenantID:        iv.TenantID,
		ApplicationID:   iv.ApplicationID,
		Status:          iv.Status,
		Format:          iv.Format,
		ScheduledAt:     fmtTimePtr(iv.ScheduledAt),
		OnlineURL:       iv.OnlineURL,
		ExternalEventID: iv.ExternalEventID,
		Notes:           iv.Notes,
		CreatedAt:       fmtTime(iv.CreatedAt),
		UpdatedAt:       fmtTime(iv.UpdatedAt),
	}
}

// CreateInterview handles POST /interviews.
func (h *Handler) CreateInterview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createInterviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	appID, err := uuid.Parse(req.ApplicationID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid application_id")
		return
	}

	iv, err := h.svc.CreateInterview(c.Request.Context(), CreateInterviewInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		ApplicationID: appID,
		Format:        req.Format,
		OnlineURL:     req.OnlineURL,
		Notes:         req.Notes,
		IP:            clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, tointerviewResponse(iv))
}

// ListInterviews handles GET /interviews?application_id=...
func (h *Handler) ListInterviews(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	var appID uuid.UUID
	if q := c.Query("application_id"); q != "" {
		id, err := uuid.Parse(q)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid application_id")
			return
		}
		appID = id
	}

	list, err := h.svc.ListInterviews(c.Request.Context(), tenantID, appID)
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	items := make([]interviewResponse, len(list))
	for i := range list {
		items[i] = tointerviewResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"interviews": items})
}

// GetInterview handles GET /interviews/:id.
func (h *Handler) GetInterview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}
	iv, err := h.svc.GetInterview(c.Request.Context(), tenantID, id)
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tointerviewResponse(iv))
}

// ---------------------------------------------------------------------------
// Slot shapes
// ---------------------------------------------------------------------------

type addSlotRequest struct {
	CandidateStart string  `json:"candidate_start" validate:"required"`
	CandidateEnd   *string `json:"candidate_end"`
}

// SlotResponse is the JSON representation of a candidate slot.
type SlotResponse struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	InterviewID    uuid.UUID `json:"interview_id"`
	CandidateStart string    `json:"candidate_start"`
	CandidateEnd   *string   `json:"candidate_end,omitempty"`
	Selected       bool      `json:"selected"`
	CreatedAt      string    `json:"created_at"`
	UpdatedAt      string    `json:"updated_at"`
}

func toSlotResponse(s *Slot) SlotResponse {
	return SlotResponse{
		ID:             s.ID,
		TenantID:       s.TenantID,
		InterviewID:    s.InterviewID,
		CandidateStart: fmtTime(s.CandidateStart),
		CandidateEnd:   fmtTimePtr(s.CandidateEnd),
		Selected:       s.Selected,
		CreatedAt:      fmtTime(s.CreatedAt),
		UpdatedAt:      fmtTime(s.UpdatedAt),
	}
}

func parseRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// AddSlot handles POST /interviews/:id/slots.
func (h *Handler) AddSlot(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	var req addSlotRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	start, err := parseRFC3339(req.CandidateStart)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "candidate_start must be RFC3339")
		return
	}
	var end *time.Time
	if req.CandidateEnd != nil && *req.CandidateEnd != "" {
		e, err := parseRFC3339(*req.CandidateEnd)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "candidate_end must be RFC3339")
			return
		}
		end = &e
	}

	slot, err := h.svc.AddSlot(c.Request.Context(), AddSlotInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		InterviewID:    ivID,
		CandidateStart: start,
		CandidateEnd:   end,
		IP:             clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSlotResponse(slot))
}

// ListSlots handles GET /interviews/:id/slots.
func (h *Handler) ListSlots(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}
	list, err := h.svc.ListSlots(c.Request.Context(), tenantID, ivID)
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	items := make([]SlotResponse, len(list))
	for i := range list {
		items[i] = toSlotResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"slots": items})
}

// ---------------------------------------------------------------------------
// Panellist shapes
// ---------------------------------------------------------------------------

type addPanelistRequest struct {
	UserID string `json:"user_id" validate:"required,uuid"`
	Role   string `json:"role"    validate:"omitempty,oneof=interviewer observer"`
}

// PanelistResponse is the JSON representation of a panellist.
type PanelistResponse struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	InterviewID uuid.UUID `json:"interview_id"`
	UserID      uuid.UUID `json:"user_id"`
	Role        string    `json:"role"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

func toPanelistResponse(p *Panellist) PanelistResponse {
	return PanelistResponse{
		ID:          p.ID,
		TenantID:    p.TenantID,
		InterviewID: p.InterviewID,
		UserID:      p.UserID,
		Role:        p.Role,
		CreatedAt:   fmtTime(p.CreatedAt),
		UpdatedAt:   fmtTime(p.UpdatedAt),
	}
}

// AddPanelist handles POST /interviews/:id/panelists.
func (h *Handler) AddPanelist(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	var req addPanelistRequest
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

	p, err := h.svc.AddPanelist(c.Request.Context(), AddPanelistInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		InterviewID: ivID,
		UserID:      userID,
		Role:        req.Role,
		IP:          clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toPanelistResponse(p))
}

// ListPanelists handles GET /interviews/:id/panelists.
func (h *Handler) ListPanelists(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}
	list, err := h.svc.ListPanelists(c.Request.Context(), tenantID, ivID)
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	items := make([]PanelistResponse, len(list))
	for i := range list {
		items[i] = toPanelistResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"panelists": items}) //nolint:misspell // JSON key matches API contract; cannot change to UK spelling
}

// ---------------------------------------------------------------------------
// Interview transitions
// ---------------------------------------------------------------------------

type confirmInterviewRequest struct {
	SlotID string `json:"slot_id" validate:"required,uuid"`
}

// ConfirmInterview handles POST /interviews/:id/confirm.
func (h *Handler) ConfirmInterview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	var req confirmInterviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	slotID, err := uuid.Parse(req.SlotID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid slot_id")
		return
	}

	iv, err := h.svc.ConfirmInterview(c.Request.Context(), ConfirmInterviewInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		InterviewID: ivID,
		SlotID:      slotID,
		IP:          clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tointerviewResponse(iv))
}

type transitionInterviewRequest struct {
	Status string `json:"status" validate:"required,oneof=completed cancelled"`
}

// TransitionInterview handles POST /interviews/:id/transition.
func (h *Handler) TransitionInterview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	var req transitionInterviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	iv, err := h.svc.TransitionInterview(c.Request.Context(), TransitionInterviewInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		InterviewID: ivID,
		Status:      req.Status,
		IP:          clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tointerviewResponse(iv))
}

type setExternalEventRequest struct {
	ExternalEventID *string `json:"external_event_id" validate:"omitempty,max=500"`
}

// SetExternalEvent handles PATCH /interviews/:id/external-event.
func (h *Handler) SetExternalEvent(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	var req setExternalEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	iv, err := h.svc.SetExternalEvent(c.Request.Context(), SetExternalEventInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		InterviewID:     ivID,
		ExternalEventID: req.ExternalEventID,
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tointerviewResponse(iv))
}

// ---------------------------------------------------------------------------
// Evaluation sheet & settings shapes
// ---------------------------------------------------------------------------

type createSheetRequest struct {
	Name      string          `json:"name"       validate:"required,max=200"`
	ItemsJSON json.RawMessage `json:"items_json"`
}

// SheetResponse is the JSON representation of an evaluation sheet.
type SheetResponse struct {
	ID        uuid.UUID       `json:"id"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	Name      string          `json:"name"`
	ItemsJSON json.RawMessage `json:"items_json"`
	Active    bool            `json:"active"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func toSheetResponse(s *EvaluationSheet) SheetResponse {
	items := json.RawMessage(s.ItemsJSON)
	if len(items) == 0 {
		items = json.RawMessage(`[]`)
	}
	return SheetResponse{
		ID:        s.ID,
		TenantID:  s.TenantID,
		Name:      s.Name,
		ItemsJSON: items,
		Active:    s.Active,
		CreatedAt: fmtTime(s.CreatedAt),
		UpdatedAt: fmtTime(s.UpdatedAt),
	}
}

// CreateSheet handles POST /evaluation-sheets.
func (h *Handler) CreateSheet(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createSheetRequest
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

	sheet, err := h.svc.CreateSheet(c.Request.Context(), CreateSheetInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		Name:      req.Name,
		ItemsJSON: items,
		IP:        clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSheetResponse(sheet))
}

// ListSheets handles GET /evaluation-sheets.
func (h *Handler) ListSheets(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	list, err := h.svc.ListSheets(c.Request.Context(), tenantID)
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	items := make([]SheetResponse, len(list))
	for i := range list {
		items[i] = toSheetResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"evaluation_sheets": items})
}

type setVisibilityRequest struct {
	PeerEvalVisible bool `json:"peer_eval_visible"`
}

// SettingsResponse is the JSON representation of tenant interview settings.
type SettingsResponse struct {
	TenantID        uuid.UUID `json:"tenant_id"`
	PeerEvalVisible bool      `json:"peer_eval_visible"`
	UpdatedAt       string    `json:"updated_at"`
}

// SetPeerEvalVisibility handles PUT /interview-settings/peer-eval-visibility.
func (h *Handler) SetPeerEvalVisibility(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req setVisibilityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}

	settings, err := h.svc.SetPeerEvalVisibility(c.Request.Context(), SetPeerEvalVisibilityInput{
		TenantID: tenantID,
		ActorID:  actorID,
		Visible:  req.PeerEvalVisible,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, SettingsResponse{
		TenantID:        settings.TenantID,
		PeerEvalVisible: settings.PeerEvalVisible,
		UpdatedAt:       fmtTime(settings.UpdatedAt),
	})
}

// ---------------------------------------------------------------------------
// Evaluation shapes
// ---------------------------------------------------------------------------

type submitEvaluationRequest struct {
	EvaluatorUserID string          `json:"evaluator_user_id" validate:"required,uuid"`
	SheetID         string          `json:"sheet_id"          validate:"required,uuid"`
	ScoresJSON      json.RawMessage `json:"scores_json"`
	OverallScore    *float64        `json:"overall_score"`
	Recommendation  string          `json:"recommendation" validate:"omitempty,oneof=strong_yes yes neutral no strong_no"`
	Comment         string          `json:"comment"        validate:"omitempty,max=5000"`
}

// EvaluationResponse is the JSON representation of an evaluation.
// Comment / scores may be masked (empty) for other panelists' evaluations.
type EvaluationResponse struct {
	ID              uuid.UUID       `json:"id"`
	TenantID        uuid.UUID       `json:"tenant_id"`
	InterviewID     uuid.UUID       `json:"interview_id"`
	ApplicationID   uuid.UUID       `json:"application_id"`
	EvaluatorUserID uuid.UUID       `json:"evaluator_user_id"`
	SheetID         uuid.UUID       `json:"sheet_id"`
	ScoresJSON      json.RawMessage `json:"scores_json"`
	OverallScore    *float64        `json:"overall_score,omitempty"`
	Recommendation  string          `json:"recommendation"`
	Comment         string          `json:"comment"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

func toEvaluationResponse(e *Evaluation) EvaluationResponse {
	scores := json.RawMessage(e.ScoresJSON)
	if len(scores) == 0 {
		scores = json.RawMessage(`{}`)
	}
	return EvaluationResponse{
		ID:              e.ID,
		TenantID:        e.TenantID,
		InterviewID:     e.InterviewID,
		ApplicationID:   e.ApplicationID,
		EvaluatorUserID: e.EvaluatorUserID,
		SheetID:         e.SheetID,
		ScoresJSON:      scores,
		OverallScore:    e.OverallScore,
		Recommendation:  e.Recommendation,
		Comment:         e.Comment,
		CreatedAt:       fmtTime(e.CreatedAt),
		UpdatedAt:       fmtTime(e.UpdatedAt),
	}
}

// SubmitEvaluation handles POST /interviews/:id/evaluations.
func (h *Handler) SubmitEvaluation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	var req submitEvaluationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.ScoresJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "scores_json: "+err.Error())
		return
	}
	evaluatorID, err := uuid.Parse(req.EvaluatorUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid evaluator_user_id")
		return
	}
	sheetID, err := uuid.Parse(req.SheetID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid sheet_id")
		return
	}
	scores := []byte(req.ScoresJSON)
	if len(scores) == 0 || string(scores) == "null" {
		scores = []byte(`{}`)
	}

	ev, err := h.svc.SubmitEvaluation(c.Request.Context(), SubmitEvaluationInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		InterviewID:     ivID,
		EvaluatorUserID: evaluatorID,
		SheetID:         sheetID,
		ScoresJSON:      scores,
		OverallScore:    req.OverallScore,
		Recommendation:  req.Recommendation,
		Comment:         req.Comment,
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toEvaluationResponse(ev))
}

// ListEvaluations handles GET /interviews/:id/evaluations.
// The route applies RequirePermission(ats:evaluation:read); the handler passes
// CanReadEvaluations=true so the service can decide unmasking (combined with
// the tenant peer-visibility setting and a service-layer permission re-check).
func (h *Handler) ListEvaluations(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	ivID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid interview id")
		return
	}

	list, err := h.svc.ListEvaluations(c.Request.Context(), ListEvaluationsInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		InterviewID:        ivID,
		CanReadEvaluations: true,
		IP:                 clientIP(c),
	})
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	items := make([]EvaluationResponse, len(list))
	for i := range list {
		items[i] = toEvaluationResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"evaluations": items})
}

// EvaluationSummaryResponse is the JSON representation of an aggregation.
type EvaluationSummaryResponse struct {
	ApplicationID     uuid.UUID `json:"application_id"`
	Count             int       `json:"count"`
	AverageScore      *float64  `json:"average_score,omitempty"`
	StrongYesCount    int       `json:"strong_yes_count"`
	YesCount          int       `json:"yes_count"`
	NeutralCount      int       `json:"neutral_count"`
	NoCount           int       `json:"no_count"`
	StrongNoCount     int       `json:"strong_no_count"`
	RecommendRatioYes float64   `json:"recommend_ratio_yes"`
}

// SummarizeApplication handles GET /applications/:application_id/evaluation-summary.
func (h *Handler) SummarizeApplication(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	appID, err := uuid.Parse(c.Param("application_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid application id")
		return
	}
	summary, err := h.svc.SummarizeApplication(c.Request.Context(), tenantID, appID)
	if err != nil {
		respondServiceErr(c, err)
		return
	}
	c.JSON(http.StatusOK, EvaluationSummaryResponse{
		ApplicationID:     summary.ApplicationID,
		Count:             summary.Count,
		AverageScore:      summary.AverageScore,
		StrongYesCount:    summary.StrongYesCount,
		YesCount:          summary.YesCount,
		NeutralCount:      summary.NeutralCount,
		NoCount:           summary.NoCount,
		StrongNoCount:     summary.StrongNoCount,
		RecommendRatioYes: summary.RecommendRatioYes,
	})
}
