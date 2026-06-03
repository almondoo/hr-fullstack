package applicant

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

// Handler exposes HTTP endpoints for the applicant domain.
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

// clientIP extracts the client IP from the gin context.
func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// mapServiceError maps a service sentinel error to an HTTP response.
// Returns true when the error was handled.
func mapServiceError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrInvalidMerge):
		httpx.RespondError(c, http.StatusConflict, "INVALID_MERGE", "merge not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	default:
		httpx.RespondInternalError(c)
	}
	return true
}

// ---------------------------------------------------------------------------
// Applicant request / response shapes
// ---------------------------------------------------------------------------

type createApplicantRequest struct {
	JobPostingID       *string `json:"job_posting_id"`
	LastName           string  `json:"last_name"  validate:"required,max=200"`
	FirstName          string  `json:"first_name" validate:"required,max=200"`
	Email              string  `json:"email"      validate:"omitempty,max=320"`
	Phone              string  `json:"phone"      validate:"omitempty,max=50"`
	BirthDate          *string `json:"birth_date"`
	ConsentStatus      string  `json:"consent_status" validate:"omitempty,oneof=granted withdrawn unknown"`
	Source             string  `json:"source"         validate:"omitempty,oneof=direct agent referral job_board other"`
	RetentionLabel     string  `json:"retention_label" validate:"omitempty,max=100"`
	RetentionExpiresOn *string `json:"retention_expires_on"`
}

type updateStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=applied screening interviewing offered hired rejected withdrawn"`
}

// ApplicantResponse is the JSON representation of an applicant.
// Email / Phone are populated only when the caller holds
// ats:applicant:read_sensitive; otherwise they are masked.
type ApplicantResponse struct { //nolint:revive // name intentionally includes package prefix for API clarity
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	JobPostingID       *uuid.UUID `json:"job_posting_id,omitempty"`
	MergedIntoID       *uuid.UUID `json:"merged_into_id,omitempty"`
	LastName           string     `json:"last_name"`
	FirstName          string     `json:"first_name"`
	BirthDate          *string    `json:"birth_date,omitempty"`
	Email              *string    `json:"email,omitempty"`
	Phone              *string    `json:"phone,omitempty"`
	ContactMasked      *string    `json:"contact_masked,omitempty"`
	Status             string     `json:"status"`
	ConsentStatus      string     `json:"consent_status"`
	Source             string     `json:"source"`
	RetentionLabel     string     `json:"retention_label"`
	RetentionExpiresOn *string    `json:"retention_expires_on,omitempty"`
	AnonymizedAt       *string    `json:"anonymized_at,omitempty"`
	CreatedAt          string     `json:"created_at"`
	UpdatedAt          string     `json:"updated_at"`
}

func toApplicantResponse(a *Applicant, sc *SensitiveContact) ApplicantResponse {
	r := ApplicantResponse{
		ID:             a.ID,
		TenantID:       a.TenantID,
		JobPostingID:   a.JobPostingID,
		MergedIntoID:   a.MergedIntoID,
		LastName:       a.LastName,
		FirstName:      a.FirstName,
		Status:         a.Status,
		ConsentStatus:  a.ConsentStatus,
		Source:         a.Source,
		RetentionLabel: a.RetentionLabel,
		CreatedAt:      a.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      a.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if a.BirthDate != nil {
		s := a.BirthDate.Format("2006-01-02")
		r.BirthDate = &s
	}
	if a.RetentionExpiresOn != nil {
		s := a.RetentionExpiresOn.Format("2006-01-02")
		r.RetentionExpiresOn = &s
	}
	if a.AnonymizedAt != nil {
		s := a.AnonymizedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.AnonymizedAt = &s
	}
	if sc != nil {
		email := sc.Email
		phone := sc.Phone
		r.Email = &email
		r.Phone = &phone
	} else {
		masked := "****"
		r.ContactMasked = &masked
	}
	return r
}

// ---------------------------------------------------------------------------
// Applicant handlers
// ---------------------------------------------------------------------------

// CreateApplicant handles POST /applicants.
func (h *Handler) CreateApplicant(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createApplicantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	var jobPostingID *uuid.UUID
	if req.JobPostingID != nil && *req.JobPostingID != "" {
		id, err := uuid.Parse(*req.JobPostingID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid job_posting_id")
			return
		}
		jobPostingID = &id
	}

	birthDate, err := parseDate(req.BirthDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	expiresOn, err := parseDate(req.RetentionExpiresOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	a, err := h.svc.CreateApplicant(c.Request.Context(), CreateApplicantInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		JobPostingID:       jobPostingID,
		LastName:           req.LastName,
		FirstName:          req.FirstName,
		EmailPlaintext:     req.Email,
		PhonePlaintext:     req.Phone,
		BirthDate:          birthDate,
		ConsentStatus:      req.ConsentStatus,
		Source:             req.Source,
		RetentionLabel:     req.RetentionLabel,
		RetentionExpiresOn: expiresOn,
		IP:                 clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toApplicantResponse(a, nil))
}

// ListApplicants handles GET /applicants.
func (h *Handler) ListApplicants(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	var jobPostingID *uuid.UUID
	if v := c.Query("job_posting_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid job_posting_id")
			return
		}
		jobPostingID = &id
	}
	status := c.Query("status")
	includeMerged := c.Query("include_merged") == "true"

	list, err := h.svc.ListApplicants(c.Request.Context(), tenantID, jobPostingID, status, includeMerged)
	if mapServiceError(c, err) {
		return
	}
	items := make([]ApplicantResponse, len(list))
	for i := range list {
		items[i] = toApplicantResponse(&list[i], nil)
	}
	c.JSON(http.StatusOK, gin.H{"applicants": items})
}

// GetApplicant handles GET /applicants/:id (masked contact).
func (h *Handler) GetApplicant(c *gin.Context) {
	h.getApplicant(c, false)
}

// GetApplicantSensitive handles GET /applicants/:id/sensitive.
// The route requires ats:applicant:read_sensitive (enforced in routes.go);
// the service layer re-validates the permission before decrypting.
func (h *Handler) GetApplicantSensitive(c *gin.Context) {
	h.getApplicant(c, true)
}

func (h *Handler) getApplicant(c *gin.Context, readSensitive bool) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	a, sc, err := h.svc.GetApplicant(c.Request.Context(), GetApplicantInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		ID:            id,
		ReadSensitive: readSensitive,
		IP:            clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toApplicantResponse(a, sc))
}

// UpdateStatus handles PATCH /applicants/:id/status.
func (h *Handler) UpdateStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
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

	a, err := h.svc.UpdateStatus(c.Request.Context(), UpdateStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toApplicantResponse(a, nil))
}

// Anonymize handles POST /applicants/:id/anonymize.
func (h *Handler) Anonymize(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	if err := h.svc.Anonymize(c.Request.Context(), AnonymizeInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ID:       id,
		IP:       clientIP(c),
	}); mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "applicant anonymized"})
}

// ---------------------------------------------------------------------------
// Document request / response shapes
// ---------------------------------------------------------------------------

type addDocumentRequest struct {
	DocType  string `json:"doc_type" validate:"omitempty,oneof=resume cv portfolio other"`
	FileRef  string `json:"file_ref" validate:"required,max=512"`
	FileName string `json:"file_name" validate:"omitempty,max=512"`
}

// DocumentResponse is the JSON representation of a document reference.
type DocumentResponse struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	ApplicantID uuid.UUID `json:"applicant_id"`
	DocType     string    `json:"doc_type"`
	FileRef     string    `json:"file_ref"`
	FileName    string    `json:"file_name"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

func toDocumentResponse(d *Document) DocumentResponse {
	return DocumentResponse{
		ID:          d.ID,
		TenantID:    d.TenantID,
		ApplicantID: d.ApplicantID,
		DocType:     d.DocType,
		FileRef:     d.FileRef,
		FileName:    d.FileName,
		CreatedAt:   d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// AddDocument handles POST /applicants/:id/documents.
func (h *Handler) AddDocument(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	applicantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	var req addDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	d, err := h.svc.AddDocument(c.Request.Context(), AddDocumentInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ApplicantID: applicantID,
		DocType:     req.DocType,
		FileRef:     req.FileRef,
		FileName:    req.FileName,
		IP:          clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toDocumentResponse(d))
}

// ListDocuments handles GET /applicants/:id/documents.
func (h *Handler) ListDocuments(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	applicantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	docs, err := h.svc.ListDocuments(c.Request.Context(), tenantID, applicantID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]DocumentResponse, len(docs))
	for i := range docs {
		items[i] = toDocumentResponse(&docs[i])
	}
	c.JSON(http.StatusOK, gin.H{"documents": items})
}

// ---------------------------------------------------------------------------
// Consent request / response shapes
// ---------------------------------------------------------------------------

type recordConsentRequest struct {
	Purpose     string `json:"purpose" validate:"required,max=200"`
	Granted     *bool  `json:"granted" validate:"required"`
	CrossBorder bool   `json:"cross_border"`
}

// ConsentResponse is the JSON representation of a consent record.
type ConsentResponse struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	ApplicantID uuid.UUID `json:"applicant_id"`
	Purpose     string    `json:"purpose"`
	GrantedAt   *string   `json:"granted_at,omitempty"`
	WithdrawnAt *string   `json:"withdrawn_at,omitempty"`
	CrossBorder bool      `json:"cross_border"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

func toConsentResponse(c *Consent) ConsentResponse {
	r := ConsentResponse{
		ID:          c.ID,
		TenantID:    c.TenantID,
		ApplicantID: c.ApplicantID,
		Purpose:     c.Purpose,
		CrossBorder: c.CrossBorder,
		CreatedAt:   c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   c.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if c.GrantedAt != nil {
		s := c.GrantedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.GrantedAt = &s
	}
	if c.WithdrawnAt != nil {
		s := c.WithdrawnAt.UTC().Format("2006-01-02T15:04:05Z")
		r.WithdrawnAt = &s
	}
	return r
}

// RecordConsent handles POST /applicants/:id/consents.
func (h *Handler) RecordConsent(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	applicantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	var req recordConsentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	consent, err := h.svc.RecordConsent(c.Request.Context(), RecordConsentInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ApplicantID: applicantID,
		Purpose:     req.Purpose,
		Granted:     *req.Granted,
		CrossBorder: req.CrossBorder,
		IP:          clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toConsentResponse(consent))
}

// ListConsents handles GET /applicants/:id/consents.
func (h *Handler) ListConsents(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	applicantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	consents, err := h.svc.ListConsents(c.Request.Context(), tenantID, applicantID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]ConsentResponse, len(consents))
	for i := range consents {
		items[i] = toConsentResponse(&consents[i])
	}
	c.JSON(http.StatusOK, gin.H{"consents": items})
}

// ---------------------------------------------------------------------------
// Duplicate detection / merge request / response shapes
// ---------------------------------------------------------------------------

type mergeRequest struct {
	SourceID string  `json:"source_id" validate:"required"`
	Notes    *string `json:"notes"`
}

// MergeResponse is the JSON representation of a merge event.
type MergeResponse struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	SourceApplicantID uuid.UUID  `json:"source_applicant_id"`
	TargetApplicantID uuid.UUID  `json:"target_applicant_id"`
	MergedBy          *uuid.UUID `json:"merged_by,omitempty"`
	MergedAt          string     `json:"merged_at"`
	Notes             *string    `json:"notes,omitempty"`
}

func toMergeResponse(m *Merge) MergeResponse {
	return MergeResponse{
		ID:                m.ID,
		TenantID:          m.TenantID,
		SourceApplicantID: m.SourceApplicantID,
		TargetApplicantID: m.TargetApplicantID,
		MergedBy:          m.MergedBy,
		MergedAt:          m.MergedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Notes:             m.Notes,
	}
}

// FindDuplicates handles GET /applicants/:id/duplicates.
func (h *Handler) FindDuplicates(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	candidates, err := h.svc.FindDuplicates(c.Request.Context(), tenantID, id)
	if mapServiceError(c, err) {
		return
	}
	items := make([]ApplicantResponse, len(candidates))
	for i := range candidates {
		items[i] = toApplicantResponse(&candidates[i], nil)
	}
	c.JSON(http.StatusOK, gin.H{"duplicates": items})
}

// Merge handles POST /applicants/:id/merge.
// The path :id is the TARGET (surviving) applicant; the body carries the
// source_id (the duplicate to retire).
func (h *Handler) Merge(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	var req mergeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	sourceID, err := uuid.Parse(req.SourceID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid source_id")
		return
	}

	m, err := h.svc.Merge(c.Request.Context(), MergeInput{
		TenantID: tenantID,
		ActorID:  actorID,
		SourceID: sourceID,
		TargetID: targetID,
		Notes:    req.Notes,
		IP:       clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toMergeResponse(m))
}

// ListMerges handles GET /applicants/:id/merges (history for a target).
func (h *Handler) ListMerges(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	targetID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid applicant id")
		return
	}

	merges, err := h.svc.ListMerges(c.Request.Context(), tenantID, &targetID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]MergeResponse, len(merges))
	for i := range merges {
		items[i] = toMergeResponse(&merges[i])
	}
	c.JSON(http.StatusOK, gin.H{"merges": items})
}
