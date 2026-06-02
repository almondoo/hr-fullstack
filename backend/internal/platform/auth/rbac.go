package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/httpx"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// permissionsJSON is the shape of the roles.permissions jsonb column.
// Example: {"perms":["employee:read","employee:write"]}
// The wildcard "*" grants all permissions.
// Namespace wildcards like "employee:*" grant all permissions under that prefix.
type permissionsJSON struct {
	Perms []string `json:"perms"`
}

// roleRow is the minimal shape fetched from roles for RBAC checks.
type roleRow struct {
	Permissions []byte `gorm:"column:permissions;type:jsonb"`
}

// userRoleRow fetches a user's role_id.
type userRoleRow struct {
	RoleID *uuid.UUID `gorm:"column:role_id"`
}

// HasPermission reports whether the given permissions slice satisfies need.
//
// Wildcard rules:
//   - "*" grants everything.
//   - "employee:*" grants any permission whose prefix before ":" equals "employee".
//   - Exact match (e.g. "employee:read") grants that specific permission.
//
// need should be a colon-separated string such as "employee:read".
func HasPermission(perms []string, need string) bool {
	for _, p := range perms {
		if p == "*" {
			return true
		}
		if p == need {
			return true
		}
		// Namespace wildcard: "employee:*" matches "employee:read", "employee:write", etc.
		if strings.HasSuffix(p, ":*") {
			ns := strings.TrimSuffix(p, ":*")
			if strings.HasPrefix(need, ns+":") {
				return true
			}
		}
	}
	return false
}

// RequirePermission returns a Gin middleware that enforces RBAC after RequireAuth.
//
// Flow:
//  1. Extract tenantID and userID from gin.Context (set by RequireAuth).
//  2. Use WithinTenant to fetch the user's role and its permissions.
//  3. Call HasPermission with the loaded permissions and need.
//  4. Grant (c.Next()) or deny (403 + abort).
//
// Tenant-crossing prevention: the user and role are both looked up under
// tenantID extracted from the session, so a user from tenant A can never
// exercise permissions against tenant B.
func RequirePermission(
	tdb *tenantdb.TenantDB,
	need string,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := TenantIDFrom(c)
		userID := UserIDFrom(c)
		if tenantID == uuid.Nil || userID == uuid.Nil {
			// RequireAuth must have been applied before this middleware.
			httpx.RespondError(c, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required")
			c.Abort()
			return
		}

		var allowed bool
		err := tdb.WithinTenant(c.Request.Context(), tenantID, func(tx *gorm.DB) error {
			// Fetch the user's role_id.
			var ur userRoleRow
			if err := tx.Raw(
				`SELECT role_id FROM users WHERE id = ? AND tenant_id = ? LIMIT 1`,
				userID, tenantID,
			).Scan(&ur).Error; err != nil {
				return fmt.Errorf("rbac: fetch user role: %w", err)
			}
			if ur.RoleID == nil {
				// No role assigned — deny.
				return nil
			}

			// Fetch the role's permissions.
			var rr roleRow
			if err := tx.Raw(
				`SELECT permissions FROM roles WHERE id = ? AND tenant_id = ? LIMIT 1`,
				ur.RoleID, tenantID,
			).Scan(&rr).Error; err != nil {
				return fmt.Errorf("rbac: fetch role permissions: %w", err)
			}
			if len(rr.Permissions) == 0 {
				return nil
			}

			var pj permissionsJSON
			if err := json.Unmarshal(rr.Permissions, &pj); err != nil {
				return fmt.Errorf("rbac: parse permissions json: %w", err)
			}

			allowed = HasPermission(pj.Perms, need)
			return nil
		})
		if err != nil {
			httpx.RespondInternalError(c)
			c.Abort()
			return
		}

		if !allowed {
			httpx.RespondError(c, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
			c.Abort()
			return
		}

		c.Next()
	}
}
