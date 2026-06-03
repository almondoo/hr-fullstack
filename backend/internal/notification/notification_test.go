package notification_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/notification"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
	"github.com/your-org/hr-saas/internal/platform/testdb"
)

// ---------------------------------------------------------------------------
// Shared test helpers (copied from onboarding_test.go pattern; AdminDB seeds)
// ---------------------------------------------------------------------------

func seedTenant(t *testing.T, adminDB *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO tenants (id, name, plan_code, status, slug) VALUES (?, ?, 'free', 'active', ?)`,
		id, "Test Tenant", id.String()[:8],
	).Error)
	return id
}

func seedUser(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO users (id, tenant_id, email, status) VALUES (?, ?, ?, 'active')`,
		id, tenantID, email,
	).Error)
	return id
}

func seedRoleWithPermissions(t *testing.T, adminDB *gorm.DB, tenantID uuid.UUID, name, permsJSON string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, adminDB.Exec(
		`INSERT INTO roles (id, tenant_id, name, permissions) VALUES (?, ?, ?, ?::jsonb)`,
		id, tenantID, name, permsJSON,
	).Error)
	return id
}

func assignRole(t *testing.T, adminDB *gorm.DB, userID, roleID uuid.UUID) {
	t.Helper()
	require.NoError(t, adminDB.Exec(
		`UPDATE users SET role_id = ? WHERE id = ?`, roleID, userID,
	).Error)
}

func truncateAll(h *testdb.Harness) {
	h.TruncateTables(
		"audit_logs",
		"email_deliveries",
		"notification_reads",
		"notifications",
		"notification_preferences",
		"notification_templates",
		"users",
		"roles",
		"sessions",
		"tenants",
	)
}

// syntheticKey returns a synthetic 32-byte test key (not a real secret).
func syntheticKey() []byte {
	return bytes.Repeat([]byte{0x42}, 32)
}

// setupCrypto injects a synthetic key for the global cipher.
func setupCrypto(t *testing.T) {
	t.Helper()
	crypto.ResetGlobalForTest()
	fc, err := crypto.NewFieldCipher(syntheticKey())
	require.NoError(t, err)
	crypto.SetGlobalForTest(fc)
	t.Cleanup(crypto.ResetGlobalForTest)
}

// failingSender is a MailSender that always errors (for retry/failure tests).
type failingSender struct{}

func (failingSender) Send(_ context.Context, _, _, _ string) (string, error) {
	return "", assert.AnError
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

func TestUpsertAndListTemplates(t *testing.T) {
	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	t.Cleanup(func() { truncateAll(h) })

	tmpl, err := svc.UpsertTemplate(ctx, notification.UpsertTemplateInput{
		TenantID:        tenantID,
		ActorID:         actorID,
		EventType:       "leave.approved",
		Channel:         notification.ChannelInApp,
		Locale:          "ja",
		SubjectTemplate: "休暇承認: {{.event_type}}",
		BodyTemplate:    "あなたの申請が承認されました。{{.deep_link}}",
		Active:          true,
	})
	require.NoError(t, err)
	assert.Equal(t, "leave.approved", tmpl.EventType)
	assert.True(t, tmpl.Active)

	// Upsert again (same key) updates rather than duplicates.
	tmpl2, err := svc.UpsertTemplate(ctx, notification.UpsertTemplateInput{
		TenantID: tenantID, ActorID: actorID, EventType: "leave.approved",
		Channel: notification.ChannelInApp, Locale: "ja",
		SubjectTemplate: "更新済み", BodyTemplate: "b", Active: true,
	})
	require.NoError(t, err)
	assert.Equal(t, tmpl.ID, tmpl2.ID, "upsert must update the same row")
	assert.Equal(t, "更新済み", tmpl2.SubjectTemplate)

	list, err := svc.ListTemplates(ctx, tenantID, "leave.approved")
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// ---------------------------------------------------------------------------
// Publish: channel expansion driven by preferences + template fallback + escaping
// ---------------------------------------------------------------------------

func TestPublishFansOutBothChannelsByDefault(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "recipient@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID:  tenantID,
		ActorID:   actorID,
		EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "recipient@example.com"},
		},
		DeepLink: "/app/approvals/abc",
	})
	require.NoError(t, err)
	// Default: both in_app and email channels deliver → 2 notifications + 1 email delivery.
	assert.Len(t, res.Notifications, 2)
	assert.Len(t, res.Deliveries, 1)
	assert.Equal(t, notification.DeliveryStatusQueued, res.Deliveries[0].Status)
	// Ciphertext is never exposed to the caller.
	assert.Nil(t, res.Deliveries[0].ToEmailEnc)
}

func TestPublishRespectsOptOut(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "r@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Opt the recipient OUT of email for this event.
	_, err := svc.SetPreference(ctx, notification.SetPreferenceInput{
		TenantID: tenantID, ActorID: actorID, UserID: recipient,
		EventType: "billing.payment_failed", Channel: notification.ChannelEmail,
		OptedIn: false,
	})
	require.NoError(t, err)

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "billing.payment_failed",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "r@example.com"},
		},
	})
	require.NoError(t, err)
	// Only the in_app notification is produced; email is suppressed.
	assert.Len(t, res.Notifications, 1)
	assert.Equal(t, notification.ChannelInApp, res.Notifications[0].Channel)
	assert.Len(t, res.Deliveries, 0)
	assert.GreaterOrEqual(t, res.Suppressed, 1)
}

func TestPublishForcedOverridesOptOut(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "sec@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Recipient opted out of BOTH channels.
	for _, ch := range []string{notification.ChannelInApp, notification.ChannelEmail} {
		_, err := svc.SetPreference(ctx, notification.SetPreferenceInput{
			TenantID: tenantID, ActorID: actorID, UserID: recipient,
			EventType: "security.alert", Channel: ch, OptedIn: false,
		})
		require.NoError(t, err)
	}

	// Mandatory (forced) publish ignores opt-out.
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "security.alert",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "sec@example.com"},
		},
		Mandatory: true,
	})
	require.NoError(t, err)
	assert.Len(t, res.Notifications, 2, "forced/mandatory must deliver despite opt-out")
	assert.Len(t, res.Deliveries, 1)
}

func TestPublishTemplateRenderingEscapesXSS(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "x@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Tenant template that interpolates a display value into the body.
	_, err := svc.UpsertTemplate(ctx, notification.UpsertTemplateInput{
		TenantID: tenantID, ActorID: actorID, EventType: "leave.requested",
		Channel: notification.ChannelInApp, Locale: "ja",
		SubjectTemplate: "申請: {{.actor_name}}",
		BodyTemplate:    "<p>{{.actor_name}} さんが申請しました</p>", Active: true,
	})
	require.NoError(t, err)

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "leave.requested",
		Recipients:   []notification.PublishRecipient{{UserID: recipient}},
		TemplateData: map[string]string{"actor_name": `<script>alert(1)</script>`},
	})
	require.NoError(t, err)
	require.Len(t, res.Notifications, 1)
	// html/template must have escaped the angle brackets — raw <script> must not appear.
	assert.NotContains(t, res.Notifications[0].BodyRef, "<script>")
	assert.Contains(t, res.Notifications[0].BodyRef, "&lt;script&gt;")
}

func TestPublishFallsBackToSystemDefaultTemplate(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "fb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// No tenant template defined → system default is used and rendered.
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{{UserID: recipient}},
		DeepLink:   "/app/x",
	})
	require.NoError(t, err)
	require.Len(t, res.Notifications, 1)
	assert.Contains(t, res.Notifications[0].Subject, "approval.pending")
	assert.Contains(t, res.Notifications[0].BodyRef, "/app/x")
}

// ---------------------------------------------------------------------------
// Reminder dedupe
// ---------------------------------------------------------------------------

func TestPublishDedupeSuppressesDuplicate(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "d@example.com")
	t.Cleanup(func() { truncateAll(h) })

	key := "reminder:approval:req-123"
	in := notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{{UserID: recipient}},
		DedupeKey:  &key,
	}
	res1, err := svc.Publish(ctx, in)
	require.NoError(t, err)
	assert.Len(t, res1.Notifications, 1)

	// Second publish with same dedupe_key is suppressed entirely.
	res2, err := svc.Publish(ctx, in)
	require.NoError(t, err)
	assert.Len(t, res2.Notifications, 0, "duplicate reminder must be suppressed by dedupe_key")
}

// TestPublishDedupeMultiChannelSingleRecipient is the regression test for the
// uq_notifications_dedupe collision: a recipient delivered on BOTH in_app AND
// email under one non-nil DedupeKey produces two notification rows that both
// carry the same dedupe_key.  A tenant-wide (tenant_id, dedupe_key) unique index
// would make the second INSERT violate uniqueness and roll the whole Publish
// back.  With the index scoped to (tenant_id, recipient_user_id, channel,
// dedupe_key) the multi-channel fan-out succeeds.
func TestPublishDedupeMultiChannelSingleRecipient(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "multi@example.com")
	t.Cleanup(func() { truncateAll(h) })

	key := "reminder:approval:multi-chan-1"
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			// Email present → default opt-in delivers BOTH in_app and email.
			{UserID: recipient, EmailPlaintext: "multi@example.com"},
		},
		DedupeKey: &key,
	})
	require.NoError(t, err, "multi-channel Publish with a dedupe_key must not violate the unique index")
	// Both channels deliver → 2 notification rows + 1 queued email delivery.
	assert.Len(t, res.Notifications, 2, "in_app + email rows must both be created")
	assert.Len(t, res.Deliveries, 1)

	// Exactly two notification rows persisted, both carrying the dedupe_key, one
	// per channel — confirms the rows coexist under the per-channel-scoped index.
	var total, withKey, inApp, email int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ?`, tenantID,
	).Scan(&total).Error)
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ? AND dedupe_key = ?`, tenantID, key,
	).Scan(&withKey).Error)
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ? AND channel = ?`, tenantID, notification.ChannelInApp,
	).Scan(&inApp).Error)
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ? AND channel = ?`, tenantID, notification.ChannelEmail,
	).Scan(&email).Error)
	assert.Equal(t, int64(2), total, "exactly two notification rows must persist")
	assert.Equal(t, int64(2), withKey, "both rows must carry the dedupe_key")
	assert.Equal(t, int64(1), inApp, "one in_app row")
	assert.Equal(t, int64(1), email, "one email row")

	// A repeat Publish of the same reminder is still suppressed entirely
	// (issuance-level dedupe via the application COUNT check).
	res2, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "multi@example.com"},
		},
		DedupeKey: &key,
	})
	require.NoError(t, err)
	assert.Len(t, res2.Notifications, 0, "repeat reminder must be suppressed by the application dedupe check")
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ?`, tenantID,
	).Scan(&total).Error)
	assert.Equal(t, int64(2), total, "no additional rows after the suppressed repeat")
}

// TestPublishDedupeMultiRecipientSharedKey covers multiple recipients sharing a
// single DedupeKey within one Publish.  Each recipient (× each delivered
// channel) gets its own notification row, all carrying the same dedupe_key; the
// per-(recipient, channel)-scoped index lets them coexist while the tenant-wide
// application COUNT check still suppresses a repeat Publish.
func TestPublishDedupeMultiRecipientSharedKey(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	rcpt1 := seedUser(t, h.AdminDB, tenantID, "share1@example.com")
	rcpt2 := seedUser(t, h.AdminDB, tenantID, "share2@example.com")
	t.Cleanup(func() { truncateAll(h) })

	key := "reminder:approval:shared-key-1"
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: rcpt1, EmailPlaintext: "share1@example.com"},
			{UserID: rcpt2, EmailPlaintext: "share2@example.com"},
		},
		DedupeKey: &key,
	})
	require.NoError(t, err, "multi-recipient Publish sharing a dedupe_key must not violate the unique index")
	// 2 recipients × (in_app + email) = 4 notification rows; 2 email deliveries.
	assert.Len(t, res.Notifications, 4, "two recipients × two channels = four rows")
	assert.Len(t, res.Deliveries, 2)

	var withKey int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ? AND dedupe_key = ?`, tenantID, key,
	).Scan(&withKey).Error)
	assert.Equal(t, int64(4), withKey, "all four rows must persist under the shared dedupe_key")

	// Repeat Publish (same key) is suppressed — no new rows.
	res2, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: rcpt1, EmailPlaintext: "share1@example.com"},
			{UserID: rcpt2, EmailPlaintext: "share2@example.com"},
		},
		DedupeKey: &key,
	})
	require.NoError(t, err)
	assert.Len(t, res2.Notifications, 0, "repeat shared-key reminder must be suppressed")
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications WHERE tenant_id = ? AND dedupe_key = ?`, tenantID, key,
	).Scan(&withKey).Error)
	assert.Equal(t, int64(4), withKey, "no additional rows after the suppressed repeat")
}

// ---------------------------------------------------------------------------
// In-app inbox: list / unread count / item-level read ownership
// ---------------------------------------------------------------------------

func TestInboxAndUnreadCountAndMarkRead(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "inbox@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{{UserID: recipient}},
	})
	require.NoError(t, err)
	require.Len(t, res.Notifications, 1)
	notifID := res.Notifications[0].ID

	// Inbox lists the recipient's notification; initially unread.
	items, err := svc.ListInbox(ctx, notification.ListInboxInput{TenantID: tenantID, UserID: recipient})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.False(t, items[0].Read)

	count, err := svc.UnreadCount(ctx, tenantID, recipient)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// Mark read (by the recipient).
	read, err := svc.MarkRead(ctx, notification.MarkReadInput{
		TenantID: tenantID, ActorID: recipient, NotificationID: notifID, UserID: recipient,
	})
	require.NoError(t, err)
	assert.Equal(t, notifID, read.NotificationID)

	count, err = svc.UnreadCount(ctx, tenantID, recipient)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	// Idempotent re-read.
	_, err = svc.MarkRead(ctx, notification.MarkReadInput{
		TenantID: tenantID, ActorID: recipient, NotificationID: notifID, UserID: recipient,
	})
	require.NoError(t, err)

	// UnreadOnly filter now returns nothing.
	unread, err := svc.ListInbox(ctx, notification.ListInboxInput{
		TenantID: tenantID, UserID: recipient, UnreadOnly: true,
	})
	require.NoError(t, err)
	assert.Empty(t, unread)
}

func TestMarkReadOtherUsersNotificationForbidden(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	owner := seedUser(t, h.AdminDB, tenantID, "owner@example.com")
	other := seedUser(t, h.AdminDB, tenantID, "other@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{{UserID: owner}},
	})
	require.NoError(t, err)
	require.Len(t, res.Notifications, 1)
	notifID := res.Notifications[0].ID

	// "other" (a different user in the same tenant) attempts to mark owner's
	// notification read → ErrForbidden (item-level ownership).
	_, err = svc.MarkRead(ctx, notification.MarkReadInput{
		TenantID: tenantID, ActorID: other, NotificationID: notifID, UserID: other,
	})
	assert.ErrorIs(t, err, notification.ErrForbidden,
		"a non-owner must not mark another user's notification read")

	// "other" cannot see owner's notification in their own inbox.
	items, err := svc.ListInbox(ctx, notification.ListInboxInput{TenantID: tenantID, UserID: other})
	require.NoError(t, err)
	assert.Empty(t, items, "a user must only see their own notifications")
}

// ---------------------------------------------------------------------------
// Email delivery: mock send success, retry, permanent failure, status bounds
// ---------------------------------------------------------------------------

func TestProcessDeliveryMockSendSucceeds(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "ok@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "ok@example.com"},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Deliveries, 1)
	deliveryID := res.Deliveries[0].ID

	d, err := svc.ProcessDelivery(ctx, tenantID, deliveryID, actorID, nil)
	require.NoError(t, err)
	assert.Equal(t, notification.DeliveryStatusSent, d.Status)
	assert.Equal(t, 1, d.Attempts)
	assert.NotEmpty(t, d.ProviderMessageID)
	assert.NotNil(t, d.SentAt)
	assert.Nil(t, d.ToEmailEnc, "ciphertext must not be returned")
}

func TestProcessDeliveryRetryThenPermanentFailure(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	// Use a sender that always fails.
	svc := notification.NewServiceWithMailer(tdb, failingSender{})
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "fail@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "fail@example.com"},
		},
	})
	require.NoError(t, err)
	require.Len(t, res.Deliveries, 1)
	deliveryID := res.Deliveries[0].ID

	// max_attempts default is 3. Process up to the limit; each stays "failed".
	var d *notification.EmailDelivery
	for i := 1; i <= 3; i++ {
		d, err = svc.ProcessDelivery(ctx, tenantID, deliveryID, actorID, nil)
		require.NoError(t, err, "attempt %d should process", i)
		assert.Equal(t, notification.DeliveryStatusFailed, d.Status)
		assert.Equal(t, i, d.Attempts)
	}

	// After reaching max attempts, further processing is rejected (permanent).
	_, err = svc.ProcessDelivery(ctx, tenantID, deliveryID, actorID, nil)
	assert.ErrorIs(t, err, notification.ErrInvalidTransition,
		"processing past max_attempts must be rejected as permanent failure")
}

func TestMarkBouncedTransitionBounds(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "b@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "b@example.com"},
		},
	})
	require.NoError(t, err)
	deliveryID := res.Deliveries[0].ID

	// queued → bounced is NOT allowed (must be sent first).
	_, err = svc.MarkBounced(ctx, notification.MarkBouncedInput{
		TenantID: tenantID, ActorID: actorID, DeliveryID: deliveryID,
		Status: notification.DeliveryStatusBounced,
	})
	assert.ErrorIs(t, err, notification.ErrInvalidTransition,
		"queued -> bounced must be rejected")

	// Send first, then bounce.
	_, err = svc.ProcessDelivery(ctx, tenantID, deliveryID, actorID, nil)
	require.NoError(t, err)
	d, err := svc.MarkBounced(ctx, notification.MarkBouncedInput{
		TenantID: tenantID, ActorID: actorID, DeliveryID: deliveryID,
		Status: notification.DeliveryStatusBounced,
	})
	require.NoError(t, err)
	assert.Equal(t, notification.DeliveryStatusBounced, d.Status)
	assert.NotNil(t, d.BouncedAt)
}

// ---------------------------------------------------------------------------
// Sensitive email decryption: RBAC re-validated at service layer
// ---------------------------------------------------------------------------

func TestGetDeliveryEmailWithPermissionDecrypts(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "vip@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["notification:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	const addr = "vip@example.com"
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: addr},
		},
	})
	require.NoError(t, err)
	deliveryID := res.Deliveries[0].ID

	email, err := svc.GetDeliveryEmail(ctx, notification.GetDeliveryEmailInput{
		TenantID: tenantID, ActorID: actorID, DeliveryID: deliveryID,
	})
	require.NoError(t, err)
	assert.Equal(t, addr, email, "decrypted destination email must match original")
}

func TestGetDeliveryEmailWithoutPermissionForbidden(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	// Actor has no role → no notification:read_sensitive.
	actorID := seedUser(t, h.AdminDB, tenantID, "noperm@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "secret@example.com")
	t.Cleanup(func() { truncateAll(h) })

	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: "secret@example.com"},
		},
	})
	require.NoError(t, err)
	deliveryID := res.Deliveries[0].ID

	email, err := svc.GetDeliveryEmail(ctx, notification.GetDeliveryEmailInput{
		TenantID: tenantID, ActorID: actorID, DeliveryID: deliveryID,
	})
	assert.ErrorIs(t, err, notification.ErrForbidden,
		"service layer must reject sensitive read without permission")
	assert.Empty(t, email, "plaintext must not be returned when permission is denied")
}

// ---------------------------------------------------------------------------
// PII / encryption-at-rest negative checks
// ---------------------------------------------------------------------------

func TestEmailAddressEncryptedAtRestNotPlaintext(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "rest@example.com")
	t.Cleanup(func() { truncateAll(h) })

	const addr = "rest@example.com"
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: addr},
		},
	})
	require.NoError(t, err)
	deliveryID := res.Deliveries[0].ID

	// The stored to_email_enc must be ciphertext, NOT the plaintext address.
	var row struct {
		ToEmailEnc  []byte `gorm:"column:to_email_enc"`
		ToEmailHash string `gorm:"column:to_email_hash"`
	}
	require.NoError(t, h.AdminDB.Raw(
		`SELECT to_email_enc, to_email_hash FROM email_deliveries WHERE id = ? LIMIT 1`,
		deliveryID,
	).Scan(&row).Error)
	require.NotNil(t, row.ToEmailEnc)
	assert.NotEqual(t, []byte(addr), row.ToEmailEnc, "plaintext email must NOT be stored")
	assert.NotContains(t, string(row.ToEmailEnc), addr, "plaintext email must NOT appear in ciphertext column")
	assert.NotEmpty(t, row.ToEmailHash, "a non-reversible hash must be stored for matching")
	assert.NotContains(t, row.ToEmailHash, "@", "hash must not contain the raw address")
}

func TestAuditLogContainsNoEmailPII(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "audit@example.com")
	roleID := seedRoleWithPermissions(t, h.AdminDB, tenantID, "sensitive_reader",
		`{"perms":["notification:read_sensitive"]}`)
	assignRole(t, h.AdminDB, actorID, roleID)
	t.Cleanup(func() { truncateAll(h) })

	const addr = "audit-secret@example.com"
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipient, EmailPlaintext: addr},
		},
	})
	require.NoError(t, err)
	deliveryID := res.Deliveries[0].ID

	// Exercise the audited paths (process + sensitive read).
	_, err = svc.ProcessDelivery(ctx, tenantID, deliveryID, actorID, nil)
	require.NoError(t, err)
	_, err = svc.GetDeliveryEmail(ctx, notification.GetDeliveryEmailInput{
		TenantID: tenantID, ActorID: actorID, DeliveryID: deliveryID,
	})
	require.NoError(t, err)

	// No audit_logs row may contain the email address (in resource_id or action).
	var matchCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs
		 WHERE resource_id LIKE ? OR action LIKE ?`,
		"%"+addr+"%", "%"+addr+"%",
	).Scan(&matchCount).Error)
	assert.Equal(t, int64(0), matchCount, "audit_logs must not contain the email address")

	// Defence-in-depth: no audit row's resource_id should even contain an "@".
	var atCount int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM audit_logs WHERE resource_id LIKE ?`, "%@%",
	).Scan(&atCount).Error)
	assert.Equal(t, int64(0), atCount, "audit resource_id must be opaque (no email)")
}

func TestNotificationBodyContainsNoSensitivePII(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantID := seedTenant(t, h.AdminDB)
	actorID := seedUser(t, h.AdminDB, tenantID, "actor@example.com")
	recipient := seedUser(t, h.AdminDB, tenantID, "body@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Default template references only the deep link / event type — no PII.
	_, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantID, ActorID: actorID, EventType: "leave.approved",
		Recipients: []notification.PublishRecipient{{UserID: recipient}},
		DeepLink:   "/app/leave/opaque-id",
	})
	require.NoError(t, err)

	// Verify no notification body_ref / subject contains an email/account marker.
	var count int64
	require.NoError(t, h.AdminDB.Raw(
		`SELECT COUNT(1) FROM notifications
		 WHERE tenant_id = ? AND (body_ref LIKE ? OR subject LIKE ?)`,
		tenantID, "%@example.com%", "%@example.com%",
	).Scan(&count).Error)
	assert.Equal(t, int64(0), count, "notification body/subject must not embed PII")
}

// ---------------------------------------------------------------------------
// RLS cross-tenant / cross-user isolation
// ---------------------------------------------------------------------------

func TestCrossTenantIsolation(t *testing.T) {
	setupCrypto(t)

	h := testdb.NewHarness(t)
	tdb := tenantdb.New(h.AppDB)
	svc := notification.NewService(tdb)
	ctx := context.Background()

	tenantA := seedTenant(t, h.AdminDB)
	actorA := seedUser(t, h.AdminDB, tenantA, "actora@example.com")
	recipientA := seedUser(t, h.AdminDB, tenantA, "ra@example.com")

	tenantB := seedTenant(t, h.AdminDB)
	actorB := seedUser(t, h.AdminDB, tenantB, "actorb@example.com")
	t.Cleanup(func() { truncateAll(h) })

	// Publish a notification in tenant A.
	res, err := svc.Publish(ctx, notification.PublishInput{
		TenantID: tenantA, ActorID: actorA, EventType: "approval.pending",
		Recipients: []notification.PublishRecipient{
			{UserID: recipientA, EmailPlaintext: "ra@example.com"},
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.Notifications)
	notifA := res.Notifications[0].ID
	deliveryA := res.Deliveries[0].ID

	// Tenant B context must not see tenant A's notifications.
	items, err := svc.ListInbox(ctx, notification.ListInboxInput{TenantID: tenantB, UserID: recipientA})
	require.NoError(t, err)
	assert.Empty(t, items, "tenant B must not see tenant A notifications")

	// Tenant B cannot mark tenant A's notification read (RLS hides the row → NotFound).
	_, err = svc.MarkRead(ctx, notification.MarkReadInput{
		TenantID: tenantB, ActorID: actorB, NotificationID: notifA, UserID: actorB,
	})
	assert.ErrorIs(t, err, notification.ErrNotFound,
		"tenant B must not mark tenant A's notification read")

	// Tenant B cannot process tenant A's delivery (RLS hides the row → NotFound).
	_, err = svc.ProcessDelivery(ctx, tenantB, deliveryA, actorB, nil)
	assert.ErrorIs(t, err, notification.ErrNotFound,
		"tenant B must not process tenant A's delivery")

	// Tenant B sees no deliveries for tenant A's notification id.
	ds, err := svc.ListDeliveries(ctx, tenantB, notifA)
	require.NoError(t, err)
	assert.Empty(t, ds, "tenant B must not list tenant A deliveries")
}
