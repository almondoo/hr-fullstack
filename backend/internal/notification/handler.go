package notification

import (
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

// Handler exposes HTTP endpoints for the notification domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts validator.ValidationErrors into a safe message.
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

// respondServiceError maps service sentinel errors to HTTP responses.
func respondServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "operation not permitted")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Template request / response shapes
// ---------------------------------------------------------------------------

type upsertTemplateRequest struct {
	EventType       string `json:"event_type"       validate:"required,max=200"`
	Channel         string `json:"channel"          validate:"required,oneof=in_app email"`
	Locale          string `json:"locale"           validate:"omitempty,max=20"`
	SubjectTemplate string `json:"subject_template" validate:"max=2000"`
	BodyTemplate    string `json:"body_template"    validate:"max=20000"`
	Active          *bool  `json:"active"`
}

// TemplateResponse is the JSON representation of a template.
type TemplateResponse struct {
	ID              uuid.UUID `json:"id"`
	TenantID        uuid.UUID `json:"tenant_id"`
	EventType       string    `json:"event_type"`
	Channel         string    `json:"channel"`
	Locale          string    `json:"locale"`
	SubjectTemplate string    `json:"subject_template"`
	BodyTemplate    string    `json:"body_template"`
	Active          bool      `json:"active"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
}

func toTemplateResponse(t *Template) TemplateResponse {
	return TemplateResponse{
		ID:              t.ID,
		TenantID:        t.TenantID,
		EventType:       t.EventType,
		Channel:         t.Channel,
		Locale:          t.Locale,
		SubjectTemplate: t.SubjectTemplate,
		BodyTemplate:    t.BodyTemplate,
		Active:          t.Active,
		CreatedAt:       t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:       t.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// UpsertTemplate handles PUT /notifications/templates.
func (h *Handler) UpsertTemplate(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req upsertTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	active := true
	if req.Active != nil {
		active = *req.Active
	}

	tmpl, err := h.svc.UpsertTemplate(c.Request.Context(), UpsertTemplateInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EventType:       req.EventType,
		Channel:         req.Channel,
		Locale:          req.Locale,
		SubjectTemplate: req.SubjectTemplate,
		BodyTemplate:    req.BodyTemplate,
		Active:          active,
		IP:              clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toTemplateResponse(tmpl))
}

// ListTemplates handles GET /notifications/templates.
func (h *Handler) ListTemplates(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	eventType := c.Query("event_type")

	tmpls, err := h.svc.ListTemplates(c.Request.Context(), tenantID, eventType)
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
// Preference request / response shapes
// ---------------------------------------------------------------------------

type setPreferenceRequest struct {
	UserID    uuid.UUID `json:"user_id"    validate:"required"`
	EventType string    `json:"event_type" validate:"required,max=200"`
	Channel   string    `json:"channel"    validate:"required,oneof=in_app email"`
	OptedIn   *bool     `json:"opted_in"   validate:"required"`
	Forced    bool      `json:"forced"`
}

// PreferenceResponse is the JSON representation of a preference.
type PreferenceResponse struct {
	ID        uuid.UUID `json:"id"`
	TenantID  uuid.UUID `json:"tenant_id"`
	UserID    uuid.UUID `json:"user_id"`
	EventType string    `json:"event_type"`
	Channel   string    `json:"channel"`
	OptedIn   bool      `json:"opted_in"`
	Forced    bool      `json:"forced"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

func toPreferenceResponse(p *Preference) PreferenceResponse {
	return PreferenceResponse{
		ID:        p.ID,
		TenantID:  p.TenantID,
		UserID:    p.UserID,
		EventType: p.EventType,
		Channel:   p.Channel,
		OptedIn:   p.OptedIn,
		Forced:    p.Forced,
		CreatedAt: p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// SetPreference handles PUT /notifications/preferences.
func (h *Handler) SetPreference(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req setPreferenceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	pref, err := h.svc.SetPreference(c.Request.Context(), SetPreferenceInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		UserID:    req.UserID,
		EventType: req.EventType,
		Channel:   req.Channel,
		OptedIn:   *req.OptedIn,
		Forced:    req.Forced,
		IP:        clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toPreferenceResponse(pref))
}

// ---------------------------------------------------------------------------
// Notification (inbox) response shapes
// ---------------------------------------------------------------------------

// NotificationResponse is the JSON representation of an in-app notification.
type NotificationResponse struct { //nolint:revive // type name intentionally includes package prefix for external API clarity
	ID           uuid.UUID  `json:"id"`
	TenantID     uuid.UUID  `json:"tenant_id"`
	EventType    string     `json:"event_type"`
	Channel      string     `json:"channel"`
	Subject      string     `json:"subject"`
	BodyRef      string     `json:"body_ref"`
	ResourceType string     `json:"resource_type"`
	ResourceID   *uuid.UUID `json:"resource_id,omitempty"`
	Status       string     `json:"status"`
	Read         bool       `json:"read"`
	CreatedAt    string     `json:"created_at"`
}

func toNotificationResponse(item *InboxItem) NotificationResponse {
	n := item.Notification
	return NotificationResponse{
		ID:           n.ID,
		TenantID:     n.TenantID,
		EventType:    n.EventType,
		Channel:      n.Channel,
		Subject:      n.Subject,
		BodyRef:      n.BodyRef,
		ResourceType: n.ResourceType,
		ResourceID:   n.ResourceID,
		Status:       n.Status,
		Read:         item.Read,
		CreatedAt:    n.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ListInbox handles GET /notifications.  Returns the authenticated user's own
// in-app notifications only.
func (h *Handler) ListInbox(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	userID := platformauth.UserIDFrom(c)

	unreadOnly := c.Query("unread") == "true"

	items, err := h.svc.ListInbox(c.Request.Context(), ListInboxInput{
		TenantID:   tenantID,
		UserID:     userID,
		UnreadOnly: unreadOnly,
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	resp := make([]NotificationResponse, len(items))
	for i := range items {
		resp[i] = toNotificationResponse(&items[i])
	}
	c.JSON(http.StatusOK, gin.H{"notifications": resp})
}

// UnreadCount handles GET /notifications/unread-count.
func (h *Handler) UnreadCount(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	userID := platformauth.UserIDFrom(c)

	count, err := h.svc.UnreadCount(c.Request.Context(), tenantID, userID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"unread_count": count})
}

// MarkRead handles POST /notifications/:id/read.  The authenticated user may
// only mark their own notifications read; otherwise the service returns
// ErrForbidden (403).
func (h *Handler) MarkRead(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	userID := platformauth.UserIDFrom(c)

	notifID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid notification id")
		return
	}

	read, err := h.svc.MarkRead(c.Request.Context(), MarkReadInput{
		TenantID:       tenantID,
		ActorID:        userID,
		NotificationID: notifID,
		UserID:         userID,
		IP:             clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"notification_id": read.NotificationID,
		"read_at":         read.ReadAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// ---------------------------------------------------------------------------
// Email delivery response shapes
// ---------------------------------------------------------------------------

// DeliveryResponse is the JSON representation of an email delivery (metadata
// only; the destination email is never included).
type DeliveryResponse struct {
	ID                uuid.UUID `json:"id"`
	TenantID          uuid.UUID `json:"tenant_id"`
	NotificationID    uuid.UUID `json:"notification_id"`
	ToEmailHash       string    `json:"to_email_hash"`
	Provider          string    `json:"provider"`
	ProviderMessageID string    `json:"provider_message_id"`
	Status            string    `json:"status"`
	Attempts          int       `json:"attempts"`
	MaxAttempts       int       `json:"max_attempts"`
	LastError         string    `json:"last_error"`
	SentAt            *string   `json:"sent_at,omitempty"`
	BouncedAt         *string   `json:"bounced_at,omitempty"`
	CreatedAt         string    `json:"created_at"`
}

func toDeliveryResponse(d *EmailDelivery) DeliveryResponse {
	r := DeliveryResponse{
		ID:                d.ID,
		TenantID:          d.TenantID,
		NotificationID:    d.NotificationID,
		ToEmailHash:       d.ToEmailHash,
		Provider:          d.Provider,
		ProviderMessageID: d.ProviderMessageID,
		Status:            d.Status,
		Attempts:          d.Attempts,
		MaxAttempts:       d.MaxAttempts,
		LastError:         d.LastError,
		CreatedAt:         d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if d.SentAt != nil {
		s := d.SentAt.UTC().Format("2006-01-02T15:04:05Z")
		r.SentAt = &s
	}
	if d.BouncedAt != nil {
		s := d.BouncedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.BouncedAt = &s
	}
	return r
}

// ListDeliveries handles GET /notifications/:id/deliveries.
func (h *Handler) ListDeliveries(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	notifID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid notification id")
		return
	}

	ds, err := h.svc.ListDeliveries(c.Request.Context(), tenantID, notifID)
	if err != nil {
		respondServiceError(c, err)
		return
	}
	resp := make([]DeliveryResponse, len(ds))
	for i := range ds {
		resp[i] = toDeliveryResponse(&ds[i])
	}
	c.JSON(http.StatusOK, gin.H{"deliveries": resp})
}

// ProcessDelivery handles POST /notifications/deliveries/:delivery_id/process.
// Sends (mock) the queued/failed delivery and applies retry accounting.
func (h *Handler) ProcessDelivery(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	deliveryID, err := uuid.Parse(c.Param("delivery_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid delivery id")
		return
	}

	d, err := h.svc.ProcessDelivery(c.Request.Context(), tenantID, deliveryID, actorID, clientIP(c))
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, toDeliveryResponse(d))
}

// GetDeliveryEmail handles GET /notifications/deliveries/:delivery_id/email.
// Requires notification:read_sensitive (route middleware) AND the service-layer
// re-check.  The decrypted address is returned in the response body only.
func (h *Handler) GetDeliveryEmail(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	deliveryID, err := uuid.Parse(c.Param("delivery_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid delivery id")
		return
	}

	email, err := h.svc.GetDeliveryEmail(c.Request.Context(), GetDeliveryEmailInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		DeliveryID: deliveryID,
		IP:         clientIP(c),
	})
	if err != nil {
		respondServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"to_email": email})
}
