package mynumber

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

// maxJSONBytes is the maximum accepted request body size for JSON fields.
const maxJSONBytes = 64 * 1024 // 64 KB

// Handler exposes HTTP endpoints for the mynumber domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts validator errors into a safe message that does not
// expose struct internals.
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

// respondServiceError maps a service error to an HTTP response.
// The 個人番号 plaintext is NEVER included in any error response.
func respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "mynumber record not found")
	case errors.Is(err, ErrDisposed):
		httpx.RespondError(c, http.StatusConflict, "DISPOSED", "mynumber record has been disposed")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "invalid status transition")
	case errors.Is(err, ErrPurposeNotAllowed):
		httpx.RespondError(c, http.StatusForbidden, "PURPOSE_NOT_ALLOWED", "requested purpose is not permitted for this record")
	case errors.Is(err, ErrInvalidPurpose):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_PURPOSE", "unknown use purpose")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Request / response shapes
// ---------------------------------------------------------------------------

type collectRequest struct {
	EmployeeID   string  `json:"employee_id"   validate:"required,uuid"`
	SubjectType  string  `json:"subject_type"  validate:"required,oneof=self dependent"`
	DependentRef *string `json:"dependent_ref" validate:"omitempty,uuid"`
	// Number is the 個人番号 plaintext.  Encrypted before storage and NEVER
	// persisted as plaintext.  Bounded length to limit ciphertext size.
	Number         string   `json:"number"          validate:"required,max=64"`
	Purposes       []string `json:"purposes"        validate:"required,min=1,dive,oneof=payroll social_insurance tax"`
	RetentionUntil *string  `json:"retention_until" validate:"omitempty"`
}

type revealRequest struct {
	Purpose string `json:"purpose" validate:"required,oneof=payroll social_insurance tax"`
}

type provideRequest struct {
	Purpose    string `json:"purpose"     validate:"required,oneof=payroll social_insurance tax"`
	ProvidedTo string `json:"provided_to" validate:"required,max=200"`
}

type disposeRequest struct {
	Reason         string  `json:"reason"          validate:"required,oneof=retention_expired resignation manual"`
	Method         string  `json:"method"          validate:"required,oneof=ciphertext_deleted key_destroyed"`
	CertificateRef *string `json:"certificate_ref" validate:"omitempty,max=200"`
}

// RecordResponse is the JSON representation of a record's metadata.
// It NEVER includes the 個人番号 value or ciphertext.
type RecordResponse struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenant_id"`
	EmployeeID     uuid.UUID  `json:"employee_id"`
	SubjectType    string     `json:"subject_type"`
	DependentRef   *uuid.UUID `json:"dependent_ref,omitempty"`
	Status         string     `json:"status"`
	CollectedAt    string     `json:"collected_at"`
	RetentionUntil *string    `json:"retention_until,omitempty"`
	DisposedAt     *string    `json:"disposed_at,omitempty"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
}

func toRecordResponse(r *Record) RecordResponse {
	resp := RecordResponse{
		ID:           r.ID,
		TenantID:     r.TenantID,
		EmployeeID:   r.EmployeeID,
		SubjectType:  r.SubjectType,
		DependentRef: r.DependentRef,
		Status:       r.Status,
		CollectedAt:  r.CollectedAt.UTC().Format("2006-01-02T15:04:05Z"),
		CreatedAt:    r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.RetentionUntil != nil {
		s := r.RetentionUntil.UTC().Format("2006-01-02T15:04:05Z")
		resp.RetentionUntil = &s
	}
	if r.DisposedAt != nil {
		s := r.DisposedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.DisposedAt = &s
	}
	return resp
}

// AccessLogResponse is the JSON representation of an access-log entry.
// It NEVER includes the 個人番号 value.
type AccessLogResponse struct {
	ID             uuid.UUID  `json:"id"`
	TargetRecordID uuid.UUID  `json:"target_record_id"`
	ActorUserID    *uuid.UUID `json:"actor_user_id,omitempty"`
	Action         string     `json:"action"`
	Purpose        string     `json:"purpose"`
	ProvidedTo     *string    `json:"provided_to,omitempty"`
	OccurredAt     string     `json:"occurred_at"`
	Seq            int64      `json:"seq"`
}

func toAccessLogResponse(l *AccessLog) AccessLogResponse {
	return AccessLogResponse{
		ID:             l.ID,
		TargetRecordID: l.TargetRecordID,
		ActorUserID:    l.ActorUserID,
		Action:         l.Action,
		Purpose:        l.Purpose,
		ProvidedTo:     l.ProvidedTo,
		OccurredAt:     l.OccurredAt.UTC().Format("2006-01-02T15:04:05Z"),
		Seq:            l.Seq,
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// Collect handles POST /mynumbers.
func (h *Handler) Collect(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	if c.Request.ContentLength > maxJSONBytes {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "request body too large")
		return
	}

	var req collectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	empID, err := uuid.Parse(req.EmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}

	var dependentRef *uuid.UUID
	if req.DependentRef != nil && *req.DependentRef != "" {
		id, err := uuid.Parse(*req.DependentRef)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid dependent_ref")
			return
		}
		dependentRef = &id
	}

	retentionUntil, err := parseDate(req.RetentionUntil)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	rec, err := h.svc.Collect(c.Request.Context(), CollectInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		SubjectType:     req.SubjectType,
		DependentRef:    dependentRef,
		NumberPlaintext: []byte(req.Number),
		Purposes:        req.Purposes,
		RetentionUntil:  retentionUntil,
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toRecordResponse(rec))
}

// GetRecord handles GET /mynumbers/:id.  Returns metadata only (no number).
func (h *Handler) GetRecord(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid record id")
		return
	}

	rec, err := h.svc.GetRecord(c.Request.Context(), tenantID, actorID, id)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toRecordResponse(rec))
}

// ListRecords handles GET /employees/:id/mynumbers.  Metadata only.
func (h *Handler) ListRecords(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	recs, err := h.svc.ListRecords(c.Request.Context(), tenantID, actorID, empID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]RecordResponse, len(recs))
	for i := range recs {
		items[i] = toRecordResponse(&recs[i])
	}
	c.JSON(http.StatusOK, gin.H{"records": items})
}

// Reveal handles POST /mynumbers/:id/reveal.
// Requires the dedicated mynumber:reveal permission (enforced at the route AND
// re-validated in the service layer) plus a registered purpose.
func (h *Handler) Reveal(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid record id")
		return
	}

	var req revealRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	plain, err := h.svc.Reveal(c.Request.Context(), RevealInput{
		TenantID: tenantID,
		ActorID:  actorID,
		RecordID: id,
		Purpose:  req.Purpose,
		IP:       clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	// The number value is returned only in the success response body to the
	// authorised caller; it is never logged.
	c.JSON(http.StatusOK, gin.H{"number": string(plain), "purpose": req.Purpose})
}

// Provide handles POST /mynumbers/:id/provide.
func (h *Handler) Provide(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid record id")
		return
	}

	var req provideRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	plain, err := h.svc.Provide(c.Request.Context(), ProvideInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		RecordID:   id,
		Purpose:    req.Purpose,
		ProvidedTo: req.ProvidedTo,
		IP:         clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"number": string(plain), "purpose": req.Purpose, "provided_to": req.ProvidedTo})
}

// Dispose handles POST /mynumbers/:id/dispose.
func (h *Handler) Dispose(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid record id")
		return
	}

	var req disposeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	disposal, err := h.svc.Dispose(c.Request.Context(), DisposeInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		RecordID:       id,
		Reason:         req.Reason,
		Method:         req.Method,
		CertificateRef: req.CertificateRef,
		IP:             clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"disposal_id": disposal.ID,
		"record_id":   disposal.RecordID,
		"reason":      disposal.Reason,
		"method":      disposal.Method,
		"disposed_at": disposal.DisposedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// ListAccessLogs handles GET /mynumbers/:id/access-logs.
func (h *Handler) ListAccessLogs(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid record id")
		return
	}

	logs, err := h.svc.ListAccessLogs(c.Request.Context(), tenantID, actorID, id)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	items := make([]AccessLogResponse, len(logs))
	for i := range logs {
		items[i] = toAccessLogResponse(&logs[i])
	}
	c.JSON(http.StatusOK, gin.H{"access_logs": items})
}
