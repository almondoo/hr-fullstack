package talent

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
const maxJSONBytes = 64 * 1024 // 64 KB

// Handler exposes HTTP endpoints for the talent domain.
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

// respondErr maps a service error to an HTTP error response.
func respondErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "resource not found")
	case errors.Is(err, ErrInvalidTransition):
		httpx.RespondError(c, http.StatusConflict, "INVALID_TRANSITION", "status transition not allowed")
	case errors.Is(err, ErrForbidden):
		httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
	case errors.Is(err, ErrInvalidLevel):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "skill level out of range")
	case errors.Is(err, ErrValidation):
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid input")
	default:
		httpx.RespondInternalError(c)
	}
}

func formatTS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func formatDatePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}

// ---------------------------------------------------------------------------
// Skill master
// ---------------------------------------------------------------------------

type createSkillRequest struct {
	Category string          `json:"category" validate:"omitempty,max=200"`
	Name     string          `json:"name"     validate:"required,max=200"`
	Levels   json.RawMessage `json:"levels"`
}

// SkillResponse is the JSON representation of a skill.
type SkillResponse struct {
	ID        uuid.UUID       `json:"id"`
	TenantID  uuid.UUID       `json:"tenant_id"`
	Category  string          `json:"category"`
	Name      string          `json:"name"`
	Levels    json.RawMessage `json:"levels"`
	Active    bool            `json:"active"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func toSkillResponse(sk *Skill) SkillResponse {
	levels := json.RawMessage(sk.LevelsJSON)
	if len(levels) == 0 {
		levels = json.RawMessage(`{}`)
	}
	return SkillResponse{
		ID:        sk.ID,
		TenantID:  sk.TenantID,
		Category:  sk.Category,
		Name:      sk.Name,
		Levels:    levels,
		Active:    sk.Active,
		CreatedAt: formatTS(sk.CreatedAt),
		UpdatedAt: formatTS(sk.UpdatedAt),
	}
}

// CreateSkill handles POST /talent/skills.
func (h *Handler) CreateSkill(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Levels); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "levels: "+err.Error())
		return
	}
	levels := []byte(req.Levels)
	if len(levels) == 0 || string(levels) == "null" {
		levels = []byte(`{}`)
	}

	sk, err := h.svc.CreateSkill(c.Request.Context(), CreateSkillInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		Category:   req.Category,
		Name:       req.Name,
		LevelsJSON: levels,
		IP:         clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSkillResponse(sk))
}

// ListSkills handles GET /talent/skills.
func (h *Handler) ListSkills(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	category := c.Query("category")

	skills, err := h.svc.ListSkills(c.Request.Context(), tenantID, category)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]SkillResponse, len(skills))
	for i := range skills {
		items[i] = toSkillResponse(&skills[i])
	}
	c.JSON(http.StatusOK, gin.H{"skills": items})
}

// ---------------------------------------------------------------------------
// Employee skills
// ---------------------------------------------------------------------------

type assignSkillRequest struct {
	SkillID    string  `json:"skill_id"    validate:"required"`
	Level      int     `json:"level"`
	AcquiredOn *string `json:"acquired_on"`
	ExpiresOn  *string `json:"expires_on"`
}

// EmployeeSkillResponse is the JSON representation of an employee skill.
type EmployeeSkillResponse struct {
	ID         uuid.UUID `json:"id"`
	EmployeeID uuid.UUID `json:"employee_id"`
	SkillID    uuid.UUID `json:"skill_id"`
	Level      int       `json:"level"`
	AcquiredOn *string   `json:"acquired_on,omitempty"`
	ExpiresOn  *string   `json:"expires_on,omitempty"`
}

func toEmployeeSkillResponse(es *EmployeeSkill) EmployeeSkillResponse {
	return EmployeeSkillResponse{
		ID:         es.ID,
		EmployeeID: es.EmployeeID,
		SkillID:    es.SkillID,
		Level:      es.Level,
		AcquiredOn: formatDatePtr(es.AcquiredOn),
		ExpiresOn:  formatDatePtr(es.ExpiresOn),
	}
}

// AssignSkill handles PUT /employees/:id/skills.
func (h *Handler) AssignSkill(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req assignSkillRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	skillID, err := uuid.Parse(req.SkillID)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid skill_id")
		return
	}
	acquiredOn, err := parseDate(req.AcquiredOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	expiresOn, err := parseDate(req.ExpiresOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	es, err := h.svc.AssignSkill(c.Request.Context(), AssignSkillInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
		SkillID:    skillID,
		Level:      req.Level,
		AcquiredOn: acquiredOn,
		ExpiresOn:  expiresOn,
		IP:         clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toEmployeeSkillResponse(es))
}

// ListEmployeeSkills handles GET /employees/:id/skills.
func (h *Handler) ListEmployeeSkills(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	skills, err := h.svc.ListEmployeeSkills(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]EmployeeSkillResponse, len(skills))
	for i := range skills {
		items[i] = toEmployeeSkillResponse(&skills[i])
	}
	c.JSON(http.StatusOK, gin.H{"skills": items})
}

// SearchSkillHolders handles GET /talent/skills/:skill_id/holders?min_level=N.
func (h *Handler) SearchSkillHolders(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	skillID, err := uuid.Parse(c.Param("skill_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid skill id")
		return
	}
	minLevel := 0
	if v := c.Query("min_level"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "min_level must be an integer")
			return
		}
		minLevel = n
	}
	holders, err := h.svc.SearchSkillHolders(c.Request.Context(), tenantID, skillID, minLevel)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"holders": holders})
}

// SkillMatrix handles GET /talent/skill-matrix.
func (h *Handler) SkillMatrix(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	cells, err := h.svc.SkillMatrix(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"matrix": cells})
}

// ---------------------------------------------------------------------------
// Certifications
// ---------------------------------------------------------------------------

type addCertificationRequest struct {
	Name            string  `json:"name"   validate:"required,max=200"`
	Issuer          string  `json:"issuer" validate:"omitempty,max=200"`
	AcquiredOn      *string `json:"acquired_on"`
	ExpiresOn       *string `json:"expires_on"`
	RenewalRequired bool    `json:"renewal_required"`
}

// CertificationResponse is the JSON representation of a certification.
type CertificationResponse struct {
	ID              uuid.UUID `json:"id"`
	EmployeeID      uuid.UUID `json:"employee_id"`
	Name            string    `json:"name"`
	Issuer          string    `json:"issuer"`
	AcquiredOn      *string   `json:"acquired_on,omitempty"`
	ExpiresOn       *string   `json:"expires_on,omitempty"`
	RenewalRequired bool      `json:"renewal_required"`
}

func toCertificationResponse(cert *Certification) CertificationResponse {
	return CertificationResponse{
		ID:              cert.ID,
		EmployeeID:      cert.EmployeeID,
		Name:            cert.Name,
		Issuer:          cert.Issuer,
		AcquiredOn:      formatDatePtr(cert.AcquiredOn),
		ExpiresOn:       formatDatePtr(cert.ExpiresOn),
		RenewalRequired: cert.RenewalRequired,
	}
}

// AddCertification handles POST /employees/:id/certifications.
func (h *Handler) AddCertification(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	var req addCertificationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	acquiredOn, err := parseDate(req.AcquiredOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	expiresOn, err := parseDate(req.ExpiresOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	cert, err := h.svc.AddCertification(c.Request.Context(), AddCertificationInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EmployeeID:      empID,
		Name:            req.Name,
		Issuer:          req.Issuer,
		AcquiredOn:      acquiredOn,
		ExpiresOn:       expiresOn,
		RenewalRequired: req.RenewalRequired,
		IP:              clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toCertificationResponse(cert))
}

// ListCertifications handles GET /employees/:id/certifications.
func (h *Handler) ListCertifications(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}
	certs, err := h.svc.ListCertifications(c.Request.Context(), tenantID, empID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	items := make([]CertificationResponse, len(certs))
	for i := range certs {
		items[i] = toCertificationResponse(&certs[i])
	}
	c.JSON(http.StatusOK, gin.H{"certifications": items})
}

// ExpiringCertifications handles GET /talent/certifications/expiring?within_days=N.
func (h *Handler) ExpiringCertifications(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	withinDays := 30
	if v := c.Query("within_days"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "within_days must be an integer")
			return
		}
		withinDays = n
	}
	certs, err := h.svc.ExpiringCertifications(c.Request.Context(), tenantID, withinDays)
	if err != nil {
		respondErr(c, err)
		return
	}
	items := make([]CertificationResponse, len(certs))
	for i := range certs {
		items[i] = toCertificationResponse(&certs[i])
	}
	c.JSON(http.StatusOK, gin.H{"certifications": items})
}

// ---------------------------------------------------------------------------
// Integrated profile
// ---------------------------------------------------------------------------

// ProfileResponse is the JSON representation of an integrated profile.
type ProfileResponse struct {
	EmployeeID      uuid.UUID               `json:"employee_id"`
	EmployeeCode    string                  `json:"employee_code"`
	LastName        string                  `json:"last_name"`
	FirstName       string                  `json:"first_name"`
	Status          string                  `json:"status"`
	DepartmentID    *uuid.UUID              `json:"department_id,omitempty"`
	Assignments     []assignmentResponse    `json:"assignments"`
	Skills          []EmployeeSkillResponse `json:"skills"`
	SensitiveMasked bool                    `json:"sensitive_masked"`
}

type assignmentResponse struct {
	ID            uuid.UUID  `json:"id"`
	DepartmentID  *uuid.UUID `json:"department_id,omitempty"`
	Position      *string    `json:"position,omitempty"`
	Grade         *string    `json:"grade,omitempty"`
	EffectiveFrom string     `json:"effective_from"`
	EffectiveTo   *string    `json:"effective_to,omitempty"`
}

// GetProfile handles GET /employees/:id/profile.
func (h *Handler) GetProfile(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	empID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid employee id")
		return
	}

	p, err := h.svc.GetIntegratedProfile(c.Request.Context(), GetProfileInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		EmployeeID: empID,
	})
	if err != nil {
		respondErr(c, err)
		return
	}

	resp := ProfileResponse{
		EmployeeID:      p.EmployeeID,
		EmployeeCode:    p.EmployeeCode,
		LastName:        p.LastName,
		FirstName:       p.FirstName,
		Status:          p.Status,
		DepartmentID:    p.DepartmentID,
		Assignments:     make([]assignmentResponse, len(p.Assignments)),
		Skills:          make([]EmployeeSkillResponse, len(p.Skills)),
		SensitiveMasked: p.SensitiveMasked,
	}
	for i, a := range p.Assignments {
		resp.Assignments[i] = assignmentResponse{
			ID:            a.ID,
			DepartmentID:  a.DepartmentID,
			Position:      a.Position,
			Grade:         a.Grade,
			EffectiveFrom: a.EffectiveFrom.Format("2006-01-02"),
			EffectiveTo:   formatDatePtr(a.EffectiveTo),
		}
	}
	for i := range p.Skills {
		resp.Skills[i] = toEmployeeSkillResponse(&p.Skills[i])
	}
	c.JSON(http.StatusOK, resp)
}

// GetOrgTree handles GET /talent/org-tree.
func (h *Handler) GetOrgTree(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	roots, err := h.svc.GetOrgTree(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}
	c.JSON(http.StatusOK, gin.H{"tree": roots})
}

// ---------------------------------------------------------------------------
// Placement simulation
// ---------------------------------------------------------------------------

type createSimulationRequest struct {
	Name string `json:"name" validate:"required,max=200"`
}

type addSimulationItemRequest struct {
	EmployeeID         string  `json:"employee_id"          validate:"required"`
	TargetDepartmentID *string `json:"target_department_id"`
	TargetPosition     *string `json:"target_position"`
	TargetGrade        *string `json:"target_grade"`
	EffectiveFrom      string  `json:"effective_from"       validate:"required"`
	Reason             *string `json:"reason"`
}

// SimulationResponse is the JSON representation of a placement simulation.
type SimulationResponse struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
}

func toSimulationResponse(sim *PlacementSimulation) SimulationResponse {
	return SimulationResponse{
		ID:        sim.ID,
		Name:      sim.Name,
		Status:    sim.Status,
		CreatedAt: formatTS(sim.CreatedAt),
		UpdatedAt: formatTS(sim.UpdatedAt),
	}
}

// CreateSimulation handles POST /talent/placement-simulations.
func (h *Handler) CreateSimulation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createSimulationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	sim, err := h.svc.CreateSimulation(c.Request.Context(), CreateSimulationInput{
		TenantID: tenantID,
		ActorID:  actorID,
		Name:     req.Name,
		IP:       clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSimulationResponse(sim))
}

// AddSimulationItem handles POST /talent/placement-simulations/:sim_id/items.
func (h *Handler) AddSimulationItem(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	simID, err := uuid.Parse(c.Param("sim_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid simulation id")
		return
	}

	var req addSimulationItemRequest
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
	var targetDept *uuid.UUID
	if req.TargetDepartmentID != nil && *req.TargetDepartmentID != "" {
		d, err := uuid.Parse(*req.TargetDepartmentID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid target_department_id")
			return
		}
		targetDept = &d
	}
	effFrom, err := time.Parse("2006-01-02", req.EffectiveFrom)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "effective_from must be YYYY-MM-DD")
		return
	}

	item, err := h.svc.AddSimulationItem(c.Request.Context(), AddSimulationItemInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		SimulationID:       simID,
		EmployeeID:         empID,
		TargetDepartmentID: targetDept,
		TargetPosition:     req.TargetPosition,
		TargetGrade:        req.TargetGrade,
		EffectiveFrom:      effFrom,
		Reason:             req.Reason,
		IP:                 clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":            item.ID,
		"simulation_id": item.SimulationID,
		"employee_id":   item.EmployeeID,
	})
}

// ListSimulationItems handles GET /talent/placement-simulations/:sim_id/items.
func (h *Handler) ListSimulationItems(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	simID, err := uuid.Parse(c.Param("sim_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid simulation id")
		return
	}
	items, err := h.svc.ListSimulationItems(c.Request.Context(), tenantID, simID)
	if err != nil {
		respondErr(c, err)
		return
	}
	out := make([]gin.H, len(items))
	for i, it := range items {
		out[i] = gin.H{
			"id":                   it.ID,
			"employee_id":          it.EmployeeID,
			"target_department_id": it.TargetDepartmentID,
			"target_position":      it.TargetPosition,
			"target_grade":         it.TargetGrade,
			"effective_from":       it.EffectiveFrom.Format("2006-01-02"),
			"reason":               it.Reason,
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

// ApplySimulation handles POST /talent/placement-simulations/:sim_id/apply.
func (h *Handler) ApplySimulation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	simID, err := uuid.Parse(c.Param("sim_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid simulation id")
		return
	}
	applied, err := h.svc.ApplySimulation(c.Request.Context(), ApplySimulationInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		SimulationID: simID,
		IP:           clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"applied_assignments": applied, "status": SimStatusApplied})
}

// DiscardSimulation handles POST /talent/placement-simulations/:sim_id/discard.
func (h *Handler) DiscardSimulation(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	simID, err := uuid.Parse(c.Param("sim_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid simulation id")
		return
	}
	if err := h.svc.DiscardSimulation(c.Request.Context(), DiscardSimulationInput{
		TenantID:     tenantID,
		ActorID:      actorID,
		SimulationID: simID,
		IP:           clientIP(c),
	}); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": SimStatusDiscarded})
}

// ---------------------------------------------------------------------------
// Pulse surveys
// ---------------------------------------------------------------------------

type createSurveyRequest struct {
	Title              string          `json:"title"     validate:"required,max=200"`
	Questions          json.RawMessage `json:"questions"`
	Anonymous          *bool           `json:"anonymous"`
	MinResponsesToShow *int            `json:"min_responses_to_show"`
	StartsOn           *string         `json:"starts_on"`
	EndsOn             *string         `json:"ends_on"`
}

type setSurveyStatusRequest struct {
	Status string `json:"status" validate:"required,oneof=draft open closed"`
}

type submitResponseRequest struct {
	EmployeeID *string         `json:"employee_id"`
	Answers    json.RawMessage `json:"answers"`
	FreeText   string          `json:"free_text" validate:"omitempty,max=8000"`
}

// SurveyResponse is the JSON representation of a pulse survey.
type SurveyResponse struct {
	ID                 uuid.UUID       `json:"id"`
	Title              string          `json:"title"`
	Questions          json.RawMessage `json:"questions"`
	Anonymous          bool            `json:"anonymous"`
	MinResponsesToShow int             `json:"min_responses_to_show"`
	StartsOn           *string         `json:"starts_on,omitempty"`
	EndsOn             *string         `json:"ends_on,omitempty"`
	Status             string          `json:"status"`
	CreatedAt          string          `json:"created_at"`
}

func toSurveyResponse(sv *PulseSurvey) SurveyResponse {
	q := json.RawMessage(sv.QuestionsJSON)
	if len(q) == 0 {
		q = json.RawMessage(`[]`)
	}
	return SurveyResponse{
		ID:                 sv.ID,
		Title:              sv.Title,
		Questions:          q,
		Anonymous:          sv.Anonymous,
		MinResponsesToShow: sv.MinResponsesToShow,
		StartsOn:           formatDatePtr(sv.StartsOn),
		EndsOn:             formatDatePtr(sv.EndsOn),
		Status:             sv.Status,
		CreatedAt:          formatTS(sv.CreatedAt),
	}
}

// CreateSurvey handles POST /talent/surveys.
func (h *Handler) CreateSurvey(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createSurveyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Questions); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "questions: "+err.Error())
		return
	}
	questions := []byte(req.Questions)
	if len(questions) == 0 || string(questions) == "null" {
		questions = []byte(`[]`)
	}
	anonymous := true
	if req.Anonymous != nil {
		anonymous = *req.Anonymous
	}
	minResp := 5
	if req.MinResponsesToShow != nil {
		minResp = *req.MinResponsesToShow
	}
	startsOn, err := parseDate(req.StartsOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	endsOn, err := parseDate(req.EndsOn)
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	sv, err := h.svc.CreateSurvey(c.Request.Context(), CreateSurveyInput{
		TenantID:           tenantID,
		ActorID:            actorID,
		Title:              req.Title,
		QuestionsJSON:      questions,
		Anonymous:          anonymous,
		MinResponsesToShow: minResp,
		StartsOn:           startsOn,
		EndsOn:             endsOn,
		IP:                 clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, toSurveyResponse(sv))
}

// SetSurveyStatus handles PATCH /talent/surveys/:survey_id/status.
func (h *Handler) SetSurveyStatus(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	surveyID, err := uuid.Parse(c.Param("survey_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid survey id")
		return
	}
	var req setSurveyStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	sv, err := h.svc.SetSurveyStatus(c.Request.Context(), SetSurveyStatusInput{
		TenantID: tenantID,
		ActorID:  actorID,
		SurveyID: surveyID,
		Status:   req.Status,
		IP:       clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, toSurveyResponse(sv))
}

// SubmitResponse handles POST /talent/surveys/:survey_id/responses.
func (h *Handler) SubmitResponse(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	surveyID, err := uuid.Parse(c.Param("survey_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid survey id")
		return
	}
	var req submitResponseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", validationMessage(err))
		return
	}
	if err := validateJSON(req.Answers); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "answers: "+err.Error())
		return
	}
	answers := []byte(req.Answers)
	if len(answers) == 0 || string(answers) == "null" {
		answers = []byte(`{}`)
	}
	var empID *uuid.UUID
	if req.EmployeeID != nil && *req.EmployeeID != "" {
		id, err := uuid.Parse(*req.EmployeeID)
		if err != nil {
			httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid employee_id")
			return
		}
		empID = &id
	}

	resp, err := h.svc.SubmitResponse(c.Request.Context(), SubmitResponseInput{
		TenantID:          tenantID,
		ActorID:           actorID,
		SurveyID:          surveyID,
		EmployeeID:        empID,
		AnswersJSON:       answers,
		FreeTextPlaintext: []byte(req.FreeText),
		IP:                clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": resp.ID, "survey_id": resp.SurveyID})
}

// AggregateSurvey handles GET /talent/surveys/:survey_id/aggregate.
func (h *Handler) AggregateSurvey(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	surveyID, err := uuid.Parse(c.Param("survey_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid survey id")
		return
	}
	agg, err := h.svc.AggregateSurvey(c.Request.Context(), tenantID, surveyID)
	if err != nil {
		respondErr(c, err)
		return
	}
	out := gin.H{
		"survey_id":      agg.SurveyID,
		"response_count": agg.ResponseCount,
		"suppressed":     agg.Suppressed,
	}
	if !agg.Suppressed {
		out["answer_summary"] = agg.AnswerSummary
	}
	c.JSON(http.StatusOK, out)
}

// ReadFreeText handles GET /talent/survey-responses/:response_id/free-text.
// Requires survey:read_freetext (enforced both at the route and service layers).
func (h *Handler) ReadFreeText(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)
	respID, err := uuid.Parse(c.Param("response_id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid response id")
		return
	}
	plaintext, err := h.svc.ReadFreeText(c.Request.Context(), ReadFreeTextInput{
		TenantID:   tenantID,
		ActorID:    actorID,
		ResponseID: respID,
		IP:         clientIP(c),
	})
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"free_text": string(plaintext)})
}
