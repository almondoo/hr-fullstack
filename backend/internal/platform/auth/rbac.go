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

// ---------------------------------------------------------------------------
// System role definitions — used when seeding/provisioning system roles.
// ---------------------------------------------------------------------------

// RoleSystemRetention is the name of the dedicated service-account role for
// the data-retention and disposal cron job (cmd/retention).
//
// This role name is stored in the roles.name column (per-tenant).
// Provision instructions: docs/ops/retention-service-account.md
const RoleSystemRetention = "system_retention"

// SystemRetentionPerms is the minimum permission set for RoleSystemRetention.
// Only the permissions strictly required by the retention job are included:
//   - "mynumber:reveal" — required by mynumber.Service.Dispose RBAC check
//   - "ledger:write"    — required to set retention_expired on ledger rows
//
// audit_logs INSERT and retention_job_runs INSERT/UPDATE are granted at the
// DB level (to hr_app); no additional RBAC permission is needed for those.
//
// DO NOT extend this set without 社労士/弁護士 review and explicit approval.
var SystemRetentionPerms = []string{
	"mynumber:reveal",
	"ledger:write",
}

// SystemRetentionPermsJSON is the pre-serialised permissions jsonb value
// for inserting/seeding the system_retention role row.
// Format must match the permissionsJSON shape: {"perms":[...]}.
const SystemRetentionPermsJSON = `{"perms":["mynumber:reveal","ledger:write"]}`

// LoadUserPermissions fetches the permission slice for userID within the
// already-open tenant transaction tx.  Callers that need to perform an
// in-service RBAC check (e.g. approval engine checking approval:admin) should
// use this rather than duplicating the role-lookup logic.
//
// Returns an empty slice (not an error) when the user has no role assigned.
// RLS is enforced by the caller's WithinTenant context; the explicit
// tenant_id conditions are an additional defence-in-depth layer.
func LoadUserPermissions(tx *gorm.DB, tenantID, userID uuid.UUID) ([]string, error) {
	var ur userRoleRow
	if err := tx.Raw(
		`SELECT role_id FROM users WHERE id = ? AND tenant_id = ? LIMIT 1`,
		userID, tenantID,
	).Scan(&ur).Error; err != nil {
		return nil, fmt.Errorf("rbac: load user role: %w", err)
	}
	if ur.RoleID == nil {
		return nil, nil
	}

	var rr roleRow
	if err := tx.Raw(
		`SELECT permissions FROM roles WHERE id = ? AND tenant_id = ? LIMIT 1`,
		ur.RoleID, tenantID,
	).Scan(&rr).Error; err != nil {
		return nil, fmt.Errorf("rbac: load role permissions: %w", err)
	}
	if len(rr.Permissions) == 0 {
		return nil, nil
	}

	var pj permissionsJSON
	if err := json.Unmarshal(rr.Permissions, &pj); err != nil {
		return nil, fmt.Errorf("rbac: parse permissions json: %w", err)
	}
	return pj.Perms, nil
}

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
//   - "ns:*" grants any permission whose namespace (the part before the first
//     colon) equals "ns".  This is a single-level namespace wildcard: it only
//     matches the immediate children of the namespace segment, not deeper paths.
//     For example, "employee:*" matches "employee:read" and "employee:write"
//     but does NOT match "employee:payroll:view" (three segments).
//     This is intentional: future multi-segment permissions (e.g.
//     "mynumber:read:sensitive") MUST NOT be accidentally granted by a
//     two-segment "mynumber:*" wildcard.  If broader grants are needed they
//     should be spelled out explicitly or a dedicated three-segment wildcard
//     ("mynumber:read:*") should be added.
//   - Exact match (e.g. "employee:read") grants that specific permission.
//
// Permission convention: need MUST be a two-segment colon-separated string
// such as "resource:action" (e.g. "employee:read").  Callers that introduce
// additional segments must update the wildcard matching rules above.
func HasPermission(perms []string, need string) bool {
	for _, p := range perms {
		if p == "*" {
			return true
		}
		if p == need {
			return true
		}
		// I-3: Namespace wildcard: "employee:*" matches "employee:read",
		// "employee:write", etc.  The match requires need to start with "ns:"
		// (i.e. ns + colon), so "emp:*" does NOT match "employee:read".
		// Multi-segment needs (three or more colons) are NOT covered by a
		// two-segment "ns:*" wildcard — the HasPrefix check below will fail
		// because "ns:action:sub" does start with "ns:" but that is actually
		// fine for current two-segment permissions.  When three-segment
		// permissions are introduced, add an explicit depth check here.
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
