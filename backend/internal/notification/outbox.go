package notification

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// InsertOutboxEntry is the input shape for inserting a notification outbox row.
//
// Callers (other domain packages) invoke InsertOutbox within their own
// transaction to atomically emit an event.  No direct import of this package's
// Service is required; callers only need the thin InsertOutbox function.
//
// SECURITY: BodyRef and ResourceID are opaque references. PII (マイナンバー /
// 口座 / 健診 etc.) MUST NOT appear in any field.
type InsertOutboxEntry struct {
	TenantID        uuid.UUID
	EventType       string
	ActorUserID     *uuid.UUID
	RecipientUserID uuid.UUID
	ResourceType    string
	ResourceID      *uuid.UUID
	// BodyRef is an opaque deep-link reference; never include PII.
	BodyRef   string
	DedupeKey *string
}

// InsertOutbox inserts a pending outbox row into notification_outbox within the
// caller's transaction tx.  It is designed to be called from other domain
// packages without importing the notification.Service; callers only import the
// notification package for this one function.
//
// tx must already be scoped to the correct tenant (e.g. via tenantdb.WithinTenant).
func InsertOutbox(tx *gorm.DB, e InsertOutboxEntry) error {
	if err := tx.Exec(
		`INSERT INTO notification_outbox
		   (id, tenant_id, event_type, actor_user_id, recipient_user_id,
		    resource_type, resource_id, body_ref, dedupe_key, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		uuid.New(), e.TenantID, e.EventType, e.ActorUserID, e.RecipientUserID,
		e.ResourceType, e.ResourceID, e.BodyRef, e.DedupeKey,
	).Error; err != nil {
		return fmt.Errorf("notification: insert outbox: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// ProcessOutbox — outbox polling / dispatch
// ---------------------------------------------------------------------------

// ProcessOutboxBatchSize is the number of pending outbox rows processed per
// ProcessOutbox call.
const ProcessOutboxBatchSize = 100

// ProcessOutbox fetches up to ProcessOutboxBatchSize pending outbox rows,
// calls Publish for each one, and marks them processed (or failed).
//
// It is intended to be called by a background goroutine or a cron job; the
// MVP wires it as a simple in-process loop.  Each row is processed in its own
// WithinTenant transaction to isolate failures.
//
// tenantID must be provided by the polling driver (rows are fetched per-tenant
// under RLS).
func (s *Service) ProcessOutbox(ctx context.Context, tenantID uuid.UUID) (processed, failed int, err error) {
	var rows []OutboxEntry
	if err = s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, event_type, actor_user_id, recipient_user_id,
			        resource_type, resource_id, body_ref, dedupe_key,
			        status, attempts, created_at, updated_at
			 FROM notification_outbox
			 WHERE tenant_id = ? AND status = 'pending'
			 ORDER BY created_at
			 LIMIT ?
			 FOR UPDATE SKIP LOCKED`,
			tenantID, ProcessOutboxBatchSize,
		).Scan(&rows).Error
	}); err != nil {
		return 0, 0, fmt.Errorf("notification: process outbox fetch: %w", err)
	}

	for _, row := range rows {
		if procErr := s.processOutboxRow(ctx, tenantID, row); procErr != nil {
			failed++
			slog.Warn("notification: outbox row failed",
				"outbox_id", row.ID.String(),
				"event_type", row.EventType,
				"error", procErr.Error(),
			)
		} else {
			processed++
		}
	}
	return processed, failed, nil
}

// processOutboxRow processes one outbox row: calls Publish and marks it
// processed or failed within its own tenant-scoped transaction.
func (s *Service) processOutboxRow(ctx context.Context, tenantID uuid.UUID, row OutboxEntry) error {
	// Build recipients — single recipient per outbox row.
	pub := PublishInput{
		TenantID:  tenantID,
		EventType: row.EventType,
		Locale:    "ja",
		Recipients: []PublishRecipient{
			{UserID: row.RecipientUserID},
		},
		ResourceType: row.ResourceType,
		ResourceID:   row.ResourceID,
		DeepLink:     row.BodyRef,
		DedupeKey:    row.DedupeKey,
		Mandatory:    false,
	}
	if row.ActorUserID != nil {
		pub.ActorID = *row.ActorUserID
	}

	_, publishErr := s.Publish(ctx, pub)

	return s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		now := time.Now()
		if publishErr != nil {
			errMsg := publishErr.Error()
			if len(errMsg) > 512 {
				errMsg = errMsg[:512]
			}
			return tx.Exec(
				`UPDATE notification_outbox
				 SET status = 'failed', attempts = attempts + 1,
				     last_error = ?, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				errMsg, row.ID, tenantID,
			).Error
		}
		return tx.Exec(
			`UPDATE notification_outbox
			 SET status = 'processed', attempts = attempts + 1,
			     processed_at = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			now, row.ID, tenantID,
		).Error
	})
}
