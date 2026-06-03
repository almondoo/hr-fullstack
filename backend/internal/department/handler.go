package department

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/httpx"
)

var validate = validator.New()

// Handler exposes HTTP endpoints for the department domain.
type Handler struct {
	svc *Service
}

// NewHandler constructs a Handler.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// ---------------------------------------------------------------------------
// Request / response shapes
// ---------------------------------------------------------------------------

// DepartmentResponse is the JSON representation of a department.
type DepartmentResponse struct { //nolint:revive // name is intentional for cross-package clarity
	ID        uuid.UUID  `json:"id"`
	TenantID  uuid.UUID  `json:"tenant_id"`
	ParentID  *uuid.UUID `json:"parent_id,omitempty"`
	Name      string     `json:"name"`
	Code      string     `json:"code"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
}

func toResponse(d *Department) DepartmentResponse {
	return DepartmentResponse{
		ID:        d.ID,
		TenantID:  d.TenantID,
		ParentID:  d.ParentID,
		Name:      d.Name,
		Code:      d.Code,
		CreatedAt: d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt: d.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// createRequest is the expected JSON body for POST /departments.
type createRequest struct {
	ParentID *uuid.UUID `json:"parent_id"`
	Name     string     `json:"name"     validate:"required,max=200"`
	Code     string     `json:"code"     validate:"required,max=50"`
}

// updateRequest is the expected JSON body for PUT /departments/:id.
type updateRequest struct {
	ParentID *uuid.UUID `json:"parent_id"`
	Name     string     `json:"name"     validate:"required,max=200"`
	Code     string     `json:"code"     validate:"required,max=50"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// Create handles POST /departments.
// Permission: department:write
func (h *Handler) Create(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	var req createRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	dept, err := h.svc.Create(c.Request.Context(), CreateInput{
		TenantID: tenantID,
		ActorID:  actorID,
		ParentID: req.ParentID,
		Name:     req.Name,
		Code:     req.Code,
		IP:       clientIP(c),
	})
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusCreated, toResponse(dept))
}

// Get handles GET /departments/:id.
// Permission: department:read
func (h *Handler) Get(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid department id")
		return
	}

	dept, err := h.svc.Get(c.Request.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "department not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toResponse(dept))
}

// List handles GET /departments.
// Permission: department:read
func (h *Handler) List(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)

	depts, err := h.svc.List(c.Request.Context(), tenantID)
	if err != nil {
		httpx.RespondInternalError(c)
		return
	}

	items := make([]DepartmentResponse, len(depts))
	for i := range depts {
		items[i] = toResponse(&depts[i])
	}
	c.JSON(http.StatusOK, gin.H{"departments": items})
}

// Update handles PUT /departments/:id.
// Permission: department:write
func (h *Handler) Update(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid department id")
		return
	}

	var req updateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON")
		return
	}
	if err := validate.Struct(&req); err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}

	dept, err := h.svc.Update(c.Request.Context(), UpdateInput{
		TenantID: tenantID,
		ID:       id,
		ActorID:  actorID,
		ParentID: req.ParentID,
		Name:     req.Name,
		Code:     req.Code,
		IP:       clientIP(c),
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "department not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, toResponse(dept))
}

// Delete handles DELETE /departments/:id.
// Permission: department:write
func (h *Handler) Delete(c *gin.Context) {
	tenantID := platformauth.TenantIDFrom(c)
	actorID := platformauth.UserIDFrom(c)

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		httpx.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid department id")
		return
	}

	if err := h.svc.Delete(c.Request.Context(), tenantID, id, actorID, clientIP(c)); err != nil {
		if errors.Is(err, ErrNotFound) {
			httpx.RespondError(c, http.StatusNotFound, "NOT_FOUND", "department not found")
			return
		}
		httpx.RespondInternalError(c)
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func clientIP(c *gin.Context) *string {
	ip := c.ClientIP()
	if ip == "" {
		return nil
	}
	return &ip
}
