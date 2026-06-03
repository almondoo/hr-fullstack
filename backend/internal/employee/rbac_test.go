package employee_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"

	"github.com/your-org/hr-saas/internal/employee"
)

// buildTestRouter builds a minimal Gin router wired with RequireAuth + RequirePermission
// for testing, without CSRF middleware.
func buildTestRouter(tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	v1 := r.Group("/api/v1")
	employee.RegisterRoutes(v1, tdb, requireAuth)
	return r
}

// fakeRequireAuth returns a middleware that injects fixed tenantID/userID from
// the test's "x-test-tenant-id" and "x-test-user-id" request headers.
// This lets tests bypass the real session/cookie machinery while still
// exercising RBAC checks (which come after RequireAuth).
func fakeRequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantHeader := c.GetHeader("x-test-tenant-id")
		userHeader := c.GetHeader("x-test-user-id")
		if tenantHeader == "" || userHeader == "" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		tid, err := uuid.Parse(tenantHeader)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		uid, err := uuid.Parse(userHeader)
		if err != nil {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set("auth_tenant_id", tid)
		c.Set("auth_user_id", uid)
		c.Next()
	}
}

// seedTenantWithRole inserts a tenant, role, and user row via admin DB.
// Returns the user UUID that has the given permissions.
func seedTenantWithRole(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, perms []string) uuid.UUID {
	t.Helper()

	require.NoError(t, adminDB.Exec(
		`INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, ?, 'free', 'active', ?)`,
		tenantID, "RBAC Tenant", tenantID.String()[:8],
	).Error)

	roleID := uuid.New()
	permJSON, _ := json.Marshal(map[string][]string{"perms": perms})
	require.NoError(t, adminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, 'test_role', ?)`,
		roleID, tenantID, permJSON,
	).Error)

	userID := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status, role_id) VALUES (?, ?, ?, 'active', ?)`,
		userID, tenantID, fmt.Sprintf("%s@example.com", userID.String()[:8]), roleID,
	).Error)

	return userID
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}, tenantID, userID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequest(method, path, bytes.NewReader(bodyBytes))
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-test-tenant-id", tenantID.String())
	req.Header.Set("x-test-user-id", userID.String())

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestEmployeeRBACPermission tests that routes require correct permissions.
func TestEmployeeRBACPermission(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	ctx := context.Background()

	tenantID := uuid.New()

	// User WITH employee:read and employee:write permissions.
	allowedUser := seedTenantWithRole(t, h.AdminDB, tenantID,
		[]string{"employee:read", "employee:write", "contract:read", "contract:write"})

	// User WITHOUT any employee permissions.
	noPermTenantID := uuid.New()
	noPermUser := seedTenantWithRole(t, h.AdminDB, noPermTenantID, []string{"department:read"})
	// Reuse same tenant for simpler test: seed a role with no employee perms in same tenant.
	noEmpPermRoleID := uuid.New()
	noEmpPermJSON, _ := json.Marshal(map[string][]string{"perms": {"department:read"}})
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, 'noperm_role', ?)`,
		noEmpPermRoleID, tenantID, noEmpPermJSON,
	).Error)
	noEmpPermUser := uuid.New()
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status, role_id) VALUES (?, ?, 'noperm@example.com', 'active', ?)`,
		noEmpPermUser, tenantID, noEmpPermRoleID,
	).Error)

	t.Cleanup(func() {
		h.TruncateTables("audit_logs", "employment_contracts", "employee_assignments",
			"employees", "departments", "users", "sessions", "roles", "tenants")
	})
	_ = ctx
	_ = noPermUser
	_ = noPermTenantID

	requireAuth := fakeRequireAuth()
	r := buildTestRouter(tdb, requireAuth)

	// Seed an employee to use for read tests.
	svc := employee.NewService(tdb)
	ip := "127.0.0.1"
	emp, err := svc.CreateEmployee(context.Background(), employee.CreateEmployeeInput{
		TenantID:       tenantID,
		ActorID:        allowedUser,
		EmployeeCode:   "RBAC001",
		LastName:       "伊藤",
		FirstName:      "五郎",
		EmploymentType: "full_time",
		Status:         "active",
		IP:             &ip,
	})
	require.NoError(t, err)

	t.Run("ListEmployees_WithPerm_200", func(t *testing.T) {
		w := doJSON(t, r, http.MethodGet, "/api/v1/employees", nil, tenantID, allowedUser)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("ListEmployees_NoPerm_403", func(t *testing.T) {
		w := doJSON(t, r, http.MethodGet, "/api/v1/employees", nil, tenantID, noEmpPermUser)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("CreateEmployee_WithPerm_201", func(t *testing.T) {
		body := map[string]interface{}{
			"employee_code":   "RBAC002",
			"last_name":       "渡辺",
			"first_name":      "六郎",
			"employment_type": "full_time",
			"status":          "active",
		}
		w := doJSON(t, r, http.MethodPost, "/api/v1/employees", body, tenantID, allowedUser)
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("CreateEmployee_NoPerm_403", func(t *testing.T) {
		body := map[string]interface{}{
			"employee_code":   "RBAC003",
			"last_name":       "松本",
			"first_name":      "七郎",
			"employment_type": "full_time",
			"status":          "active",
		}
		w := doJSON(t, r, http.MethodPost, "/api/v1/employees", body, tenantID, noEmpPermUser)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("CreateContract_WithPerm_201", func(t *testing.T) {
		body := map[string]interface{}{
			"contract_type": "permanent",
			"start_date":    "2024-04-01",
		}
		path := fmt.Sprintf("/api/v1/employees/%s/contracts", emp.ID.String())
		w := doJSON(t, r, http.MethodPost, path, body, tenantID, allowedUser)
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("ListContracts_WithPerm_200", func(t *testing.T) {
		path := fmt.Sprintf("/api/v1/employees/%s/contracts", emp.ID.String())
		w := doJSON(t, r, http.MethodGet, path, nil, tenantID, allowedUser)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("AssignmentCreate_WithPerm_201", func(t *testing.T) {
		body := map[string]interface{}{
			"effective_from": time.Now().Format("2006-01-02"),
		}
		path := fmt.Sprintf("/api/v1/employees/%s/assignments", emp.ID.String())
		w := doJSON(t, r, http.MethodPost, path, body, tenantID, allowedUser)
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("AssignmentCreate_NoPerm_403", func(t *testing.T) {
		body := map[string]interface{}{
			"effective_from": time.Now().Format("2006-01-02"),
		}
		path := fmt.Sprintf("/api/v1/employees/%s/assignments", emp.ID.String())
		w := doJSON(t, r, http.MethodPost, path, body, tenantID, noEmpPermUser)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	// Admin wildcard: "*" should allow all operations.
	adminRoleID := uuid.New()
	adminPermJSON, _ := json.Marshal(map[string][]string{"perms": {"*"}})
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, 'admin_role', ?)`,
		adminRoleID, tenantID, adminPermJSON,
	).Error)
	adminUser := uuid.New()
	require.NoError(t, h.AdminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status, role_id) VALUES (?, ?, 'admin@example.com', 'active', ?)`,
		adminUser, tenantID, adminRoleID,
	).Error)

	t.Run("AdminWildcard_ListEmployees_200", func(t *testing.T) {
		w := doJSON(t, r, http.MethodGet, "/api/v1/employees", nil, tenantID, adminUser)
		assert.Equal(t, http.StatusOK, w.Code)
	})

	t.Run("AdminWildcard_CreateEmployee_201", func(t *testing.T) {
		body := map[string]interface{}{
			"employee_code":   "ADM001",
			"last_name":       "管理",
			"first_name":      "者",
			"employment_type": "full_time",
			"status":          "active",
		}
		w := doJSON(t, r, http.MethodPost, "/api/v1/employees", body, tenantID, adminUser)
		assert.Equal(t, http.StatusCreated, w.Code)
	})

	t.Run("Unauthenticated_401", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/employees", nil)
		// No auth headers — fakeRequireAuth returns 401.
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}
