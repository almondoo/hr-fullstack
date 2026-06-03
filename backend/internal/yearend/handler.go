package yearend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/httpx"
)

var validate = validator.New()

// maxJSONBytes is the maximum size for any JSON field in request bodies.
const maxJSONBytes = 64 * 1024

// Handler exposes HTTP endpoints for the year-end adjustment domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

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

// parseTaxYear parses the tax_year query / path param.
func parseTaxYear(raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < 2000 || v > 2100 {
		return 0, fmt.Errorf("tax_year must be a valid year between 2000 and 2100")
	}
	return v, nil
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

// mapServiceError converts a service sentinel error to an HTTP response.
// Returns true when a response was written.
func mapServiceError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "operation not allowed in current state")
	case errors.Is(err, ErrLocked):
		httpx.RespondError(c, http.StatusConflict, "LOCKED", "submission is locked and immutable")
	case errors.Is(err, ErrFinalised):
		httpx.RespondError(c, http.StatusConflict, "FINALISED", "calculation is finalised and immutable")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	case errors.Is(err, ErrInvalidInput):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
	default:
		httpx.RespondInternalError(c)
	}
	return true
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	TaxYear             int             `json:"tax_year"              validate:"required,min=2000,max=2100"`
	RateTable           json.RawMessage `json:"rate_table"`
	DeductionLimits     json.RawMessage `json:"deduction_limits"`
}

// SettingsResponse is the JSON representation of yearend settings.
type SettingsResponse struct {
	ID              uuid.UUID       `json:"id"`
	TenantID        uuid.UUID       `json:"tenant_id"`
	TaxYear         int             `json:"tax_year"`
	RateTable       json.RawMessage `json:"rate_table"`
	DeductionLimits json.RawMessage `json:"deduction_limits"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

func toSettingsResponse(st *Settings) SettingsResponse {
	rt := json.RawMessage(st.RateTableJSON)
	if len(rt) == 0 {
		rt = json.RawMessage(`{}`)
	}
	dl := json.RawMessage(st.DeductionLimitsJSON)
	if len(dl) == 0 {
		dl = json.RawMessage(`{}`)
	}
	r := SettingsResponse{
		ID:              st.ID,
		TenantID:        st.TenantID,
		TaxYear:         st.TaxYear,
		RateTable:       rt,
		DeductionLimits: dl,
	}
	if !st.CreatedAt.IsZero() {
		r.CreatedAt = st.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !st.UpdatedAt.IsZero() {
		r.UpdatedAt = st.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return r
}

// UpsertSettings handles PUT /yearend/settings.
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
	if err := validateJSON(req.RateTable); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "rate_table: "+err.Error())
		return
	}
	if err := validateJSON(req.DeductionLimits); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "deduction_limits: "+err.Error())
		return
	}

	st, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:            tenantID,
		ActorID:             actorID,
		TaxYear:             req.TaxYear,
		RateTableJSON:       []byte(req.RateTable),
		DeductionLimitsJSON: []byte(req.DeductionLimits),
		IP:                  clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(st))
}

// GetSettings handles GET /yearend/settings?tax_year=2026.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	taxYear, err := parseTaxYear(c.Query("tax_year"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	st, err := h.svc.GetSettings(c.Request.Context(), tenantID, taxYear)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(st))
}

// ---------------------------------------------------------------------------
// Submission
// ---------------------------------------------------------------------------

type upsertSubmissionRequest struct {
	TaxYear         int             `json:"tax_year"         validate:"required,min=2000,max=2100"`
	DeclarationJSON json.RawMessage `json:"declaration_json" validate:"required"`
}

// SubmissionResponse is the JSON representation of a submission.
// Security: DeclarationEnc is NEVER included in responses.
// DeclarationJSON is only included when the caller holds yearend:reveal.
type SubmissionResponse struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	EmployeeID  uuid.UUID `json:"employee_id"`
	TaxYear     int       `json:"tax_year"`
	Status      string    `json:"status"`
	// DeclarationJSON is the decrypted declaration, only present when reveal=true
	// and the actor holds yearend:reveal permission.
	DeclarationJSON json.RawMessage `json:"declaration_json,omitempty"`
	SubmittedAt     *string         `json:"submitted_at,omitempty"`
	LockedAt        *string         `json:"locked_at,omitempty"`
	CreatedAt       string          `json:"created_at"`
	UpdatedAt       string          `json:"updated_at"`
}

func toSubmissionResponse(sub *Submission, decl []byte) SubmissionResponse {
	r := SubmissionResponse{
		ID:         sub.ID,
		TenantID:   sub.TenantID,
		EmployeeID: sub.EmployeeID,
		TaxYear:    sub.TaxYear,
		Status:     sub.Status,
		CreatedAt:  sub.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:  sub.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if len(decl) > 0 {
		r.DeclarationJSON = json.RawMessage(decl)
	}
	if sub.SubmittedAt != nil {
		s := sub.SubmittedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.SubmittedAt = &s
	}
	if sub.LockedAt != nil {
		s := sub.LockedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.LockedAt = &s
	}
	return r
}

// UpsertSubmission handles POST /employees/:id/yearend/submissions.
func (h *Handler) UpsertSubmission(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req upsertSubmissionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.DeclarationJSON); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "declaration_json: "+err.Error())
		return
	}

	sub, err := h.svc.UpsertSubmission(c.Request.Context(), UpsertSubmissionInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		TaxYear:         req.TaxYear,
		DeclarationJSON: []byte(req.DeclarationJSON),
		IP:              clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toSubmissionResponse(sub, nil))
}

// SubmitSubmission handles POST /employees/:id/yearend/submissions/submit.
func (h *Handler) SubmitSubmission(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	taxYear, err := parseTaxYear(c.Query("tax_year"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	sub, err := h.svc.SubmitSubmission(c.Request.Context(), SubmitSubmissionInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TaxYear:    taxYear,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSubmissionResponse(sub, nil))
}

// GetSubmission handles GET /employees/:id/yearend/submissions?tax_year=2026[&reveal=true].
// reveal=true requires yearend:reveal permission (enforced in service, defence-in-depth).
func (h *Handler) GetSubmission(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	taxYear, err := parseTaxYear(c.Query("tax_year"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	reveal := c.Query("reveal") == "true"

	result, err := h.svc.GetSubmission(c.Request.Context(), GetSubmissionInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TaxYear:    taxYear,
		Reveal:     reveal,
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSubmissionResponse(result.Submission, result.DeclarationJSON))
}

// ---------------------------------------------------------------------------
// Calculation
// ---------------------------------------------------------------------------

type runCalculationRequest struct {
	TaxYear                      int   `json:"tax_year"                         validate:"required,min=2000,max=2100"`
	GrossIncome                  int64 `json:"gross_income"                     validate:"min=0"`
	EmploymentDeduction          int64 `json:"employment_deduction"             validate:"min=0"`
	BasicDeduction               int64 `json:"basic_deduction"                  validate:"min=0"`
	DependentDeduction           int64 `json:"dependent_deduction"              validate:"min=0"`
	SpouseDeduction              int64 `json:"spouse_deduction"                 validate:"min=0"`
	LifeInsuranceDeduction       int64 `json:"life_insurance_deduction"         validate:"min=0"`
	EarthquakeInsuranceDeduction int64 `json:"earthquake_insurance_deduction"   validate:"min=0"`
	SocialInsuranceDeduction     int64 `json:"social_insurance_deduction"       validate:"min=0"`
	HousingLoanDeduction         int64 `json:"housing_loan_deduction"           validate:"min=0"`
	WithheldTax                  int64 `json:"withheld_tax"                     validate:"min=0"`
}

// CalculationResponse is the JSON representation of a calculation.
type CalculationResponse struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	EmployeeID   uuid.UUID       `json:"employee_id"`
	TaxYear      int             `json:"tax_year"`
	SubmissionID uuid.UUID       `json:"submission_id"`
	Status       string          `json:"status"`
	Result       json.RawMessage `json:"result"`
	CalculatedAt *string         `json:"calculated_at,omitempty"`
	FinalisedAt  *string         `json:"finalised_at,omitempty"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func toCalculationResponse(calc *Calculation) CalculationResponse {
	rj := json.RawMessage(calc.ResultJSON)
	if len(rj) == 0 {
		rj = json.RawMessage(`{}`)
	}
	r := CalculationResponse{
		ID:           calc.ID,
		TenantID:     calc.TenantID,
		EmployeeID:   calc.EmployeeID,
		TaxYear:      calc.TaxYear,
		SubmissionID: calc.SubmissionID,
		Status:       calc.Status,
		Result:       rj,
		CreatedAt:    calc.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    calc.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if calc.CalculatedAt != nil {
		s := calc.CalculatedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.CalculatedAt = &s
	}
	if calc.FinalisedAt != nil {
		s := calc.FinalisedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.FinalisedAt = &s
	}
	return r
}

// RunCalculation handles POST /employees/:id/yearend/calculations.
func (h *Handler) RunCalculation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req runCalculationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	calc, err := h.svc.RunCalculation(c.Request.Context(), RunCalculationInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TaxYear:    req.TaxYear,
		TaxIn: TaxInput{
			GrossIncome:                  req.GrossIncome,
			EmploymentDeduction:          req.EmploymentDeduction,
			BasicDeduction:               req.BasicDeduction,
			DependentDeduction:           req.DependentDeduction,
			SpouseDeduction:              req.SpouseDeduction,
			LifeInsuranceDeduction:       req.LifeInsuranceDeduction,
			EarthquakeInsuranceDeduction: req.EarthquakeInsuranceDeduction,
			SocialInsuranceDeduction:     req.SocialInsuranceDeduction,
			HousingLoanDeduction:         req.HousingLoanDeduction,
			WithheldTax:                  req.WithheldTax,
		},
		IP: clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toCalculationResponse(calc))
}

// FinaliseCalculation handles POST /employees/:id/yearend/calculations/finalise.
func (h *Handler) FinaliseCalculation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	taxYear, err := parseTaxYear(c.Query("tax_year"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	calc, err := h.svc.FinaliseCalculation(c.Request.Context(), FinaliseCalculationInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TaxYear:    taxYear,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toCalculationResponse(calc))
}

// GetCalculation handles GET /employees/:id/yearend/calculations?tax_year=2026.
func (h *Handler) GetCalculation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	taxYear, err := parseTaxYear(c.Query("tax_year"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	calc, err := h.svc.GetCalculation(c.Request.Context(), tenantID, empID, taxYear)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toCalculationResponse(calc))
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

type generateReportRequest struct {
	TaxYear int    `json:"tax_year" validate:"required,min=2000,max=2100"`
	Format  string `json:"format"   validate:"required,oneof=csv pdf"`
}

// ReportResponse is the JSON representation of a report record.
type ReportResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	EmployeeID  *uuid.UUID `json:"employee_id,omitempty"`
	TaxYear     int        `json:"tax_year"`
	ReportType  string     `json:"report_type"`
	CalcID      *uuid.UUID `json:"calc_id,omitempty"`
	ContentRef  string     `json:"content_ref"`
	Format      string     `json:"format"`
	GeneratedAt string     `json:"generated_at"`
}

func toReportResponse(r *Report) ReportResponse {
	resp := ReportResponse{
		ID:          r.ID,
		TenantID:    r.TenantID,
		EmployeeID:  r.EmployeeID,
		TaxYear:     r.TaxYear,
		ReportType:  r.ReportType,
		CalcID:      r.CalcID,
		ContentRef:  r.ContentRef,
		Format:      r.Format,
		GeneratedAt: r.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	return resp
}

// GenerateWithholdingSlip handles POST /employees/:id/yearend/reports/withholding-slip.
// For CSV: returns the CSV inline.  For PDF: returns the report metadata (scaffold).
func (h *Handler) GenerateWithholdingSlip(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req generateReportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	report, content, err := h.svc.GenerateWithholdingSlip(c.Request.Context(), GenerateWithholdingSlipInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TaxYear:    req.TaxYear,
		Format:     req.Format,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}

	if req.Format == ReportFormatCSV && len(content) > 0 {
		c.Header("Content-Disposition",
			fmt.Sprintf("attachment; filename=\"withholding_slip_%d.csv\"", req.TaxYear))
		c.Data(http.StatusOK, "text/csv; charset=utf-8", content)
		return
	}
	// PDF scaffold or no inline content: return the report metadata.
	c.JSON(http.StatusCreated, toReportResponse(report))
}

// ---------------------------------------------------------------------------
// Payroll SaaS push
// ---------------------------------------------------------------------------

type pushToPayrollRequest struct {
	TaxYear int `json:"tax_year" validate:"required,min=2000,max=2100"`
}

// PayrollPushResponse is the JSON representation of a payroll push record.
type PayrollPushResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenant_id"`
	EmployeeID  uuid.UUID  `json:"employee_id"`
	TaxYear     int        `json:"tax_year"`
	CalcID      uuid.UUID  `json:"calc_id"`
	Provider    string     `json:"provider"`
	Status      string     `json:"status"`
	ProviderRef string     `json:"provider_ref"`
	PushedAt    *string    `json:"pushed_at,omitempty"`
	CreatedAt   string     `json:"created_at"`
	UpdatedAt   string     `json:"updated_at"`
}

func toPayrollPushResponse(p *PayrollPush) PayrollPushResponse {
	r := PayrollPushResponse{
		ID:          p.ID,
		TenantID:    p.TenantID,
		EmployeeID:  p.EmployeeID,
		TaxYear:     p.TaxYear,
		CalcID:      p.CalcID,
		Provider:    p.Provider,
		Status:      p.Status,
		ProviderRef: p.ProviderRef,
		CreatedAt:   p.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   p.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if p.PushedAt != nil {
		s := p.PushedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.PushedAt = &s
	}
	return r
}

// PushToPayroll handles POST /employees/:id/yearend/payroll-push.
func (h *Handler) PushToPayroll(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req pushToPayrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	push, err := h.svc.PushToPayroll(c.Request.Context(), PushToPayrollInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		TaxYear:    req.TaxYear,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toPayrollPushResponse(push))
}
