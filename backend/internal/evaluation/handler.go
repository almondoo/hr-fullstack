package evaluation

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

// Handler exposes HTTP endpoints for the evaluation domain.
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

// respondServiceError maps a service error to an HTTP response.
func respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrConfirmed):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "review is confirmed (read-only)")
	case errors.Is(err, ErrIncomplete):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "stage has unanswered items")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "transition not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Template DTOs
// ---------------------------------------------------------------------------

type createTemplateRequest struct {
	Name        string          `json:"name"               validate:"required,max=200"`
	Stages      json.RawMessage `json:"stages"`
	Items       json.RawMessage `json:"items"`
	RatingScale json.RawMessage `json:"rating_scale"`
}

// TemplateResponse is the JSON representation of a review template.
type TemplateResponse struct {
	ID          uuid.UUID       `json:"id"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	Name        string          `json:"name"`
	Stages      json.RawMessage `json:"stages"`
	Items       json.RawMessage `json:"items"`
	RatingScale json.RawMessage `json:"rating_scale"`
	Active      bool            `json:"active"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

func toTemplateResponse(t *Template) TemplateResponse {
	return TemplateResponse{
		ID:          t.ID,
		TenantID:    t.TenantID,
		Name:        t.Name,
		Stages:      rawOrDefault(t.StagesJSON, "[]"),
		Items:       rawOrDefault(t.ItemsJSON, "[]"),
		RatingScale: rawOrDefault(t.RatingScaleJSON, "{}"),
		Active:      t.Active,
		CreatedAt:   t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func rawOrDefault(b []byte, fallback string) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage(fallback)
	}
	return json.RawMessage(b)
}

// CreateTemplate handles POST /evaluations/templates.
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
	for name, raw := range map[string]json.RawMessage{
		"stages": req.Stages, "items": req.Items, "rating_scale": req.RatingScale,
	} {
		if err := validateJSON(raw); err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", name+": "+err.Error())
			return
		}
	}

	tmpl, err := h.svc.CreateTemplate(c.Request.Context(), CreateTemplateInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		Name:            req.Name,
		StagesJSON:      []byte(req.Stages),
		ItemsJSON:       []byte(req.Items),
		RatingScaleJSON: []byte(req.RatingScale),
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toTemplateResponse(tmpl))
}

// ListTemplates handles GET /evaluations/templates.
func (h *Handler) ListTemplates(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	tmpls, err := h.svc.ListTemplates(c.Request.Context(), tenantID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]TemplateResponse, len(tmpls))
	for i := range tmpls {
		items[i] = toTemplateResponse(&tmpls[i])
	}
	c.JSON(http.StatusOK, gin.H{"templates": items})
}

// ---------------------------------------------------------------------------
// Review DTOs
// ---------------------------------------------------------------------------

type createReviewRequest struct {
	CycleID             string  `json:"cycle_id"             validate:"required"`
	TemplateID          string  `json:"template_id"          validate:"required"`
	EmployeeID          string  `json:"employee_id"          validate:"required"`
	PrimaryReviewerID   *string `json:"primary_reviewer_id"`
	SecondaryReviewerID *string `json:"secondary_reviewer_id"`
}

// ReviewResponse is the JSON representation of a review header.
type ReviewResponse struct {
	ID                  uuid.UUID  `json:"id"`
	TenantID            uuid.UUID  `json:"tenant_id"`
	CycleID             uuid.UUID  `json:"cycle_id"`
	TemplateID          uuid.UUID  `json:"template_id"`
	EmployeeID          uuid.UUID  `json:"employee_id"`
	PrimaryReviewerID   *uuid.UUID `json:"primary_reviewer_id,omitempty"`
	SecondaryReviewerID *uuid.UUID `json:"secondary_reviewer_id,omitempty"`
	Status              string     `json:"status"`
	FinalRating         *float64   `json:"final_rating,omitempty"`
	AdjustedRating      *float64   `json:"adjusted_rating,omitempty"`
	ConfirmedAt         *string    `json:"confirmed_at,omitempty"`
	CreatedAt           string     `json:"created_at"`
	UpdatedAt           string     `json:"updated_at"`
}

func toReviewResponse(r *Review) ReviewResponse {
	resp := ReviewResponse{
		ID:                  r.ID,
		TenantID:            r.TenantID,
		CycleID:             r.CycleID,
		TemplateID:          r.TemplateID,
		EmployeeID:          r.EmployeeID,
		PrimaryReviewerID:   r.PrimaryReviewerID,
		SecondaryReviewerID: r.SecondaryReviewerID,
		Status:              r.Status,
		FinalRating:         r.FinalRating,
		AdjustedRating:      r.AdjustedRating,
		CreatedAt:           r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:           r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.ConfirmedAt != nil {
		s := r.ConfirmedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.ConfirmedAt = &s
	}
	return resp
}

// CreateReview handles POST /evaluations/reviews.
func (h *Handler) CreateReview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createReviewRequest
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
	templateID, err := uuid.Parse(req.TemplateID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid template_id")
		return
	}
	employeeID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}
	primaryID, err := parseOptionalUUID(req.PrimaryReviewerID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid primary_reviewer_id")
		return
	}
	secondaryID, err := parseOptionalUUID(req.SecondaryReviewerID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid secondary_reviewer_id")
		return
	}

	review, err := h.svc.CreateReview(c.Request.Context(), CreateReviewInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		CycleID:             cycleID,
		TemplateID:          templateID,
		EmployeeID:          employeeID,
		PrimaryReviewerID:   primaryID,
		SecondaryReviewerID: secondaryID,
		IP:                  clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toReviewResponse(review))
}

// GetReview handles GET /evaluations/reviews/:id.
func (h *Handler) GetReview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}
	review, err := h.svc.GetReview(c.Request.Context(), tenantID, id)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toReviewResponse(review))
}

// ListReviews handles GET /evaluations/reviews?cycle_id=...
func (h *Handler) ListReviews(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	cycleStr := c.Query("cycle_id")
	cycleID, err := uuid.Parse(cycleStr)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "cycle_id query parameter is required")
		return
	}
	reviews, err := h.svc.ListReviewsByCycle(c.Request.Context(), tenantID, cycleID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]ReviewResponse, len(reviews))
	for i := range reviews {
		items[i] = toReviewResponse(&reviews[i])
	}
	c.JSON(http.StatusOK, gin.H{"reviews": items})
}

// ---------------------------------------------------------------------------
// Entry DTOs
// ---------------------------------------------------------------------------

type upsertEntryRequest struct {
	Stage          string   `json:"stage"            validate:"required,oneof=self primary secondary 360"`
	ReviewerUserID *string  `json:"reviewer_user_id"`
	ItemKey        string   `json:"item_key"         validate:"required,max=200"`
	Score          *float64 `json:"score"`
	Comment        *string  `json:"comment"`
}

// EntryResponse is the JSON representation of a review entry.
type EntryResponse struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	ReviewID       uuid.UUID  `json:"review_id"`
	Stage          string     `json:"stage"`
	ReviewerUserID *uuid.UUID `json:"reviewer_user_id,omitempty"`
	ItemKey        string     `json:"item_key"`
	Score          *float64   `json:"score,omitempty"`
	Comment        *string    `json:"comment,omitempty"`
	SubmittedAt    *string    `json:"submitted_at,omitempty"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
}

func toEntryResponse(e *Entry) EntryResponse {
	resp := EntryResponse{
		ID:             e.ID,
		TenantID:       e.TenantID,
		ReviewID:       e.ReviewID,
		Stage:          e.Stage,
		ReviewerUserID: e.ReviewerUserID,
		ItemKey:        e.ItemKey,
		Score:          e.Score,
		Comment:        e.Comment,
		CreatedAt:      e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      e.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if e.SubmittedAt != nil {
		s := e.SubmittedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.SubmittedAt = &s
	}
	return resp
}

// UpsertEntry handles POST /evaluations/reviews/:id/entries.
func (h *Handler) UpsertEntry(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}

	var req upsertEntryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	reviewerID, err := parseOptionalUUID(req.ReviewerUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid reviewer_user_id")
		return
	}

	entry, err := h.svc.UpsertEntry(c.Request.Context(), UpsertEntryInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		ReviewID:       reviewID,
		Stage:          req.Stage,
		ReviewerUserID: reviewerID,
		ItemKey:        req.ItemKey,
		Score:          req.Score,
		Comment:        req.Comment,
		IP:             clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toEntryResponse(entry))
}

// ListEntries handles GET /evaluations/reviews/:id/entries?stage=...
func (h *Handler) ListEntries(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}
	stage := c.Query("stage")
	entries, err := h.svc.ListEntries(c.Request.Context(), tenantID, reviewID, stage)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]EntryResponse, len(entries))
	for i := range entries {
		items[i] = toEntryResponse(&entries[i])
	}
	c.JSON(http.StatusOK, gin.H{"entries": items})
}

// ---------------------------------------------------------------------------
// Stage submission / confirmation
// ---------------------------------------------------------------------------

type submitStageRequest struct {
	NextStatus   string  `json:"next_status" validate:"required,oneof=self_submitted primary_submitted secondary_submitted calibrated not_started"`
	DepartmentID *string `json:"department_id"`
}

// SubmitStage handles POST /evaluations/reviews/:id/submit.
func (h *Handler) SubmitStage(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}

	var req submitStageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	deptID, err := parseOptionalUUID(req.DepartmentID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid department_id")
		return
	}

	review, err := h.svc.SubmitStage(c.Request.Context(), SubmitStageInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		ReviewID:     reviewID,
		NextStatus:   req.NextStatus,
		DepartmentID: deptID,
		IP:           clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toReviewResponse(review))
}

// ConfirmReview handles POST /evaluations/reviews/:id/confirm.
func (h *Handler) ConfirmReview(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}

	review, err := h.svc.ConfirmReview(c.Request.Context(), ConfirmReviewInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ReviewID: reviewID,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toReviewResponse(review))
}

// ---------------------------------------------------------------------------
// 360-degree DTOs
// ---------------------------------------------------------------------------

type create360Request struct {
	RaterEmployeeID string `json:"rater_employee_id" validate:"required"`
	Relationship    string `json:"relationship"      validate:"omitempty,oneof=peer subordinate other"`
	Anonymous       bool   `json:"anonymous"`
}

// Request360Response is the JSON representation of a 360 request.
type Request360Response struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	ReviewID        uuid.UUID `json:"review_id"`
	RaterEmployeeID uuid.UUID `json:"rater_employee_id"`
	Relationship    string    `json:"relationship"`
	Anonymous       bool      `json:"anonymous"`
	Status          string    `json:"status"`
	RespondedAt     *string   `json:"responded_at,omitempty"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
}

func toRequest360Response(r *Request360) Request360Response {
	resp := Request360Response{
		ID:              r.ID,
		TenantID:        r.TenantID,
		ReviewID:        r.ReviewID,
		RaterEmployeeID: r.RaterEmployeeID,
		Relationship:    r.Relationship,
		Anonymous:       r.Anonymous,
		Status:          r.Status,
		CreatedAt:       r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:       r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.RespondedAt != nil {
		s := r.RespondedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.RespondedAt = &s
	}
	return resp
}

// Create360Request handles POST /evaluations/reviews/:id/360/requests.
func (h *Handler) Create360Request(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}

	var req create360Request
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	raterID, err := uuid.Parse(req.RaterEmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid rater_employee_id")
		return
	}

	created, err := h.svc.Create360Request(c.Request.Context(), Create360RequestInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ReviewID:        reviewID,
		RaterEmployeeID: raterID,
		Relationship:    req.Relationship,
		Anonymous:       req.Anonymous,
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toRequest360Response(created))
}

type submit360Item struct {
	ItemKey string   `json:"item_key" validate:"required,max=200"`
	Score   *float64 `json:"score"`
	Comment *string  `json:"comment"`
}

type submit360Request struct {
	Entries []submit360Item `json:"entries" validate:"required,min=1,dive"`
}

// Submit360Response handles POST /evaluations/360/requests/:request_id/respond.
func (h *Handler) Submit360Response(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	requestID, err := uuid.Parse(c.Param("request_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	var req submit360Request
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	items := make([]Item360, len(req.Entries))
	for i, e := range req.Entries {
		items[i] = Item360(e)
	}

	if err := h.svc.Submit360Response(c.Request.Context(), Submit360ResponseInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		RequestID: requestID,
		Entries:   items,
		IP:        clientIP(c),
	}); err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "360 response submitted"})
}

// Aggregate360Response is the anonymity-safe aggregation JSON.
type Aggregate360Response struct {
	ReviewID         uuid.UUID          `json:"review_id"`
	ResponseCount    int                `json:"response_count"`
	Suppressed       bool               `json:"suppressed"`
	ItemAverages     map[string]float64 `json:"item_averages"`
	RaterEmployeeIDs []uuid.UUID        `json:"rater_employee_ids,omitempty"`
}

// Aggregate360 handles GET /evaluations/reviews/:id/360/aggregate?min_responses=N.
func (h *Handler) Aggregate360(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	reviewID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid review id")
		return
	}

	minResponses := 0
	if v := c.Query("min_responses"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &minResponses); err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "min_responses must be an integer")
			return
		}
	}

	res, err := h.svc.Aggregate360(c.Request.Context(), tenantID, reviewID, minResponses)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, Aggregate360Response{
		ReviewID:         res.ReviewID,
		ResponseCount:    res.ResponseCount,
		Suppressed:       res.Suppressed,
		ItemAverages:     res.ItemAverages,
		RaterEmployeeIDs: res.RaterEmployeeIDs,
	})
}

// ---------------------------------------------------------------------------
// Calibration DTOs
// ---------------------------------------------------------------------------

type createCalibrationRequest struct {
	CycleID           string  `json:"cycle_id" validate:"required"`
	Name              string  `json:"name"     validate:"required,max=200"`
	FacilitatorUserID *string `json:"facilitator_user_id"`
}

// CalibrationSessionResponse is the JSON representation of a calibration session.
type CalibrationSessionResponse struct {
	ID                uuid.UUID       `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	CycleID           uuid.UUID       `json:"cycle_id"`
	Name              string          `json:"name"`
	FacilitatorUserID *uuid.UUID      `json:"facilitator_user_id,omitempty"`
	Status            string          `json:"status"`
	Decisions         json.RawMessage `json:"decisions"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
}

func toCalibrationResponse(s *CalibrationSession) CalibrationSessionResponse {
	return CalibrationSessionResponse{
		ID:                s.ID,
		TenantID:          s.TenantID,
		CycleID:           s.CycleID,
		Name:              s.Name,
		FacilitatorUserID: s.FacilitatorUserID,
		Status:            s.Status,
		Decisions:         rawOrDefault(s.DecisionsJSON, "[]"),
		CreatedAt:         s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// CreateCalibrationSession handles POST /evaluations/calibration-sessions.
func (h *Handler) CreateCalibrationSession(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createCalibrationRequest
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
	facilitatorID, err := parseOptionalUUID(req.FacilitatorUserID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid facilitator_user_id")
		return
	}

	sess, err := h.svc.CreateCalibrationSession(c.Request.Context(), CreateCalibrationInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		CycleID:           cycleID,
		Name:              req.Name,
		FacilitatorUserID: facilitatorID,
		IP:                clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toCalibrationResponse(sess))
}

type applyCalibrationRequest struct {
	ReviewID string  `json:"review_id" validate:"required"`
	After    float64 `json:"after"`
	Reason   string  `json:"reason"    validate:"max=2000"`
}

// ApplyCalibration handles POST /evaluations/calibration-sessions/:session_id/decisions.
func (h *Handler) ApplyCalibration(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}

	var req applyCalibrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	reviewID, err := uuid.Parse(req.ReviewID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid review_id")
		return
	}

	review, err := h.svc.ApplyCalibration(c.Request.Context(), ApplyCalibrationInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		SessionID: sessionID,
		ReviewID:  reviewID,
		After:     req.After,
		Reason:    req.Reason,
		IP:        clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toReviewResponse(review))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// parseOptionalUUID parses an optional UUID string pointer.
// Returns (nil, nil) for nil/empty input.
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
