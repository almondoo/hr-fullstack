package govfiling

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

// maxDocumentBytes limits the plaintext size of an attached document body.
const maxDocumentBytes = 256 * 1024 // 256 KB

// Handler exposes HTTP endpoints for the govfiling domain.
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

// ---------------------------------------------------------------------------
// Settings request / response shapes
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	InsurerKind        string          `json:"insurer_kind"        validate:"required,oneof=kyokai kumiai"`
	RateTable          json.RawMessage `json:"rate_table"`
	GradeTable         json.RawMessage `json:"grade_table"`
	JudgementThreshold json.RawMessage `json:"judgement_threshold"`
	FormVersion        json.RawMessage `json:"form_version"`
}

// SettingsResponse is the JSON representation of insurance settings.
type SettingsResponse struct {
	ID                 uuid.UUID       `json:"id"`
	TenantID           uuid.UUID       `json:"tenant_id"`
	InsurerKind        string          `json:"insurer_kind"`
	RateTable          json.RawMessage `json:"rate_table"`
	GradeTable         json.RawMessage `json:"grade_table"`
	JudgementThreshold json.RawMessage `json:"judgement_threshold"`
	FormVersion        json.RawMessage `json:"form_version"`
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

func jsonOrEmptyObject(b []byte) json.RawMessage {
	r := json.RawMessage(b)
	if len(r) == 0 {
		return json.RawMessage(`{}`)
	}
	return r
}

func toSettingsResponse(s *InsuranceSettings) SettingsResponse {
	return SettingsResponse{
		ID:                 s.ID,
		TenantID:           s.TenantID,
		InsurerKind:        s.InsurerKind,
		RateTable:          jsonOrEmptyObject(s.RateTableJSON),
		GradeTable:         jsonOrEmptyObject(s.GradeTableJSON),
		JudgementThreshold: jsonOrEmptyObject(s.JudgementThresholdJSON),
		FormVersion:        jsonOrEmptyObject(s.FormVersionJSON),
		CreatedAt:          s.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:          s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func bytesOrEmptyObject(raw json.RawMessage) []byte {
	b := []byte(raw)
	if len(b) == 0 || string(b) == "null" {
		return []byte(`{}`)
	}
	return b
}

// ---------------------------------------------------------------------------
// Settings handlers
// ---------------------------------------------------------------------------

// UpsertSettings handles PUT /govfilings/settings.
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
	for name, raw := range map[string]json.RawMessage{
		"rate_table":          req.RateTable,
		"grade_table":         req.GradeTable,
		"judgement_threshold": req.JudgementThreshold,
		"form_version":        req.FormVersion,
	} {
		if err := validateJSON(raw); err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", name+": "+err.Error())
			return
		}
	}

	settings, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:               tenantID,
		ActorID:                actorID,
		InsurerKind:            req.InsurerKind,
		RateTableJSON:          bytesOrEmptyObject(req.RateTable),
		GradeTableJSON:         bytesOrEmptyObject(req.GradeTable),
		JudgementThresholdJSON: bytesOrEmptyObject(req.JudgementThreshold),
		FormVersionJSON:        bytesOrEmptyObject(req.FormVersion),
		IP:                     clientIP(c),
	})
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(settings))
}

// GetSettings handles GET /govfilings/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	settings, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "insurance settings not configured")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(settings))
}

// ---------------------------------------------------------------------------
// Grade judgement handler (LM-010)
// ---------------------------------------------------------------------------

type judgeMonthlyChangeRequest struct {
	CurrentMonthly int64 `json:"current_monthly" validate:"gte=0"`
	NewMonthly     int64 `json:"new_monthly"     validate:"gte=0"`
}

// JudgeGradeResponse is the JSON representation of a grade judgement result.
type JudgeGradeResponse struct {
	CurrentGrade          int  `json:"current_grade"`
	NewGrade              int  `json:"new_grade"`
	GradeDiff             int  `json:"grade_diff"`
	MonthlyChangeRequired bool `json:"monthly_change_required"`
}

// JudgeMonthlyChange handles POST /govfilings/judge/monthly-change.
func (h *Handler) JudgeMonthlyChange(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	var req judgeMonthlyChangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	res, err := h.svc.JudgeMonthlyChange(c.Request.Context(), JudgeMonthlyChangeInput{
		TenantID:       tenantID,
		CurrentMonthly: req.CurrentMonthly,
		NewMonthly:     req.NewMonthly,
	})
	if err != nil {
		if errors.Is(err, ErrSettingsMissing) {
			httpx.RespondError(c, http.StatusBadRequest, "SETTINGS_MISSING",
				"grade table not configured; configure insurance settings first")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, JudgeGradeResponse{
		CurrentGrade:          res.CurrentGrade,
		NewGrade:              res.NewGrade,
		GradeDiff:             res.GradeDiff,
		MonthlyChangeRequired: res.MonthlyChangeRequired,
	})
}

// ---------------------------------------------------------------------------
// Filing request / response shapes
// ---------------------------------------------------------------------------

type createFilingRequest struct {
	EmployeeID     string          `json:"employee_id"     validate:"required"`
	FilingType     string          `json:"filing_type"     validate:"required,oneof=health_insurance_acquire health_insurance_lose pension_calc pension_change employment_insurance_acquire employment_insurance_lose employment_insurance_separation workers_comp_report"`
	Channel        string          `json:"channel"         validate:"required,oneof=egov myna"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key" validate:"required,max=200"`
}

type updateStatusRequest struct {
	ToStatus        string  `json:"to_status"        validate:"required,oneof=draft submitted accepted returned completed error"`
	Note            *string `json:"note"             validate:"omitempty,max=2000"`
	ExternalMessage *string `json:"external_message" validate:"omitempty,max=4000"`
}

// FilingResponse is the JSON representation of a filing.
type FilingResponse struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenant_id"`
	EmployeeID     uuid.UUID       `json:"employee_id"`
	FilingType     string          `json:"filing_type"`
	Channel        string          `json:"channel"`
	Status         string          `json:"status"`
	Payload        json.RawMessage `json:"payload"`
	ExternalRef    *string         `json:"external_ref,omitempty"`
	IdempotencyKey string          `json:"idempotency_key"`
	SubmittedAt    *string         `json:"submitted_at,omitempty"`
	LastError      *string         `json:"last_error,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

func toFilingResponse(f *Filing) FilingResponse {
	payload := json.RawMessage(f.PayloadJSON)
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	r := FilingResponse{
		ID:             f.ID,
		TenantID:       f.TenantID,
		EmployeeID:     f.EmployeeID,
		FilingType:     f.FilingType,
		Channel:        f.Channel,
		Status:         f.Status,
		Payload:        payload,
		ExternalRef:    f.ExternalRef,
		IdempotencyKey: f.IdempotencyKey,
		LastError:      f.LastError,
		CreatedAt:      f.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      f.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if f.SubmittedAt != nil {
		s := f.SubmittedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.SubmittedAt = &s
	}
	return r
}

// ---------------------------------------------------------------------------
// Filing handlers
// ---------------------------------------------------------------------------

// CreateFiling handles POST /govfilings.
func (h *Handler) CreateFiling(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createFilingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Payload); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "payload: "+err.Error())
		return
	}
	empID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}

	filing, err := h.svc.CreateFiling(c.Request.Context(), CreateFilingInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		EmployeeID:     empID,
		FilingType:     req.FilingType,
		Channel:        req.Channel,
		PayloadJSON:    bytesOrEmptyObject(req.Payload),
		IdempotencyKey: req.IdempotencyKey,
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
	c.JSON(http.StatusCreated, toFilingResponse(filing))
}

// GetFiling handles GET /govfilings/:id.
func (h *Handler) GetFiling(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid filing id")
		return
	}

	filing, err := h.svc.GetFiling(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "filing not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toFilingResponse(filing))
}

// ListFilings handles GET /govfilings (filtered by employee_id query param).
func (h *Handler) ListFilings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Query("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "employee_id query param is required")
		return
	}

	filings, err := h.svc.ListFilings(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]FilingResponse, len(filings))
	for i := range filings {
		items[i] = toFilingResponse(&filings[i])
	}
	c.JSON(http.StatusOK, gin.H{"filings": items})
}

// SubmitFiling handles POST /govfilings/:id/submit.
func (h *Handler) SubmitFiling(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid filing id")
		return
	}

	filing, err := h.svc.SubmitFiling(c.Request.Context(), SubmitFilingInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		IP:       clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "filing not found")
			return
		}
		if errors.Is(err, ErrInvalidTransition) {
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "filing status transition not allowed")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toFilingResponse(filing))
}

// UpdateStatus handles PATCH /govfilings/:id/status.
func (h *Handler) UpdateStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid filing id")
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

	filing, err := h.svc.UpdateStatus(c.Request.Context(), UpdateStatusInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		ID:              id,
		ToStatus:        req.ToStatus,
		Note:            req.Note,
		ExternalMessage: req.ExternalMessage,
		IP:              clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "filing not found")
			return
		}
		if errors.Is(err, ErrInvalidTransition) {
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "filing status transition not allowed")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toFilingResponse(filing))
}

// ---------------------------------------------------------------------------
// Status history handler
// ---------------------------------------------------------------------------

// StatusHistoryResponse is the JSON representation of a status-history row.
type StatusHistoryResponse struct {
	ID              uuid.UUID  `json:"id"`
	FilingID        uuid.UUID  `json:"filing_id"`
	FromStatus      string     `json:"from_status"`
	ToStatus        string     `json:"to_status"`
	Note            *string    `json:"note,omitempty"`
	ExternalMessage *string    `json:"external_message,omitempty"`
	ChangedBy       *uuid.UUID `json:"changed_by,omitempty"`
	ChangedAt       string     `json:"changed_at"`
}

func toStatusHistoryResponse(hst *StatusHistory) StatusHistoryResponse {
	return StatusHistoryResponse{
		ID:              hst.ID,
		FilingID:        hst.FilingID,
		FromStatus:      hst.FromStatus,
		ToStatus:        hst.ToStatus,
		Note:            hst.Note,
		ExternalMessage: hst.ExternalMessage,
		ChangedBy:       hst.ChangedBy,
		ChangedAt:       hst.ChangedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ListStatusHistory handles GET /govfilings/:id/history.
func (h *Handler) ListStatusHistory(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid filing id")
		return
	}

	hist, err := h.svc.ListStatusHistory(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "filing not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	items := make([]StatusHistoryResponse, len(hist))
	for i := range hist {
		items[i] = toStatusHistoryResponse(&hist[i])
	}
	c.JSON(http.StatusOK, gin.H{"history": items})
}

// ---------------------------------------------------------------------------
// Document request / response shapes
// ---------------------------------------------------------------------------

type attachDocumentRequest struct {
	DocKind        string `json:"doc_kind"        validate:"required,oneof=receipt decision return_reason generated_form"`
	Content        string `json:"content"         validate:"omitempty"`
	RetentionLabel string `json:"retention_label" validate:"omitempty,max=50"`
}

// DocumentResponse is the JSON representation of a document (metadata only;
// the encrypted/decrypted content is delivered via a dedicated endpoint).
type DocumentResponse struct {
	ID             uuid.UUID `json:"id"`
	FilingID       uuid.UUID `json:"filing_id"`
	DocKind        string    `json:"doc_kind"`
	RetentionLabel string    `json:"retention_label"`
	// Content is populated only by the sensitive content endpoint.
	Content *string `json:"content,omitempty"`
	// ContentMasked indicates the body exists but is not decrypted for this caller.
	ContentMasked *string `json:"content_masked,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

func toDocumentResponse(d *FilingDocument, plaintext []byte) DocumentResponse {
	r := DocumentResponse{
		ID:             d.ID,
		FilingID:       d.FilingID,
		DocKind:        d.DocKind,
		RetentionLabel: d.RetentionLabel,
		CreatedAt:      d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if len(plaintext) > 0 {
		s := string(plaintext)
		r.Content = &s
	} else {
		masked := "****"
		r.ContentMasked = &masked
	}
	return r
}

// ---------------------------------------------------------------------------
// Document handlers
// ---------------------------------------------------------------------------

// AttachDocument handles POST /govfilings/:id/documents.
func (h *Handler) AttachDocument(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	filingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid filing id")
		return
	}

	var req attachDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if len(req.Content) > maxDocumentBytes {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT",
			fmt.Sprintf("content exceeds maximum size of %d bytes", maxDocumentBytes))
		return
	}

	doc, err := h.svc.AttachDocument(c.Request.Context(), AttachDocumentInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		FilingID:         filingID,
		DocKind:          req.DocKind,
		ContentPlaintext: []byte(req.Content),
		RetentionLabel:   req.RetentionLabel,
		IP:               clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "filing not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusCreated, toDocumentResponse(doc, nil))
}

// ListDocuments handles GET /govfilings/:id/documents.
func (h *Handler) ListDocuments(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	filingID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid filing id")
		return
	}

	docs, err := h.svc.ListDocuments(c.Request.Context(), tenantID, filingID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "filing not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	items := make([]DocumentResponse, len(docs))
	for i := range docs {
		items[i] = toDocumentResponse(&docs[i], nil)
	}
	c.JSON(http.StatusOK, gin.H{"documents": items})
}

// GetDocumentContent handles GET /govfilings/documents/:doc_id/content.
// This route requires filing:read_sensitive (enforced in routes.go); the
// service re-validates the permission and decrypts the body only then.
func (h *Handler) GetDocumentContent(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	docID, err := uuid.Parse(c.Param("doc_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid document id")
		return
	}

	doc, plaintext, err := h.svc.GetDocumentContent(c.Request.Context(), GetDocumentContentInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		DocumentID:    docID,
		ReadSensitive: true, // route enforced filing:read_sensitive
		IP:            clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "document not found")
			return
		}
		if errors.Is(err, ErrForbidden) {
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toDocumentResponse(doc, plaintext))
}
