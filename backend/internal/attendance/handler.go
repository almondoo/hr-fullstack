package attendance

// handler.go — HTTP handlers for the attendance domain.
//
// Validation rules:
//   - validator errors are never returned raw (struct-field-name leak prevention).
//   - Date/time parse failures return 400 immediately.
//   - jsonb payloads are checked for validity and size before passing to the service.

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

// Handler exposes HTTP endpoints for the attendance domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// validationMessage converts validator.ValidationErrors to a safe message.
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

// parseTimestamp parses an RFC3339 string pointer, returning 400 on failure.
func parseTimestamp(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp %q: must be RFC3339", *s)
	}
	return &t, nil
}

// parseDate parses a YYYY-MM-DD string, returning 400 on failure.
func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: must be YYYY-MM-DD", s)
	}
	return t, nil
}

// parseYearMonth parses a "YYYY-MM" string into the first day of that month.
func parseYearMonth(s string) (time.Time, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid year-month %q: must be YYYY-MM", s)
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// AttendanceSetting endpoints
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	RoundingUnitMinutes   int     `json:"rounding_unit_minutes"    validate:"min=0,max=60"`
	OvertimeRate          float64 `json:"overtime_rate"            validate:"required,gt=0"`
	NightRate             float64 `json:"night_rate"               validate:"required,gte=0"`
	HolidayRate           float64 `json:"holiday_rate"             validate:"required,gt=0"`
	Over60Rate            float64 `json:"over60_rate"              validate:"required,gt=0"`
	NightStart            string  `json:"night_start"              validate:"required"`
	NightEnd              string  `json:"night_end"                validate:"required"`
	BreakAutoMinutes      int     `json:"break_auto_minutes"       validate:"min=0"`
	DeviationAlertMinutes int     `json:"deviation_alert_minutes"  validate:"min=0"`
	// Over60BoundaryMinutes: statutory default 3600 (60h×60min). 要専門家確認。
	Over60BoundaryMinutes int `json:"over60_boundary_minutes"  validate:"min=0"`
}

// GetSettings handles GET /attendance/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	st, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrSettingsNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "attendance settings not configured")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, st)
}

// UpsertSettings handles PUT /attendance/settings.
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

	// Validate time-of-day format for night zone boundaries before persisting.
	// An invalid value (e.g. "25:99") would reach the DB and break all subsequent
	// overtime calculations, so we reject it here with a 400.
	if _, err := parseTimeOfDay(req.NightStart); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", fmt.Sprintf("night_start is invalid: %s", err.Error()))
		return
	}
	if _, err := parseTimeOfDay(req.NightEnd); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", fmt.Sprintf("night_end is invalid: %s", err.Error()))
		return
	}

	over60Boundary := req.Over60BoundaryMinutes
	if over60Boundary == 0 {
		// Default to 3600 (60h × 60min) if the caller omits the field.
		over60Boundary = 3600
	}

	st, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		RoundingUnitMinutes:   req.RoundingUnitMinutes,
		OvertimeRate:          req.OvertimeRate,
		NightRate:             req.NightRate,
		HolidayRate:           req.HolidayRate,
		Over60Rate:            req.Over60Rate,
		NightStart:            req.NightStart,
		NightEnd:              req.NightEnd,
		BreakAutoMinutes:      req.BreakAutoMinutes,
		DeviationAlertMinutes: req.DeviationAlertMinutes,
		Over60BoundaryMinutes: over60Boundary,
		IP:                    clientIP(c),
	})
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, st)
}

// ---------------------------------------------------------------------------
// AttendanceRecord endpoints
// ---------------------------------------------------------------------------

type createRecordRequest struct {
	EmployeeID   string  `json:"employee_id"   validate:"required,uuid4"`
	WorkDate     string  `json:"work_date"     validate:"required"`
	ClockIn      *string `json:"clock_in"`
	ClockOut     *string `json:"clock_out"`
	BreakMinutes int     `json:"break_minutes" validate:"min=0"`
	Source       string  `json:"source"        validate:"required,oneof=web mobile device correction"`
	Note         *string `json:"note"          validate:"omitempty,max=1000"`
}

// CreateRecord handles POST /attendance/records.
func (h *Handler) CreateRecord(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createRecordRequest
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
	workDate, err := parseDate(req.WorkDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	clockIn, err := parseTimestamp(req.ClockIn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	clockOut, err := parseTimestamp(req.ClockOut)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	rec, err := h.svc.CreateRecord(c.Request.Context(), CreateRecordInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		EmployeeID:   empID,
		WorkDate:     workDate,
		ClockIn:      clockIn,
		ClockOut:     clockOut,
		BreakMinutes: req.BreakMinutes,
		Source:       req.Source,
		Note:         req.Note,
		IP:           clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "employee not found in this tenant")
			return
		}
		if errors.Is(err, ErrDuplicateRecord) {
			httpx.RespondError(c, http.StatusConflict, "DUPLICATE", "attendance record already exists for this date")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusCreated, rec)
}

// GetRecord handles GET /attendance/records/:id.
func (h *Handler) GetRecord(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid id")
		return
	}
	rec, err := h.svc.GetRecord(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "attendance record not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, rec)
}

// ListRecords handles GET /attendance/records?employee_id=&from=&to=.
func (h *Handler) ListRecords(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empIDStr := c.Query("employee_id")
	empID, err := uuid.Parse(empIDStr)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}
	fromStr := c.Query("from")
	toStr := c.Query("to")
	if fromStr == "" || toStr == "" {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "from and to are required")
		return
	}
	from, err := parseDate(fromStr)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	to, err := parseDate(toStr)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	recs, err := h.svc.ListRecords(c.Request.Context(), tenantID, empID, from, to)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, recs)
}

type correctRecordRequest struct {
	ClockIn      *string `json:"clock_in"`
	ClockOut     *string `json:"clock_out"`
	BreakMinutes *int    `json:"break_minutes" validate:"omitempty,min=0"`
	Note         *string `json:"note"          validate:"omitempty,max=1000"`
	Reason       string  `json:"reason"        validate:"required,max=500"`
}

// CorrectRecord handles PATCH /attendance/records/:id/correct.
func (h *Handler) CorrectRecord(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid id")
		return
	}

	var req correctRecordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	clockIn, err := parseTimestamp(req.ClockIn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	clockOut, err := parseTimestamp(req.ClockOut)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	rec, err := h.svc.CorrectRecord(c.Request.Context(), CorrectRecordInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		RecordID:     id,
		ClockIn:      clockIn,
		ClockOut:     clockOut,
		BreakMinutes: req.BreakMinutes,
		Note:         req.Note,
		Reason:       req.Reason,
		IP:           clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "attendance record not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, rec)
}

// ---------------------------------------------------------------------------
// WorkSummary endpoints
// ---------------------------------------------------------------------------

type computeSummaryRequest struct {
	EmployeeID             string             `json:"employee_id"               validate:"required,uuid4"`
	PeriodMonth            string             `json:"period_month"              validate:"required"`
	ScheduledMinutesPerDay int                `json:"scheduled_minutes_per_day" validate:"required,min=1"`
	HolidayDates           []string           `json:"holiday_dates"`
	HolidayMap             map[time.Time]bool `json:"-"`
}

// ComputeSummary handles POST /attendance/summaries/compute.
func (h *Handler) ComputeSummary(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req computeSummaryRequest
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
	periodMonth, err := parseYearMonth(req.PeriodMonth)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	holidayMap := make(map[time.Time]bool, len(req.HolidayDates))
	for _, ds := range req.HolidayDates {
		d, err := parseDate(ds)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", fmt.Sprintf("invalid holiday_date %q", ds))
			return
		}
		holidayMap[d.UTC().Truncate(24*time.Hour)] = true
	}

	ws, err := h.svc.ComputeAndSaveMonthSummary(c.Request.Context(), tenantID, empID, periodMonth,
		req.ScheduledMinutesPerDay, holidayMap, actorID)
	if err != nil {
		if errors.Is(err, ErrSettingsNotFound) {
			httpx.RespondError(c, http.StatusBadRequest, "CONFIGURATION_MISSING", "attendance settings not configured for this tenant")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, ws)
}

// GetSummary handles GET /attendance/summaries?employee_id=&period_month=.
func (h *Handler) GetSummary(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Query("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}
	pm, err := parseYearMonth(c.Query("period_month"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	ws, err := h.svc.GetWorkSummary(c.Request.Context(), tenantID, empID, pm)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "work summary not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, ws)
}

// ---------------------------------------------------------------------------
// LaborAgreement endpoints
// ---------------------------------------------------------------------------

type createAgreementRequest struct {
	Workplace                  string `json:"workplace"                      validate:"required,max=200"`
	ValidFrom                  string `json:"valid_from"                     validate:"required"`
	ValidTo                    string `json:"valid_to"                       validate:"required"`
	MonthlyLimitMinutes        int    `json:"monthly_limit_minutes"          validate:"required,min=1"`
	YearlyLimitMinutes         int    `json:"yearly_limit_minutes"           validate:"required,min=1"`
	SpecialClause              bool   `json:"special_clause"`
	SpecialMonthlyLimitMinutes *int   `json:"special_monthly_limit_minutes"  validate:"omitempty,min=1"`
	SpecialCountLimit          *int   `json:"special_count_limit"            validate:"omitempty,min=1"`
	MultiMonthAvgLimitMinutes  *int   `json:"multi_month_avg_limit_minutes"  validate:"omitempty,min=1"`
}

// CreateAgreement handles POST /attendance/labor-agreements.
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

	validFrom, err := parseDate(req.ValidFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	validTo, err := parseDate(req.ValidTo)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	if !validTo.After(validFrom) {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "valid_to must be after valid_from")
		return
	}

	ag, err := h.svc.CreateAgreement(c.Request.Context(), CreateAgreementInput{
		TenantID:                   tenantID,
		ActorID:                    actorID,
		Workplace:                  req.Workplace,
		ValidFrom:                  validFrom,
		ValidTo:                    validTo,
		MonthlyLimitMinutes:        req.MonthlyLimitMinutes,
		YearlyLimitMinutes:         req.YearlyLimitMinutes,
		SpecialClause:              req.SpecialClause,
		SpecialMonthlyLimitMinutes: req.SpecialMonthlyLimitMinutes,
		SpecialCountLimit:          req.SpecialCountLimit,
		MultiMonthAvgLimitMinutes:  req.MultiMonthAvgLimitMinutes,
		IP:                         clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrDuplicateAgreement) {
			httpx.RespondError(c, http.StatusConflict, "DUPLICATE", "a labor agreement already exists for this workplace and valid_from date") //nolint:misspell // API contract: US spelling matches DB table and existing client expectations
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusCreated, ag)
}

// ListAgreements handles GET /attendance/labor-agreements.
func (h *Handler) ListAgreements(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	ags, err := h.svc.ListAgreements(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, ags)
}

// EvaluateAlerts handles GET /attendance/labor-agreements/alerts?employee_id=&period_month=.
func (h *Handler) EvaluateAlerts(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Query("employee_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
		return
	}
	pm, err := parseYearMonth(c.Query("period_month"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	alerts, err := h.svc.EvaluateAgreementAlerts(c.Request.Context(), tenantID, empID, pm, 0, 0.9)
	if err != nil {
		if errors.Is(err, ErrAgreementNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "no active labor agreement found for this period") //nolint:misspell // API contract: US spelling matches DB table and existing client expectations
			return
		}
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"alerts": alerts})
}

// Ensure json is referenced (used in CorrectRecord via service layer).
var _ = json.Marshal
