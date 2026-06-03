package offer

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

// Handler exposes HTTP endpoints for the offer domain.
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

// mapServiceError maps a service-layer error to a unified HTTP error response.
// Returns true when the error was handled (response written).
func mapServiceError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "offer status transition not allowed")
	case errors.Is(err, ErrNotApproved):
		httpx.RespondError(c, http.StatusConflict, "NOT_APPROVED", "offer issuance approval not granted")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	case errors.Is(err, ErrSettingNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "offer settings not found")
	default:
		httpx.RespondInternalError(c)
	}
	return true
}

// ---------------------------------------------------------------------------
// Settings shapes / handlers
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	RequiredFields        json.RawMessage `json:"required_fields"`
	RetentionYears        int             `json:"retention_years"         validate:"required,min=1"`
	EsignStorageMode      string          `json:"esign_storage_mode"      validate:"required,max=100"`
	DefaultExpiryLeadDays int             `json:"default_expiry_lead_days" validate:"min=0"`
}

// SettingResponse is the JSON representation of offer settings.
type SettingResponse struct {
	ID                    uuid.UUID       `json:"id"`
	TenantID              uuid.UUID       `json:"tenant_id"`
	RequiredFields        json.RawMessage `json:"required_fields"`
	RetentionYears        int             `json:"retention_years"`
	EsignStorageMode      string          `json:"esign_storage_mode"`
	DefaultExpiryLeadDays int             `json:"default_expiry_lead_days"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
}

func toSettingResponse(s *Setting) SettingResponse {
	rf := json.RawMessage(s.RequiredFieldsJSON)
	if len(rf) == 0 {
		rf = json.RawMessage(`{"fields":[]}`)
	}
	return SettingResponse{
		ID:                    s.ID,
		TenantID:              s.TenantID,
		RequiredFields:        rf,
		RetentionYears:        s.RetentionYears,
		EsignStorageMode:      s.EsignStorageMode,
		DefaultExpiryLeadDays: s.DefaultExpiryLeadDays,
		CreatedAt:             s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:             s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// GetSettings handles GET /offers/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	s, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSettingResponse(s))
}

// UpsertSettings handles PUT /offers/settings.
func (h *Handler) UpsertSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req upsertSettingsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.RequiredFields); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "required_fields: "+err.Error())
		return
	}

	rf := []byte(req.RequiredFields)
	if len(rf) == 0 || string(rf) == "null" {
		rf = []byte(`{"fields":[]}`)
	}

	s, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		RequiredFieldsJSON:    rf,
		RetentionYears:        req.RetentionYears,
		EsignStorageMode:      req.EsignStorageMode,
		DefaultExpiryLeadDays: req.DefaultExpiryLeadDays,
		IP:                    clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSettingResponse(s))
}

// ---------------------------------------------------------------------------
// Offer shapes / handlers
// ---------------------------------------------------------------------------

type createOfferRequest struct {
	ApplicationID  string  `json:"application_id"  validate:"required"`
	Position       string  `json:"position"        validate:"omitempty,max=200"`
	EmploymentType string  `json:"employment_type" validate:"omitempty,max=50"`
	StartDate      *string `json:"start_date"`
	ExpiryDate     *string `json:"expiry_date"`
	// AnnualSalary and CompensationDetail are sensitive offer terms in plaintext.
	// They are encrypted before storage and NEVER persisted as plaintext.
	AnnualSalary       string `json:"annual_salary"       validate:"omitempty,max=100"`
	CompensationDetail string `json:"compensation_detail" validate:"omitempty,max=2000"`
}

// OfferResponse is the JSON representation of an offer.
// AnnualSalary / CompensationDetail are populated only for callers holding
// offer:read_sensitive; otherwise they are omitted and a masked marker is set.
type OfferResponse struct { //nolint:revive // name intentionally includes package prefix for API clarity
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	ApplicationID     uuid.UUID  `json:"application_id"`
	Status            string     `json:"status"`
	Position          string     `json:"position"`
	EmploymentType    string     `json:"employment_type"`
	StartDate         *string    `json:"start_date,omitempty"`
	ExpiryDate        *string    `json:"expiry_date,omitempty"`
	ApprovalRequestID *uuid.UUID `json:"approval_request_id,omitempty"`
	// AnnualSalary is only populated when the caller holds offer:read_sensitive.
	AnnualSalary *string `json:"annual_salary,omitempty"`
	// CompensationDetail is only populated when the caller holds offer:read_sensitive.
	CompensationDetail *string `json:"compensation_detail,omitempty"`
	// SalaryMasked indicates a non-sensitive read; "****" when set.
	SalaryMasked *string `json:"salary_masked,omitempty"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

func toOfferResponse(o *Offer, terms *DecryptedTerms) OfferResponse {
	r := OfferResponse{
		ID:                o.ID,
		TenantID:          o.TenantID,
		ApplicationID:     o.ApplicationID,
		Status:            o.Status,
		Position:          o.Position,
		EmploymentType:    o.EmploymentType,
		ApprovalRequestID: o.ApprovalRequestID,
		CreatedAt:         o.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         o.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if o.StartDate != nil {
		s := o.StartDate.Format("2006-01-02")
		r.StartDate = &s
	}
	if o.ExpiryDate != nil {
		s := o.ExpiryDate.Format("2006-01-02")
		r.ExpiryDate = &s
	}
	if terms != nil {
		if len(terms.AnnualSalary) > 0 {
			s := string(terms.AnnualSalary)
			r.AnnualSalary = &s
		}
		if len(terms.CompensationDetail) > 0 {
			s := string(terms.CompensationDetail)
			r.CompensationDetail = &s
		}
	} else {
		masked := "****"
		r.SalaryMasked = &masked
	}
	return r
}

// CreateOffer handles POST /offers.
func (h *Handler) CreateOffer(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createOfferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	applicationID, err := uuid.Parse(req.ApplicationID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid application_id")
		return
	}

	startDate, err := parseDate(req.StartDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	expiryDate, err := parseDate(req.ExpiryDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	off, err := h.svc.CreateOffer(c.Request.Context(), CreateOfferInput{
		TenantID:                    tenantID,
		ActorID:                     actorID,
		ApplicationID:               applicationID,
		Position:                    req.Position,
		EmploymentType:              req.EmploymentType,
		StartDate:                   startDate,
		ExpiryDate:                  expiryDate,
		AnnualSalaryPlaintext:       []byte(req.AnnualSalary),
		CompensationDetailPlaintext: []byte(req.CompensationDetail),
		IP:                          clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toOfferResponse(off, nil))
}

// GetOffer handles GET /offers/:id (masked sensitive terms).
func (h *Handler) GetOffer(c *gin.Context) {
	h.getOffer(c, false)
}

// GetOfferSensitive handles GET /offers/:id/sensitive (decrypted sensitive
// terms). The route requires offer:read_sensitive (enforced in routes.go).
func (h *Handler) GetOfferSensitive(c *gin.Context) {
	h.getOffer(c, true)
}

func (h *Handler) getOffer(c *gin.Context, readSensitive bool) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	off, terms, err := h.svc.GetOffer(c.Request.Context(), GetOfferInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		ID:            id,
		ReadSensitive: readSensitive,
		IP:            clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toOfferResponse(off, terms))
}

// ListOffers handles GET /offers (optional ?application_id= filter).
func (h *Handler) ListOffers(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	var applicationID uuid.UUID
	if raw := c.Query("application_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid application_id")
			return
		}
		applicationID = id
	}

	offers, err := h.svc.ListOffers(c.Request.Context(), tenantID, applicationID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]OfferResponse, len(offers))
	for i := range offers {
		items[i] = toOfferResponse(&offers[i], nil)
	}
	c.JSON(http.StatusOK, gin.H{"offers": items})
}

// SubmitForApproval handles POST /offers/:id/submit-approval.
func (h *Handler) SubmitForApproval(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	off, err := h.svc.SubmitForApproval(c.Request.Context(), SubmitForApprovalInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		IP:       clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toOfferResponse(off, nil))
}

// SendOffer handles POST /offers/:id/send.
func (h *Handler) SendOffer(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	off, err := h.svc.SendOffer(c.Request.Context(), SendOfferInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		IP:       clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toOfferResponse(off, nil))
}

// RescindOffer handles POST /offers/:id/rescind.
func (h *Handler) RescindOffer(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	off, err := h.svc.RescindOffer(c.Request.Context(), RescindOfferInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		IP:       clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toOfferResponse(off, nil))
}

// ---------------------------------------------------------------------------
// Letter shapes / handlers
// ---------------------------------------------------------------------------

type issueLetterRequest struct {
	FileRef         string  `json:"file_ref"          validate:"omitempty,max=500"`
	EsignProvider   string  `json:"esign_provider"    validate:"omitempty,max=100"`
	EsignEnvelopeID string  `json:"esign_envelope_id" validate:"omitempty,max=200"`
	ContentHash     string  `json:"content_hash"      validate:"omitempty,max=128"`
	SignerRef       *string `json:"signer_ref"        validate:"omitempty,max=200"`
	SignedAt        *string `json:"signed_at"`
}

// LetterResponse is the JSON representation of an offer letter.
type LetterResponse struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	OfferID         uuid.UUID `json:"offer_id"`
	FileRef         string    `json:"file_ref"`
	Version         int       `json:"version"`
	EsignProvider   string    `json:"esign_provider"`
	EsignEnvelopeID string    `json:"esign_envelope_id"`
	ContentHash     string    `json:"content_hash"`
	SignerRef       *string   `json:"signer_ref,omitempty"`
	SignedAt        *string   `json:"signed_at,omitempty"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
}

func toLetterResponse(l *Letter) LetterResponse {
	r := LetterResponse{
		ID:              l.ID,
		TenantID:        l.TenantID,
		OfferID:         l.OfferID,
		FileRef:         l.FileRef,
		Version:         l.Version,
		EsignProvider:   l.EsignProvider,
		EsignEnvelopeID: l.EsignEnvelopeID,
		ContentHash:     l.ContentHash,
		SignerRef:       l.SignerRef,
		CreatedAt:       l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:       l.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if l.SignedAt != nil {
		s := l.SignedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.SignedAt = &s
	}
	return r
}

// IssueLetter handles POST /offers/:id/letters.
func (h *Handler) IssueLetter(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	offerID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	var req issueLetterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	var signedAt *time.Time
	if req.SignedAt != nil && *req.SignedAt != "" {
		t, perr := time.Parse(time.RFC3339, *req.SignedAt)
		if perr != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "signed_at must be RFC3339")
			return
		}
		signedAt = &t
	}

	letter, err := h.svc.IssueLetter(c.Request.Context(), IssueLetterInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		OfferID:         offerID,
		FileRef:         req.FileRef,
		EsignProvider:   req.EsignProvider,
		EsignEnvelopeID: req.EsignEnvelopeID,
		ContentHash:     req.ContentHash,
		SignerRef:       req.SignerRef,
		SignedAt:        signedAt,
		IP:              clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toLetterResponse(letter))
}

// ListLetters handles GET /offers/:id/letters.
func (h *Handler) ListLetters(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	offerID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	letters, err := h.svc.ListLetters(c.Request.Context(), tenantID, offerID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]LetterResponse, len(letters))
	for i := range letters {
		items[i] = toLetterResponse(&letters[i])
	}
	c.JSON(http.StatusOK, gin.H{"letters": items})
}

// ---------------------------------------------------------------------------
// Response shapes / handlers
// ---------------------------------------------------------------------------

type respondRequest struct {
	Response     string `json:"response"      validate:"required,oneof=accepted declined"`
	RespondedVia string `json:"responded_via" validate:"omitempty,oneof=portal esign manual"`
}

// ResponseDTO is the JSON representation of an offer response.
type ResponseDTO struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	OfferID      uuid.UUID `json:"offer_id"`
	Response     string    `json:"response"`
	RespondedVia string    `json:"responded_via"`
	RespondedAt  string    `json:"responded_at"`
}

func toResponseDTO(r *Response) ResponseDTO {
	return ResponseDTO{
		ID:           r.ID,
		TenantID:     r.TenantID,
		OfferID:      r.OfferID,
		Response:     r.Response,
		RespondedVia: r.RespondedVia,
		RespondedAt:  r.RespondedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// Respond handles POST /offers/:id/respond.
func (h *Handler) Respond(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	offerID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	var req respondRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	resp, err := h.svc.Respond(c.Request.Context(), RespondInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		OfferID:      offerID,
		Response:     req.Response,
		RespondedVia: req.RespondedVia,
		IP:           clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toResponseDTO(resp))
}

// ListResponses handles GET /offers/:id/responses.
func (h *Handler) ListResponses(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	offerID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid offer id")
		return
	}

	responses, err := h.svc.ListResponses(c.Request.Context(), tenantID, offerID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]ResponseDTO, len(responses))
	for i := range responses {
		items[i] = toResponseDTO(&responses[i])
	}
	c.JSON(http.StatusOK, gin.H{"responses": items})
}
