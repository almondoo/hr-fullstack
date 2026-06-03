package oneonone

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

// Handler exposes HTTP endpoints for the 1on1 domain.
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

// parseRFC3339 parses an optional RFC3339 timestamp pointer.
func parseRFC3339(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp %q: must be RFC3339", *s)
	}
	return &t, nil
}

// parseDate parses an optional YYYY-MM-DD date pointer.
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

// respondError maps a service error onto an HTTP response.
func respondError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "operation not allowed in current state")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "permission denied")
	default:
		httpx.RespondInternalError(c)
	}
}

func fmtTS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func fmtTSPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format("2006-01-02T15:04:05Z")
	return &s
}

func fmtDatePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}

// ---------------------------------------------------------------------------
// Series
// ---------------------------------------------------------------------------

type createSeriesRequest struct {
	ManagerEmployeeID string `json:"manager_employee_id" validate:"required"`
	MemberEmployeeID  string `json:"member_employee_id"  validate:"required"`
	Title             string `json:"title"               validate:"omitempty,max=200"`
	Cadence           string `json:"cadence"             validate:"omitempty,oneof=weekly biweekly monthly quarterly adhoc"`
}

type updateManagerRequest struct {
	NewManagerEmployeeID string `json:"new_manager_employee_id" validate:"required"`
}

// SeriesResponse is the JSON representation of a 1on1 series.
type SeriesResponse struct {
	ID                uuid.UUID `json:"id"`
	TenantID          uuid.UUID `json:"tenant_id"`
	ManagerEmployeeID uuid.UUID `json:"manager_employee_id"`
	MemberEmployeeID  uuid.UUID `json:"member_employee_id"`
	Title             string    `json:"title"`
	Cadence           string    `json:"cadence"`
	Status            string    `json:"status"`
	CreatedAt         string    `json:"created_at"`
	UpdatedAt         string    `json:"updated_at"`
}

func toSeriesResponse(s *Series) SeriesResponse {
	return SeriesResponse{
		ID:                s.ID,
		TenantID:          s.TenantID,
		ManagerEmployeeID: s.ManagerEmployeeID,
		MemberEmployeeID:  s.MemberEmployeeID,
		Title:             s.Title,
		Cadence:           s.Cadence,
		Status:            s.Status,
		CreatedAt:         fmtTS(s.CreatedAt),
		UpdatedAt:         fmtTS(s.UpdatedAt),
	}
}

// CreateSeries handles POST /oneonone/series.
func (h *Handler) CreateSeries(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createSeriesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	managerID, err := uuid.Parse(req.ManagerEmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid manager_employee_id")
		return
	}
	memberID, err := uuid.Parse(req.MemberEmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid member_employee_id")
		return
	}

	series, err := h.svc.CreateSeries(c.Request.Context(), CreateSeriesInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		ManagerEmployeeID: managerID,
		MemberEmployeeID:  memberID,
		Title:             req.Title,
		Cadence:           req.Cadence,
		IP:                clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSeriesResponse(series))
}

// ListSeries handles GET /oneonone/series.
// Optional ?employee_id= filters to series where the employee is a participant.
func (h *Handler) ListSeries(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	employeeFilter := uuid.Nil
	if s := c.Query("employee_id"); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
			return
		}
		employeeFilter = id
	}

	list, err := h.svc.ListSeries(c.Request.Context(), tenantID, employeeFilter)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]SeriesResponse, len(list))
	for i := range list {
		items[i] = toSeriesResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"series": items})
}

// UpdateSeriesManager handles PATCH /oneonone/series/:series_id/manager.
func (h *Handler) UpdateSeriesManager(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	seriesID, err := uuid.Parse(c.Param("series_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid series id")
		return
	}
	var req updateManagerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	newMgr, err := uuid.Parse(req.NewManagerEmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid new_manager_employee_id")
		return
	}

	series, err := h.svc.UpdateSeriesManager(c.Request.Context(), UpdateSeriesManagerInput{
		TenantID:             tenantID,
		ActorID:              actorID,
		SeriesID:             seriesID,
		NewManagerEmployeeID: newMgr,
		IP:                   clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSeriesResponse(series))
}

// CloseSeries handles POST /oneonone/series/:series_id/close.
func (h *Handler) CloseSeries(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	seriesID, err := uuid.Parse(c.Param("series_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid series id")
		return
	}
	series, err := h.svc.CloseSeries(c.Request.Context(), CloseSeriesInput{
		TenantID: tenantID,
		ActorID:  actorID,
		SeriesID: seriesID,
		IP:       clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSeriesResponse(series))
}

// GetSeriesMetadata handles GET /oneonone/series/:series_id/metadata.
// Returns aggregate meta only — never note bodies (HR-manager view).
func (h *Handler) GetSeriesMetadata(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	seriesID, err := uuid.Parse(c.Param("series_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid series id")
		return
	}
	meta, err := h.svc.GetSeriesMetadata(c.Request.Context(), tenantID, seriesID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"series":            toSeriesResponse(&meta.Series),
		"session_count":     meta.SessionCount,
		"last_held_at":      fmtTSPtr(meta.LastHeldAt),
		"open_action_count": meta.OpenActionCount,
	})
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

type createSessionRequest struct {
	ScheduledAt *string `json:"scheduled_at"`
	HeldAt      *string `json:"held_at"`
	Summary     string  `json:"summary" validate:"omitempty,max=2000"`
}

type updateSessionStatusRequest struct {
	Status string  `json:"status" validate:"required,oneof=done canceled"`
	HeldAt *string `json:"held_at"`
}

// SessionResponse is the JSON representation of a session.
type SessionResponse struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"tenant_id"`
	SeriesID    uuid.UUID `json:"series_id"`
	ScheduledAt *string   `json:"scheduled_at,omitempty"`
	HeldAt      *string   `json:"held_at,omitempty"`
	Status      string    `json:"status"`
	Summary     string    `json:"summary"`
	CreatedAt   string    `json:"created_at"`
	UpdatedAt   string    `json:"updated_at"`
}

func toSessionResponse(s *Session) SessionResponse {
	return SessionResponse{
		ID:          s.ID,
		TenantID:    s.TenantID,
		SeriesID:    s.SeriesID,
		ScheduledAt: fmtTSPtr(s.ScheduledAt),
		HeldAt:      fmtTSPtr(s.HeldAt),
		Status:      s.Status,
		Summary:     s.Summary,
		CreatedAt:   fmtTS(s.CreatedAt),
		UpdatedAt:   fmtTS(s.UpdatedAt),
	}
}

// CreateSession handles POST /oneonone/series/:series_id/sessions.
func (h *Handler) CreateSession(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	seriesID, err := uuid.Parse(c.Param("series_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid series id")
		return
	}
	var req createSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	scheduledAt, err := parseRFC3339(req.ScheduledAt)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	heldAt, err := parseRFC3339(req.HeldAt)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	session, err := h.svc.CreateSession(c.Request.Context(), CreateSessionInput{
		TenantID:    tenantID,
		ActorID:     actorID,
		SeriesID:    seriesID,
		ScheduledAt: scheduledAt,
		HeldAt:      heldAt,
		Summary:     req.Summary,
		IP:          clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSessionResponse(session))
}

// ListSessions handles GET /oneonone/series/:series_id/sessions.
func (h *Handler) ListSessions(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	seriesID, err := uuid.Parse(c.Param("series_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid series id")
		return
	}
	list, err := h.svc.ListSessions(c.Request.Context(), tenantID, actorID, seriesID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]SessionResponse, len(list))
	for i := range list {
		items[i] = toSessionResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"sessions": items})
}

// UpdateSessionStatus handles PATCH /oneonone/sessions/:session_id/status.
func (h *Handler) UpdateSessionStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	var req updateSessionStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	heldAt, err := parseRFC3339(req.HeldAt)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	session, err := h.svc.UpdateSessionStatus(c.Request.Context(), UpdateSessionStatusInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		SessionID: sessionID,
		Status:    req.Status,
		HeldAt:    heldAt,
		IP:        clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSessionResponse(session))
}

// ---------------------------------------------------------------------------
// Agenda items
// ---------------------------------------------------------------------------

type addAgendaItemRequest struct {
	Topic        string  `json:"topic"          validate:"required,max=500"`
	AuthorUserID *string `json:"author_user_id"`
	SortOrder    int     `json:"sort_order"`
}

type carryOverAgendaRequest struct {
	FromSessionID string `json:"from_session_id" validate:"required"`
}

// AgendaItemResponse is the JSON representation of an agenda item.
type AgendaItemResponse struct {
	ID                uuid.UUID  `json:"id"`
	TenantID          uuid.UUID  `json:"tenant_id"`
	SessionID         uuid.UUID  `json:"session_id"`
	Topic             string     `json:"topic"`
	AuthorUserID      *uuid.UUID `json:"author_user_id,omitempty"`
	SortOrder         int        `json:"sort_order"`
	CarriedOverFromID *uuid.UUID `json:"carried_over_from_id,omitempty"`
	CreatedAt         string     `json:"created_at"`
}

func toAgendaItemResponse(a *AgendaItem) AgendaItemResponse {
	return AgendaItemResponse{
		ID:                a.ID,
		TenantID:          a.TenantID,
		SessionID:         a.SessionID,
		Topic:             a.Topic,
		AuthorUserID:      a.AuthorUserID,
		SortOrder:         a.SortOrder,
		CarriedOverFromID: a.CarriedOverFromID,
		CreatedAt:         fmtTS(a.CreatedAt),
	}
}

// AddAgendaItem handles POST /oneonone/sessions/:session_id/agenda.
func (h *Handler) AddAgendaItem(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	var req addAgendaItemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	var authorID *uuid.UUID
	if req.AuthorUserID != nil && *req.AuthorUserID != "" {
		id, err := uuid.Parse(*req.AuthorUserID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid author_user_id")
			return
		}
		authorID = &id
	}

	item, err := h.svc.AddAgendaItem(c.Request.Context(), AddAgendaItemInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		SessionID:    sessionID,
		Topic:        req.Topic,
		AuthorUserID: authorID,
		SortOrder:    req.SortOrder,
		IP:           clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toAgendaItemResponse(item))
}

// ListAgendaItems handles GET /oneonone/sessions/:session_id/agenda.
func (h *Handler) ListAgendaItems(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	list, err := h.svc.ListAgendaItems(c.Request.Context(), tenantID, actorID, sessionID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]AgendaItemResponse, len(list))
	for i := range list {
		items[i] = toAgendaItemResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"agenda_items": items})
}

// CarryOverAgenda handles POST /oneonone/sessions/:session_id/agenda/carry-over.
// The :session_id path param is the destination session; from_session_id is the source.
func (h *Handler) CarryOverAgenda(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	toSessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	var req carryOverAgendaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	fromSessionID, err := uuid.Parse(req.FromSessionID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid from_session_id")
		return
	}

	created, err := h.svc.CarryOverAgenda(c.Request.Context(), CarryOverAgendaInput{
		TenantID:      tenantID,
		ActorID:       actorID,
		FromSessionID: fromSessionID,
		ToSessionID:   toSessionID,
		IP:            clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]AgendaItemResponse, len(created))
	for i := range created {
		items[i] = toAgendaItemResponse(&created[i])
	}
	c.JSON(http.StatusCreated, gin.H{"agenda_items": items})
}

// ---------------------------------------------------------------------------
// Notes
// ---------------------------------------------------------------------------

type addNoteRequest struct {
	Visibility string `json:"visibility" validate:"omitempty,oneof=shared private"`
	Body       string `json:"body"       validate:"required,max=20000"`
}

// NoteResponse is the JSON representation of a note.
type NoteResponse struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	SessionID    uuid.UUID `json:"session_id"`
	AuthorUserID uuid.UUID `json:"author_user_id"`
	Visibility   string    `json:"visibility"`
	Body         string    `json:"body"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
}

func toNoteResponse(n *Note) NoteResponse {
	return NoteResponse{
		ID:           n.ID,
		TenantID:     n.TenantID,
		SessionID:    n.SessionID,
		AuthorUserID: n.AuthorUserID,
		Visibility:   n.Visibility,
		Body:         n.Body,
		CreatedAt:    fmtTS(n.CreatedAt),
		UpdatedAt:    fmtTS(n.UpdatedAt),
	}
}

// AddNote handles POST /oneonone/sessions/:session_id/notes.
// The note author is always the authenticated user (records who wrote it for
// the private-note visibility check).
func (h *Handler) AddNote(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	var req addNoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	note, err := h.svc.AddNote(c.Request.Context(), AddNoteInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		SessionID:    sessionID,
		AuthorUserID: actorID,
		Visibility:   req.Visibility,
		Body:         req.Body,
		IP:           clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toNoteResponse(note))
}

// ListNotes handles GET /oneonone/sessions/:session_id/notes.
// Returns shared notes plus the caller's own private notes only.
func (h *Handler) ListNotes(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	list, err := h.svc.ListNotes(c.Request.Context(), ListNotesInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		SessionID:    sessionID,
		ViewerUserID: actorID,
		IP:           clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]NoteResponse, len(list))
	for i := range list {
		items[i] = toNoteResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"notes": items})
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

type addActionRequest struct {
	AssigneeEmployeeID string  `json:"assignee_employee_id" validate:"required"`
	Description        string  `json:"description"          validate:"required,max=2000"`
	DueDate            *string `json:"due_date"`
}

type updateActionStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=done canceled"`
}

// ActionResponse is the JSON representation of an action.
type ActionResponse struct {
	ID                 uuid.UUID `json:"id"`
	TenantID           uuid.UUID `json:"tenant_id"`
	SessionID          uuid.UUID `json:"session_id"`
	AssigneeEmployeeID uuid.UUID `json:"assignee_employee_id"`
	Description        string    `json:"description"`
	DueDate            *string   `json:"due_date,omitempty"`
	Status             string    `json:"status"`
	CompletedAt        *string   `json:"completed_at,omitempty"`
	CreatedAt          string    `json:"created_at"`
	UpdatedAt          string    `json:"updated_at"`
}

func toActionResponse(a *Action) ActionResponse {
	return ActionResponse{
		ID:                 a.ID,
		TenantID:           a.TenantID,
		SessionID:          a.SessionID,
		AssigneeEmployeeID: a.AssigneeEmployeeID,
		Description:        a.Description,
		DueDate:            fmtDatePtr(a.DueDate),
		Status:             a.Status,
		CompletedAt:        fmtTSPtr(a.CompletedAt),
		CreatedAt:          fmtTS(a.CreatedAt),
		UpdatedAt:          fmtTS(a.UpdatedAt),
	}
}

// AddAction handles POST /oneonone/sessions/:session_id/actions.
func (h *Handler) AddAction(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	sessionID, err := uuid.Parse(c.Param("session_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid session id")
		return
	}
	var req addActionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	assigneeID, err := uuid.Parse(req.AssigneeEmployeeID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid assignee_employee_id")
		return
	}
	dueDate, err := parseDate(req.DueDate)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	action, err := h.svc.AddAction(c.Request.Context(), AddActionInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		SessionID:          sessionID,
		AssigneeEmployeeID: assigneeID,
		Description:        req.Description,
		DueDate:            dueDate,
		IP:                 clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusCreated, toActionResponse(action))
}

// UpdateActionStatus handles PATCH /oneonone/actions/:action_id/status.
func (h *Handler) UpdateActionStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	actionID, err := uuid.Parse(c.Param("action_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid action id")
		return
	}
	var req updateActionStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}

	action, err := h.svc.UpdateActionStatus(c.Request.Context(), UpdateActionStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ActionID: actionID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toActionResponse(action))
}

// ListOpenActions handles GET /oneonone/series/:series_id/open-actions.
// Returns open (未完了) actions carried across the series.
func (h *Handler) ListOpenActions(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	seriesID, err := uuid.Parse(c.Param("series_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid series id")
		return
	}
	list, err := h.svc.ListOpenActionsForSeries(c.Request.Context(), tenantID, actorID, seriesID)
	if err != nil {
		respondError(c, err)
		return
	}
	items := make([]ActionResponse, len(list))
	for i := range list {
		items[i] = toActionResponse(&list[i])
	}
	c.JSON(http.StatusOK, gin.H{"actions": items})
}

// ---------------------------------------------------------------------------
// TM settings
// ---------------------------------------------------------------------------

type upsertSettingsRequest struct {
	HRManagerBodyDisclosure bool `json:"hr_manager_body_disclosure"`
	NoteRetentionDays       *int `json:"note_retention_days" validate:"omitempty,min=0"`
}

// SettingsResponse is the JSON representation of TM settings.
type SettingsResponse struct {
	TenantID                uuid.UUID `json:"tenant_id"`
	HRManagerBodyDisclosure bool      `json:"hr_manager_body_disclosure"`
	NoteRetentionDays       *int      `json:"note_retention_days,omitempty"`
}

func toSettingsResponse(s *Settings) SettingsResponse {
	return SettingsResponse{
		TenantID:                s.TenantID,
		HRManagerBodyDisclosure: s.HRManagerBodyDisclosure,
		NoteRetentionDays:       s.NoteRetentionDays,
	}
}

// GetSettings handles GET /oneonone/settings.
func (h *Handler) GetSettings(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	st, err := h.svc.GetSettings(c.Request.Context(), tenantID)
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(st))
}

// UpsertSettings handles PUT /oneonone/settings.
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
	st, err := h.svc.UpsertSettings(c.Request.Context(), UpsertSettingsInput{
		TenantID:                tenantID,
		ActorID:                 actorID,
		HRManagerBodyDisclosure: req.HRManagerBodyDisclosure,
		NoteRetentionDays:       req.NoteRetentionDays,
		IP:                      clientIP(c),
	})
	if err != nil {
		respondError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSettingsResponse(st))
}
