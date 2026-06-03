package notification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("notification: not found")
	ErrInvalidTransition = errors.New("notification: invalid status transition")
	ErrForbidden         = errors.New("notification: permission denied")
)

// permReadSensitive is the RBAC permission required to decrypt the destination
// email address of an email delivery (re-validated at the service layer).
const permReadSensitive = "notification:read_sensitive"

// ---------------------------------------------------------------------------
// Email delivery status transitions (allow-list)
// ---------------------------------------------------------------------------

// allowedDeliveryTransitions defines legal email_deliveries.status moves.
//
//	queued    -> sent | failed
//	failed    -> sent | failed (retry; permanent failure is failed-at-max)
//	sent      -> bounced | complained
//	bounced   -> (terminal)
//	complained-> (terminal)
var allowedDeliveryTransitions = map[string]map[string]bool{
	DeliveryStatusQueued: {
		DeliveryStatusSent:   true,
		DeliveryStatusFailed: true,
	},
	DeliveryStatusFailed: {
		DeliveryStatusSent:   true,
		DeliveryStatusFailed: true,
	},
	DeliveryStatusSent: {
		DeliveryStatusBounced:    true,
		DeliveryStatusComplained: true,
	},
}

// isDeliveryTransitionAllowed reports whether moving from current → next is valid.
func isDeliveryTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedDeliveryTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// ---------------------------------------------------------------------------
// MailSender abstraction (mock for MVP; SES/SendGrid adapter in production)
// ---------------------------------------------------------------------------

// MailSender abstracts the real email transport.  Send returns an opaque
// provider message id used later for bounce/complaint reconciliation.
//
// Implementations MUST NOT log the recipient address or message body as PII.
type MailSender interface {
	Send(ctx context.Context, to, subject, body string) (providerMessageID string, err error)
}

// MockSender is the development MailSender: it does not send anything.  It logs
// only non-PII metadata (a hash of the recipient and the subject length) and
// returns a synthetic provider message id.  Used as the default by NewService.
type MockSender struct{}

// Send implements MailSender for development.  It never transmits mail and never
// logs the recipient address or body.
func (MockSender) Send(_ context.Context, to, subject, _ string) (string, error) {
	// Log only non-PII: a hash prefix of the recipient and the subject length.
	slog.Info("notification: mock mail send",
		"recipient_hash", hashEmail(to)[:12],
		"subject_len", len(subject),
	)
	return "mock-" + uuid.NewString(), nil
}

// hashEmail returns a deterministic non-reversible hex SHA-256 of a normalised
// email address.  Used for matching/dedupe without exposing the address.
func hashEmail(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service provides business logic for the notification platform.
type Service struct {
	tdb    *tenantdb.TenantDB
	mailer MailSender
}

// NewService constructs a Service with the default MockSender.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb, mailer: MockSender{}}
}

// NewServiceWithMailer constructs a Service with a custom MailSender (e.g. an
// SES/SendGrid adapter in production, or a test double).
func NewServiceWithMailer(tdb *tenantdb.TenantDB, mailer MailSender) *Service {
	if mailer == nil {
		mailer = MockSender{}
	}
	return &Service{tdb: tdb, mailer: mailer}
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

// UpsertTemplateInput holds fields for creating/updating a template.
type UpsertTemplateInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	EventType       string
	Channel         string
	Locale          string
	SubjectTemplate string
	BodyTemplate    string
	Active          bool
	IP              *string
}

// UpsertTemplate creates or updates a per-tenant template keyed by
// (tenant_id, event_type, channel, locale).
func (s *Service) UpsertTemplate(ctx context.Context, in UpsertTemplateInput) (*Template, error) {
	locale := in.Locale
	if locale == "" {
		locale = "ja"
	}
	tmpl := Template{
		ID:              uuid.New(),
		TenantID:        in.TenantID,
		EventType:       in.EventType,
		Channel:         in.Channel,
		Locale:          locale,
		SubjectTemplate: in.SubjectTemplate,
		BodyTemplate:    in.BodyTemplate,
		Active:          in.Active,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO notification_templates
			   (id, tenant_id, event_type, channel, locale,
			    subject_template, body_template, active)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (tenant_id, event_type, channel, locale) DO UPDATE
			   SET subject_template = EXCLUDED.subject_template,
			       body_template    = EXCLUDED.body_template,
			       active           = EXCLUDED.active,
			       updated_at       = now()`,
			tmpl.ID, tmpl.TenantID, tmpl.EventType, tmpl.Channel, tmpl.Locale,
			tmpl.SubjectTemplate, tmpl.BodyTemplate, tmpl.Active,
		).Error; err != nil {
			return fmt.Errorf("notification: upsert template: %w", err)
		}
		// Re-read to obtain the persisted row (handles the update path).
		if err := tx.Raw(
			`SELECT id, tenant_id, event_type, channel, locale,
			        subject_template, body_template, active, created_at, updated_at
			 FROM notification_templates
			 WHERE tenant_id = ? AND event_type = ? AND channel = ? AND locale = ? LIMIT 1`,
			in.TenantID, in.EventType, in.Channel, locale,
		).Scan(&tmpl).Error; err != nil {
			return fmt.Errorf("notification: upsert template re-read: %w", err)
		}
		idStr := tmpl.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "notification_template.upserted",
			ResourceType: "notification_template",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &tmpl, nil
}

// ListTemplates returns active templates for a tenant, optionally filtered by
// event_type.
func (s *Service) ListTemplates(ctx context.Context, tenantID uuid.UUID, eventType string) ([]Template, error) {
	var tmpls []Template
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, event_type, channel, locale,
		             subject_template, body_template, active, created_at, updated_at
		      FROM notification_templates
		      WHERE tenant_id = ?`
		args := []any{tenantID}
		if eventType != "" {
			q += ` AND event_type = ?`
			args = append(args, eventType)
		}
		q += ` ORDER BY event_type, channel, locale`
		return tx.Raw(q, args...).Scan(&tmpls).Error
	})
	if err != nil {
		return nil, err
	}
	return tmpls, nil
}

// ---------------------------------------------------------------------------
// Template rendering (server-side HTML escaping for XSS safety)
// ---------------------------------------------------------------------------

// renderTemplate renders a subject/body template with the given non-sensitive
// values.  html/template performs contextual escaping so injected display
// values cannot break out of the template (XSS prevention).
//
// SECURITY: callers MUST pass only non-sensitive display values / opaque IDs in
// data — never マイナンバー/口座/健診 or other sensitive PII.
func renderTemplate(tmplText string, data map[string]string) (string, error) {
	t, err := template.New("n").Option("missingkey=zero").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("notification: parse template: %w", err)
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("notification: execute template: %w", err)
	}
	return sb.String(), nil
}

// systemDefaultTemplate returns a built-in fallback template for an event/channel
// when no tenant-specific template exists.  Bodies contain placeholders only and
// never embed sensitive PII; detail is referenced via the deep link.
func systemDefaultTemplate(eventType, channel string) (subject, body string) {
	subject = "通知: {{.event_type}}"
	body = "イベント {{.event_type}} が発生しました。詳細はアプリ内のリンクをご確認ください: {{.deep_link}}"
	_ = channel
	return subject, body
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

// SetPreferenceInput holds fields for setting a recipient's channel preference.
type SetPreferenceInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	UserID    uuid.UUID
	EventType string
	Channel   string
	OptedIn   bool
	Forced    bool
	IP        *string
}

// SetPreference upserts a recipient preference keyed by
// (tenant_id, user_id, event_type, channel).  The target user must belong to
// the tenant (verified at the service layer; users has no composite FK).
func (s *Service) SetPreference(ctx context.Context, in SetPreferenceInput) (*Preference, error) {
	pref := Preference{
		ID:        uuid.New(),
		TenantID:  in.TenantID,
		UserID:    in.UserID,
		EventType: in.EventType,
		Channel:   in.Channel,
		OptedIn:   in.OptedIn,
		Forced:    in.Forced,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := verifyUserInTenant(tx, in.TenantID, in.UserID); err != nil {
			return err
		}
		if err := tx.Exec(
			`INSERT INTO notification_preferences
			   (id, tenant_id, user_id, event_type, channel, opted_in, forced)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (tenant_id, user_id, event_type, channel) DO UPDATE
			   SET opted_in   = EXCLUDED.opted_in,
			       forced     = EXCLUDED.forced,
			       updated_at = now()`,
			pref.ID, pref.TenantID, pref.UserID, pref.EventType,
			pref.Channel, pref.OptedIn, pref.Forced,
		).Error; err != nil {
			return fmt.Errorf("notification: set preference: %w", err)
		}
		if err := tx.Raw(
			`SELECT id, tenant_id, user_id, event_type, channel, opted_in, forced,
			        created_at, updated_at
			 FROM notification_preferences
			 WHERE tenant_id = ? AND user_id = ? AND event_type = ? AND channel = ? LIMIT 1`,
			in.TenantID, in.UserID, in.EventType, in.Channel,
		).Scan(&pref).Error; err != nil {
			return fmt.Errorf("notification: set preference re-read: %w", err)
		}
		idStr := pref.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "notification_preference.set",
			ResourceType: "notification_preference",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &pref, nil
}

// channelDecision is the resolved per-channel delivery decision for a recipient.
type channelDecision struct {
	channel string
	deliver bool
}

// resolveChannelDecision computes whether to deliver on a channel for a recipient,
// applying preference + forced rules.  Fallback defaults when no row exists:
//   - in_app: on
//   - email:  on
//
// A mandatory event (mandatory==true) or a stored forced=true preference always
// delivers, ignoring opt-out.
func resolveChannelDecision(tx *gorm.DB, tenantID, userID uuid.UUID, eventType, channel string, mandatory bool) (channelDecision, error) {
	var rows []struct {
		OptedIn bool `gorm:"column:opted_in"`
		Forced  bool `gorm:"column:forced"`
	}
	if err := tx.Raw(
		`SELECT opted_in, forced FROM notification_preferences
		 WHERE tenant_id = ? AND user_id = ? AND event_type = ? AND channel = ? LIMIT 1`,
		tenantID, userID, eventType, channel,
	).Scan(&rows).Error; err != nil {
		return channelDecision{}, fmt.Errorf("notification: resolve preference: %w", err)
	}

	// Default (no explicit preference): both in_app and email default to on.
	optedIn := true
	forced := false
	if len(rows) > 0 {
		optedIn = rows[0].OptedIn
		forced = rows[0].Forced
	}

	deliver := optedIn || forced || mandatory
	return channelDecision{channel: channel, deliver: deliver}, nil
}

// ---------------------------------------------------------------------------
// Publish
// ---------------------------------------------------------------------------

// PublishRecipient identifies one target user and their email (plaintext, used
// only to encrypt/hash — never persisted in plaintext).
type PublishRecipient struct {
	UserID uuid.UUID
	// EmailPlaintext is the recipient email address.  It is encrypted and hashed
	// before storage; the plaintext is NEVER persisted, logged, or audited.
	EmailPlaintext string
}

// PublishInput is a single notification event to fan out to recipients.
type PublishInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	EventType string
	Locale    string
	// Recipients to deliver to.
	Recipients []PublishRecipient
	// ResourceType / ResourceID: opaque deep-link target.
	ResourceType string
	ResourceID   *uuid.UUID
	// DeepLink is a non-sensitive opaque deep-link reference rendered into the body.
	DeepLink string
	// DedupeKey suppresses duplicate reminders within the tenant when set.
	DedupeKey *string
	// Mandatory forces delivery ignoring opt-out (security/legal/critical events).
	Mandatory bool
	// TemplateData are extra non-sensitive display values for rendering.
	// SECURITY: never include sensitive PII here.
	TemplateData map[string]string
	IP           *string
}

// PublishResult summarises what Publish produced.
type PublishResult struct {
	Notifications []Notification
	Deliveries    []EmailDelivery
	// Suppressed is the number of (recipient, channel) pairs skipped by opt-out.
	Suppressed int
}

// Publish fans out an event to recipients.  For each recipient it resolves the
// channel decision from preferences (honouring forced/mandatory), renders the
// tenant template (falling back to a system default), and creates an in-app
// notification row and/or a queued email delivery row.  All writes happen in a
// single transaction together with the audit record.
//
// Email addresses are encrypted (to_email_enc) and hashed (to_email_hash); the
// plaintext is never persisted.  When a DedupeKey collides with an existing
// notification for the tenant, the duplicate is suppressed (reminder dedupe).
func (s *Service) Publish(ctx context.Context, in PublishInput) (*PublishResult, error) {
	locale := in.Locale
	if locale == "" {
		locale = "ja"
	}

	// Pre-encrypt recipient emails BEFORE opening the transaction (fail-fast on
	// crypto error; plaintext never appears in error messages).
	type recCrypto struct {
		userID  uuid.UUID
		enc     []byte
		hash    string
		hasMail bool
	}
	prepped := make([]recCrypto, 0, len(in.Recipients))
	for _, r := range in.Recipients {
		rc := recCrypto{userID: r.UserID}
		if r.EmailPlaintext != "" {
			enc, err := crypto.Encrypt([]byte(r.EmailPlaintext))
			if err != nil {
				return nil, fmt.Errorf("notification: encrypt recipient email: %w", err)
			}
			rc.enc = enc
			rc.hash = hashEmail(r.EmailPlaintext)
			rc.hasMail = true
		}
		prepped = append(prepped, rc)
	}

	result := &PublishResult{}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Reminder (issuance-level) dedupe: if ANY notification row already carries
		// this dedupe_key for the tenant, a prior Publish of the same reminder has
		// run — suppress this whole Publish (idempotent).  This is keyed on
		// (tenant_id, dedupe_key) to match the logical reminder identity: one key =
		// one issuance, regardless of how many (recipient × channel) rows that
		// issuance expands into.  The DB-level uq_notifications_dedupe index is
		// scoped to (tenant_id, recipient_user_id, channel, dedupe_key) so that the
		// per-channel / per-recipient rows of a SINGLE Publish (which all share the
		// key) do not collide — see db/migrations/00009_notification.sql.
		if in.DedupeKey != nil && *in.DedupeKey != "" {
			var existing int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM notifications
				 WHERE tenant_id = ? AND dedupe_key = ?`,
				in.TenantID, *in.DedupeKey,
			).Scan(&existing).Error; err != nil {
				return fmt.Errorf("notification: publish dedupe check: %w", err)
			}
			if existing > 0 {
				// Duplicate reminder — suppress entirely (idempotent).
				return nil
			}
		}

		// Resolve template once per channel (tenant-specific, else system default).
		inAppSubjTmpl, inAppBodyTmpl, err := s.resolveTemplate(tx, in.TenantID, in.EventType, ChannelInApp, locale)
		if err != nil {
			return err
		}
		emailSubjTmpl, emailBodyTmpl, err := s.resolveTemplate(tx, in.TenantID, in.EventType, ChannelEmail, locale)
		if err != nil {
			return err
		}

		data := map[string]string{
			"event_type": in.EventType,
			"deep_link":  in.DeepLink,
		}
		for k, v := range in.TemplateData {
			data[k] = v
		}

		for ri, r := range in.Recipients {
			rc := prepped[ri]

			// Verify recipient belongs to the tenant (users has no composite FK).
			if err := verifyUserInTenant(tx, in.TenantID, r.UserID); err != nil {
				return err
			}

			// --- in-app channel ---
			inAppDec, err := resolveChannelDecision(tx, in.TenantID, r.UserID, in.EventType, ChannelInApp, in.Mandatory)
			if err != nil {
				return err
			}
			if inAppDec.deliver {
				subj, err := renderTemplate(inAppSubjTmpl, data)
				if err != nil {
					return err
				}
				bodyRef, err := renderTemplate(inAppBodyTmpl, data)
				if err != nil {
					return err
				}
				n, err := insertNotification(tx, in, r.UserID, ChannelInApp, subj, bodyRef)
				if err != nil {
					return err
				}
				result.Notifications = append(result.Notifications, *n)
			} else {
				result.Suppressed++
			}

			// --- email channel ---
			emailDec, err := resolveChannelDecision(tx, in.TenantID, r.UserID, in.EventType, ChannelEmail, in.Mandatory)
			if err != nil {
				return err
			}
			if emailDec.deliver && rc.hasMail {
				subj, err := renderTemplate(emailSubjTmpl, data)
				if err != nil {
					return err
				}
				bodyRef, err := renderTemplate(emailBodyTmpl, data)
				if err != nil {
					return err
				}
				n, err := insertNotification(tx, in, r.UserID, ChannelEmail, subj, bodyRef)
				if err != nil {
					return err
				}
				result.Notifications = append(result.Notifications, *n)

				d := EmailDelivery{
					ID:             uuid.New(),
					TenantID:       in.TenantID,
					NotificationID: n.ID,
					ToEmailHash:    rc.hash,
					ToEmailEnc:     rc.enc,
					Provider:       "mock",
					Status:         DeliveryStatusQueued,
					Attempts:       0,
					MaxAttempts:    3,
				}
				if err := tx.Exec(
					`INSERT INTO email_deliveries
					   (id, tenant_id, notification_id, to_email_hash, to_email_enc,
					    provider, status, attempts, max_attempts)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					d.ID, d.TenantID, d.NotificationID, d.ToEmailHash, d.ToEmailEnc,
					d.Provider, d.Status, d.Attempts, d.MaxAttempts,
				).Error; err != nil {
					return fmt.Errorf("notification: publish insert delivery: %w", err)
				}
				d.ToEmailEnc = nil // never expose ciphertext to callers
				result.Deliveries = append(result.Deliveries, d)
			} else {
				result.Suppressed++ // suppressed: no permission, no preference, or no address
			}
		}

		// Audit: opaque resource id only (the event's deep-link resource, or nil).
		var idStr *string
		if in.ResourceID != nil {
			s := in.ResourceID.String()
			idStr = &s
		}
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "notification.published",
			ResourceType: "notification_event",
			ResourceID:   idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// resolveTemplate returns the (subject, body) template text for an event/channel,
// preferring the tenant-specific active template and falling back to the system
// default when none exists.
func (s *Service) resolveTemplate(tx *gorm.DB, tenantID uuid.UUID, eventType, channel, locale string) (string, string, error) {
	var rows []struct {
		SubjectTemplate string `gorm:"column:subject_template"`
		BodyTemplate    string `gorm:"column:body_template"`
	}
	if err := tx.Raw(
		`SELECT subject_template, body_template FROM notification_templates
		 WHERE tenant_id = ? AND event_type = ? AND channel = ? AND locale = ? AND active = true
		 LIMIT 1`,
		tenantID, eventType, channel, locale,
	).Scan(&rows).Error; err != nil {
		return "", "", fmt.Errorf("notification: resolve template: %w", err)
	}
	if len(rows) > 0 {
		return rows[0].SubjectTemplate, rows[0].BodyTemplate, nil
	}
	subj, body := systemDefaultTemplate(eventType, channel)
	return subj, body, nil
}

// insertNotification inserts a single notification row and returns it.
func insertNotification(tx *gorm.DB, in PublishInput, recipientUserID uuid.UUID, channel, subject, bodyRef string) (*Notification, error) {
	n := Notification{
		ID:              uuid.New(),
		TenantID:        in.TenantID,
		RecipientUserID: recipientUserID,
		EventType:       in.EventType,
		Channel:         channel,
		Subject:         subject,
		BodyRef:         bodyRef,
		ResourceType:    in.ResourceType,
		ResourceID:      in.ResourceID,
		DedupeKey:       in.DedupeKey,
		Status:          NotificationStatusCreated,
	}
	if err := tx.Exec(
		`INSERT INTO notifications
		   (id, tenant_id, recipient_user_id, event_type, channel, subject,
		    body_ref, resource_type, resource_id, dedupe_key, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.TenantID, n.RecipientUserID, n.EventType, n.Channel, n.Subject,
		n.BodyRef, n.ResourceType, n.ResourceID, n.DedupeKey, n.Status,
	).Error; err != nil {
		return nil, fmt.Errorf("notification: insert notification: %w", err)
	}
	return &n, nil
}

// verifyUserInTenant confirms the user belongs to the tenant.  users has no
// UNIQUE(id, tenant_id) constraint so a composite FK is not possible; this is
// the service-layer defence (mirrors onboarding assignee verification).
func verifyUserInTenant(tx *gorm.DB, tenantID, userID uuid.UUID) error {
	var count int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
		userID, tenantID,
	).Scan(&count).Error; err != nil {
		return fmt.Errorf("notification: verify user: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// In-app inbox: list / unread count / mark-read (recipient-only)
// ---------------------------------------------------------------------------

// ListInboxInput holds parameters for listing a user's in-app notifications.
type ListInboxInput struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	// UnreadOnly limits the result to notifications without a read marker.
	UnreadOnly bool
}

// InboxItem is an in-app notification with its read state for the recipient.
type InboxItem struct {
	Notification Notification
	Read         bool
}

// ListInbox returns the in-app notifications addressed to UserID.  Only the
// recipient's own notifications are returned (recipient_user_id = UserID), so a
// caller cannot read another user's inbox even within the same tenant.
func (s *Service) ListInbox(ctx context.Context, in ListInboxInput) ([]InboxItem, error) {
	var items []InboxItem
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			Notification
			ReadMarker *uuid.UUID `gorm:"column:read_marker"`
		}
		q := `SELECT n.id, n.tenant_id, n.recipient_user_id, n.event_type, n.channel,
		             n.subject, n.body_ref, n.resource_type, n.resource_id,
		             n.dedupe_key, n.status, n.created_at, n.updated_at,
		             r.id AS read_marker
		      FROM notifications n
		      LEFT JOIN notification_reads r
		        ON r.notification_id = n.id AND r.tenant_id = n.tenant_id
		           AND r.recipient_user_id = n.recipient_user_id
		      WHERE n.tenant_id = ? AND n.recipient_user_id = ? AND n.channel = ?`
		args := []any{in.TenantID, in.UserID, ChannelInApp}
		if in.UnreadOnly {
			q += ` AND r.id IS NULL`
		}
		q += ` ORDER BY n.created_at DESC`
		if err := tx.Raw(q, args...).Scan(&rows).Error; err != nil {
			return fmt.Errorf("notification: list inbox: %w", err)
		}
		items = make([]InboxItem, 0, len(rows))
		for i := range rows {
			items = append(items, InboxItem{
				Notification: rows[i].Notification,
				Read:         rows[i].ReadMarker != nil,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// UnreadCount returns the number of unread in-app notifications for UserID.
func (s *Service) UnreadCount(ctx context.Context, tenantID, userID uuid.UUID) (int64, error) {
	var count int64
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT COUNT(1) FROM notifications n
			 WHERE n.tenant_id = ? AND n.recipient_user_id = ? AND n.channel = ?
			   AND NOT EXISTS (
			       SELECT 1 FROM notification_reads r
			       WHERE r.notification_id = n.id AND r.tenant_id = n.tenant_id
			         AND r.recipient_user_id = n.recipient_user_id
			   )`,
			tenantID, userID, ChannelInApp,
		).Scan(&count).Error
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// MarkReadInput holds fields for marking a notification read.
type MarkReadInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	NotificationID uuid.UUID
	// UserID is the authenticated user marking their own notification read.
	UserID uuid.UUID
	IP     *string
}

// MarkRead records a read marker for (notification, user).
//
// Item-level authorisation: the notification must exist AND its recipient must
// equal in.UserID (the authenticated user).  A user — even an admin — cannot
// mark another user's notification read; that attempt returns ErrForbidden.
func (s *Service) MarkRead(ctx context.Context, in MarkReadInput) (*Read, error) {
	var read Read
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Load the notification's recipient to enforce item-level ownership.
		var rows []struct {
			RecipientUserID uuid.UUID `gorm:"column:recipient_user_id"`
		}
		if err := tx.Raw(
			`SELECT recipient_user_id FROM notifications
			 WHERE id = ? AND tenant_id = ? AND channel = ? LIMIT 1`,
			in.NotificationID, in.TenantID, ChannelInApp,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("notification: mark read load notification: %w", err)
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		if rows[0].RecipientUserID != in.UserID {
			// Not the owner — refuse (item-level permission).
			return ErrForbidden
		}

		read = Read{
			ID:              uuid.New(),
			TenantID:        in.TenantID,
			NotificationID:  in.NotificationID,
			RecipientUserID: in.UserID,
		}
		// Idempotent: ON CONFLICT keeps the first read_at.
		if err := tx.Exec(
			`INSERT INTO notification_reads
			   (id, tenant_id, notification_id, recipient_user_id)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (notification_id, recipient_user_id) DO NOTHING`,
			read.ID, read.TenantID, read.NotificationID, read.RecipientUserID,
		).Error; err != nil {
			return fmt.Errorf("notification: mark read insert: %w", err)
		}
		// Re-read the persisted marker (handles the idempotent conflict path).
		if err := tx.Raw(
			`SELECT id, tenant_id, notification_id, recipient_user_id, read_at,
			        created_at, updated_at
			 FROM notification_reads
			 WHERE notification_id = ? AND recipient_user_id = ? AND tenant_id = ? LIMIT 1`,
			in.NotificationID, in.UserID, in.TenantID,
		).Scan(&read).Error; err != nil {
			return fmt.Errorf("notification: mark read re-read: %w", err)
		}

		idStr := in.NotificationID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "notification.read",
			ResourceType: "notification",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &read, nil
}

// ---------------------------------------------------------------------------
// Email delivery lifecycle: send (mock), retry, status transitions
// ---------------------------------------------------------------------------

// ProcessDelivery attempts to send a queued/failed email delivery via the
// MailSender, applying retry accounting.  On success it transitions to sent; on
// failure it increments attempts and stays failed, marking permanent failure
// when attempts reach max_attempts.
//
// The destination address is decrypted ONLY transiently to hand to the mailer;
// the plaintext is never persisted, logged, or audited.
func (s *Service) ProcessDelivery(ctx context.Context, tenantID, deliveryID uuid.UUID, actorID uuid.UUID, ip *string) (*EmailDelivery, error) {
	// Load + decrypt + send outside the final status-write decision so the mailer
	// is invoked once.  We decrypt inside the tenant tx (RLS), call the mailer,
	// then write the resulting status in the same tx.
	var out EmailDelivery
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var d EmailDelivery
		if err := tx.Raw(
			`SELECT id, tenant_id, notification_id, to_email_hash, to_email_enc,
			        provider, provider_message_id, status, attempts, max_attempts,
			        last_error, sent_at, bounced_at, created_at, updated_at
			 FROM email_deliveries
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			deliveryID, tenantID,
		).Scan(&d).Error; err != nil {
			return fmt.Errorf("notification: process delivery load: %w", err)
		}
		if d.ID == uuid.Nil {
			return ErrNotFound
		}
		// Only queued/failed (not yet permanent) deliveries are processable.
		if d.Status != DeliveryStatusQueued && d.Status != DeliveryStatusFailed {
			return fmt.Errorf("%w: cannot process delivery in status %s", ErrInvalidTransition, d.Status)
		}
		if d.Status == DeliveryStatusFailed && d.Attempts >= d.MaxAttempts {
			return fmt.Errorf("%w: delivery permanently failed (attempts %d >= max %d)",
				ErrInvalidTransition, d.Attempts, d.MaxAttempts)
		}

		// Decrypt the destination address transiently for the mailer only.
		var toAddr string
		if len(d.ToEmailEnc) > 0 {
			plain, derr := crypto.Decrypt(d.ToEmailEnc)
			if derr != nil {
				return fmt.Errorf("notification: process delivery decrypt: %w", derr)
			}
			toAddr = string(plain)
		}

		newAttempts := d.Attempts + 1
		providerMsgID, sendErr := s.mailer.Send(ctx, toAddr, "", "")
		// Scrub the plaintext address from memory promptly (intentional security measure).
		toAddr = "" //nolint:ineffassign // intentional security scrub: clear plaintext address from memory after use

		if sendErr != nil {
			// Failure path: increment attempts, stay/become failed.  last_error is
			// a non-PII category — never the address or body.
			if !isDeliveryTransitionAllowed(d.Status, DeliveryStatusFailed) &&
				d.Status != DeliveryStatusFailed {
				return fmt.Errorf("%w: %s -> failed", ErrInvalidTransition, d.Status)
			}
			res := tx.Exec(
				`UPDATE email_deliveries
				 SET status = ?, attempts = ?, last_error = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				DeliveryStatusFailed, newAttempts, "send_failed", deliveryID, tenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("notification: process delivery update failed: %w", res.Error)
			}
		} else {
			if !isDeliveryTransitionAllowed(d.Status, DeliveryStatusSent) {
				return fmt.Errorf("%w: %s -> sent", ErrInvalidTransition, d.Status)
			}
			res := tx.Exec(
				`UPDATE email_deliveries
				 SET status = ?, attempts = ?, provider_message_id = ?,
				     last_error = '', sent_at = now(), updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				DeliveryStatusSent, newAttempts, providerMsgID, deliveryID, tenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("notification: process delivery update sent: %w", res.Error)
			}
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, notification_id, to_email_hash, to_email_enc,
			        provider, provider_message_id, status, attempts, max_attempts,
			        last_error, sent_at, bounced_at, created_at, updated_at
			 FROM email_deliveries
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			deliveryID, tenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("notification: process delivery re-read: %w", err)
		}

		idStr := deliveryID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     tenantID,
			UserID:       &actorID,
			Action:       "email_delivery.processed",
			ResourceType: "email_delivery",
			ResourceID:   &idStr,
			IP:           ip,
		})
	})
	if err != nil {
		return nil, err
	}
	out.ToEmailEnc = nil // never expose ciphertext
	return &out, nil
}

// MarkBouncedInput holds fields for recording a bounce/complaint (webhook intake).
type MarkBouncedInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	DeliveryID uuid.UUID
	// Status must be "bounced" or "complained".
	Status string
	IP     *string
}

// MarkBounced records a bounce/complaint for a delivery (future webhook intake).
// Only sent → bounced/complained is allowed.
func (s *Service) MarkBounced(ctx context.Context, in MarkBouncedInput) (*EmailDelivery, error) {
	if in.Status != DeliveryStatusBounced && in.Status != DeliveryStatusComplained {
		return nil, fmt.Errorf("%w: target status must be bounced or complained", ErrInvalidTransition)
	}
	var out EmailDelivery
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var rows []struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM email_deliveries WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.DeliveryID, in.TenantID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("notification: mark bounced load: %w", err)
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		if !isDeliveryTransitionAllowed(rows[0].Status, in.Status) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, rows[0].Status, in.Status)
		}

		res := tx.Exec(
			`UPDATE email_deliveries
			 SET status = ?, bounced_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.DeliveryID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("notification: mark bounced update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, notification_id, to_email_hash, to_email_enc,
			        provider, provider_message_id, status, attempts, max_attempts,
			        last_error, sent_at, bounced_at, created_at, updated_at
			 FROM email_deliveries
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.DeliveryID, in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("notification: mark bounced re-read: %w", err)
		}

		idStr := in.DeliveryID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "email_delivery.bounced",
			ResourceType: "email_delivery",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	out.ToEmailEnc = nil
	return &out, nil
}

// ---------------------------------------------------------------------------
// Sensitive read: decrypt destination email (RBAC re-validated at service layer)
// ---------------------------------------------------------------------------

// GetDeliveryEmailInput holds parameters for reading a delivery's destination
// email in plaintext (sensitive).
type GetDeliveryEmailInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	DeliveryID uuid.UUID
	IP         *string
}

// GetDeliveryEmail decrypts and returns the destination email of a delivery.
//
// This is a sensitive operation.  Even if an HTTP middleware already checked the
// permission, the service re-validates notification:read_sensitive inside the
// transaction (defence-in-depth).  The decrypted plaintext is returned as a
// separate value (never persisted) and is NEVER written to logs or audit.
func (s *Service) GetDeliveryEmail(ctx context.Context, in GetDeliveryEmailInput) (string, error) {
	var plaintext string
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
		if err != nil {
			return fmt.Errorf("notification: get delivery email load permissions: %w", err)
		}
		if !platformauth.HasPermission(perms, permReadSensitive) {
			return ErrForbidden
		}

		var rows []struct {
			ToEmailEnc []byte `gorm:"column:to_email_enc"`
		}
		if err := tx.Raw(
			`SELECT to_email_enc FROM email_deliveries
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.DeliveryID, in.TenantID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("notification: get delivery email load: %w", err)
		}
		if len(rows) == 0 {
			return ErrNotFound
		}
		if len(rows[0].ToEmailEnc) > 0 {
			plain, derr := crypto.Decrypt(rows[0].ToEmailEnc)
			if derr != nil {
				return fmt.Errorf("notification: get delivery email decrypt: %w", derr)
			}
			plaintext = string(plain)
		}

		// Audit the sensitive access — opaque delivery id only, no plaintext.
		idStr := in.DeliveryID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "email_delivery.read_sensitive",
			ResourceType: "email_delivery",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

// ListDeliveries returns the email deliveries for a notification (metadata only;
// to_email_enc is never returned).
func (s *Service) ListDeliveries(ctx context.Context, tenantID, notificationID uuid.UUID) ([]EmailDelivery, error) {
	var ds []EmailDelivery
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, notification_id, to_email_hash,
			        provider, provider_message_id, status, attempts, max_attempts,
			        last_error, sent_at, bounced_at, created_at, updated_at
			 FROM email_deliveries
			 WHERE tenant_id = ? AND notification_id = ?
			 ORDER BY created_at`,
			tenantID, notificationID,
		).Scan(&ds).Error
	})
	if err != nil {
		return nil, err
	}
	return ds, nil
}
