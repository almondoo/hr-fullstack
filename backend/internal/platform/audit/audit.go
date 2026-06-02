// Package audit provides tamper-evident audit logging with SHA-256 hash chains.
//
// Security invariants:
//   - PII, passwords, and tokens are NEVER stored in audit log rows.
//     Entries reference resource IDs only; callers must not pass PII as resource_id.
//   - Hash chain integrity: each tenant's audit log forms a linked chain.
//     prev_hash of the first row is ""; each subsequent row's prev_hash equals
//     the previous row's hash.
//   - Concurrency: pg_advisory_xact_lock(hashtext(tenant_id::text)) serialises
//     INSERTs within the same tenant inside the caller's transaction so that
//     the chain is always linear (no forked hash chains from concurrent writes).
//   - VerifyChain re-derives every hash from scratch and confirms linkage,
//     enabling tamper detection.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Entry holds the fields that describe a single audit event.
// The caller supplies these; Record derives the hash chain fields automatically.
//
// PII rules:
//   - ResourceID should be an opaque ID (UUID string, row ID) — never a name,
//     email, or other personal data.
//   - IP may be stored for security forensics; it is considered non-PII at
//     the network layer but should be treated carefully.
type Entry struct {
	TenantID     uuid.UUID
	UserID       *uuid.UUID
	Action       string
	ResourceType string
	ResourceID   *string
	IP           *string
}

// auditRow is the GORM model for audit_logs.
// Only the columns required by Record / VerifyChain are listed.
type auditRow struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	UserID       *uuid.UUID `gorm:"column:user_id"`
	Action       string     `gorm:"column:action"`
	ResourceType string     `gorm:"column:resource_type"`
	ResourceID   *string    `gorm:"column:resource_id"`
	IP           *string    `gorm:"column:ip"`
	OccurredAt   time.Time  `gorm:"column:occurred_at"`
	PrevHash     string     `gorm:"column:prev_hash"`
	Hash         string     `gorm:"column:hash"`
	Seq          int64      `gorm:"column:seq"`
}

func (auditRow) TableName() string { return "audit_logs" }

// Record writes a single audit entry within the provided transaction tx.
//
// Contract:
//   - tx must already be executing inside WithinTenant for e.TenantID so that
//     RLS is satisfied and the advisory lock acquired here is scoped to that
//     tenant's connection.
//   - Record does NOT call WithinTenant itself; the caller is responsible for
//     atomicity (business operation + audit = one transaction).
//
// Concurrency:
//   - pg_advisory_xact_lock serialises all Record calls for the same tenant
//     within the current transaction, preventing hash chain forks.
//   - The lock is released automatically when the transaction commits or rolls back.
func Record(tx *gorm.DB, e Entry) error {
	// Acquire a tenant-scoped advisory lock for the duration of this transaction.
	// hashtext() converts the tenant UUID string to a stable int4 suitable for
	// pg_advisory_xact_lock's int8 parameter (cast to bigint).
	if err := tx.Exec(
		`SELECT pg_advisory_xact_lock(hashtext(?::text)::bigint)`,
		e.TenantID.String(),
	).Error; err != nil {
		return fmt.Errorf("audit: acquire advisory lock: %w", err)
	}

	// Fetch the most recent row's hash for this tenant (ordered by seq DESC).
	// If no rows exist, prevHash remains "".
	var prevHash string
	{
		var rows []struct {
			Hash string `gorm:"column:hash"`
		}
		if err := tx.Raw(
			`SELECT hash FROM audit_logs
			 WHERE tenant_id = ?
			 ORDER BY seq DESC
			 LIMIT 1`,
			e.TenantID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("audit: fetch prev_hash: %w", err)
		}
		if len(rows) > 0 {
			prevHash = rows[0].Hash
		}
	}

	now := time.Now().UTC()
	rowID := uuid.New()

	// Compute the hash over a deterministic canonical string.
	// Format: prevHash|action|resource_type|resource_id|user_id|occurred_at(RFC3339Nano)
	resourceID := ""
	if e.ResourceID != nil {
		resourceID = *e.ResourceID
	}
	userIDStr := ""
	if e.UserID != nil {
		userIDStr = e.UserID.String()
	}
	canonical := fmt.Sprintf("%s|%s|%s|%s|%s|%s",
		prevHash,
		e.Action,
		e.ResourceType,
		resourceID,
		userIDStr,
		now.Format(time.RFC3339Nano),
	)
	sum := sha256.Sum256([]byte(canonical))
	hash := hex.EncodeToString(sum[:])

	// Convert IP: store as text; Postgres inet column accepts text literals.
	var ipVal *string
	if e.IP != nil && *e.IP != "" {
		ipVal = e.IP
	}

	// INSERT the row. The seq column is bigserial — assigned by the DB.
	if err := tx.Exec(
		`INSERT INTO audit_logs
		   (id, tenant_id, user_id, action, resource_type, resource_id, ip,
		    occurred_at, prev_hash, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?::inet, ?, ?, ?)`,
		rowID,
		e.TenantID,
		e.UserID,
		e.Action,
		e.ResourceType,
		e.ResourceID,
		ipVal,
		now,
		prevHash,
		hash,
	).Error; err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}

	return nil
}

// VerifyChain re-derives every hash in the tenant's audit log (ordered by seq)
// and checks that each row's stored hash matches the re-derived value and that
// each row's prev_hash equals the preceding row's hash.
//
// Returns (true, nil) when the chain is intact.
// Returns (false, nil) when a mismatch is detected (possible tampering).
// Returns (false, err) when a database error occurs.
//
// tx must be inside WithinTenant for tenantID.
func VerifyChain(tx *gorm.DB, tenantID uuid.UUID) (bool, error) {
	var rows []auditRow
	if err := tx.Raw(
		`SELECT id, tenant_id, user_id, action, resource_type, resource_id,
		        occurred_at, prev_hash, hash, seq
		 FROM audit_logs
		 WHERE tenant_id = ?
		 ORDER BY seq ASC`,
		tenantID,
	).Scan(&rows).Error; err != nil {
		return false, fmt.Errorf("audit: verify chain fetch: %w", err)
	}

	var expectedPrevHash string
	for _, row := range rows {
		if row.PrevHash != expectedPrevHash {
			return false, nil
		}

		resourceID := ""
		if row.ResourceID != nil {
			resourceID = *row.ResourceID
		}
		userIDStr := ""
		if row.UserID != nil {
			userIDStr = row.UserID.String()
		}
		canonical := fmt.Sprintf("%s|%s|%s|%s|%s|%s",
			row.PrevHash,
			row.Action,
			row.ResourceType,
			resourceID,
			userIDStr,
			row.OccurredAt.UTC().Format(time.RFC3339Nano),
		)
		sum := sha256.Sum256([]byte(canonical))
		derived := hex.EncodeToString(sum[:])

		if row.Hash != derived {
			return false, nil
		}

		expectedPrevHash = row.Hash
	}

	return true, nil
}
