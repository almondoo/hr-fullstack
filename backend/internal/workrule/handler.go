package workrule

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

// Handler exposes HTTP endpoints for the workrule domain.
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

// respondServiceError maps a service-layer error to the unified HTTP response.
func respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	case errors.Is(err, ErrInvalidInput):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid input")
	default:
		httpx.RespondInternalError(c)
	}
}

func formatTS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func formatTSPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTS(*t)
	return &s
}

func formatDatePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	AgreementAlertLeadDays int             `json:"agreement_alert_lead_days" validate:"min=0,max=3650"`
	RetentionPolicy        string          `json:"retention_policy"          validate:"omitempty,max=50"`
	TemplatesJSON          json.RawMessage `json:"templates_json"`
}

// SettingsResponse is the JSON representation of workrule settings.
type SettingsResponse struct {
	ID                     uuid.UUID       `json:"id"`
	TenantID               uuid.UUID       `json:"tenant_id"`
	AgreementAlertLeadDays int             `json:"agreement_alert_lead_days"`
	RetentionPolicy        string          `json:"retention_policy"`
	TemplatesJSON          json.RawMessage `json:"templates_json"`
	CreatedAt              string          `json:"created_at"`
	UpdatedAt              string          `json:"updated_at"`
}

func toSettingsResponse(s *Settings) SettingsResponse {
	tpl := json.RawMessage(s.TemplatesJSON)
	if len(tpl) == 0 {
		tpl = json.RawMessage(`{}`)
	}
	return SettingsResponse{
		ID:                     s.ID,
		TenantID:               s.TenantID,
		AgreementAlertLeadDays: s.AgreementAlertLeadDays,
		RetentionPolicy:        s.RetentionPolicy,
		TemplatesJSON:          tpl,
		CreatedAt:              formatTS(s.CreatedAt),
		UpdatedAt:              formatTS(s.UpdatedAt),
	}
}

// UpsertSettings handles PUT /workrules/settings.
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
	if err := validateJSON(req.TemplatesJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "templates_json: "+err.Error())
		return
	}
	templates := []byte(req.TemplatesJSON)
	if len(templates) == 0 || string(templates) == "null" {
		templates = []byte(`{}`)
	}

	out, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:               tenantID,
		ActorID:                actorID,
		AgreementAlertLeadDays: req.AgreementAlertLeadDays,
		RetentionPolicy:        req.RetentionPolicy,
		TemplatesJSON:          templates,
		IP:                     clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(out))
}

// GetSettings handles GET /workrules/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	out, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(out))
}

// ---------------------------------------------------------------------------
// Work rules
// ---------------------------------------------------------------------------

type createWorkRuleRequest struct {
	Title           string `json:"title"            validate:"required,max=200"`
	Category        string `json:"category"         validate:"omitempty,oneof=main childcare_caregiving wage safety_health other"`
	RetentionPolicy string `json:"retention_policy" validate:"omitempty,max=50"`
}

// WorkRuleResponse is the JSON representation of a work rule.
type WorkRuleResponse struct { //nolint:revive // name intentionally includes package prefix for API clarity
	ID               uuid.UUID  `json:"id"`
	TenantID         uuid.UUID  `json:"tenant_id"`
	Title            string     `json:"title"`
	Category         string     `json:"category"`
	CurrentVersionID *uuid.UUID `json:"current_version_id,omitempty"`
	RetentionPolicy  string     `json:"retention_policy"`
	CreatedAt        string     `json:"created_at"`
	UpdatedAt        string     `json:"updated_at"`
}

func toWorkRuleResponse(w *WorkRule) WorkRuleResponse {
	return WorkRuleResponse{
		ID:               w.ID,
		TenantID:         w.TenantID,
		Title:            w.Title,
		Category:         w.Category,
		CurrentVersionID: w.CurrentVersionID,
		RetentionPolicy:  w.RetentionPolicy,
		CreatedAt:        formatTS(w.CreatedAt),
		UpdatedAt:        formatTS(w.UpdatedAt),
	}
}

// CreateWorkRule handles POST /workrules.
func (h *Handler) CreateWorkRule(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createWorkRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	wr, err := h.svc.CreateWorkRule(c.Request.Context(), CreateWorkRuleInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		Title:           req.Title,
		Category:        req.Category,
		RetentionPolicy: req.RetentionPolicy,
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toWorkRuleResponse(wr))
}

// ListWorkRules handles GET /workrules.
func (h *Handler) ListWorkRules(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	category := c.Query("category")

	rules, err := h.svc.ListWorkRules(c.Request.Context(), tenantID, category)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]WorkRuleResponse, len(rules))
	for i := range rules {
		items[i] = toWorkRuleResponse(&rules[i])
	}
	c.JSON(http.StatusOK, gin.H{"work_rules": items})
}

// GetWorkRule handles GET /workrules/:id.
func (h *Handler) GetWorkRule(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid work rule id")
		return
	}
	wr, err := h.svc.GetWorkRule(c.Request.Context(), tenantID, id)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toWorkRuleResponse(wr))
}

// ---------------------------------------------------------------------------
// Work rule versions
// ---------------------------------------------------------------------------

type createVersionRequest struct {
	RevisedOn            *string `json:"revised_on"`
	RevisionReason       string  `json:"revision_reason"        validate:"omitempty,max=1000"`
	DocumentRef          *string `json:"document_ref"           validate:"omitempty,max=512"`
	RequiresExpertReview bool    `json:"requires_expert_review"`
}

// VersionResponse is the JSON representation of a work rule version.
type VersionResponse struct {
	ID                   uuid.UUID  `json:"id"`
	TenantID             uuid.UUID  `json:"tenant_id"`
	WorkRuleID           uuid.UUID  `json:"work_rule_id"`
	Version              int        `json:"version"`
	RevisedOn            *string    `json:"revised_on,omitempty"`
	RevisionReason       string     `json:"revision_reason"`
	DocumentRef          *string    `json:"document_ref,omitempty"`
	Status               string     `json:"status"`
	PublishedAt          *string    `json:"published_at,omitempty"`
	RequiresExpertReview bool       `json:"requires_expert_review"`
	CreatedBy            *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt            string     `json:"created_at"`
	UpdatedAt            string     `json:"updated_at"`
}

func toVersionResponse(v *WorkRuleVersion) VersionResponse {
	return VersionResponse{
		ID:                   v.ID,
		TenantID:             v.TenantID,
		WorkRuleID:           v.WorkRuleID,
		Version:              v.Version,
		RevisedOn:            formatDatePtr(v.RevisedOn),
		RevisionReason:       v.RevisionReason,
		DocumentRef:          v.DocumentRef,
		Status:               v.Status,
		PublishedAt:          formatTSPtr(v.PublishedAt),
		RequiresExpertReview: v.RequiresExpertReview,
		CreatedBy:            v.CreatedBy,
		CreatedAt:            formatTS(v.CreatedAt),
		UpdatedAt:            formatTS(v.UpdatedAt),
	}
}

// CreateVersion handles POST /workrules/:id/versions.
func (h *Handler) CreateVersion(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	workRuleID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid work rule id")
		return
	}

	var req createVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	revisedOn, err := parseDate(req.RevisedOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	v, err := h.svc.CreateVersion(c.Request.Context(), CreateVersionInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		WorkRuleID:           workRuleID,
		RevisedOn:            revisedOn,
		RevisionReason:       req.RevisionReason,
		DocumentRef:          req.DocumentRef,
		RequiresExpertReview: req.RequiresExpertReview,
		IP:                   clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toVersionResponse(v))
}

// ListVersions handles GET /workrules/:id/versions.
func (h *Handler) ListVersions(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	workRuleID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid work rule id")
		return
	}
	versions, err := h.svc.ListVersions(c.Request.Context(), tenantID, workRuleID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]VersionResponse, len(versions))
	for i := range versions {
		items[i] = toVersionResponse(&versions[i])
	}
	c.JSON(http.StatusOK, gin.H{"versions": items})
}

// PublishVersion handles POST /workrule-versions/:version_id/publish.
func (h *Handler) PublishVersion(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	versionID, err := uuid.Parse(c.Param("version_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid version id")
		return
	}

	v, err := h.svc.PublishVersion(c.Request.Context(), PublishVersionInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		VersionID: versionID,
		IP:        clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toVersionResponse(v))
}

// ---------------------------------------------------------------------------
// Acknowledgements
// ---------------------------------------------------------------------------

type acknowledgeRequest struct {
	EmployeeID string `json:"employee_id" validate:"required"`
	Consent    string `json:"consent"     validate:"omitempty,oneof=read agreed"`
}

// AcknowledgementResponse is the JSON representation of an acknowledgement.
type AcknowledgementResponse struct {
	ID                uuid.UUID `json:"id"`
	TenantID          uuid.UUID `json:"tenant_id"`
	WorkRuleVersionID uuid.UUID `json:"work_rule_version_id"`
	EmployeeID        uuid.UUID `json:"employee_id"`
	Consent           string    `json:"consent"`
	AcknowledgedAt    string    `json:"acknowledged_at"`
	CreatedAt         string    `json:"created_at"`
	UpdatedAt         string    `json:"updated_at"`
}

func toAcknowledgementResponse(a *Acknowledgement) AcknowledgementResponse {
	return AcknowledgementResponse{
		ID:                a.ID,
		TenantID:          a.TenantID,
		WorkRuleVersionID: a.WorkRuleVersionID,
		EmployeeID:        a.EmployeeID,
		Consent:           a.Consent,
		AcknowledgedAt:    formatTS(a.AcknowledgedAt),
		CreatedAt:         formatTS(a.CreatedAt),
		UpdatedAt:         formatTS(a.UpdatedAt),
	}
}

// Acknowledge handles POST /workrule-versions/:version_id/acknowledge.
func (h *Handler) Acknowledge(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	versionID, err := uuid.Parse(c.Param("version_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid version id")
		return
	}

	var req acknowledgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	employeeID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}

	ack, err := h.svc.Acknowledge(c.Request.Context(), AcknowledgeInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		VersionID:  versionID,
		EmployeeID: employeeID,
		Consent:    req.Consent,
		IP:         clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAcknowledgementResponse(ack))
}

// ListAcknowledgements handles GET /workrule-versions/:version_id/acknowledgements.
func (h *Handler) ListAcknowledgements(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	versionID, err := uuid.Parse(c.Param("version_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid version id")
		return
	}
	acks, err := h.svc.ListAcknowledgements(c.Request.Context(), tenantID, versionID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]AcknowledgementResponse, len(acks))
	for i := range acks {
		items[i] = toAcknowledgementResponse(&acks[i])
	}
	c.JSON(http.StatusOK, gin.H{"acknowledgements": items})
}

// ListUnacknowledged handles GET /workrule-versions/:version_id/unacknowledged.
func (h *Handler) ListUnacknowledged(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	versionID, err := uuid.Parse(c.Param("version_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid version id")
		return
	}
	ids, err := h.svc.ListUnacknowledgedEmployees(c.Request.Context(), tenantID, versionID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"employee_ids": ids})
}

// ---------------------------------------------------------------------------
// Labour agreement documents
// ---------------------------------------------------------------------------

type createAgreementRequest struct {
	Title                  string  `json:"title"                     validate:"required,max=200"`
	AgreementType          string  `json:"agreement_type"            validate:"omitempty,oneof=article36 other"`
	ValidFrom              string  `json:"valid_from"                validate:"required"`
	ValidTo                string  `json:"valid_to"                  validate:"required"`
	LinkedLaborAgreementID *string `json:"linked_labor_agreement_id"` //nolint:misspell // JSON key is API contract (schema uses American spelling)
	DocumentRef            *string `json:"document_ref"              validate:"omitempty,max=512"`
}

type updateFilingStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=draft filed accepted"`
}

// AgreementResponse is the JSON representation of a labour agreement document.
type AgreementResponse struct {
	ID                     uuid.UUID  `json:"id"`
	TenantID               uuid.UUID  `json:"tenant_id"`
	Title                  string     `json:"title"`
	AgreementType          string     `json:"agreement_type"`
	Version                int        `json:"version"`
	ValidFrom              string     `json:"valid_from"`
	ValidTo                string     `json:"valid_to"`
	FilingStatus           string     `json:"filing_status"`
	FiledAt                *string    `json:"filed_at,omitempty"`
	AcceptedAt             *string    `json:"accepted_at,omitempty"`
	DocumentRef            *string    `json:"document_ref,omitempty"`
	LinkedLaborAgreementID *uuid.UUID `json:"linked_labor_agreement_id,omitempty"` //nolint:misspell // JSON key is API contract
	RenewalAlertAt         *string    `json:"renewal_alert_at,omitempty"`
	CreatedBy              *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt              string     `json:"created_at"`
	UpdatedAt              string     `json:"updated_at"`
}

func toAgreementResponse(d *LaborAgreementDocument) AgreementResponse {
	return AgreementResponse{
		ID:                     d.ID,
		TenantID:               d.TenantID,
		Title:                  d.Title,
		AgreementType:          d.AgreementType,
		Version:                d.Version,
		ValidFrom:              d.ValidFrom.Format("2006-01-02"),
		ValidTo:                d.ValidTo.Format("2006-01-02"),
		FilingStatus:           d.FilingStatus,
		FiledAt:                formatTSPtr(d.FiledAt),
		AcceptedAt:             formatTSPtr(d.AcceptedAt),
		DocumentRef:            d.DocumentRef,
		LinkedLaborAgreementID: d.LinkedLaborAgreementID,
		RenewalAlertAt:         formatDatePtr(d.RenewalAlertAt),
		CreatedBy:              d.CreatedBy,
		CreatedAt:              formatTS(d.CreatedAt),
		UpdatedAt:              formatTS(d.UpdatedAt),
	}
}

// CreateAgreement handles POST /labor-agreements.
func (h *Handler) CreateAgreement(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createAgreementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	validFrom, err := time.Parse("2006-01-02", req.ValidFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "valid_from must be YYYY-MM-DD")
		return
	}
	validTo, err := time.Parse("2006-01-02", req.ValidTo)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "valid_to must be YYYY-MM-DD")
		return
	}

	var linkedID *uuid.UUID
	if req.LinkedLaborAgreementID != nil && *req.LinkedLaborAgreementID != "" {
		id, err := uuid.Parse(*req.LinkedLaborAgreementID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid linked_labor_agreement_id") //nolint:misspell // error message references API field name (schema contract)
			return
		}
		linkedID = &id
	}

	doc, err := h.svc.CreateAgreement(c.Request.Context(), CreateAgreementInput{
		TenantID:               tenantID,
		ActorID:                actorID,
		Title:                  req.Title,
		Type:                   req.AgreementType,
		ValidFrom:              validFrom,
		ValidTo:                validTo,
		LinkedLaborAgreementID: linkedID,
		DocumentRef:            req.DocumentRef,
		IP:                     clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAgreementResponse(doc))
}

// ListAgreements handles GET /labor-agreements.
func (h *Handler) ListAgreements(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	agreementType := c.Query("agreement_type")

	docs, err := h.svc.ListAgreements(c.Request.Context(), tenantID, agreementType)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]AgreementResponse, len(docs))
	for i := range docs {
		items[i] = toAgreementResponse(&docs[i])
	}
	c.JSON(http.StatusOK, gin.H{"labor_agreements": items}) //nolint:misspell // JSON key is API contract
}

// GetAgreement handles GET /labor-agreements/:agreement_id.
func (h *Handler) GetAgreement(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("agreement_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agreement id")
		return
	}
	doc, err := h.svc.GetAgreement(c.Request.Context(), tenantID, id)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toAgreementResponse(doc))
}

// UpdateFilingStatus handles PATCH /labor-agreements/:agreement_id/filing-status.
func (h *Handler) UpdateFilingStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("agreement_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agreement id")
		return
	}

	var req updateFilingStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	doc, err := h.svc.UpdateFilingStatus(c.Request.Context(), UpdateFilingStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toAgreementResponse(doc))
}

// ListExpiringAgreements handles GET /labor-agreements/expiring.
// Optional query param as_of=YYYY-MM-DD (default: today).
func (h *Handler) ListExpiringAgreements(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	asOf := time.Now()
	if q := c.Query("as_of"); q != "" {
		t, err := time.Parse("2006-01-02", q)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "as_of must be YYYY-MM-DD")
			return
		}
		asOf = t
	}

	docs, err := h.svc.ListExpiringAgreements(c.Request.Context(), tenantID, asOf)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]AgreementResponse, len(docs))
	for i := range docs {
		items[i] = toAgreementResponse(&docs[i])
	}
	c.JSON(http.StatusOK, gin.H{"labor_agreements": items}) //nolint:misspell // JSON key is API contract
}

// LinkedLimitsResponse is the JSON representation of the linked 36協定 limits.
type LinkedLimitsResponse struct {
	LaborAgreementID           uuid.UUID `json:"labor_agreement_id"` //nolint:misspell // JSON key is API contract
	MonthlyLimitMinutes        int       `json:"monthly_limit_minutes"`
	YearlyLimitMinutes         int       `json:"yearly_limit_minutes"`
	SpecialClause              bool      `json:"special_clause"`
	SpecialMonthlyLimitMinutes *int      `json:"special_monthly_limit_minutes,omitempty"`
}

// GetLinkedLimits handles GET /labor-agreements/:agreement_id/linked-limits.
// Returns the upper-limit values from the linked attendance.labor_agreements row
// (source of truth) without duplicating them in this package.
func (h *Handler) GetLinkedLimits(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("agreement_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agreement id")
		return
	}
	limits, err := h.svc.GetLinkedLimits(c.Request.Context(), tenantID, id)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, LinkedLimitsResponse{
		LaborAgreementID:           limits.LaborAgreementID,
		MonthlyLimitMinutes:        limits.MonthlyLimitMinutes,
		YearlyLimitMinutes:         limits.YearlyLimitMinutes,
		SpecialClause:              limits.SpecialClause,
		SpecialMonthlyLimitMinutes: limits.SpecialMonthlyLimitMinutes,
	})
}
