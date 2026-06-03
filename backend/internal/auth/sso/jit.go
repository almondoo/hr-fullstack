package sso

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// ErrJITDisabled is returned by JITProvisioner when JIT provisioning is
// disabled for the tenant's IdP configuration.
var ErrJITDisabled = errors.New("sso: JIT provisioning is disabled for this IdP")

// ErrEmailDomainNotAllowed is returned when the user's email domain is not in
// JITConfig.AllowedEmailDomains.
var ErrEmailDomainNotAllowed = errors.New("sso: email domain not in allowed list for JIT provisioning")

// ProvisionedUser carries the result of a successful JIT provisioning or
// look-up operation.
type ProvisionedUser struct {
	// UserID is the application user identifier (UUID).
	UserID uuid.UUID

	// TenantID is the tenant the user was provisioned into.
	TenantID uuid.UUID

	// RoleID is the role assigned to the user. Determined by role mapping
	// rules; falls back to the default role when no rule matches.
	RoleID uuid.UUID

	// IsNew is true when the user was created by this JIT provisioning call,
	// false when an existing user was found and returned.
	IsNew bool
}

// JITProvisioner is the interface for Just-In-Time user provisioning.
// It is called after a successful SSO assertion to either look up an existing
// user or create a new one with mapped role and minimum required fields.
//
// Security constraints:
//   - Users are always created in the scope of their own tenant (tenantID).
//   - The assigned role must exist in the tenant's roles table; unknown role
//     names are rejected rather than silently defaulted.
//   - Email domain restrictions in JITConfig.AllowedEmailDomains are enforced
//     before any database write.
//   - Created users have no password_hash (SSO-only); password-based login is
//     disabled for these accounts unless explicitly enabled later.
//
// TODO(db): implement a PostgreSQL-backed JITProvisioner using
// tenantdb.WithinTenant once the identity_providers table migration is applied.
type JITProvisioner interface {
	// ProvisionOrGet looks up the user by (tenantID, idpID, subjectID).
	// If the user does not exist and cfg.Enabled == true, creates a new user
	// row with the mapped role and returns IsNew == true.
	//
	// If the user already exists, updates last_login_at and returns IsNew == false.
	//
	// Returns ErrJITDisabled when cfg.Enabled == false.
	// Returns ErrEmailDomainNotAllowed when the email domain is not allowed.
	ProvisionOrGet(ctx context.Context, tenantID uuid.UUID, idpID uuid.UUID, claims UserClaims, cfg JITConfig) (ProvisionedUser, error)
}

// noopJITProvisioner is a stub JITProvisioner that always returns
// ErrNotImplemented. It satisfies the JITProvisioner interface so that the
// application can compile and start without a real provisioner.
type noopJITProvisioner struct{}

// NewNoopJITProvisioner returns a stub JITProvisioner.
// Replace with a real implementation once the identity_providers table exists.
func NewNoopJITProvisioner() JITProvisioner {
	return &noopJITProvisioner{}
}

// ProvisionOrGet implements JITProvisioner (stub).
func (n *noopJITProvisioner) ProvisionOrGet(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ UserClaims, _ JITConfig) (ProvisionedUser, error) {
	return ProvisionedUser{}, ErrNotImplemented
}

// ResolveRole applies the role mapping rules in cfg to the groups in claims
// and returns the application role name that should be assigned to the user.
//
// Resolution order:
//  1. Iterate cfg.RoleMappingRules in order; return the first matching AppRole.
//  2. Fall back to cfg.DefaultRole.
//  3. Return an error when cfg.DefaultRole is empty.
//
// This function is exported so that unit tests can exercise rule evaluation
// independently of the database.
func ResolveRole(claims UserClaims, cfg JITConfig) (string, error) {
	groupSet := make(map[string]bool, len(claims.Groups))
	for _, g := range claims.Groups {
		groupSet[strings.TrimSpace(g)] = true
	}

	for _, rule := range cfg.RoleMappingRules {
		if groupSet[strings.TrimSpace(rule.IDPGroup)] {
			if strings.TrimSpace(rule.AppRole) != "" {
				return rule.AppRole, nil
			}
		}
	}

	if strings.TrimSpace(cfg.DefaultRole) == "" {
		return "", errors.New("sso: no role mapping matched and DefaultRole is empty")
	}
	return cfg.DefaultRole, nil
}

// IsEmailDomainAllowed returns true when the email's domain is in the
// allowedDomains list, or when the list is empty (no restriction).
func IsEmailDomainAllowed(email string, allowedDomains []string) bool {
	if len(allowedDomains) == 0 {
		return true
	}
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range allowedDomains {
		if strings.ToLower(strings.TrimSpace(d)) == domain {
			return true
		}
	}
	return false
}
