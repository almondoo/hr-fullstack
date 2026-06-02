package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Model helpers (minimal, matching DB columns)
// ---------------------------------------------------------------------------

type dbTenant struct {
	ID       uuid.UUID `gorm:"column:id;primaryKey"`
	Name     string    `gorm:"column:name"`
	PlanCode string    `gorm:"column:plan_code"`
	Status   string    `gorm:"column:status"`
	Slug     string    `gorm:"column:slug"`
}

func (dbTenant) TableName() string { return "tenants" }

type dbUser struct {
	ID       uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID uuid.UUID `gorm:"column:tenant_id"`
	Email    string    `gorm:"column:email"`
	Status   string    `gorm:"column:status"`
}

func (dbUser) TableName() string { return "users" }

// ---------------------------------------------------------------------------
// Seed helpers
// ---------------------------------------------------------------------------

func seedTenant(t *testing.T, adminDB *gorm.DB, name, slug string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := adminDB.Exec(
		"INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, ?, ?, ?, ?)",
		id, name, "free", "active", slug,
	).Error
	require.NoError(t, err)
	return id
}

func seedUser(t *testing.T, tdb *tenantdb.TenantDB, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := tdb.WithinTenant(context.Background(), tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			"INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, ?)",
			id, tenantID, email, "active",
		).Error
	})
	require.NoError(t, err)
	return id
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSession_CreateAndResolveSuccess(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-Auth-1", "tenant-auth-1")
	userID := seedUser(t, tdb, tenantID, "user1@example.test")

	rawToken, err := store.Create(ctx, tdb, tenantID, userID, 24*time.Hour, nil)
	require.NoError(t, err)
	require.NotEmpty(t, rawToken)

	gotTenant, gotUser, err := store.Resolve(ctx, h.AppDB, tdb, rawToken)
	require.NoError(t, err)
	assert.Equal(t, tenantID, gotTenant)
	assert.Equal(t, userID, gotUser)
}

func TestSession_RawTokenNotStoredInDB(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-NoRawToken", "tenant-no-raw-token")
	userID := seedUser(t, tdb, tenantID, "noraw@example.test")

	rawToken, err := store.Create(ctx, tdb, tenantID, userID, 24*time.Hour, nil)
	require.NoError(t, err)

	// Confirm no row in sessions has a token_hash that equals the raw token.
	var count int64
	err = h.AdminDB.Raw(
		"SELECT COUNT(*) FROM sessions WHERE token_hash = ?",
		rawToken,
	).Scan(&count).Error
	require.NoError(t, err)
	assert.Equal(t, int64(0), count, "raw token must NOT be stored in token_hash column")
}

func TestSession_ExpiredTokenIsInvalid(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-Expired", "tenant-expired")
	userID := seedUser(t, tdb, tenantID, "expired@example.test")

	// Create a session that expired 1 second ago.
	rawToken, err := store.Create(ctx, tdb, tenantID, userID, -1*time.Second, nil)
	require.NoError(t, err)

	_, _, err = store.Resolve(ctx, h.AppDB, tdb, rawToken)
	assert.ErrorIs(t, err, auth.ErrSessionExpired)
}

func TestSession_RevokedTokenIsInvalid(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-Revoke", "tenant-revoke")
	userID := seedUser(t, tdb, tenantID, "revoke@example.test")

	rawToken, err := store.Create(ctx, tdb, tenantID, userID, 24*time.Hour, nil)
	require.NoError(t, err)

	err = store.RevokeByRawToken(ctx, tdb, tenantID, rawToken)
	require.NoError(t, err)

	_, _, err = store.Resolve(ctx, h.AppDB, tdb, rawToken)
	assert.ErrorIs(t, err, auth.ErrSessionRevoked)
}

func TestSession_UnknownTokenIsNotFound(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	_, _, err := store.Resolve(ctx, h.AppDB, tdb, "completely-unknown-token-xyz")
	assert.ErrorIs(t, err, auth.ErrSessionNotFound)
}

func TestSession_CrossTenantIsolation(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	// Two tenants, each with a user and session.
	tenantA := seedTenant(t, h.AdminDB, "Tenant-CrossA", "tenant-cross-a")
	tenantB := seedTenant(t, h.AdminDB, "Tenant-CrossB", "tenant-cross-b")
	userA := seedUser(t, tdb, tenantA, "a@example.test")
	userB := seedUser(t, tdb, tenantB, "b@example.test")

	tokenA, err := store.Create(ctx, tdb, tenantA, userA, 24*time.Hour, nil)
	require.NoError(t, err)
	tokenB, err := store.Create(ctx, tdb, tenantB, userB, 24*time.Hour, nil)
	require.NoError(t, err)

	// Token A resolves to tenant A user A.
	gotTenantA, gotUserA, err := store.Resolve(ctx, h.AppDB, tdb, tokenA)
	require.NoError(t, err)
	assert.Equal(t, tenantA, gotTenantA)
	assert.Equal(t, userA, gotUserA)

	// Token B resolves to tenant B user B.
	gotTenantB, gotUserB, err := store.Resolve(ctx, h.AppDB, tdb, tokenB)
	require.NoError(t, err)
	assert.Equal(t, tenantB, gotTenantB)
	assert.Equal(t, userB, gotUserB)

	// Ensure tokens are distinct and do not cross.
	assert.NotEqual(t, tokenA, tokenB)
	assert.NotEqual(t, gotTenantA, gotTenantB)
	assert.NotEqual(t, gotUserA, gotUserB)
}

func TestSession_ResolveWorksWithoutTenantContext(t *testing.T) {
	// This test confirms that auth_resolve_session (SECURITY DEFINER) works
	// without a tenant context set — i.e., outside WithinTenant.
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-NoCtx", "tenant-no-ctx")
	userID := seedUser(t, tdb, tenantID, "noctx@example.test")

	rawToken, err := store.Create(ctx, tdb, tenantID, userID, 24*time.Hour, nil)
	require.NoError(t, err)

	// Resolve directly on the raw appDB (no WithinTenant wrapper) — this is
	// the path that RequireAuth takes before the tenant is known.
	// Verify no "context not set" error arises.
	gotTenantID, gotUserID, err := store.Resolve(ctx, h.AppDB, tdb, rawToken)
	require.NoError(t, err, "SECURITY DEFINER must work without a tenant context")
	assert.Equal(t, tenantID, gotTenantID)
	assert.Equal(t, userID, gotUserID)
}

func TestResolveTenantBySlug(t *testing.T) {
	h := testdb.NewHarness(t)
	ctx := context.Background()
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-Slug", "my-company-slug")

	// Found.
	gotID, err := store.ResolveTenantBySlug(ctx, h.AppDB, "my-company-slug")
	require.NoError(t, err)
	assert.Equal(t, tenantID, gotID)

	// Not found → uuid.Nil, no error.
	nilID, err := store.ResolveTenantBySlug(ctx, h.AppDB, "does-not-exist")
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, nilID)
}

func TestTruncateSessionsInHarness(t *testing.T) {
	// Smoke test: TruncateTables with sessions must not fail.
	h := testdb.NewHarness(t)
	ctx := context.Background()
	tdb := tenantdb.New(h.AppDB)
	store := auth.NewSessionStore()

	tenantID := seedTenant(t, h.AdminDB, "Tenant-Trunc", "tenant-trunc")
	userID := seedUser(t, tdb, tenantID, "trunc@example.test")

	_, err := store.Create(ctx, tdb, tenantID, userID, 24*time.Hour, nil)
	require.NoError(t, err)

	h.TruncateTables("sessions", "users", "tenants")

	var count int64
	err = h.AdminDB.Raw("SELECT COUNT(*) FROM sessions").Scan(&count).Error
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)
}
