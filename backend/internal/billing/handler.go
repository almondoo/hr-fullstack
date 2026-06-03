package billing

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

// Handler exposes HTTP endpoints for the billing domain.
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

// respondErr maps a service error to the unified HTTP error envelope.
func respondErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "operation not allowed in current state")
	case errors.Is(err, ErrSeatLimitExceeded):
		httpx.RespondError(c, http.StatusConflict, "SEAT_LIMIT_EXCEEDED", "seat limit exceeded under hard enforcement")
	case errors.Is(err, ErrAlreadyExists):
		httpx.RespondError(c, http.StatusConflict, "ALREADY_EXISTS", "resource already exists")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Response DTOs
// ---------------------------------------------------------------------------

func fmtDatePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}

func fmtDateTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05Z")
	return &s
}

// PlanResponse is the JSON representation of a plan.
type PlanResponse struct {
	ID             uuid.UUID       `json:"id"`
	PlanCode       string          `json:"plan_code"`
	Name           string          `json:"name"`
	MonthlyBaseFee int64           `json:"monthly_base_fee"`
	PerSeatFee     int64           `json:"per_seat_fee"`
	IncludedSeats  int             `json:"included_seats"`
	TrialDays      int             `json:"trial_days"`
	FeatureFlags   json.RawMessage `json:"feature_flags"`
	Currency       string          `json:"currency"`
	Active         bool            `json:"active"`
}

func toPlanResponse(p *Plan) PlanResponse {
	flags := json.RawMessage(p.FeatureFlagsJSON)
	if len(flags) == 0 {
		flags = json.RawMessage(`{}`)
	}
	return PlanResponse{
		ID:             p.ID,
		PlanCode:       p.PlanCode,
		Name:           p.Name,
		MonthlyBaseFee: p.MonthlyBaseFee,
		PerSeatFee:     p.PerSeatFee,
		IncludedSeats:  p.IncludedSeats,
		TrialDays:      p.TrialDays,
		FeatureFlags:   flags,
		Currency:       p.Currency,
		Active:         p.Active,
	}
}

// SubscriptionResponse is the JSON representation of a subscription.
type SubscriptionResponse struct {
	ID                 uuid.UUID `json:"id"`
	TenantID           uuid.UUID `json:"tenant_id"`
	PlanCode           string    `json:"plan_code"`
	Status             string    `json:"status"`
	TrialEndsOn        *string   `json:"trial_ends_on,omitempty"`
	CurrentPeriodStart string    `json:"current_period_start"`
	CurrentPeriodEnd   string    `json:"current_period_end"`
	CanceledAt         *string   `json:"canceled_at,omitempty"`
	CancelAtPeriodEnd  bool      `json:"cancel_at_period_end"`
	EnforcementMode    string    `json:"enforcement_mode"`
	WriteRestricted    bool      `json:"write_restricted"`
}

func toSubscriptionResponse(s *Subscription) SubscriptionResponse {
	return SubscriptionResponse{
		ID:                 s.ID,
		TenantID:           s.TenantID,
		PlanCode:           s.PlanCode,
		Status:             s.Status,
		TrialEndsOn:        fmtDatePtr(s.TrialEndsOn),
		CurrentPeriodStart: s.CurrentPeriodStart.Format("2006-01-02"),
		CurrentPeriodEnd:   s.CurrentPeriodEnd.Format("2006-01-02"),
		CanceledAt:         fmtDateTimePtr(s.CanceledAt),
		CancelAtPeriodEnd:  s.CancelAtPeriodEnd,
		EnforcementMode:    s.EnforcementMode,
		WriteRestricted:    IsWriteRestricted(s),
	}
}

// InvoiceResponse is the JSON representation of an invoice.
type InvoiceResponse struct {
	ID             uuid.UUID `json:"id"`
	TenantID       uuid.UUID `json:"tenant_id"`
	SubscriptionID uuid.UUID `json:"subscription_id"`
	InvoiceNumber  string    `json:"invoice_number"`
	PeriodStart    string    `json:"period_start"`
	PeriodEnd      string    `json:"period_end"`
	Subtotal       int64     `json:"subtotal"`
	TaxAmount      int64     `json:"tax_amount"`
	Total          int64     `json:"total"`
	Currency       string    `json:"currency"`
	Status         string    `json:"status"`
	IssuedOn       *string   `json:"issued_on,omitempty"`
	DueOn          *string   `json:"due_on,omitempty"`
	PaidAt         *string   `json:"paid_at,omitempty"`
}

func toInvoiceResponse(i *Invoice) InvoiceResponse {
	return InvoiceResponse{
		ID:             i.ID,
		TenantID:       i.TenantID,
		SubscriptionID: i.SubscriptionID,
		InvoiceNumber:  i.InvoiceNumber,
		PeriodStart:    i.PeriodStart.Format("2006-01-02"),
		PeriodEnd:      i.PeriodEnd.Format("2006-01-02"),
		Subtotal:       i.Subtotal,
		TaxAmount:      i.TaxAmount,
		Total:          i.Total,
		Currency:       i.Currency,
		Status:         i.Status,
		IssuedOn:       fmtDatePtr(i.IssuedOn),
		DueOn:          fmtDatePtr(i.DueOn),
		PaidAt:         fmtDateTimePtr(i.PaidAt),
	}
}

// LineItemResponse is the JSON representation of an invoice line item.
type LineItemResponse struct {
	ID          uuid.UUID `json:"id"`
	InvoiceID   uuid.UUID `json:"invoice_id"`
	Kind        string    `json:"kind"`
	Description string    `json:"description"`
	Quantity    int       `json:"quantity"`
	UnitPrice   int64     `json:"unit_price"`
	Amount      int64     `json:"amount"`
}

func toLineItemResponse(li *InvoiceLineItem) LineItemResponse {
	return LineItemResponse{
		ID:          li.ID,
		InvoiceID:   li.InvoiceID,
		Kind:        li.Kind,
		Description: li.Description,
		Quantity:    li.Quantity,
		UnitPrice:   li.UnitPrice,
		Amount:      li.Amount,
	}
}

// PaymentAttemptResponse is the JSON representation of a payment attempt.
// SECURITY: never includes card data; provider_ref is opaque.
type PaymentAttemptResponse struct {
	ID            uuid.UUID `json:"id"`
	InvoiceID     uuid.UUID `json:"invoice_id"`
	Provider      string    `json:"provider"`
	ProviderRef   *string   `json:"provider_ref,omitempty"`
	Amount        int64     `json:"amount"`
	Status        string    `json:"status"`
	FailureReason *string   `json:"failure_reason,omitempty"`
}

func toPaymentAttemptResponse(p *PaymentAttempt) PaymentAttemptResponse {
	return PaymentAttemptResponse{
		ID:            p.ID,
		InvoiceID:     p.InvoiceID,
		Provider:      p.Provider,
		ProviderRef:   p.ProviderRef,
		Amount:        p.Amount,
		Status:        p.Status,
		FailureReason: p.FailureReason,
	}
}

// ProvisioningResponse is the JSON representation of a provisioning record.
type ProvisioningResponse struct {
	ID               uuid.UUID       `json:"id"`
	TenantID         uuid.UUID       `json:"tenant_id"`
	Status           string          `json:"status"`
	Steps            json.RawMessage `json:"steps"`
	SampleDataLoaded bool            `json:"sample_data_loaded"`
	CompletedAt      *string         `json:"completed_at,omitempty"`
}

func toProvisioningResponse(p *TenantProvisioning) ProvisioningResponse {
	steps := json.RawMessage(p.StepsJSON)
	if len(steps) == 0 {
		steps = json.RawMessage(`{}`)
	}
	return ProvisioningResponse{
		ID:               p.ID,
		TenantID:         p.TenantID,
		Status:           p.Status,
		Steps:            steps,
		SampleDataLoaded: p.SampleDataLoaded,
		CompletedAt:      fmtDateTimePtr(p.CompletedAt),
	}
}

// ---------------------------------------------------------------------------
// Plan handlers
// ---------------------------------------------------------------------------

// ListPlans handles GET /billing/plans.
func (h *Handler) ListPlans(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	plans, err := h.svc.ListPlans(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]PlanResponse, len(plans))
	for i := range plans {
		items[i] = toPlanResponse(&plans[i])
	}
	c.JSON(http.StatusOK, gin.H{"plans": items})
}

// ---------------------------------------------------------------------------
// Subscription handlers
// ---------------------------------------------------------------------------

type createSubscriptionRequest struct {
	PlanCode        string  `json:"plan_code"        validate:"required,max=100"`
	PeriodStart     string  `json:"period_start"     validate:"required"`
	PeriodEnd       string  `json:"period_end"       validate:"required"`
	EnforcementMode *string `json:"enforcement_mode" validate:"omitempty,oneof=soft hard"`
}

// CreateSubscription handles POST /billing/subscriptions.
func (h *Handler) CreateSubscription(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createSubscriptionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	ps, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_start must be YYYY-MM-DD")
		return
	}
	pe, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_end must be YYYY-MM-DD")
		return
	}
	mode := ""
	if req.EnforcementMode != nil {
		mode = *req.EnforcementMode
	}

	sub, err := h.svc.CreateSubscription(c.Request.Context(), CreateSubscriptionInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		PlanCode:        req.PlanCode,
		PeriodStart:     ps,
		PeriodEnd:       pe,
		EnforcementMode: mode,
		IP:              clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSubscriptionResponse(sub))
}

// GetSubscription handles GET /billing/subscriptions/current.
func (h *Handler) GetSubscription(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	sub, err := h.svc.GetSubscription(c.Request.Context(), tenantID)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toSubscriptionResponse(sub))
}

type changeStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=trialing active past_due canceled expired"` //nolint:misspell // Stripe API uses "trialing"; changing would break API contract
}

// ChangeSubscriptionStatus handles PATCH /billing/subscriptions/current/status.
func (h *Handler) ChangeSubscriptionStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req changeStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	sub, err := h.svc.ChangeSubscriptionStatus(c.Request.Context(), ChangeSubscriptionStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toSubscriptionResponse(sub))
}

type cancelSubscriptionRequest struct {
	AtPeriodEnd bool `json:"at_period_end"`
}

// CancelSubscription handles POST /billing/subscriptions/current/cancel.
func (h *Handler) CancelSubscription(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req cancelSubscriptionRequest
	// Body is optional; ignore bind error for empty body.
	_ = c.ShouldBindJSON(&req)

	sub, err := h.svc.CancelSubscription(c.Request.Context(), CancelSubscriptionInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		AtPeriodEnd: req.AtPeriodEnd,
		IP:          clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toSubscriptionResponse(sub))
}

// ---------------------------------------------------------------------------
// Seat usage handlers
// ---------------------------------------------------------------------------

type captureSeatUsageRequest struct {
	PeriodStart string `json:"period_start" validate:"required"`
	PeriodEnd   string `json:"period_end"   validate:"required"`
	Source      string `json:"source"       validate:"omitempty,oneof=users employees"`
}

// CaptureSeatUsage handles POST /billing/subscriptions/current/seat-usage.
func (h *Handler) CaptureSeatUsage(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req captureSeatUsageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	ps, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_start must be YYYY-MM-DD")
		return
	}
	pe, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_end must be YYYY-MM-DD")
		return
	}
	src := SeatSourceUsers
	if req.Source == string(SeatSourceEmployees) {
		src = SeatSourceEmployees
	}

	snap, err := h.svc.CaptureSeatUsage(c.Request.Context(), CaptureSeatUsageInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		PeriodStart: ps,
		PeriodEnd:   pe,
		Source:      src,
		IP:          clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":              snap.ID,
		"billable_seats":  snap.BillableSeats,
		"period_start":    snap.PeriodStart.Format("2006-01-02"),
		"period_end":      snap.PeriodEnd.Format("2006-01-02"),
		"subscription_id": snap.SubscriptionID,
	})
}

// ---------------------------------------------------------------------------
// Invoice handlers
// ---------------------------------------------------------------------------

type generateInvoiceRequest struct {
	PeriodStart string `json:"period_start" validate:"required"`
	PeriodEnd   string `json:"period_end"   validate:"required"`
	// TaxRateBps is supplied from tax-rate configuration (basis points).
	TaxRateBps int64 `json:"tax_rate_bps" validate:"min=0,max=100000"`
	Discount   int64 `json:"discount"     validate:"min=0"`
	DueInDays  int   `json:"due_in_days"  validate:"min=0,max=365"`
}

// GenerateInvoice handles POST /billing/invoices.
func (h *Handler) GenerateInvoice(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req generateInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	ps, err := time.Parse("2006-01-02", req.PeriodStart)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_start must be YYYY-MM-DD")
		return
	}
	pe, err := time.Parse("2006-01-02", req.PeriodEnd)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_end must be YYYY-MM-DD")
		return
	}

	inv, lines, err := h.svc.GenerateInvoice(c.Request.Context(), GenerateInvoiceInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		PeriodStart: ps,
		PeriodEnd:   pe,
		TaxRateBps:  req.TaxRateBps,
		Discount:    req.Discount,
		DueInDays:   req.DueInDays,
		IP:          clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	lineItems := make([]LineItemResponse, len(lines))
	for i := range lines {
		lineItems[i] = toLineItemResponse(&lines[i])
	}
	c.JSON(http.StatusCreated, gin.H{
		"invoice":    toInvoiceResponse(inv),
		"line_items": lineItems,
	})
}

// GetInvoice handles GET /billing/invoices/:id.
func (h *Handler) GetInvoice(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid invoice id")
		return
	}
	inv, err := h.svc.GetInvoice(c.Request.Context(), tenantID, id)
	if err != nil {
		respondErr(c, err)
		return
	}
	lines, err := h.svc.ListInvoiceLineItems(c.Request.Context(), tenantID, id)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	lineItems := make([]LineItemResponse, len(lines))
	for i := range lines {
		lineItems[i] = toLineItemResponse(&lines[i])
	}
	c.JSON(http.StatusOK, gin.H{
		"invoice":    toInvoiceResponse(inv),
		"line_items": lineItems,
	})
}

// ListInvoices handles GET /billing/invoices.
func (h *Handler) ListInvoices(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	invs, err := h.svc.ListInvoices(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]InvoiceResponse, len(invs))
	for i := range invs {
		items[i] = toInvoiceResponse(&invs[i])
	}
	c.JSON(http.StatusOK, gin.H{"invoices": items})
}

// ---------------------------------------------------------------------------
// Payment handlers
// ---------------------------------------------------------------------------

// PayInvoice handles POST /billing/invoices/:id/pay.
func (h *Handler) PayInvoice(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid invoice id")
		return
	}

	attempt, err := h.svc.PayInvoice(c.Request.Context(), PayInvoiceInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		InvoiceID: id,
		IP:        clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toPaymentAttemptResponse(attempt))
}

// ListPaymentAttempts handles GET /billing/invoices/:id/payments.
func (h *Handler) ListPaymentAttempts(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid invoice id")
		return
	}
	attempts, err := h.svc.ListPaymentAttempts(c.Request.Context(), tenantID, id)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]PaymentAttemptResponse, len(attempts))
	for i := range attempts {
		items[i] = toPaymentAttemptResponse(&attempts[i])
	}
	c.JSON(http.StatusOK, gin.H{"payments": items})
}

// ---------------------------------------------------------------------------
// Provisioning handlers
// ---------------------------------------------------------------------------

type provisionRequest struct {
	LoadSampleData bool `json:"load_sample_data"`
}

// ProvisionTenant handles POST /billing/provisioning.
func (h *Handler) ProvisionTenant(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req provisionRequest
	_ = c.ShouldBindJSON(&req) // body optional

	prov, err := h.svc.ProvisionTenant(c.Request.Context(), ProvisionTenantInput{
		TenantID:       tenantID,
		ActorID:        actorID,
		LoadSampleData: req.LoadSampleData,
		IP:             clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toProvisioningResponse(prov))
}

// GetProvisioning handles GET /billing/provisioning.
func (h *Handler) GetProvisioning(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	prov, err := h.svc.GetProvisioning(c.Request.Context(), tenantID)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toProvisioningResponse(prov))
}
