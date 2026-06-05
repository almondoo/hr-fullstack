package auth

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRoleSystemRetention_ConstantsDefined verifies that the system_retention
// role constants are non-empty and well-formed.
func TestRoleSystemRetention_ConstantsDefined(t *testing.T) {
	assert.Equal(t, "system_retention", RoleSystemRetention,
		"RoleSystemRetention must be the canonical role name used in users.role_id lookup")
}

// TestSystemRetentionPerms_MinimumSet verifies that the permission set for
// the retention service account contains exactly the required minimum
// permissions and nothing more (principle of least privilege).
func TestSystemRetentionPerms_MinimumSet(t *testing.T) {
	require.Len(t, SystemRetentionPerms, 2,
		"system_retention must hold exactly 2 permissions (mynumber:reveal, ledger:write)")
	assert.Contains(t, SystemRetentionPerms, "mynumber:reveal")
	assert.Contains(t, SystemRetentionPerms, "ledger:write")

	// Ensure no broad wildcards are present.
	for _, p := range SystemRetentionPerms {
		assert.NotEqual(t, "*", p,
			"system_retention must not hold the wildcard '*' permission")
		assert.NotContains(t, p, ":*",
			"system_retention must not hold namespace wildcard permissions")
	}
}

// TestSystemRetentionPermsJSON_WellFormed verifies the pre-serialised JSON
// constant round-trips correctly and matches the canonical permission slice.
func TestSystemRetentionPermsJSON_WellFormed(t *testing.T) {
	var pj permissionsJSON
	err := json.Unmarshal([]byte(SystemRetentionPermsJSON), &pj)
	require.NoError(t, err, "SystemRetentionPermsJSON must be valid JSON")

	assert.ElementsMatch(t, SystemRetentionPerms, pj.Perms,
		"SystemRetentionPermsJSON perms must match SystemRetentionPerms slice")
}

// TestSystemRetention_HasPermission verifies that HasPermission correctly
// grants and denies access when SystemRetentionPerms is used as the permission
// source (simulating the in-memory RBAC check path).
func TestSystemRetention_HasPermission(t *testing.T) {
	perms := SystemRetentionPerms

	// Required permissions must be granted.
	assert.True(t, HasPermission(perms, "mynumber:reveal"),
		"system_retention must be granted mynumber:reveal")
	assert.True(t, HasPermission(perms, "ledger:write"),
		"system_retention must be granted ledger:write")

	// Permissions that must NOT be granted to the retention service account.
	denied := []string{
		"*",
		"employee:read",
		"employee:write",
		"mynumber:read",
		"ledger:read",
		"hr_admin",
		"super_admin",
		"approval:admin",
	}
	for _, d := range denied {
		assert.False(t, HasPermission(perms, d),
			"system_retention must NOT be granted %q", d)
	}
}
