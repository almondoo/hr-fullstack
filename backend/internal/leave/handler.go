package leave

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

// maxJSONBytes caps the size of JSONB payload fields in this handler.
const maxJSONBytes = 64 * 1024 // 64 KB

// Handler exposes HTTP endpoints for the leave domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

// validationMessage converts validator.ValidationErrors into a safe, non-leaking
// error message following the pattern established in the employee package.
func validationMessage(err error) string {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		e := ve[0]
		return fmt.Sprintf("validation failed on field '%s' (%s)", e.Field(), e.Tag())
	}
	return "validation failed"
}

// parseDateRequired parses a required YYYY-MM-DD string.
func parseDateRequired(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: must be YYYY-MM-DD", s)
	}
	return t, nil
}

// validateJSONPayload checks that raw JSON is either empty or valid and within size.
func validateJSONPayload(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if len(raw) > maxJSONBytes {
		return fmt.Errorf("JSON payload exceeds maximum size of %d bytes", maxJSONBytes)
	}
	if !json.Valid(raw) {
		return fmt.Errorf("JSON payload is not valid JSON")
	}
	return nil
}

// clientIP extracts the client IP from a gin.Context.
func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}

// ---------------------------------------------------------------------------
// Settings handlers
// ---------------------------------------------------------------------------

// GetSettings handles GET /leave/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	setting, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "leave settings not found for this tenant")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toSettingResponse(setting))
}

type upsertSettingsRequest struct {
	BaseDateRule               string          `json:"base_date_rule"                 validate:"required,max=100"`
	GrantTableJSON             json.RawMessage `json:"grant_table_json"`
	ProportionalTableJSON      json.RawMessage `json:"proportional_table_json"`
	FiveDayObligationThreshold int             `json:"five_day_obligation_threshold"  validate:"required,min=1"`
	ExpiryMonths               int             `json:"expiry_months"                  validate:"required,min=1"`
}

// UpsertSettings handles PUT /leave/settings.
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
	if err := validateJSONPayload(req.GrantTableJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "grant_table_json: "+err.Error())
		return
	}
	if err := validateJSONPayload(req.ProportionalTableJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "proportional_table_json: "+err.Error())
		return
	}

	setting, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		BaseDateRule:               req.BaseDateRule,
		GrantTableJSON:             []byte(req.GrantTableJSON),
		ProportionalTableJSON:      []byte(req.ProportionalTableJSON),
		FiveDayObligationThreshold: req.FiveDayObligationThreshold,
		ExpiryMonths:               req.ExpiryMonths,
		IP:                         clientIP(c),
	})
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toSettingResponse(setting))
}

// ---------------------------------------------------------------------------
// Grant handlers
// ---------------------------------------------------------------------------

type grantLeaveRequest struct {
	EmployeeID string  `json:"employee_id" validate:"required,uuid"`
	GrantDate  string  `json:"grant_date"  validate:"required"`
	Days       float64 `json:"days"        validate:"required,gt=0"`
	Source     string  `json:"source"      validate:"required,oneof=annual_grant proportional_grant carry_over manual"`
}

// CreateGrant handles POST /leave/grants.
func (h *Handler) CreateGrant(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req grantLeaveRequest
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
	grantDate, err := parseDateRequired(req.GrantDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	grant, err := h.svc.GrantLeave(c.Request.Context(), GrantLeaveInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		GrantDate:  grantDate,
		Days:       req.Days,
		Source:     req.Source,
		IP:         clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrEmployeeNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found in this tenant")
			return
		}
		if errors.Is(err, ErrSettingNotFound) {
			httpx.RespondError(c, http.StatusUnprocessableEntity, "SETTINGS_NOT_FOUND", "leave settings not configured for this tenant")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusCreated, toGrantResponse(grant))
}

// ListGrants handles GET /leave/employees/:employee_id/grants.
func (h *Handler) ListGrants(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee_id")
		return
	}

	grants, err := h.svc.ListGrants(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]GrantResponse, len(grants))
	for i := range grants {
		items[i] = toGrantResponse(&grants[i])
	}
	c.JSON(http.StatusOK, gin.H{"grants": items})
}

// ---------------------------------------------------------------------------
// Balance handler
// ---------------------------------------------------------------------------

// GetBalance handles GET /leave/employees/:employee_id/balance.
func (h *Handler) GetBalance(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee_id")
		return
	}

	asOf := time.Now().UTC()
	if asOfStr := c.Query("as_of"); asOfStr != "" {
		t, err := parseDateRequired(asOfStr)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "as_of: "+err.Error())
			return
		}
		asOf = t
	}

	balance, err := h.svc.GetBalance(c.Request.Context(), tenantID, empID, asOf)
	if err != nil {
		if errors.Is(err, ErrEmployeeNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toBalanceResponse(balance))
}

// ---------------------------------------------------------------------------
// 5-day obligation handler
// ---------------------------------------------------------------------------

// GetFiveDayObligation handles GET /leave/employees/:employee_id/five-day-obligation.
func (h *Handler) GetFiveDayObligation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee_id")
		return
	}

	asOf := time.Now().UTC()
	if asOfStr := c.Query("as_of"); asOfStr != "" {
		t, err := parseDateRequired(asOfStr)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "as_of: "+err.Error())
			return
		}
		asOf = t
	}

	obl, err := h.svc.GetFiveDayObligation(c.Request.Context(), tenantID, empID, asOf)
	if err != nil {
		if errors.Is(err, ErrEmployeeNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found")
			return
		}
		if errors.Is(err, ErrSettingNotFound) {
			httpx.RespondError(c, http.StatusUnprocessableEntity, "SETTINGS_NOT_FOUND", "leave settings not configured for this tenant")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toFiveDayObligationResponse(obl))
}

// ---------------------------------------------------------------------------
// Request handlers
// ---------------------------------------------------------------------------

type createRequestBody struct {
	EmployeeID string  `json:"employee_id" validate:"required,uuid"`
	LeaveType  string  `json:"leave_type"  validate:"required,oneof=annual special condolence maternity childcare care absence"`
	StartDate  string  `json:"start_date"  validate:"required"`
	EndDate    string  `json:"end_date"    validate:"required"`
	Days       float64 `json:"days"        validate:"required,gt=0"`
	Reason     *string `json:"reason"`
}

// CreateRequest handles POST /leave/requests.
func (h *Handler) CreateRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createRequestBody
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
	startDate, err := parseDateRequired(req.StartDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	endDate, err := parseDateRequired(req.EndDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	if endDate.Before(startDate) {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "end_date must not be before start_date")
		return
	}

	leaveReq, err := h.svc.CreateRequest(c.Request.Context(), CreateRequestInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		LeaveType:  req.LeaveType,
		StartDate:  startDate,
		EndDate:    endDate,
		Days:       req.Days,
		Reason:     req.Reason,
		IP:         clientIP(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrEmployeeNotFound):
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found in this tenant")
		case errors.Is(err, ErrInsufficientBalance):
			httpx.RespondError(c, http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "insufficient annual leave balance")
		case errors.Is(err, ErrInvalidLeaveType):
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid leave type")
		case errors.Is(err, ErrInvalidDates):
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "end_date must not be before start_date")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}
	c.JSON(http.StatusCreated, toRequestResponse(leaveReq))
}

// GetRequest handles GET /leave/requests/:id.
func (h *Handler) GetRequest(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	req, err := h.svc.GetRequest(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrRequestNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "leave request not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, toRequestResponse(req))
}

// ListRequests handles GET /leave/employees/:employee_id/requests.
func (h *Handler) ListRequests(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee_id")
		return
	}

	reqs, err := h.svc.ListRequests(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]RequestResponse, len(reqs))
	for i := range reqs {
		items[i] = toRequestResponse(&reqs[i])
	}
	c.JSON(http.StatusOK, gin.H{"requests": items})
}

type updateRequestStatusBody struct {
	Status string `json:"status" validate:"required,oneof=approved rejected cancelled"`
}

// UpdateRequestStatus handles PATCH /leave/requests/:id/status.
func (h *Handler) UpdateRequestStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid request id")
		return
	}

	var req updateRequestStatusBody
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	updated, err := h.svc.UpdateRequestStatus(c.Request.Context(), UpdateRequestStatusInput{
		TenantID: tenantID,
		ID:       id,
		ActorID:  actorID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestNotFound):
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "leave request not found")
		case errors.Is(err, ErrInvalidTransition):
			httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
		case errors.Is(err, ErrInsufficientBalance):
			httpx.RespondError(c, http.StatusUnprocessableEntity, "INSUFFICIENT_BALANCE", "insufficient annual leave balance")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}
	c.JSON(http.StatusOK, toRequestResponse(updated))
}

// ---------------------------------------------------------------------------
// Compute and grant annual leave handler (LM-040)
// ---------------------------------------------------------------------------

type computeGrantRequest struct {
	EmployeeID string   `json:"employee_id"  validate:"required,uuid"`
	HiredOn    string   `json:"hired_on"     validate:"required"`
	GrantDate  string   `json:"grant_date"   validate:"required"`
	WeeklyDays *float64 `json:"weekly_days"`
}

// ComputeAndGrantAnnual handles POST /leave/grants/compute-annual.
func (h *Handler) ComputeAndGrantAnnual(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req computeGrantRequest
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
	hiredOn, err := parseDateRequired(req.HiredOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	grantDate, err := parseDateRequired(req.GrantDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	grant, err := h.svc.ComputeAndGrantAnnual(c.Request.Context(), ComputeAndGrantAnnualInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		HiredOn:    hiredOn,
		GrantDate:  grantDate,
		WeeklyDays: req.WeeklyDays,
		IP:         clientIP(c),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrEmployeeNotFound):
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "employee not found in this tenant")
		case errors.Is(err, ErrSettingNotFound):
			httpx.RespondError(c, http.StatusUnprocessableEntity, "SETTINGS_NOT_FOUND", "leave settings not configured for this tenant")
		default:
			httpx.RespondInternalError(c)
		}
		return
	}

	if grant == nil {
		// Tenure below minimum threshold — no grant.
		c.JSON(http.StatusOK, gin.H{"message": "no grant: tenure below minimum threshold"})
		return
	}
	c.JSON(http.StatusCreated, toGrantResponse(grant))
}

// ---------------------------------------------------------------------------
// JSON response shapes
// ---------------------------------------------------------------------------

// SettingResponse is the JSON representation of leave_settings.
type SettingResponse struct {
	ID                         uuid.UUID       `json:"id"`
	TenantID                   uuid.UUID       `json:"tenant_id"`
	BaseDateRule               string          `json:"base_date_rule"`
	GrantTableJSON             json.RawMessage `json:"grant_table_json"`
	ProportionalTableJSON      json.RawMessage `json:"proportional_table_json"`
	FiveDayObligationThreshold int             `json:"five_day_obligation_threshold"`
	ExpiryMonths               int             `json:"expiry_months"`
	UpdatedAt                  string          `json:"updated_at"`
}

func toSettingResponse(s *Setting) SettingResponse {
	gj := json.RawMessage(s.GrantTableJSON)
	if len(gj) == 0 {
		gj = json.RawMessage(`[]`)
	}
	pj := json.RawMessage(s.ProportionalTableJSON)
	if len(pj) == 0 {
		pj = json.RawMessage(`[]`)
	}
	return SettingResponse{
		ID:                         s.ID,
		TenantID:                   s.TenantID,
		BaseDateRule:               s.BaseDateRule,
		GrantTableJSON:             gj,
		ProportionalTableJSON:      pj,
		FiveDayObligationThreshold: s.FiveDayObligationThreshold,
		ExpiryMonths:               s.ExpiryMonths,
		UpdatedAt:                  s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// GrantResponse is the JSON representation of a leave_grant.
type GrantResponse struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	EmployeeID uuid.UUID `json:"employee_id"`
	GrantDate  string    `json:"grant_date"`
	Days       float64   `json:"days"`
	Source     string    `json:"source"`
	ExpiresOn  string    `json:"expires_on"`
	CreatedAt  string    `json:"created_at"`
}

func toGrantResponse(g *Grant) GrantResponse {
	return GrantResponse{
		ID:         g.ID,
		TenantID:   g.TenantID,
		EmployeeID: g.EmployeeID,
		GrantDate:  g.GrantDate.Format("2006-01-02"),
		Days:       g.Days,
		Source:     g.Source,
		ExpiresOn:  g.ExpiresOn.Format("2006-01-02"),
		CreatedAt:  g.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// BalanceResponse is the JSON representation of a leave balance.
type BalanceResponse struct {
	EmployeeID   uuid.UUID       `json:"employee_id"`
	TotalGranted float64         `json:"total_granted"`
	TotalUsed    float64         `json:"total_used"`
	Remaining    float64         `json:"remaining"`
	AsOf         string          `json:"as_of"`
	Grants       []GrantResponse `json:"grants"`
}

func toBalanceResponse(b *Balance) BalanceResponse {
	items := make([]GrantResponse, len(b.Grants))
	for i := range b.Grants {
		items[i] = toGrantResponse(&b.Grants[i])
	}
	return BalanceResponse{
		EmployeeID:   b.EmployeeID,
		TotalGranted: b.TotalGranted,
		TotalUsed:    b.TotalUsed,
		Remaining:    b.Remaining,
		AsOf:         b.AsOf.Format("2006-01-02"),
		Grants:       items,
	}
}

// FiveDayObligationResponse is the JSON representation of 5-day obligation status.
type FiveDayObligationResponse struct {
	EmployeeID     uuid.UUID `json:"employee_id"`
	GrantYearStart string    `json:"grant_year_start"`
	GrantYearEnd   string    `json:"grant_year_end"`
	GrantDays      float64   `json:"grant_days"`
	UsedDays       float64   `json:"used_days"`
	Obligated      bool      `json:"obligated"`
	Met            bool      `json:"met"`
	ShortfallDays  float64   `json:"shortfall_days"`
}

func toFiveDayObligationResponse(o *FiveDayObligation) FiveDayObligationResponse {
	r := FiveDayObligationResponse{
		EmployeeID:    o.EmployeeID,
		GrantDays:     o.GrantDays,
		UsedDays:      o.UsedDays,
		Obligated:     o.Obligated,
		Met:           o.Met,
		ShortfallDays: o.ShortfallDays,
	}
	if !o.GrantYearStart.IsZero() {
		r.GrantYearStart = o.GrantYearStart.Format("2006-01-02")
		r.GrantYearEnd = o.GrantYearEnd.Format("2006-01-02")
	}
	return r
}

// RequestResponse is the JSON representation of a leave_request.
type RequestResponse struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	EmployeeID        uuid.UUID  `json:"employee_id"`
	LeaveType         string     `json:"leave_type"`
	StartDate         string     `json:"start_date"`
	EndDate           string     `json:"end_date"`
	Days              float64    `json:"days"`
	Status            string     `json:"status"`
	ApprovalRequestID *uuid.UUID `json:"approval_request_id,omitempty"`
	Reason            *string    `json:"reason,omitempty"`
	CreatedAt         string     `json:"created_at"`
	UpdatedAt         string     `json:"updated_at"`
}

func toRequestResponse(r *Request) RequestResponse {
	return RequestResponse{
		ID:                r.ID,
		TenantID:          r.TenantID,
		EmployeeID:        r.EmployeeID,
		LeaveType:         r.LeaveType,
		StartDate:         r.StartDate.Format("2006-01-02"),
		EndDate:           r.EndDate.Format("2006-01-02"),
		Days:              r.Days,
		Status:            r.Status,
		ApprovalRequestID: r.ApprovalRequestID,
		Reason:            r.Reason,
		CreatedAt:         r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:         r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}
