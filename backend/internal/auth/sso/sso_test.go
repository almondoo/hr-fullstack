package sso

import (
	"errors"
	"testing"
)

// TestValidateOIDCAlgorithm verifies that the alg=none rejection and the
// allowed-algorithm gate work correctly without any external dependency.
func TestValidateOIDCAlgorithm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		alg     string
		wantErr bool
	}{
		{"RS256", false},
		{"ES256", false},
		{"RS384", false},
		{"RS512", false},
		{"ES384", false},
		{"ES512", false},
		{"none", true},
		{"None", true},
		{"NONE", true},
		{"HS256", true},
		{"", true},
		{"alg=none", true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.alg, func(t *testing.T) {
			t.Parallel()
			err := validateOIDCAlgorithm(tc.alg)
			if tc.wantErr && err == nil {
				t.Errorf("validateOIDCAlgorithm(%q): expected error, got nil", tc.alg)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateOIDCAlgorithm(%q): unexpected error: %v", tc.alg, err)
			}
		})
	}
}

// TestResolveRole verifies role mapping rule evaluation order and fallback.
func TestResolveRole(t *testing.T) {
	t.Parallel()

	cfg := JITConfig{
		Enabled:     true,
		DefaultRole: "employee",
		RoleMappingRules: []RoleMappingRule{
			{IDPGroup: "hr-admins", AppRole: "admin"},
			{IDPGroup: "hr-managers", AppRole: "manager"},
		},
	}

	cases := []struct {
		name     string
		groups   []string
		wantRole string
		wantErr  bool
	}{
		{
			name:     "first rule matches",
			groups:   []string{"hr-admins"},
			wantRole: "admin",
		},
		{
			name:     "second rule matches",
			groups:   []string{"hr-managers"},
			wantRole: "manager",
		},
		{
			name:     "no rule matches — use default",
			groups:   []string{"other-group"},
			wantRole: "employee",
		},
		{
			name:     "empty groups — use default",
			groups:   nil,
			wantRole: "employee",
		},
		{
			name:     "first rule wins over second",
			groups:   []string{"hr-admins", "hr-managers"},
			wantRole: "admin",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := UserClaims{Groups: tc.groups}
			role, err := ResolveRole(claims, cfg)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if role != tc.wantRole {
				t.Errorf("got role %q, want %q", role, tc.wantRole)
			}
		})
	}

	t.Run("empty DefaultRole returns error", func(t *testing.T) {
		t.Parallel()
		noCfg := JITConfig{Enabled: true, DefaultRole: ""}
		_, err := ResolveRole(UserClaims{}, noCfg)
		if err == nil {
			t.Error("expected error when DefaultRole is empty, got nil")
		}
	})
}

// TestIsEmailDomainAllowed verifies email domain allow-list enforcement.
func TestIsEmailDomainAllowed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		email   string
		allowed []string
		want    bool
	}{
		{"user@example.com", []string{"example.com"}, true},
		{"user@EXAMPLE.COM", []string{"example.com"}, true},
		{"user@other.com", []string{"example.com"}, false},
		{"user@example.com", nil, true},  // no restriction
		{"user@example.com", []string{}, true}, // empty list = no restriction
		{"notanemail", []string{"example.com"}, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.email, func(t *testing.T) {
			t.Parallel()
			got := IsEmailDomainAllowed(tc.email, tc.allowed)
			if got != tc.want {
				t.Errorf("IsEmailDomainAllowed(%q, %v) = %v, want %v", tc.email, tc.allowed, got, tc.want)
			}
		})
	}
}

// TestStubsReturnNotImplemented verifies that stub implementations correctly
// return ErrNotImplemented (not panic).
func TestStubsReturnNotImplemented(t *testing.T) {
	t.Parallel()

	t.Run("OIDCProvider.AuthRedirectURL", func(t *testing.T) {
		t.Parallel()
		p := &OIDCProvider{}
		idp := IdentityProvider{Enabled: true, Protocol: ProtocolOIDC}
		_, err := p.AuthRedirectURL(t.Context(), idp, "state")
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("got %v, want ErrNotImplemented", err)
		}
	})

	t.Run("OIDCProvider.HandleCallback", func(t *testing.T) {
		t.Parallel()
		p := &OIDCProvider{}
		idp := IdentityProvider{Enabled: true, Protocol: ProtocolOIDC}
		_, err := p.HandleCallback(t.Context(), idp, CallbackParams{})
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("got %v, want ErrNotImplemented", err)
		}
	})

	t.Run("SAMLProvider.AuthRedirectURL", func(t *testing.T) {
		t.Parallel()
		p := &SAMLProvider{}
		idp := IdentityProvider{Enabled: true, Protocol: ProtocolSAML}
		_, err := p.AuthRedirectURL(t.Context(), idp, "relay")
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("got %v, want ErrNotImplemented", err)
		}
	})

	t.Run("SAMLProvider.HandleCallback", func(t *testing.T) {
		t.Parallel()
		p := &SAMLProvider{}
		idp := IdentityProvider{Enabled: true, Protocol: ProtocolSAML}
		_, err := p.HandleCallback(t.Context(), idp, CallbackParams{})
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("got %v, want ErrNotImplemented", err)
		}
	})

	t.Run("noopJITProvisioner.ProvisionOrGet", func(t *testing.T) {
		t.Parallel()
		prov := NewNoopJITProvisioner()
		_, err := prov.ProvisionOrGet(t.Context(), [16]byte{}, [16]byte{}, UserClaims{}, JITConfig{})
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("got %v, want ErrNotImplemented", err)
		}
	})
}

// TestDisabledProviderReturnsErrProviderDisabled confirms that stubs respect
// the Enabled flag without requiring ErrNotImplemented to overlap.
func TestDisabledProviderReturnsErrProviderDisabled(t *testing.T) {
	t.Parallel()

	idp := IdentityProvider{Enabled: false}

	t.Run("OIDC AuthRedirectURL", func(t *testing.T) {
		t.Parallel()
		p := &OIDCProvider{}
		_, err := p.AuthRedirectURL(t.Context(), idp, "s")
		if !errors.Is(err, ErrProviderDisabled) {
			t.Errorf("got %v, want ErrProviderDisabled", err)
		}
	})

	t.Run("OIDC HandleCallback", func(t *testing.T) {
		t.Parallel()
		p := &OIDCProvider{}
		_, err := p.HandleCallback(t.Context(), idp, CallbackParams{})
		if !errors.Is(err, ErrProviderDisabled) {
			t.Errorf("got %v, want ErrProviderDisabled", err)
		}
	})

	t.Run("SAML AuthRedirectURL", func(t *testing.T) {
		t.Parallel()
		p := &SAMLProvider{}
		_, err := p.AuthRedirectURL(t.Context(), idp, "r")
		if !errors.Is(err, ErrProviderDisabled) {
			t.Errorf("got %v, want ErrProviderDisabled", err)
		}
	})

	t.Run("SAML HandleCallback", func(t *testing.T) {
		t.Parallel()
		p := &SAMLProvider{}
		_, err := p.HandleCallback(t.Context(), idp, CallbackParams{})
		if !errors.Is(err, ErrProviderDisabled) {
			t.Errorf("got %v, want ErrProviderDisabled", err)
		}
	})
}
