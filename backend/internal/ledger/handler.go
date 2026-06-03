package ledger

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
const maxJSONBytes = 64 * 1024

// Handler exposes HTTP endpoints for the ledger domain.
type Handler struct {
	svc *Service
	// importer is the payroll-SaaS adapter used by ImportPayroll.
	// MVP wires a mock; real providers are P3.
	importer PayrollImporter
}

// NewHandler constructs a Handler with the given payroll importer.
func NewHandler(svc *Service, importer PayrollImporter) *Handler {
	return &Handler{svc: svc, importer: importer}
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
	case errors.Is(err, ErrFinalised):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "record is finalised and immutable")
	case errors.Is(err, ErrInvalidLedger):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "unknown ledger type")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	default:
		httpx.RespondInternalError(c)
	}
	return true
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	RetentionYears    int             `json:"retention_years"    validate:"omitempty,min=1,max=100"`
	RetentionBasis    string          `json:"retention_basis"    validate:"omitempty,oneof=resignation last_entry last_attendance"`
	ElectronicStorage json.RawMessage `json:"electronic_storage"`
}

// SettingsResponse is the JSON representation of ledger settings.
type SettingsResponse struct {
	ID                uuid.UUID       `json:"id"`
	TenantID          uuid.UUID       `json:"tenant_id"`
	RetentionYears    int             `json:"retention_years"`
	RetentionBasis    string          `json:"retention_basis"`
	ElectronicStorage json.RawMessage `json:"electronic_storage"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
}

func toSettingsResponse(st *Settings) SettingsResponse {
	es := json.RawMessage(st.ElectronicStorageJSON)
	if len(es) == 0 {
		es = json.RawMessage(`{}`)
	}
	r := SettingsResponse{
		ID:                st.ID,
		TenantID:          st.TenantID,
		RetentionYears:    st.DefaultRetentionYears,
		RetentionBasis:    st.DefaultRetentionBasis,
		ElectronicStorage: es,
	}
	if !st.CreatedAt.IsZero() {
		r.CreatedAt = st.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if !st.UpdatedAt.IsZero() {
		r.UpdatedAt = st.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return r
}

// UpsertSettings handles PUT /ledgers/settings.
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
	if err := validateJSON(req.ElectronicStorage); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "electronic_storage: "+err.Error())
		return
	}

	storage := []byte(req.ElectronicStorage)
	if len(storage) == 0 || string(storage) == "null" {
		storage = []byte(`{}`)
	}

	st, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		RetentionYears:        req.RetentionYears,
		RetentionBasis:        req.RetentionBasis,
		ElectronicStorageJSON: storage,
		IP:                    clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(st))
}

// GetSettings handles GET /ledgers/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	st, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(st))
}

// ---------------------------------------------------------------------------
// Payroll import
// ---------------------------------------------------------------------------

type importPayrollRequest struct {
	Period string `json:"period" validate:"required,max=64"`
}

// PayrollLinkResponse is the JSON representation of a payroll import link.
type PayrollLinkResponse struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	EmployeeID   uuid.UUID       `json:"employee_id"`
	Provider     string          `json:"provider"`
	Period       string          `json:"period"`
	ProviderRef  string          `json:"provider_ref"`
	ImportedData json.RawMessage `json:"imported_data"`
	Status       string          `json:"status"`
	ImportedAt   string          `json:"imported_at"`
}

func toPayrollLinkResponse(l *PayrollLink) PayrollLinkResponse {
	data := json.RawMessage(l.ImportedPayloadJSON)
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return PayrollLinkResponse{
		ID:           l.ID,
		TenantID:     l.TenantID,
		EmployeeID:   l.EmployeeID,
		Provider:     l.Provider,
		Period:       l.Period,
		ProviderRef:  l.ProviderRef,
		ImportedData: data,
		Status:       l.Status,
		ImportedAt:   l.ImportedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// ImportPayroll handles POST /employees/:id/ledgers/payroll-import.
func (h *Handler) ImportPayroll(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req importPayrollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	link, err := h.svc.ImportPayroll(c.Request.Context(), h.importer, ImportPayrollInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		Period:     req.Period,
		IP:         clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toPayrollLinkResponse(link))
}

// ---------------------------------------------------------------------------
// Worker roster
// ---------------------------------------------------------------------------

type buildRosterRequest struct {
	ResignationDate *string `json:"resignation_date"`
}

// WorkerRosterResponse is the JSON representation of a worker roster.
type WorkerRosterResponse struct {
	ID                 uuid.UUID       `json:"id"`
	TenantID           uuid.UUID       `json:"tenant_id"`
	EmployeeID         uuid.UUID       `json:"employee_id"`
	Roster             json.RawMessage `json:"roster"`
	RetentionBasis     string          `json:"retention_basis"`
	RetentionBasisDate *string         `json:"retention_basis_date,omitempty"`
	RetentionUntil     *string         `json:"retention_until,omitempty"`
	Finalised          bool            `json:"finalized"`              //nolint:misspell // JSON tag uses finalized_at (HTTP API contract)
	FinalisedAt        *string         `json:"finalized_at,omitempty"` //nolint:misspell // JSON tag uses finalized_at (HTTP API contract)
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

func toWorkerRosterResponse(r *WorkerRoster) WorkerRosterResponse {
	rj := json.RawMessage(r.RosterJSON)
	if len(rj) == 0 {
		rj = json.RawMessage(`{}`)
	}
	resp := WorkerRosterResponse{
		ID:             r.ID,
		TenantID:       r.TenantID,
		EmployeeID:     r.EmployeeID,
		Roster:         rj,
		RetentionBasis: r.RetentionBasis,
		Finalised:      r.FinalisedAt != nil,
		CreatedAt:      r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      r.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if r.RetentionBasisDate != nil {
		s := r.RetentionBasisDate.Format("2006-01-02")
		resp.RetentionBasisDate = &s
	}
	if r.RetentionUntil != nil {
		s := r.RetentionUntil.Format("2006-01-02")
		resp.RetentionUntil = &s
	}
	if r.FinalisedAt != nil {
		s := r.FinalisedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.FinalisedAt = &s
	}
	return resp
}

// BuildWorkerRoster handles POST /employees/:id/ledgers/roster.
func (h *Handler) BuildWorkerRoster(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req buildRosterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	resignationDate, err := parseDate(req.ResignationDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	roster, err := h.svc.BuildWorkerRoster(c.Request.Context(), BuildRosterInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		ResignationDate: resignationDate,
		IP:              clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toWorkerRosterResponse(roster))
}

// GetWorkerRoster handles GET /employees/:id/ledgers/roster.
func (h *Handler) GetWorkerRoster(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	roster, err := h.svc.GetWorkerRoster(c.Request.Context(), tenantID, empID)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toWorkerRosterResponse(roster))
}

// ---------------------------------------------------------------------------
// Attendance book
// ---------------------------------------------------------------------------

type buildAttendanceBookRequest struct {
	PeriodMonth        string  `json:"period_month"          validate:"required,len=7"`
	LastAttendanceDate *string `json:"last_attendance_date"`
}

// AttendanceBookResponse is the JSON representation of an attendance book.
type AttendanceBookResponse struct {
	ID                 uuid.UUID       `json:"id"`
	TenantID           uuid.UUID       `json:"tenant_id"`
	EmployeeID         uuid.UUID       `json:"employee_id"`
	PeriodMonth        string          `json:"period_month"`
	Book               json.RawMessage `json:"book"`
	RetentionBasis     string          `json:"retention_basis"`
	RetentionBasisDate *string         `json:"retention_basis_date,omitempty"`
	RetentionUntil     *string         `json:"retention_until,omitempty"`
	Finalised          bool            `json:"finalized"`              //nolint:misspell // JSON tag uses finalized_at (HTTP API contract)
	FinalisedAt        *string         `json:"finalized_at,omitempty"` //nolint:misspell // JSON tag uses finalized_at (HTTP API contract)
	CreatedAt          string          `json:"created_at"`
	UpdatedAt          string          `json:"updated_at"`
}

func toAttendanceBookResponse(b *AttendanceBook) AttendanceBookResponse {
	bj := json.RawMessage(b.BookJSON)
	if len(bj) == 0 {
		bj = json.RawMessage(`{}`)
	}
	resp := AttendanceBookResponse{
		ID:             b.ID,
		TenantID:       b.TenantID,
		EmployeeID:     b.EmployeeID,
		PeriodMonth:    b.PeriodMonth,
		Book:           bj,
		RetentionBasis: b.RetentionBasis,
		Finalised:      b.FinalisedAt != nil,
		CreatedAt:      b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      b.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if b.RetentionBasisDate != nil {
		s := b.RetentionBasisDate.Format("2006-01-02")
		resp.RetentionBasisDate = &s
	}
	if b.RetentionUntil != nil {
		s := b.RetentionUntil.Format("2006-01-02")
		resp.RetentionUntil = &s
	}
	if b.FinalisedAt != nil {
		s := b.FinalisedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.FinalisedAt = &s
	}
	return resp
}

// BuildAttendanceBook handles POST /employees/:id/ledgers/attendance-book.
func (h *Handler) BuildAttendanceBook(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req buildAttendanceBookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	lastAttendance, err := parseDate(req.LastAttendanceDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	book, err := h.svc.BuildAttendanceBook(c.Request.Context(), BuildAttendanceBookInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		EmployeeID:         empID,
		PeriodMonth:        req.PeriodMonth,
		LastAttendanceDate: lastAttendance,
		IP:                 clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toAttendanceBookResponse(book))
}

// GetAttendanceBook handles GET /employees/:id/ledgers/attendance-book.
func (h *Handler) GetAttendanceBook(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	periodMonth := c.Query("period_month")
	if periodMonth == "" {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period_month query param is required")
		return
	}

	book, err := h.svc.GetAttendanceBook(c.Request.Context(), tenantID, empID, periodMonth)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toAttendanceBookResponse(book))
}

// ---------------------------------------------------------------------------
// Wage ledger
// ---------------------------------------------------------------------------

type buildWageLedgerRequest struct {
	Period        string  `json:"period"          validate:"required,max=64"`
	LastEntryDate *string `json:"last_entry_date"`
}

// WageLedgerResponse is the JSON representation of a wage ledger.
type WageLedgerResponse struct {
	ID                  uuid.UUID       `json:"id"`
	TenantID            uuid.UUID       `json:"tenant_id"`
	EmployeeID          uuid.UUID       `json:"employee_id"`
	Period              string          `json:"period"`
	Wage                json.RawMessage `json:"wage"`
	SourcePayrollLinkID *uuid.UUID      `json:"source_payroll_link_id,omitempty"`
	RetentionBasis      string          `json:"retention_basis"`
	RetentionBasisDate  *string         `json:"retention_basis_date,omitempty"`
	RetentionUntil      *string         `json:"retention_until,omitempty"`
	Finalised           bool            `json:"finalized"`              //nolint:misspell // JSON tag uses finalized_at (HTTP API contract)
	FinalisedAt         *string         `json:"finalized_at,omitempty"` //nolint:misspell // JSON tag uses finalized_at (HTTP API contract)
	CreatedAt           string          `json:"created_at"`
	UpdatedAt           string          `json:"updated_at"`
}

func toWageLedgerResponse(l *WageLedger) WageLedgerResponse {
	wj := json.RawMessage(l.WageJSON)
	if len(wj) == 0 {
		wj = json.RawMessage(`{}`)
	}
	resp := WageLedgerResponse{
		ID:                  l.ID,
		TenantID:            l.TenantID,
		EmployeeID:          l.EmployeeID,
		Period:              l.Period,
		Wage:                wj,
		SourcePayrollLinkID: l.SourcePayrollLinkID,
		RetentionBasis:      l.RetentionBasis,
		Finalised:           l.FinalisedAt != nil,
		CreatedAt:           l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:           l.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if l.RetentionBasisDate != nil {
		s := l.RetentionBasisDate.Format("2006-01-02")
		resp.RetentionBasisDate = &s
	}
	if l.RetentionUntil != nil {
		s := l.RetentionUntil.Format("2006-01-02")
		resp.RetentionUntil = &s
	}
	if l.FinalisedAt != nil {
		s := l.FinalisedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.FinalisedAt = &s
	}
	return resp
}

// BuildWageLedger handles POST /employees/:id/ledgers/wage.
func (h *Handler) BuildWageLedger(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req buildWageLedgerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	lastEntry, err := parseDate(req.LastEntryDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	ledger, err := h.svc.BuildWageLedger(c.Request.Context(), BuildWageLedgerInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		Period:        req.Period,
		LastEntryDate: lastEntry,
		IP:            clientIP(c),
	})
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusCreated, toWageLedgerResponse(ledger))
}

// GetWageLedger handles GET /employees/:id/ledgers/wage.
func (h *Handler) GetWageLedger(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	period := c.Query("period")
	if period == "" {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period query param is required")
		return
	}

	ledger, err := h.svc.GetWageLedger(c.Request.Context(), tenantID, empID, period)
	if mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, toWageLedgerResponse(ledger))
}

// ---------------------------------------------------------------------------
// Finalise / retention / export
// ---------------------------------------------------------------------------

type finaliseRequest struct {
	LedgerType string `json:"ledger_type" validate:"required,oneof=worker_roster wage_ledger attendance_book"`
	LedgerID   string `json:"ledger_id"   validate:"required"`
}

// Finalise handles POST /ledgers/finalize.
func (h *Handler) Finalise(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req finaliseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	ledgerID, err := uuid.Parse(req.LedgerID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid ledger_id")
		return
	}

	if err := h.svc.Finalise(c.Request.Context(), FinaliseInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		LedgerType: req.LedgerType,
		ID:         ledgerID,
		IP:         clientIP(c),
	}); mapServiceError(c, err) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "ledger finalised"})
}

// RetentionDecisionResponse is the JSON representation of a retention decision.
type RetentionDecisionResponse struct {
	LedgerType         string    `json:"ledger_type"`
	LedgerID           uuid.UUID `json:"ledger_id"`
	RetentionUntil     *string   `json:"retention_until,omitempty"`
	OffboardingExpires *string   `json:"offboarding_expires,omitempty"`
	MustRetain         bool      `json:"must_retain"`
}

// EvaluateRetention handles GET /employees/:id/ledgers/retention.
func (h *Handler) EvaluateRetention(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	asOf := time.Now()
	if v := c.Query("as_of"); v != "" {
		t, perr := time.Parse("2006-01-02", v)
		if perr != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "as_of must be YYYY-MM-DD")
			return
		}
		asOf = t
	}

	decisions, err := h.svc.EvaluateRetentionPrecedence(c.Request.Context(), tenantID, empID, asOf)
	if mapServiceError(c, err) {
		return
	}

	items := make([]RetentionDecisionResponse, len(decisions))
	for i, d := range decisions {
		r := RetentionDecisionResponse{
			LedgerType: d.LedgerType,
			LedgerID:   d.LedgerID,
			MustRetain: d.MustRetain,
		}
		if d.RetentionUntil != nil {
			s := d.RetentionUntil.Format("2006-01-02")
			r.RetentionUntil = &s
		}
		if d.OffboardingExpires != nil {
			s := d.OffboardingExpires.Format("2006-01-02")
			r.OffboardingExpires = &s
		}
		items[i] = r
	}
	c.JSON(http.StatusOK, gin.H{"decisions": items})
}

// ExportWageLedgerCSV handles GET /ledgers/wage/export.
func (h *Handler) ExportWageLedgerCSV(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	period := c.Query("period")
	if period == "" {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "period query param is required")
		return
	}

	csvData, err := h.svc.ExportWageLedgerCSV(c.Request.Context(), tenantID, period)
	if mapServiceError(c, err) {
		return
	}
	c.Header("Content-Disposition", "attachment; filename=\"wage_ledgers.csv\"")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", []byte(csvData))
}
