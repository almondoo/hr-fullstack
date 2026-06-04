package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// ---------------------------------------------------------------------------
// DB row types
// ---------------------------------------------------------------------------

// dbIdentityProvider maps to the identity_providers table.
type dbIdentityProvider struct {
	ID          uuid.UUID       `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID       `gorm:"column:tenant_id"`
	Protocol    string          `gorm:"column:protocol"`
	Enabled     bool            `gorm:"column:enabled"`
	DisplayName string          `gorm:"column:display_name"`
	OIDCConfig  json.RawMessage `gorm:"column:oidc_config;type:jsonb"`
	SAMLConfig  json.RawMessage `gorm:"column:saml_config;type:jsonb"`
	JITConfig   json.RawMessage `gorm:"column:jit_config;type:jsonb"`
	CreatedAt   time.Time       `gorm:"column:created_at"`
	UpdatedAt   time.Time       `gorm:"column:updated_at"`
}

// TableName maps dbIdentityProvider to identity_providers.
func (dbIdentityProvider) TableName() string { return "identity_providers" }

// dbSSOState maps to the sso_state table.
type dbSSOState struct {
	ID              uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID `gorm:"column:tenant_id"`
	IdpID           uuid.UUID `gorm:"column:idp_id"`
	State           string    `gorm:"column:state"`
	CodeVerifier    string    `gorm:"column:code_verifier"`
	AutohnRequestID string    `gorm:"column:authn_request_id"`
	ExpiresAt       time.Time `gorm:"column:expires_at"`
	CreatedAt       time.Time `gorm:"column:created_at"`
}

// TableName maps dbSSOState to sso_state.
func (dbSSOState) TableName() string { return "sso_state" }

// dbSSOIdentity maps to the sso_identities table.
type dbSSOIdentity struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	UserID      uuid.UUID  `gorm:"column:user_id"`
	IdpID       uuid.UUID  `gorm:"column:idp_id"`
	SubjectID   string     `gorm:"column:subject_id"`
	LastLoginAt *time.Time `gorm:"column:last_login_at"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps dbSSOIdentity to sso_identities.
func (dbSSOIdentity) TableName() string { return "sso_identities" }

// ---------------------------------------------------------------------------
// JSON shapes for config columns
// ---------------------------------------------------------------------------

type oidcConfigJSON struct {
	IssuerURL         string   `json:"issuer_url"`
	ClientID          string   `json:"client_id"`
	ClientSecretRef   string   `json:"client_secret_ref"` // Secret Manager reference, NOT the value
	RedirectURL       string   `json:"redirect_url"`
	Scopes            []string `json:"scopes"`
	ExpectedAlgorithm string   `json:"expected_algorithm"`
	AllowedAudiences  []string `json:"allowed_audiences"`
}

type samlConfigJSON struct {
	SPEntityID            string            `json:"sp_entity_id"`
	IDPMetadataURL        string            `json:"idp_metadata_url"`
	IDPCertificateRef     string            `json:"idp_certificate_ref"` // Secret Manager reference
	ACSURL                string            `json:"acs_url"`
	SPPrivateKeyRef       string            `json:"sp_private_key_ref"` // Secret Manager reference
	SPCertificate         string            `json:"sp_certificate"`
	NameIDFormat          string            `json:"name_id_format"`
	AllowedClockSkewS     int               `json:"allowed_clock_skew_s"`
	AttributeMap          map[string]string `json:"attribute_map"`
}

type jitConfigJSON struct {
	Enabled             bool              `json:"enabled"`
	DefaultRole         string            `json:"default_role"`
	RoleMappingRules    []roleMappingJSON `json:"role_mapping_rules"`
	AllowedEmailDomains []string          `json:"allowed_email_domains"`
}

type roleMappingJSON struct {
	IDPGroup string `json:"idp_group"`
	AppRole  string `json:"app_role"`
}

// ---------------------------------------------------------------------------
// PostgreSQL ProviderRepository
// ---------------------------------------------------------------------------

// pgProviderRepository is the PostgreSQL-backed implementation of ProviderRepository.
// All queries are executed inside a tenantdb.WithinTenant scope to enforce RLS.
type pgProviderRepository struct {
	tdb *tenantdb.TenantDB
}

// NewPGProviderRepository returns a PostgreSQL-backed ProviderRepository.
func NewPGProviderRepository(tdb *tenantdb.TenantDB) ProviderRepository {
	return &pgProviderRepository{tdb: tdb}
}

// FindByID implements ProviderRepository.
func (r *pgProviderRepository) FindByID(ctx context.Context, tenantID, id uuid.UUID) (IdentityProvider, error) {
	var row dbIdentityProvider
	err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, protocol, enabled, display_name,
			        oidc_config, saml_config, jit_config, created_at, updated_at
			 FROM identity_providers
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&row).Error
	})
	if err != nil {
		return IdentityProvider{}, fmt.Errorf("sso: FindByID: %w", err)
	}
	if row.ID == uuid.Nil {
		return IdentityProvider{}, fmt.Errorf("sso: identity provider %s not found in tenant %s", id, tenantID)
	}
	return dbRowToIdentityProvider(row)
}

// ListByTenant implements ProviderRepository.
func (r *pgProviderRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]IdentityProvider, error) {
	var rows []dbIdentityProvider
	err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, protocol, enabled, display_name,
			        oidc_config, saml_config, jit_config, created_at, updated_at
			 FROM identity_providers
			 WHERE tenant_id = ? AND enabled = true
			 ORDER BY created_at`,
			tenantID,
		).Scan(&rows).Error
	})
	if err != nil {
		return nil, fmt.Errorf("sso: ListByTenant: %w", err)
	}

	providers := make([]IdentityProvider, 0, len(rows))
	for _, row := range rows {
		p, err := dbRowToIdentityProvider(row)
		if err != nil {
			return nil, err
		}
		providers = append(providers, p)
	}
	return providers, nil
}

// dbRowToIdentityProvider converts a DB row to the domain IdentityProvider type.
func dbRowToIdentityProvider(row dbIdentityProvider) (IdentityProvider, error) {
	idp := IdentityProvider{
		ID:       row.ID,
		TenantID: row.TenantID,
		Protocol: Protocol(row.Protocol),
		Enabled:  row.Enabled,
	}

	switch idp.Protocol {
	case ProtocolOIDC:
		var cfg oidcConfigJSON
		if err := jsonUnmarshalNonEmpty(row.OIDCConfig, &cfg); err != nil {
			return IdentityProvider{}, fmt.Errorf("sso: unmarshal oidc_config for %s: %w", row.ID, err)
		}
		idp.OIDCConfig = &OIDCConfig{
			IssuerURL:         cfg.IssuerURL,
			ClientID:          cfg.ClientID,
			ClientSecret:      cfg.ClientSecretRef, // ref, not value; resolved at runtime
			RedirectURL:       cfg.RedirectURL,
			Scopes:            cfg.Scopes,
			ExpectedAlgorithm: cfg.ExpectedAlgorithm,
			AllowedAudiences:  cfg.AllowedAudiences,
		}
	case ProtocolSAML:
		var cfg samlConfigJSON
		if err := jsonUnmarshalNonEmpty(row.SAMLConfig, &cfg); err != nil {
			return IdentityProvider{}, fmt.Errorf("sso: unmarshal saml_config for %s: %w", row.ID, err)
		}
		idp.SAMLConfig = &SAMLConfig{
			SPENTITYID:              cfg.SPEntityID,
			IDPMetadataURL:          cfg.IDPMetadataURL,
			IDPCertificate:          cfg.IDPCertificateRef, // ref; resolved at runtime
			ACSURL:                  cfg.ACSURL,
			SPPrivateKey:            cfg.SPPrivateKeyRef, // ref; resolved at runtime
			SPCertificate:           cfg.SPCertificate,
			NameIDFormat:            cfg.NameIDFormat,
			AllowedClockSkewSeconds: cfg.AllowedClockSkewS,
			AttributeMap:            cfg.AttributeMap,
		}
	}

	return idp, nil
}

// jsonUnmarshalNonEmpty unmarshals JSON but treats an empty or null payload
// as a no-op (leaves target at its zero value).
func jsonUnmarshalNonEmpty(data json.RawMessage, target interface{}) error {
	if len(data) == 0 || string(data) == "null" || string(data) == "{}" {
		return nil
	}
	return json.Unmarshal(data, target)
}

// ---------------------------------------------------------------------------
// State store (sso_state)
// ---------------------------------------------------------------------------

// StateStore manages the short-lived SSO flow state (OIDC PKCE/nonce +
// SAML AuthnRequest ID) in the sso_state table.
type StateStore struct {
	tdb *tenantdb.TenantDB
}

// NewStateStore returns a StateStore.
func NewStateStore(tdb *tenantdb.TenantDB) *StateStore {
	return &StateStore{tdb: tdb}
}

// Save persists an SSO state entry. The state value must be cryptographically
// random and unique per tenant.
func (s *StateStore) Save(ctx context.Context, tenantID, idpID uuid.UUID, state, codeVerifier, authnRequestID string) error {
	return s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`INSERT INTO sso_state
			   (tenant_id, idp_id, state, code_verifier, authn_request_id, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			tenantID, idpID, state, codeVerifier, authnRequestID,
			time.Now().Add(10*time.Minute),
		).Error
	})
}

// Consume retrieves and deletes the state entry. Returns an error when not
// found, expired, or on DB failure.
func (s *StateStore) Consume(ctx context.Context, tenantID uuid.UUID, state string) (codeVerifier, authnRequestID string, idpID uuid.UUID, err error) {
	err = s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var row dbSSOState
		if scanErr := tx.Raw(
			`SELECT id, idp_id, code_verifier, authn_request_id, expires_at
			 FROM sso_state
			 WHERE tenant_id = ? AND state = ?
			 LIMIT 1`,
			tenantID, state,
		).Scan(&row).Error; scanErr != nil {
			return scanErr
		}
		if row.ID == uuid.Nil {
			return errors.New("sso: state not found")
		}
		if time.Now().After(row.ExpiresAt) {
			// Clean up expired row.
			_ = tx.Exec(`DELETE FROM sso_state WHERE id = ? AND tenant_id = ?`, row.ID, tenantID).Error
			return errors.New("sso: state expired")
		}

		codeVerifier = row.CodeVerifier
		authnRequestID = row.AutohnRequestID
		idpID = row.IdpID

		// Consume: delete the row to prevent replay.
		return tx.Exec(`DELETE FROM sso_state WHERE id = ? AND tenant_id = ?`, row.ID, tenantID).Error
	})
	return
}

// ---------------------------------------------------------------------------
// PostgreSQL JITProvisioner
// ---------------------------------------------------------------------------

// pgJITProvisioner is the PostgreSQL-backed JITProvisioner.
// It uses tenantdb.WithinTenant for all DB operations.
type pgJITProvisioner struct {
	tdb *tenantdb.TenantDB
}

// NewPGJITProvisioner returns a PostgreSQL-backed JITProvisioner.
func NewPGJITProvisioner(tdb *tenantdb.TenantDB) JITProvisioner {
	return &pgJITProvisioner{tdb: tdb}
}

// ProvisionOrGet implements JITProvisioner.
//
// Logic:
//  1. Validate JIT enabled and email domain.
//  2. Look up sso_identities by (tenant_id, idp_id, subject_id).
//  3. If found: update last_login_at and return IsNew=false.
//  4. If not found and JIT enabled: resolve role, create user + sso_identity.
func (p *pgJITProvisioner) ProvisionOrGet(ctx context.Context, tenantID, idpID uuid.UUID, claims UserClaims, cfg JITConfig) (ProvisionedUser, error) {
	if !cfg.Enabled {
		return ProvisionedUser{}, ErrJITDisabled
	}

	if !IsEmailDomainAllowed(claims.Email, cfg.AllowedEmailDomains) {
		return ProvisionedUser{}, ErrEmailDomainNotAllowed
	}

	roleName, err := ResolveRole(claims, cfg)
	if err != nil {
		return ProvisionedUser{}, fmt.Errorf("sso: JIT role resolution: %w", err)
	}

	var result ProvisionedUser

	txErr := p.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Look up existing SSO identity.
		var existing dbSSOIdentity
		if err := tx.Raw(
			`SELECT id, user_id, tenant_id FROM sso_identities
			 WHERE tenant_id = ? AND idp_id = ? AND subject_id = ?
			 LIMIT 1`,
			tenantID, idpID, claims.SubjectID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("JIT: lookup sso_identity: %w", err)
		}

		if existing.ID != uuid.Nil {
			// Existing user: update last_login_at.
			now := time.Now()
			if updateErr := tx.Exec(
				`UPDATE sso_identities
				 SET last_login_at = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				now, existing.ID, tenantID,
			).Error; updateErr != nil {
				return fmt.Errorf("JIT: update last_login_at: %w", updateErr)
			}

			// Also update user's last_login_at.
			_ = tx.Exec(
				`UPDATE users SET last_login_at = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				now, existing.UserID, tenantID,
			).Error

			result = ProvisionedUser{
				UserID:   existing.UserID,
				TenantID: tenantID,
				IsNew:    false,
			}
			return nil
		}

		// New user: resolve role ID.
		var roleID uuid.UUID
		if err := tx.Raw(
			`SELECT id FROM roles WHERE tenant_id = ? AND name = ? LIMIT 1`,
			tenantID, roleName,
		).Scan(&roleID).Error; err != nil {
			return fmt.Errorf("JIT: lookup role %q: %w", roleName, err)
		}
		if roleID == uuid.Nil {
			return fmt.Errorf("JIT: role %q not found in tenant", roleName)
		}

		userID := uuid.New()
		identityID := uuid.New()

		// Insert user (no password_hash — SSO-only account).
		if err := tx.Exec(
			`INSERT INTO users
			   (id, tenant_id, email, password_hash, role_id, status)
			 VALUES (?, ?, ?, NULL, ?, 'active')`,
			userID, tenantID, claims.Email, roleID,
		).Error; err != nil {
			return fmt.Errorf("JIT: insert user: %w", err)
		}

		// Insert sso_identity.
		if err := tx.Exec(
			`INSERT INTO sso_identities
			   (id, tenant_id, user_id, idp_id, subject_id, last_login_at)
			 VALUES (?, ?, ?, ?, ?, now())`,
			identityID, tenantID, userID, idpID, claims.SubjectID,
		).Error; err != nil {
			return fmt.Errorf("JIT: insert sso_identity: %w", err)
		}

		result = ProvisionedUser{
			UserID:   userID,
			TenantID: tenantID,
			RoleID:   roleID,
			IsNew:    true,
		}
		return nil
	})

	if txErr != nil {
		return ProvisionedUser{}, txErr
	}
	return result, nil
}
