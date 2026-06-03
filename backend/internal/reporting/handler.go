package reporting

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// Handler exposes HTTP endpoints for the reporting domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func validationMessage(err error) string {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) && len(ve) > 0 {
		e := ve[0]
		return fmt.Sprintf("validation failed on field '%s' (%s)", e.Field(), e.Tag())
	}
	return "validation failed"
}

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

func parseDateParam(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

// mapError maps a service error to a unified HTTP error response.
func mapError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrUnknownReport):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "unknown report key")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "invalid status transition")
	case errors.Is(err, ErrOverlap):
		httpx.RespondError(c, http.StatusConflict, "OVERLAP", "overlapping effective period")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	default:
		httpx.RespondInternalError(c)
	}
}

// ---------------------------------------------------------------------------
// Report definitions
// ---------------------------------------------------------------------------

type upsertReportDefinitionRequest struct {
	ReportKey    string          `json:"report_key"   validate:"required,oneof=employee_roster attendance_monthly leave_status billing_summary"`
	Name         string          `json:"name"         validate:"required,max=200"`
	ParamsSchema json.RawMessage `json:"params_schema"`
	Columns      json.RawMessage `json:"columns"`
	Active       *bool           `json:"active"`
}

// ReportDefinitionResponse is the JSON representation of a report definition.
type ReportDefinitionResponse struct {
	ID           uuid.UUID       `json:"id"`
	TenantID     uuid.UUID       `json:"tenant_id"`
	ReportKey    string          `json:"report_key"`
	Name         string          `json:"name"`
	ParamsSchema json.RawMessage `json:"params_schema"`
	Columns      json.RawMessage `json:"columns"`
	Active       bool            `json:"active"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

func toReportDefinitionResponse(d *ReportDefinition) ReportDefinitionResponse {
	params := json.RawMessage(d.ParamsSchemaJSON)
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	cols := json.RawMessage(d.ColumnsJSON)
	if len(cols) == 0 {
		cols = json.RawMessage(`[]`)
	}
	return ReportDefinitionResponse{
		ID:           d.ID,
		TenantID:     d.TenantID,
		ReportKey:    d.ReportKey,
		Name:         d.Name,
		ParamsSchema: params,
		Columns:      cols,
		Active:       d.Active,
		CreatedAt:    d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:    d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// UpsertReportDefinition handles POST /reports/definitions.
func (h *Handler) UpsertReportDefinition(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req upsertReportDefinitionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.ParamsSchema); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "params_schema: "+err.Error())
		return
	}
	if err := validateJSON(req.Columns); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "columns: "+err.Error())
		return
	}

	active := true
	if req.Active != nil {
		active = *req.Active
	}

	def, err := h.svc.UpsertReportDefinition(c.Request.Context(), UpsertReportDefinitionInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		ReportKey:        req.ReportKey,
		Name:             req.Name,
		ParamsSchemaJSON: []byte(req.ParamsSchema),
		ColumnsJSON:      []byte(req.Columns),
		Active:           active,
		IP:               clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, toReportDefinitionResponse(def))
}

// ListReportDefinitions handles GET /reports/definitions.
func (h *Handler) ListReportDefinitions(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	defs, err := h.svc.ListReportDefinitions(c.Request.Context(), tenantID)
	if err != nil {
		mapError(c, err)
		return
	}
	items := make([]ReportDefinitionResponse, len(defs))
	for i := range defs {
		items[i] = toReportDefinitionResponse(&defs[i])
	}
	c.JSON(http.StatusOK, gin.H{"definitions": items})
}

// ---------------------------------------------------------------------------
// Report execution
// ---------------------------------------------------------------------------

// RunReport handles GET /reports/run/:report_key.
// Query params: include_sensitive=true, year=YYYY, month=MM.
func (h *Handler) RunReport(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	reportKey := c.Param("report_key")
	includeSensitive := c.Query("include_sensitive") == "true"
	year, _ := strconv.Atoi(c.Query("year"))
	month, _ := strconv.Atoi(c.Query("month"))

	result, err := h.svc.RunReport(c.Request.Context(), RunReportInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		ReportKey:        reportKey,
		IncludeSensitive: includeSensitive,
		Year:             year,
		Month:            month,
		IP:               clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Export jobs
// ---------------------------------------------------------------------------

type createExportJobRequest struct {
	ReportKey        string          `json:"report_key"        validate:"required,oneof=employee_roster attendance_monthly leave_status billing_summary"`
	Format           string          `json:"format"            validate:"required,oneof=csv xlsx"`
	Params           json.RawMessage `json:"params"`
	IncludeSensitive bool            `json:"include_sensitive"`
}

// ExportJobResponse is the JSON representation of an export job.
type ExportJobResponse struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	ReportKey         string     `json:"report_key"`
	Format            string     `json:"format"`
	Status            string     `json:"status"`
	RequestedByUserID *uuid.UUID `json:"requested_by_user_id,omitempty"`
	ResultDocumentID  *uuid.UUID `json:"result_document_id,omitempty"`
	IncludeSensitive  bool       `json:"include_sensitive"`
	ErrorMessage      *string    `json:"error_message,omitempty"`
	CreatedAt         string     `json:"created_at"`
	CompletedAt       *string    `json:"completed_at,omitempty"`
}

func toExportJobResponse(j *ExportJob) ExportJobResponse {
	r := ExportJobResponse{
		ID:                j.ID,
		TenantID:          j.TenantID,
		ReportKey:         j.ReportKey,
		Format:            j.Format,
		Status:            j.Status,
		RequestedByUserID: j.RequestedByUserID,
		ResultDocumentID:  j.ResultDocumentID,
		IncludeSensitive:  j.IncludeSensitive,
		ErrorMessage:      j.ErrorMessage,
		CreatedAt:         j.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if j.CompletedAt != nil {
		s := j.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
		r.CompletedAt = &s
	}
	return r
}

// CreateExportJob handles POST /reports/exports.
func (h *Handler) CreateExportJob(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createExportJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Params); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "params: "+err.Error())
		return
	}

	job, err := h.svc.CreateExportJob(c.Request.Context(), CreateExportJobInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		ReportKey:        req.ReportKey,
		Format:           req.Format,
		ParamsJSON:       []byte(req.Params),
		IncludeSensitive: req.IncludeSensitive,
		IP:               clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, toExportJobResponse(job))
}

// GetExportJob handles GET /reports/exports/:job_id.
func (h *Handler) GetExportJob(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	jobID, err := uuid.Parse(c.Param("job_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job id")
		return
	}

	job, err := h.svc.GetExportJob(c.Request.Context(), tenantID, jobID)
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, toExportJobResponse(job))
}

// ProcessExportJob handles POST /reports/exports/:job_id/process.
// Query params: year=YYYY, month=MM (scope attendance/leave reports).
// Returns the job and the document UUID (opaque download reference).
func (h *Handler) ProcessExportJob(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	jobID, err := uuid.Parse(c.Param("job_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid job id")
		return
	}
	year, _ := strconv.Atoi(c.Query("year"))
	month, _ := strconv.Atoi(c.Query("month"))

	// The generated file bytes are returned to the caller (intended for the
	// ST-FND-10 document store); they are never logged or returned in the API
	// envelope.  The opaque document UUID is the download reference.
	job, _, err := h.svc.ProcessExportJob(c.Request.Context(), ProcessExportJobInput{
		TenantID: tenantID,
		ActorID:  actorID,
		JobID:    jobID,
		Year:     year,
		Month:    month,
		IP:       clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, toExportJobResponse(job))
}

// ---------------------------------------------------------------------------
// Company calendars
// ---------------------------------------------------------------------------

type createCalendarRequest struct {
	Name                  string          `json:"name"          validate:"required,max=200"`
	FiscalYear            int             `json:"fiscal_year"   validate:"required"`
	DefaultWeeklyHolidays json.RawMessage `json:"default_weekly_holidays"`
	EffectiveFrom         string          `json:"effective_from" validate:"required"`
	EffectiveTo           *string         `json:"effective_to"`
}

// CalendarResponse is the JSON representation of a company calendar.
type CalendarResponse struct {
	ID            uuid.UUID       `json:"id"`
	TenantID      uuid.UUID       `json:"tenant_id"`
	Name          string          `json:"name"`
	FiscalYear    int             `json:"fiscal_year"`
	WeeklyHols    json.RawMessage `json:"default_weekly_holidays"`
	Active        bool            `json:"active"`
	EffectiveFrom string          `json:"effective_from"`
	EffectiveTo   *string         `json:"effective_to,omitempty"`
}

func toCalendarResponse(cal *CompanyCalendar) CalendarResponse {
	wh := json.RawMessage(cal.DefaultWeeklyHolidaysJSON)
	if len(wh) == 0 {
		wh = json.RawMessage(`{"weekdays":[0,6]}`)
	}
	r := CalendarResponse{
		ID:            cal.ID,
		TenantID:      cal.TenantID,
		Name:          cal.Name,
		FiscalYear:    cal.FiscalYear,
		WeeklyHols:    wh,
		Active:        cal.Active,
		EffectiveFrom: cal.EffectiveFrom.Format("2006-01-02"),
	}
	if cal.EffectiveTo != nil {
		s := cal.EffectiveTo.Format("2006-01-02")
		r.EffectiveTo = &s
	}
	return r
}

// CreateCalendar handles POST /calendars.
func (h *Handler) CreateCalendar(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createCalendarRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.DefaultWeeklyHolidays); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "default_weekly_holidays: "+err.Error())
		return
	}

	effFrom, err := parseDateParam(req.EffectiveFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_from must be YYYY-MM-DD")
		return
	}
	var effTo *time.Time
	if req.EffectiveTo != nil && *req.EffectiveTo != "" {
		t, err := parseDateParam(*req.EffectiveTo)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_to must be YYYY-MM-DD")
			return
		}
		effTo = &t
	}

	cal, err := h.svc.CreateCalendar(c.Request.Context(), CreateCalendarInput{
		TenantID:              tenantID,
		ActorID:               actorID,
		Name:                  req.Name,
		FiscalYear:            req.FiscalYear,
		DefaultWeeklyHolidays: []byte(req.DefaultWeeklyHolidays),
		EffectiveFrom:         effFrom,
		EffectiveTo:           effTo,
		IP:                    clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toCalendarResponse(cal))
}

type addCalendarDayRequest struct {
	Date    string `json:"date"     validate:"required"`
	DayType string `json:"day_type" validate:"required,oneof=holiday business_day special_holiday"`
	Label   string `json:"label"    validate:"max=200"`
}

// CalendarDayResponse is the JSON representation of a calendar day override.
type CalendarDayResponse struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenant_id"`
	CalendarID uuid.UUID `json:"calendar_id"`
	Date       string    `json:"date"`
	DayType    string    `json:"day_type"`
	Label      string    `json:"label"`
}

func toCalendarDayResponse(d *CalendarDay) CalendarDayResponse {
	return CalendarDayResponse{
		ID:         d.ID,
		TenantID:   d.TenantID,
		CalendarID: d.CalendarID,
		Date:       d.Date.Format("2006-01-02"),
		DayType:    d.DayType,
		Label:      d.Label,
	}
}

// AddCalendarDay handles POST /calendars/:calendar_id/days.
func (h *Handler) AddCalendarDay(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	calendarID, err := uuid.Parse(c.Param("calendar_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid calendar id")
		return
	}

	var req addCalendarDayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	date, err := parseDateParam(req.Date)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "date must be YYYY-MM-DD")
		return
	}

	day, err := h.svc.AddCalendarDay(c.Request.Context(), AddCalendarDayInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		CalendarID: calendarID,
		Date:       date,
		DayType:    req.DayType,
		Label:      req.Label,
		IP:         clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toCalendarDayResponse(day))
}

// IsBusinessDay handles GET /calendars/:calendar_id/business-day?date=YYYY-MM-DD.
func (h *Handler) IsBusinessDay(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	calendarID, err := uuid.Parse(c.Param("calendar_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid calendar id")
		return
	}
	date, err := parseDateParam(c.Query("date"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "date must be YYYY-MM-DD")
		return
	}

	business, err := h.svc.IsBusinessDay(c.Request.Context(), tenantID, calendarID, date)
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"date":         date.Format("2006-01-02"),
		"business_day": business,
	})
}

// ---------------------------------------------------------------------------
// Work patterns / shifts / assignments
// ---------------------------------------------------------------------------

type createWorkPatternRequest struct {
	Name             string          `json:"name"              validate:"required,max=200"`
	PatternType      string          `json:"pattern_type"      validate:"required,oneof=fixed flex variable discretionary shift"`
	ScheduledMinutes int             `json:"scheduled_minutes" validate:"gte=0"`
	BreakMinutes     int             `json:"break_minutes"     validate:"gte=0"`
	CoreTime         json.RawMessage `json:"core_time"`
	Settings         json.RawMessage `json:"settings"`
	EffectiveFrom    string          `json:"effective_from"    validate:"required"`
	EffectiveTo      *string         `json:"effective_to"`
}

// WorkPatternResponse is the JSON representation of a work pattern.
type WorkPatternResponse struct {
	ID               uuid.UUID       `json:"id"`
	TenantID         uuid.UUID       `json:"tenant_id"`
	Name             string          `json:"name"`
	PatternType      string          `json:"pattern_type"`
	ScheduledMinutes int             `json:"scheduled_minutes"`
	BreakMinutes     int             `json:"break_minutes"`
	CoreTime         json.RawMessage `json:"core_time"`
	Settings         json.RawMessage `json:"settings"`
	Active           bool            `json:"active"`
	EffectiveFrom    string          `json:"effective_from"`
	EffectiveTo      *string         `json:"effective_to,omitempty"`
}

func toWorkPatternResponse(wp *WorkPattern) WorkPatternResponse {
	core := json.RawMessage(wp.CoreTimeJSON)
	if len(core) == 0 {
		core = json.RawMessage(`{}`)
	}
	settings := json.RawMessage(wp.SettingsJSON)
	if len(settings) == 0 {
		settings = json.RawMessage(`{}`)
	}
	r := WorkPatternResponse{
		ID:               wp.ID,
		TenantID:         wp.TenantID,
		Name:             wp.Name,
		PatternType:      wp.PatternType,
		ScheduledMinutes: wp.ScheduledMinutes,
		BreakMinutes:     wp.BreakMinutes,
		CoreTime:         core,
		Settings:         settings,
		Active:           wp.Active,
		EffectiveFrom:    wp.EffectiveFrom.Format("2006-01-02"),
	}
	if wp.EffectiveTo != nil {
		s := wp.EffectiveTo.Format("2006-01-02")
		r.EffectiveTo = &s
	}
	return r
}

// CreateWorkPattern handles POST /work-patterns.
func (h *Handler) CreateWorkPattern(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createWorkPatternRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.CoreTime); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "core_time: "+err.Error())
		return
	}
	if err := validateJSON(req.Settings); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "settings: "+err.Error())
		return
	}

	effFrom, err := parseDateParam(req.EffectiveFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_from must be YYYY-MM-DD")
		return
	}
	var effTo *time.Time
	if req.EffectiveTo != nil && *req.EffectiveTo != "" {
		t, err := parseDateParam(*req.EffectiveTo)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_to must be YYYY-MM-DD")
			return
		}
		effTo = &t
	}

	wp, err := h.svc.CreateWorkPattern(c.Request.Context(), CreateWorkPatternInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		Name:             req.Name,
		PatternType:      req.PatternType,
		ScheduledMinutes: req.ScheduledMinutes,
		BreakMinutes:     req.BreakMinutes,
		CoreTimeJSON:     []byte(req.CoreTime),
		SettingsJSON:     []byte(req.Settings),
		EffectiveFrom:    effFrom,
		EffectiveTo:      effTo,
		IP:               clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toWorkPatternResponse(wp))
}

type addShiftPatternRequest struct {
	Name             string `json:"name"              validate:"required,max=200"`
	StartTime        string `json:"start_time"        validate:"required"`
	EndTime          string `json:"end_time"          validate:"required"`
	BreakMinutes     int    `json:"break_minutes"     validate:"gte=0"`
	ScheduledMinutes int    `json:"scheduled_minutes" validate:"gte=0"`
}

// ShiftPatternResponse is the JSON representation of a shift pattern.
type ShiftPatternResponse struct {
	ID               uuid.UUID `json:"id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	WorkPatternID    uuid.UUID `json:"work_pattern_id"`
	Name             string    `json:"name"`
	StartTime        string    `json:"start_time"`
	EndTime          string    `json:"end_time"`
	BreakMinutes     int       `json:"break_minutes"`
	ScheduledMinutes int       `json:"scheduled_minutes"`
}

func toShiftPatternResponse(sp *ShiftPattern) ShiftPatternResponse {
	return ShiftPatternResponse{
		ID:               sp.ID,
		TenantID:         sp.TenantID,
		WorkPatternID:    sp.WorkPatternID,
		Name:             sp.Name,
		StartTime:        sp.StartTime,
		EndTime:          sp.EndTime,
		BreakMinutes:     sp.BreakMinutes,
		ScheduledMinutes: sp.ScheduledMinutes,
	}
}

// AddShiftPattern handles POST /work-patterns/:work_pattern_id/shifts.
func (h *Handler) AddShiftPattern(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	wpID, err := uuid.Parse(c.Param("work_pattern_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid work pattern id")
		return
	}

	var req addShiftPatternRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	sp, err := h.svc.AddShiftPattern(c.Request.Context(), AddShiftPatternInput{
		TenantID:         tenantID,
		ActorID:          actorID,
		WorkPatternID:    wpID,
		Name:             req.Name,
		StartTime:        req.StartTime,
		EndTime:          req.EndTime,
		BreakMinutes:     req.BreakMinutes,
		ScheduledMinutes: req.ScheduledMinutes,
		IP:               clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toShiftPatternResponse(sp))
}

type assignWorkPatternRequest struct {
	WorkPatternID string  `json:"work_pattern_id" validate:"required"`
	EffectiveFrom string  `json:"effective_from"  validate:"required"`
	EffectiveTo   *string `json:"effective_to"`
}

// WorkAssignmentResponse is the JSON representation of an employee work assignment.
type WorkAssignmentResponse struct {
	ID            uuid.UUID `json:"id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	EmployeeID    uuid.UUID `json:"employee_id"`
	WorkPatternID uuid.UUID `json:"work_pattern_id"`
	EffectiveFrom string    `json:"effective_from"`
	EffectiveTo   *string   `json:"effective_to,omitempty"`
}

func toWorkAssignmentResponse(a *EmployeeWorkAssignment) WorkAssignmentResponse {
	r := WorkAssignmentResponse{
		ID:            a.ID,
		TenantID:      a.TenantID,
		EmployeeID:    a.EmployeeID,
		WorkPatternID: a.WorkPatternID,
		EffectiveFrom: a.EffectiveFrom.Format("2006-01-02"),
	}
	if a.EffectiveTo != nil {
		s := a.EffectiveTo.Format("2006-01-02")
		r.EffectiveTo = &s
	}
	return r
}

// AssignWorkPattern handles POST /employees/:id/work-pattern.
func (h *Handler) AssignWorkPattern(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req assignWorkPatternRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	wpID, err := uuid.Parse(req.WorkPatternID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid work_pattern_id")
		return
	}
	effFrom, err := parseDateParam(req.EffectiveFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_from must be YYYY-MM-DD")
		return
	}
	var effTo *time.Time
	if req.EffectiveTo != nil && *req.EffectiveTo != "" {
		t, err := parseDateParam(*req.EffectiveTo)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_to must be YYYY-MM-DD")
			return
		}
		effTo = &t
	}

	a, err := h.svc.AssignWorkPattern(c.Request.Context(), AssignWorkPatternInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		EmployeeID:    empID,
		WorkPatternID: wpID,
		EffectiveFrom: effFrom,
		EffectiveTo:   effTo,
		IP:            clientIP(c),
	})
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toWorkAssignmentResponse(a))
}

// ResolveWorkPattern handles GET /employees/:id/work-pattern?date=YYYY-MM-DD.
func (h *Handler) ResolveWorkPattern(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	dateStr := c.Query("date")
	date := time.Now().UTC()
	if dateStr != "" {
		d, err := parseDateParam(dateStr)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "date must be YYYY-MM-DD")
			return
		}
		date = d
	}

	wp, err := h.svc.ResolveWorkPattern(c.Request.Context(), tenantID, empID, date)
	if err != nil {
		mapError(c, err)
		return
	}
	c.JSON(http.StatusOK, toWorkPatternResponse(wp))
}
