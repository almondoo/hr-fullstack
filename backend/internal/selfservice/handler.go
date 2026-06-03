package selfservice

import (
	"encoding/base64"
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

// maxBodyBytes caps the overall request body (CSV / document uploads included).
const maxBodyBytes = 12 * 1024 * 1024 // 12 MB

// Handler exposes HTTP endpoints for the self-service / CSV / document domains.
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

// clientIP extracts the client IP from the gin context.
func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
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

// mapServiceError converts a service sentinel error to an HTTP response.
// Returns true when it handled (responded to) the error.
func mapServiceError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "invalid status transition")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	case errors.Is(err, ErrLegalHold):
		httpx.RespondError(c, http.StatusConflict, "LEGAL_HOLD", "document is under legal hold")
	case errors.Is(err, ErrValidation):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
	default:
		httpx.RespondInternalError(c)
	}
	return true
}

// ---------------------------------------------------------------------------
// Change request DTOs
// ---------------------------------------------------------------------------

type submitChangeRequest struct {
	EmployeeID string          `json:"employee_id" validate:"required"`
	TargetType string          `json:"target_type" validate:"required,oneof=employee_profile emergency_contact commute bank_account dependents"`
	Changes    json.RawMessage `json:"changes"`
	// SensitiveBase64 carries sensitive change values base64-encoded; they are
	// encrypted at rest and never returned in the clear without permission.
	SensitiveBase64 string `json:"sensitive_base64" validate:"omitempty"`
}

// ChangeRequestResponse is the JSON representation of a change request.
type ChangeRequestResponse struct {
	ID                uuid.UUID       `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	EmployeeID        uuid.UUID       `json:"employee_id"`
	RequestedByUserID uuid.UUID       `json:"requested_by_user_id"`
	TargetType        string          `json:"target_type"`
	Changes           json.RawMessage `json:"changes"`
	ApprovalRequestID *uuid.UUID      `json:"approval_request_id,omitempty"`
	Status            string          `json:"status"`
	ReflectedAt       *string         `json:"reflected_at,omitempty"`
	// Sensitive is only populated when the caller holds selfservice:read_sensitive.
	Sensitive *string `json:"sensitive,omitempty"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

func toChangeRequestResponse(r *ChangeRequest, sensitivePlaintext []byte) ChangeRequestResponse {
	changes := json.RawMessage(r.ChangesJSON)
	if len(changes) == 0 {
		changes = json.RawMessage(`{}`)
	}
	resp := ChangeRequestResponse{
		ID:                r.ID,
		TenantID:          r.TenantID,
		EmployeeID:        r.EmployeeID,
		RequestedByUserID: r.RequestedByUserID,
		TargetType:        r.TargetType,
		Changes:           changes,
		ApprovalRequestID: r.ApprovalRequestID,
		Status:            r.Status,
		CreatedAt:         r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.ReflectedAt != nil {
		s := r.ReflectedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.ReflectedAt = &s
	}
	if len(sensitivePlaintext) > 0 {
		s := base64.StdEncoding.EncodeToString(sensitivePlaintext)
		resp.Sensitive = &s
	}
	return resp
}

// SubmitChangeRequest handles POST /selfservice/change-requests.
func (h *Handler) SubmitChangeRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req submitChangeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Changes); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "changes: "+err.Error())
		return
	}
	empID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}

	var sensitive []byte
	if req.SensitiveBase64 != "" {
		sensitive, err = base64.StdEncoding.DecodeString(req.SensitiveBase64)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "sensitive_base64: invalid base64")
			return
		}
	}

	changes := []byte(req.Changes)
	if len(changes) == 0 || string(changes) == "null" {
		changes = []byte(`{}`)
	}

	out, err := h.svc.SubmitChangeRequest(c.Request.Context(), SubmitChangeRequestInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		EmployeeID:         empID,
		TargetType:         req.TargetType,
		ChangesJSON:        changes,
		SensitivePlaintext: sensitive,
		IP:                 clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toChangeRequestResponse(out, nil))
}

// ApproveChangeRequest handles POST /selfservice/change-requests/:id/approve.
func (h *Handler) ApproveChangeRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid id")
		return
	}

	out, err := h.svc.ApproveChangeRequest(c.Request.Context(), ApproveChangeRequestInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		RequestID: id,
		IP:        clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toChangeRequestResponse(out, nil))
}

// RejectChangeRequest handles POST /selfservice/change-requests/:id/reject.
func (h *Handler) RejectChangeRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid id")
		return
	}

	out, err := h.svc.RejectChangeRequest(c.Request.Context(), RejectChangeRequestInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		RequestID: id,
		IP:        clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toChangeRequestResponse(out, nil))
}

// GetChangeRequest handles GET /selfservice/change-requests/:id.
func (h *Handler) GetChangeRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid id")
		return
	}

	readSensitive := false
	if v, ok := c.Get("selfservice_read_sensitive"); ok {
		if b, ok := v.(bool); ok {
			readSensitive = b
		}
	}

	out, sensitive, err := h.svc.GetChangeRequest(c.Request.Context(), GetChangeRequestInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		RequestID:     id,
		ReadSensitive: readSensitive,
		IP:            clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toChangeRequestResponse(out, sensitive))
}

// GetChangeRequestSensitive handles GET /selfservice/change-requests/:id/sensitive.
func (h *Handler) GetChangeRequestSensitive(c *gin.Context) {
	c.Set("selfservice_read_sensitive", true)
	h.GetChangeRequest(c)
}

// ListChangeRequests handles GET /selfservice/change-requests.
func (h *Handler) ListChangeRequests(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	var employeeID *uuid.UUID
	if v := c.Query("employee_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
			return
		}
		employeeID = &id
	}
	status := c.Query("status")

	reqs, err := h.svc.ListChangeRequests(c.Request.Context(), tenantID, employeeID, status)
	if mapServiceError(c, err) {
		return
	}
	items := make([]ChangeRequestResponse, len(reqs))
	for i := range reqs {
		items[i] = toChangeRequestResponse(&reqs[i], nil)
	}
	c.JSON(http.StatusOK, gin.H{"change_requests": items})
}

// ---------------------------------------------------------------------------
// CSV import DTOs
// ---------------------------------------------------------------------------

type csvImportRequest struct {
	ImportType string `json:"import_type" validate:"required,oneof=employees departments"`
	Encoding   string `json:"encoding" validate:"omitempty,oneof=utf-8 shift_jis"`
	// ApplyPolicy is only meaningful for the apply endpoint.
	ApplyPolicy string `json:"apply_policy" validate:"omitempty,oneof=all_or_nothing skip_errors"`
	// CSVBase64 is the base64-encoded CSV file content in the declared encoding.
	CSVBase64 string `json:"csv_base64" validate:"required"`
}

// ImportResultResponse is the JSON representation of a CSV import result.
type ImportResultResponse struct {
	JobID       uuid.UUID  `json:"job_id"`
	ImportType  string     `json:"import_type"`
	Mode        string     `json:"mode"`
	Status      string     `json:"status"`
	TotalRows   int        `json:"total_rows"`
	ValidRows   int        `json:"valid_rows"`
	ErrorRows   int        `json:"error_rows"`
	AppliedRows int        `json:"applied_rows"`
	RowErrors   []RowError `json:"row_errors"`
}

func toImportResultResponse(r *ImportResult) ImportResultResponse {
	errs := r.RowErrors
	if errs == nil {
		errs = []RowError{}
	}
	return ImportResultResponse{
		JobID:       r.Job.ID,
		ImportType:  r.Job.ImportType,
		Mode:        r.Job.Mode,
		Status:      r.Job.Status,
		TotalRows:   r.TotalRows,
		ValidRows:   r.ValidRows,
		ErrorRows:   r.ErrorRows,
		AppliedRows: r.AppliedRows,
		RowErrors:   errs,
	}
}

// ValidateCSV handles POST /selfservice/imports/validate (dry-run).
func (h *Handler) ValidateCSV(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	req, csvData, ok := h.bindCSVRequest(c)
	if !ok {
		return
	}

	out, err := h.svc.ValidateCSV(c.Request.Context(), ValidateCSVInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		ImportType: req.ImportType,
		Encoding:   req.Encoding,
		CSVData:    csvData,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toImportResultResponse(out))
}

// ApplyCSV handles POST /selfservice/imports/apply.
func (h *Handler) ApplyCSV(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	req, csvData, ok := h.bindCSVRequest(c)
	if !ok {
		return
	}

	out, err := h.svc.ApplyCSV(c.Request.Context(), ApplyCSVInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		ImportType:  req.ImportType,
		Encoding:    req.Encoding,
		ApplyPolicy: req.ApplyPolicy,
		CSVData:     csvData,
		IP:          clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toImportResultResponse(out))
}

func (h *Handler) bindCSVRequest(c *gin.Context) (csvImportRequest, []byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	var req csvImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return req, nil, false
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return req, nil, false
	}
	csvData, err := base64.StdEncoding.DecodeString(req.CSVBase64)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "csv_base64: invalid base64")
		return req, nil, false
	}
	return req, csvData, true
}

// ---------------------------------------------------------------------------
// Document DTOs
// ---------------------------------------------------------------------------

type createDocumentRequest struct {
	OwnerEmployeeID    *string `json:"owner_employee_id"`
	Category           string  `json:"category" validate:"required,oneof=contract certificate payslip misc"`
	Title              string  `json:"title" validate:"required,max=300"`
	RetentionLabel     string  `json:"retention_label" validate:"omitempty,max=50"`
	RetentionExpiresOn *string `json:"retention_expires_on"`
	LegalHold          bool    `json:"legal_hold"`
	StorageKey         string  `json:"storage_key" validate:"omitempty,max=500"`
	Filename           string  `json:"filename" validate:"omitempty,max=300"`
	MimeType           string  `json:"mime_type" validate:"omitempty,max=120"`
	EncKeyRef          string  `json:"enc_key_ref" validate:"omitempty,max=200"`
	ContentBase64      string  `json:"content_base64"`
}

type addVersionRequest struct {
	StorageKey    string `json:"storage_key" validate:"omitempty,max=500"`
	Filename      string `json:"filename" validate:"omitempty,max=300"`
	MimeType      string `json:"mime_type" validate:"omitempty,max=120"`
	EncKeyRef     string `json:"enc_key_ref" validate:"omitempty,max=200"`
	ContentBase64 string `json:"content_base64"`
}

// DocumentResponse is the JSON representation of a document.
type DocumentResponse struct {
	ID                 uuid.UUID  `json:"id"`
	TenantID           uuid.UUID  `json:"tenant_id"`
	OwnerEmployeeID    *uuid.UUID `json:"owner_employee_id,omitempty"`
	Category           string     `json:"category"`
	Title              string     `json:"title"`
	CurrentVersionID   *uuid.UUID `json:"current_version_id,omitempty"`
	RetentionLabel     string     `json:"retention_label"`
	RetentionExpiresOn *string    `json:"retention_expires_on,omitempty"`
	LogicallyExpired   bool       `json:"logically_expired"`
	LegalHold          bool       `json:"legal_hold"`
	CreatedAt          string     `json:"created_at"`
	UpdatedAt          string     `json:"updated_at"`
}

func toDocumentResponse(d *Document) DocumentResponse {
	r := DocumentResponse{
		ID:               d.ID,
		TenantID:         d.TenantID,
		OwnerEmployeeID:  d.OwnerEmployeeID,
		Category:         d.Category,
		Title:            d.Title,
		CurrentVersionID: d.CurrentVersionID,
		RetentionLabel:   d.RetentionLabel,
		LogicallyExpired: d.LogicallyExpired,
		LegalHold:        d.LegalHold,
		CreatedAt:        d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:        d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if d.RetentionExpiresOn != nil {
		s := d.RetentionExpiresOn.Format("2006-01-02")
		r.RetentionExpiresOn = &s
	}
	return r
}

// DocumentVersionResponse is the JSON representation of a document version.
// ContentEnc is never exposed.
type DocumentVersionResponse struct {
	ID               uuid.UUID `json:"id"`
	DocumentID       uuid.UUID `json:"document_id"`
	VersionNo        int       `json:"version_no"`
	StorageKey       string    `json:"storage_key"`
	ContentHash      string    `json:"content_hash"`
	MimeType         string    `json:"mime_type"`
	SizeBytes        int64     `json:"size_bytes"`
	EncKeyRef        string    `json:"enc_key_ref"`
	UploadedByUserID uuid.UUID `json:"uploaded_by_user_id"`
	UploadedAt       string    `json:"uploaded_at"`
}

func toDocumentVersionResponse(v *DocumentVersion) DocumentVersionResponse {
	return DocumentVersionResponse{
		ID:               v.ID,
		DocumentID:       v.DocumentID,
		VersionNo:        v.VersionNo,
		StorageKey:       v.StorageKey,
		ContentHash:      v.ContentHash,
		MimeType:         v.MimeType,
		SizeBytes:        v.SizeBytes,
		EncKeyRef:        v.EncKeyRef,
		UploadedByUserID: v.UploadedByUserID,
		UploadedAt:       v.UploadedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// CreateDocument handles POST /selfservice/documents.
func (h *Handler) CreateDocument(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	var req createDocumentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	var ownerEmp *uuid.UUID
	if req.OwnerEmployeeID != nil && *req.OwnerEmployeeID != "" {
		id, err := uuid.Parse(*req.OwnerEmployeeID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid owner_employee_id")
			return
		}
		ownerEmp = &id
	}

	expiresOn, err := parseDate(req.RetentionExpiresOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	var content []byte
	if req.ContentBase64 != "" {
		content, err = base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "content_base64: invalid base64")
			return
		}
	}

	doc, ver, err := h.svc.CreateDocument(c.Request.Context(), CreateDocumentInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		OwnerEmployeeID:    ownerEmp,
		Category:           req.Category,
		Title:              req.Title,
		RetentionLabel:     req.RetentionLabel,
		RetentionExpiresOn: expiresOn,
		LegalHold:          req.LegalHold,
		StorageKey:         req.StorageKey,
		Filename:           req.Filename,
		MimeType:           req.MimeType,
		EncKeyRef:          req.EncKeyRef,
		Content:            content,
		IP:                 clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"document": toDocumentResponse(doc),
		"version":  toDocumentVersionResponse(ver),
	})
}

// AddVersion handles POST /selfservice/documents/:id/versions.
func (h *Handler) AddVersion(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	docID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid document id")
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	var req addVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	var content []byte
	if req.ContentBase64 != "" {
		content, err = base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "content_base64: invalid base64")
			return
		}
	}

	ver, err := h.svc.AddVersion(c.Request.Context(), AddVersionInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		DocumentID: docID,
		StorageKey: req.StorageKey,
		Filename:   req.Filename,
		MimeType:   req.MimeType,
		EncKeyRef:  req.EncKeyRef,
		Content:    content,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toDocumentVersionResponse(ver))
}

// GetDocument handles GET /selfservice/documents/:id.
func (h *Handler) GetDocument(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	docID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid document id")
		return
	}

	doc, err := h.svc.GetDocument(c.Request.Context(), tenantID, docID)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toDocumentResponse(doc))
}

// ListDocuments handles GET /selfservice/documents.
func (h *Handler) ListDocuments(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	category := c.Query("category")
	includeExpired := c.Query("include_expired") == "true"

	docs, err := h.svc.ListDocuments(c.Request.Context(), tenantID, category, includeExpired)
	if mapServiceError(c, err) {
		return
	}
	items := make([]DocumentResponse, len(docs))
	for i := range docs {
		items[i] = toDocumentResponse(&docs[i])
	}
	c.JSON(http.StatusOK, gin.H{"documents": items})
}

// ListVersions handles GET /selfservice/documents/:id/versions.
func (h *Handler) ListVersions(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	docID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid document id")
		return
	}

	vers, err := h.svc.ListVersions(c.Request.Context(), tenantID, docID)
	if mapServiceError(c, err) {
		return
	}
	items := make([]DocumentVersionResponse, len(vers))
	for i := range vers {
		items[i] = toDocumentVersionResponse(&vers[i])
	}
	c.JSON(http.StatusOK, gin.H{"versions": items})
}

// DownloadVersion handles GET /selfservice/document-versions/:version_id/download.
func (h *Handler) DownloadVersion(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	verID, err := uuid.Parse(c.Param("version_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid version id")
		return
	}

	out, err := h.svc.DownloadVersion(c.Request.Context(), DownloadVersionInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		VersionID: verID,
		IP:        clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}

	resp := gin.H{
		"version":       toDocumentVersionResponse(&out.Version),
		"hash_verified": out.HashVerified,
	}
	if len(out.Content) > 0 {
		resp["content_base64"] = base64.StdEncoding.EncodeToString(out.Content)
	}
	c.JSON(http.StatusOK, resp)
}

// ExpireDocument handles POST /selfservice/documents/:id/expire.
func (h *Handler) ExpireDocument(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	docID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid document id")
		return
	}

	doc, err := h.svc.ExpireDocument(c.Request.Context(), ExpireDocumentInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		DocumentID: docID,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toDocumentResponse(doc))
}
