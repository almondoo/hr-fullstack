// Package notification implements the ST-FND-09 notification platform:
// in-app + email notifications, per-tenant templates, delivery records,
// per-user read management, recipient preferences (opt-in/out, forced), and
// reminder dedupe.
//
// Security / PII posture:
//   - Notification bodies and templates never carry sensitive PII
//     (マイナンバー/口座/健診 etc.).  Detail is referenced via an opaque deep
//     link (resource_type + resource_id UUID).
//   - The destination email address is PII: it is stored only as AES-256-GCM
//     ciphertext (to_email_enc) plus a non-reversible hash (to_email_hash).
//     Plaintext email is never persisted, logged, or written to audit records.
//   - Real email sending is abstracted behind MailSender; the MVP uses a
//     log-only MockSender.  No external mail/CSV/Excel/PDF dependency is used.
//
// Legal / config note: notification & delivery retention periods, retry limits,
// deliverability (SPF/DKIM/DMARC) and the forced-notification scope are
// configuration values, NOT hard-coded business rules.  This package encodes
// structure only and is not legal advice; values must follow the latest
// official guidance with 社労士/弁護士 review to track regulatory changes.
package notification

import (
	"time"

	"github.com/google/uuid"
)

// Channel and status string constants (mirror the DB CHECK constraints).
const (
	ChannelInApp = "in_app"
	ChannelEmail = "email"

	// notifications.status
	NotificationStatusCreated   = "created"
	NotificationStatusDelivered = "delivered"
	NotificationStatusCancelled = "cancelled"

	// email_deliveries.status
	DeliveryStatusQueued     = "queued"
	DeliveryStatusSent       = "sent"
	DeliveryStatusFailed     = "failed"
	DeliveryStatusBounced    = "bounced"
	DeliveryStatusComplained = "complained"
)

// Template is the GORM model for notification_templates.
type Template struct {
	ID              uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID `gorm:"column:tenant_id"`
	EventType       string    `gorm:"column:event_type"`
	Channel         string    `gorm:"column:channel"`
	Locale          string    `gorm:"column:locale"`
	SubjectTemplate string    `gorm:"column:subject_template"`
	BodyTemplate    string    `gorm:"column:body_template"`
	Active          bool      `gorm:"column:active"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps Template to notification_templates.
func (Template) TableName() string { return "notification_templates" }

// Notification is the GORM model for notifications.
//
// BodyRef holds an opaque deep-link reference / non-sensitive rendered snippet.
// SECURITY: it MUST NOT contain sensitive PII.
type Notification struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	RecipientUserID uuid.UUID  `gorm:"column:recipient_user_id"`
	EventType       string     `gorm:"column:event_type"`
	Channel         string     `gorm:"column:channel"`
	Subject         string     `gorm:"column:subject"`
	BodyRef         string     `gorm:"column:body_ref"`
	ResourceType    string     `gorm:"column:resource_type"`
	ResourceID      *uuid.UUID `gorm:"column:resource_id"`
	DedupeKey       *string    `gorm:"column:dedupe_key"`
	Status          string     `gorm:"column:status"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps Notification to notifications.
func (Notification) TableName() string { return "notifications" }

// Read is the GORM model for notification_reads.
type Read struct {
	ID              uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID `gorm:"column:tenant_id"`
	NotificationID  uuid.UUID `gorm:"column:notification_id"`
	RecipientUserID uuid.UUID `gorm:"column:recipient_user_id"`
	ReadAt          time.Time `gorm:"column:read_at"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps Read to notification_reads.
func (Read) TableName() string { return "notification_reads" }

// EmailDelivery is the GORM model for email_deliveries.
//
// Security note on ToEmailEnc:
//   - This field holds the AES-256-GCM ciphertext of the destination email.
//   - The plaintext email is NEVER stored or returned to callers without the
//     notification:read_sensitive permission check.
//   - ToEmailHash is a non-reversible hash usable for matching without exposing
//     the address.
type EmailDelivery struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	NotificationID    uuid.UUID  `gorm:"column:notification_id"`
	ToEmailHash       string     `gorm:"column:to_email_hash"`
	ToEmailEnc        []byte     `gorm:"column:to_email_enc;type:bytea"`
	Provider          string     `gorm:"column:provider"`
	ProviderMessageID string     `gorm:"column:provider_message_id"`
	Status            string     `gorm:"column:status"`
	Attempts          int        `gorm:"column:attempts"`
	MaxAttempts       int        `gorm:"column:max_attempts"`
	LastError         string     `gorm:"column:last_error"`
	SentAt            *time.Time `gorm:"column:sent_at"`
	BouncedAt         *time.Time `gorm:"column:bounced_at"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps EmailDelivery to email_deliveries.
func (EmailDelivery) TableName() string { return "email_deliveries" }

// Preference is the GORM model for notification_preferences.
type Preference struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID `gorm:"column:tenant_id"`
	UserID    uuid.UUID `gorm:"column:user_id"`
	EventType string    `gorm:"column:event_type"`
	Channel   string    `gorm:"column:channel"`
	OptedIn   bool      `gorm:"column:opted_in"`
	Forced    bool      `gorm:"column:forced"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps Preference to notification_preferences.
func (Preference) TableName() string { return "notification_preferences" }

// OutboxEntry is the GORM model for notification_outbox.
//
// Domains INSERT rows here atomically within their own transactions (outbox
// pattern).  The notification service polls pending rows and calls Publish.
//
// Security: BodyRef and ResourceID are opaque references.  PII (マイナンバー
// /口座/健診 etc.) MUST NOT appear here.
type OutboxEntry struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	EventType       string     `gorm:"column:event_type"`
	ActorUserID     *uuid.UUID `gorm:"column:actor_user_id"`
	RecipientUserID uuid.UUID  `gorm:"column:recipient_user_id"`
	ResourceType    string     `gorm:"column:resource_type"`
	ResourceID      *uuid.UUID `gorm:"column:resource_id"`
	BodyRef         string     `gorm:"column:body_ref"`
	DedupeKey       *string    `gorm:"column:dedupe_key"`
	Status          string     `gorm:"column:status"`
	Attempts        int        `gorm:"column:attempts"`
	LastError       string     `gorm:"column:last_error"`
	ProcessedAt     *time.Time `gorm:"column:processed_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps OutboxEntry to notification_outbox.
func (OutboxEntry) TableName() string { return "notification_outbox" }

// Outbox status constants.
const (
	OutboxStatusPending   = "pending"
	OutboxStatusProcessed = "processed"
	OutboxStatusFailed    = "failed"
)
