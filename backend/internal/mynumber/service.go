package mynumber

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("mynumber: not found")
	ErrInvalidTransition = errors.New("mynumber: invalid status transition")
	ErrForbidden         = errors.New("mynumber: permission denied")
	ErrPurposeNotAllowed = errors.New("mynumber: purpose not permitted for this record")
	ErrDisposed          = errors.New("mynumber: record already disposed")
	ErrInvalidPurpose    = errors.New("mynumber: unknown use purpose")
)

// Permission constants (strict separation of duties / least privilege).  Two
// segments each per the platform RBAC convention.
//
//   - readPermission   — read record metadata and access logs (no number value).
//   - writePermission  — collect / write record metadata.
//   - revealPermission — DECRYPT / DISPLAY / PROVIDE the 個人番号.  This is the
//     dedicated, strictly-controlled permission; ordinary HR read/write do NOT
//     grant it.
//
// Every permission is enforced at the route layer (RequirePermission middleware)
// AND independently re-validated here in the service layer (defence-in-depth).
// The service-layer check is the authoritative gate: it holds even when the
// service is invoked directly (e.g. a different router wiring, a background job,
// or a future caller), so no operation depends on the HTTP middleware alone for
// authorization.
const (
	readPermission   = "mynumber:read"
	writePermission  = "mynumber:write"
	revealPermission = "mynumber:reveal"
)

// requirePermission re-validates that the actor holds need within the open
// tenant transaction tx.  Returns ErrForbidden when the permission is absent.
// This mirrors platformauth.RequirePermission but runs in-service so that the
// authorization gate does not depend on the HTTP route layer being wired.
func requirePermission(tx *gorm.DB, tenantID, actorID uuid.UUID, need string) error {
	perms, err := platformauth.LoadUserPermissions(tx, tenantID, actorID)
	if err != nil {
		return fmt.Errorf("mynumber: load permissions: %w", err)
	}
	if !platformauth.HasPermission(perms, need) {
		return ErrForbidden
	}
	return nil
}

// allowedPurposes is the enumerated (限定列挙) use-purpose set.
// Legal value — externalise via configuration to support additions when the law
// or operations change.
var allowedPurposes = map[string]bool{
	PurposePayroll:         true,
	PurposeSocialInsurance: true,
	PurposeTax:             true,
}

// allowedRecordTransitions defines legal record status moves.
// Terminal state: disposed — no transitions out.
//
//	active  → expired   (保管期限到来)
//	active  → disposed  (廃棄)
//	expired → disposed  (廃棄)
var allowedRecordTransitions = map[string]map[string]bool{
	StatusActive: {
		StatusExpired:  true,
		StatusDisposed: true,
	},
	StatusExpired: {
		StatusDisposed: true,
	},
}

// isRecordTransitionAllowed reports whether moving current → next is valid.
func isRecordTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedRecordTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// isPurposeAllowed reports whether p is in the enumerated purpose set.
func isPurposeAllowed(p string) bool {
	return allowedPurposes[p]
}

// Service provides business logic for the mynumber domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Collection — 収集・暗号化保管
// ---------------------------------------------------------------------------

// CollectInput holds fields for collecting and storing a 個人番号.
//
// NumberPlaintext contains the 個人番号 in plaintext.  It is encrypted with
// AES-256-GCM before storage; the plaintext is NEVER persisted to the database,
// logged, or written to any audit / access-log record.
type CollectInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	EmployeeID   uuid.UUID
	SubjectType  string
	DependentRef *uuid.UUID
	// NumberPlaintext is the unencrypted 個人番号.  MUST NOT be logged, written
	// to audit records, or persisted as plaintext.  Encrypted before INSERT.
	NumberPlaintext []byte
	// Purposes is the limited-enumeration list of registered use purposes.
	Purposes []string
	// RetentionUntil is the retention deadline (legal value — config-driven).
	RetentionUntil *time.Time
	IP             *string
}

// Collect stores a 個人番号 in the separated store with column encryption and
// records its registered use purposes, all in a single transaction together
// with the audit record.  The employee must belong to the same tenant.
//
// Security: the plaintext is encrypted BEFORE the transaction opens (fail-fast)
// and never appears in any error, log, or audit row.
func (s *Service) Collect(ctx context.Context, in CollectInput) (*Record, error) {
	if in.SubjectType != SubjectSelf && in.SubjectType != SubjectDependent {
		return nil, fmt.Errorf("%w: subject_type %q", ErrInvalidTransition, in.SubjectType)
	}
	// Validate purposes up-front (enumerated list).
	for _, p := range in.Purposes {
		if !isPurposeAllowed(p) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidPurpose, p)
		}
	}

	// Encrypt the 個人番号 BEFORE opening the transaction so that any crypto
	// error fails fast.  The plaintext value never appears in an error message.
	var numberEnc []byte
	if len(in.NumberPlaintext) > 0 {
		var err error
		numberEnc, err = crypto.Encrypt(in.NumberPlaintext)
		if err != nil {
			return nil, fmt.Errorf("mynumber: collect encrypt number: %w", err)
		}
	}

	rec := Record{
		ID:             uuid.New(),
		TenantID:       in.TenantID,
		EmployeeID:     in.EmployeeID,
		SubjectType:    in.SubjectType,
		DependentRef:   in.DependentRef,
		NumberEnc:      numberEnc,
		Status:         StatusActive,
		RetentionUntil: in.RetentionUntil,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Service-layer RBAC: collecting requires mynumber:write.  This is the
		// authoritative gate — it holds even if the HTTP route middleware is not
		// wired (the route-layer guard is defence-in-depth on top of this).
		if err := requirePermission(tx, in.TenantID, in.ActorID, writePermission); err != nil {
			return err
		}

		// Verify employee belongs to this tenant (defence-in-depth on top of FK).
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("mynumber: collect verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO mynumber_records
			   (id, tenant_id, employee_id, subject_type, dependent_ref,
			    number_enc, status, retention_until)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.TenantID, rec.EmployeeID, rec.SubjectType, rec.DependentRef,
			rec.NumberEnc, rec.Status, rec.RetentionUntil,
		).Error; err != nil {
			return fmt.Errorf("mynumber: collect insert record: %w", err)
		}

		for _, p := range in.Purposes {
			if err := tx.Exec(
				`INSERT INTO mynumber_purposes (id, tenant_id, record_id, purpose)
				 VALUES (?, ?, ?, ?)
				 ON CONFLICT (record_id, purpose) DO NOTHING`,
				uuid.New(), in.TenantID, rec.ID, p,
			).Error; err != nil {
				return fmt.Errorf("mynumber: collect insert purpose: %w", err)
			}
		}

		// Audit: record only the opaque record ID — never PII or the number.
		idStr := rec.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "mynumber.collected",
			ResourceType: "mynumber_record",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers of Collect.
	rec.NumberEnc = nil
	return &rec, nil
}

// ---------------------------------------------------------------------------
// Read (metadata only) — 番号は返さない
// ---------------------------------------------------------------------------

// GetRecord fetches a record's metadata (no number) within the tenant.
// The ciphertext is cleared from the returned struct — use Reveal to decrypt.
//
// Authorization: the actor must hold mynumber:read.  This service-layer gate is
// authoritative (it does not depend on the HTTP route middleware being wired).
func (s *Service) GetRecord(ctx context.Context, tenantID, actorID, id uuid.UUID) (*Record, error) {
	var rec Record
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := requirePermission(tx, tenantID, actorID, readPermission); err != nil {
			return err
		}
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, subject_type, dependent_ref,
			        number_enc, status, collected_at, retention_until, disposed_at,
			        created_at, updated_at
			 FROM mynumber_records
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&rec).Error
	})
	if err != nil {
		return nil, err
	}
	if rec.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	rec.NumberEnc = nil // never expose ciphertext via metadata read
	return &rec, nil
}

// ListRecords returns record metadata for an employee within the tenant.
// Ciphertext is never included.
//
// Authorization: the actor must hold mynumber:read.  This service-layer gate is
// authoritative (it does not depend on the HTTP route middleware being wired).
func (s *Service) ListRecords(ctx context.Context, tenantID, actorID, employeeID uuid.UUID) ([]Record, error) {
	var recs []Record
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := requirePermission(tx, tenantID, actorID, readPermission); err != nil {
			return err
		}
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, subject_type, dependent_ref,
			        status, collected_at, retention_until, disposed_at,
			        created_at, updated_at
			 FROM mynumber_records
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY subject_type, collected_at`,
			tenantID, employeeID,
		).Scan(&recs).Error
	})
	if err != nil {
		return nil, err
	}
	return recs, nil
}

// ListPurposes returns the registered use purposes for a record.
func (s *Service) ListPurposes(ctx context.Context, tenantID, recordID uuid.UUID) ([]string, error) {
	var purposes []string
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT purpose FROM mynumber_purposes
			 WHERE tenant_id = ? AND record_id = ? ORDER BY purpose`,
			tenantID, recordID,
		).Scan(&purposes).Error
	})
	if err != nil {
		return nil, err
	}
	return purposes, nil
}

// ---------------------------------------------------------------------------
// Reveal — 復号/表示 (専用権限 + 利用目的の二重検証 + 利用提供ログ)
// ---------------------------------------------------------------------------

// RevealInput holds fields for decrypting / displaying a 個人番号.
type RevealInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	RecordID uuid.UUID
	// Purpose is the requested use purpose; it must match a registered purpose
	// for the record (目的外利用拒否).
	Purpose string
	IP      *string
}

// Reveal decrypts a 個人番号 and returns the plaintext, but ONLY when all of
// the following hold (multi-layer defence):
//
//  1. The actor holds the dedicated mynumber:reveal permission (re-validated in
//     the service layer, not just the HTTP middleware).
//  2. The requested purpose is one of the record's registered purposes.
//  3. The record is not disposed (and has a ciphertext).
//
// On success a "decrypt" entry is written to the tamper-evident access log in
// the SAME transaction (log-and-reveal atomicity).  The decrypted value is
// returned separately and is NEVER written to the access log, audit log, or any
// other log.
func (s *Service) Reveal(ctx context.Context, in RevealInput) ([]byte, error) {
	if !isPurposeAllowed(in.Purpose) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPurpose, in.Purpose)
	}

	var numberEnc []byte
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Layer: service-layer RBAC re-validation of the dedicated permission.
		if err := requirePermission(tx, in.TenantID, in.ActorID, revealPermission); err != nil {
			return err
		}

		enc, err := s.loadRevealableCiphertext(tx, in.TenantID, in.RecordID, in.Purpose)
		if err != nil {
			return err
		}
		numberEnc = enc

		// Log-and-reveal atomicity: record the decrypt in the access log within
		// the same tx.  If the log write fails, the whole reveal rolls back.
		return s.appendAccessLog(tx, accessLogEntry{
			TenantID:       in.TenantID,
			TargetRecordID: in.RecordID,
			ActorUserID:    &in.ActorID,
			Action:         ActionDecrypt,
			Purpose:        in.Purpose,
		})
	})
	if err != nil {
		return nil, err
	}

	// Decrypt outside / after the access-log write succeeded.  The plaintext is
	// returned to the caller only; it is never persisted or logged.
	if len(numberEnc) == 0 {
		// Disposed records have their ciphertext destroyed; loadRevealableCiphertext
		// already rejects disposed status, so reaching here with empty enc means
		// no number was ever stored.
		return nil, ErrNotFound
	}
	plain, err := crypto.Decrypt(numberEnc)
	if err != nil {
		return nil, fmt.Errorf("mynumber: reveal decrypt: %w", err)
	}
	return plain, nil
}

// ---------------------------------------------------------------------------
// Provide — 第三者提供 (社保手続き等への引渡し)
// ---------------------------------------------------------------------------

// ProvideInput holds fields for providing a 個人番号 to an external process
// (e.g. social-insurance procedures, ST-LM-08).
type ProvideInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	RecordID uuid.UUID
	Purpose  string
	// ProvidedTo identifies the recipient process/system (社保手続き等).
	// SECURITY: must not contain personal-identifying data (names, the number).
	ProvidedTo string
	IP         *string
}

// Provide decrypts and returns a 個人番号 for handing off to an external
// process, subject to the same reveal-permission + purpose checks as Reveal,
// and records a "provide" entry (with provided_to) in the access log within the
// same transaction.  The number value is never stored in the log.
func (s *Service) Provide(ctx context.Context, in ProvideInput) ([]byte, error) {
	if !isPurposeAllowed(in.Purpose) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPurpose, in.Purpose)
	}

	var numberEnc []byte
	providedTo := in.ProvidedTo
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := requirePermission(tx, in.TenantID, in.ActorID, revealPermission); err != nil {
			return err
		}

		enc, err := s.loadRevealableCiphertext(tx, in.TenantID, in.RecordID, in.Purpose)
		if err != nil {
			return err
		}
		numberEnc = enc

		return s.appendAccessLog(tx, accessLogEntry{
			TenantID:       in.TenantID,
			TargetRecordID: in.RecordID,
			ActorUserID:    &in.ActorID,
			Action:         ActionProvide,
			Purpose:        in.Purpose,
			ProvidedTo:     &providedTo,
		})
	})
	if err != nil {
		return nil, err
	}

	if len(numberEnc) == 0 {
		return nil, ErrNotFound
	}
	plain, err := crypto.Decrypt(numberEnc)
	if err != nil {
		return nil, fmt.Errorf("mynumber: provide decrypt: %w", err)
	}
	return plain, nil
}

// ---------------------------------------------------------------------------
// View log — 参照ログ記録 (番号は返さない)
// ---------------------------------------------------------------------------

// LogViewInput records a metadata-only view (参照) of a record.
type LogViewInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	RecordID uuid.UUID
	Purpose  string
	IP       *string
}

// LogView records a "view" access-log entry for a metadata-only reference.
// It does NOT decrypt or return the number.  Purpose must be registered.
func (s *Service) LogView(ctx context.Context, in LogViewInput) error {
	if !isPurposeAllowed(in.Purpose) {
		return fmt.Errorf("%w: %q", ErrInvalidPurpose, in.Purpose)
	}
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// A metadata view requires mynumber:read (service-layer authoritative gate).
		if err := requirePermission(tx, in.TenantID, in.ActorID, readPermission); err != nil {
			return err
		}
		// Verify the record exists, is not disposed, and the purpose is allowed.
		if _, err := s.loadRevealableCiphertext(tx, in.TenantID, in.RecordID, in.Purpose); err != nil {
			return err
		}
		return s.appendAccessLog(tx, accessLogEntry{
			TenantID:       in.TenantID,
			TargetRecordID: in.RecordID,
			ActorUserID:    &in.ActorID,
			Action:         ActionView,
			Purpose:        in.Purpose,
		})
	})
}

// ---------------------------------------------------------------------------
// Disposal — 廃棄 (論理失効 + 復号不能化 + 廃棄証跡)
// ---------------------------------------------------------------------------

// DisposeInput holds fields for disposing of a 個人番号.
type DisposeInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	RecordID uuid.UUID
	// Reason: retention_expired | resignation | manual (legal/operational value).
	Reason string
	// Method: ciphertext_deleted | key_destroyed.
	Method string
	// CertificateRef is an opaque reference to the disposal certificate/ledger.
	CertificateRef *string
	IP             *string
}

// Dispose logically disposes of a 個人番号: it transitions the record to
// status=disposed, destroys the ciphertext (number_enc → NULL) so the value can
// no longer be decrypted (復号不能化), and records a disposal certificate row.
// Disposing requires the dedicated mynumber:reveal permission (the same strict
// privilege that governs access to the number).  After disposal, Reveal /
// Provide / view-with-purpose all fail.
func (s *Service) Dispose(ctx context.Context, in DisposeInput) (*Disposal, error) {
	if in.Reason != ReasonRetentionExpired && in.Reason != ReasonResignation && in.Reason != ReasonManual {
		return nil, fmt.Errorf("%w: reason %q", ErrInvalidTransition, in.Reason)
	}
	if in.Method != MethodCiphertextDeleted && in.Method != MethodKeyDestroyed {
		return nil, fmt.Errorf("%w: method %q", ErrInvalidTransition, in.Method)
	}

	var disposal Disposal
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Disposal is as privileged as reveal — re-validate in the service layer.
		if err := requirePermission(tx, in.TenantID, in.ActorID, revealPermission); err != nil {
			return err
		}

		// Read current status.
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM mynumber_records WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RecordID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("mynumber: dispose read status: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if current.Status == StatusDisposed {
			return ErrDisposed
		}
		if !isRecordTransitionAllowed(current.Status, StatusDisposed) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, StatusDisposed)
		}

		// Logical expiry + 復号不能化: set status=disposed and destroy ciphertext.
		res := tx.Exec(
			`UPDATE mynumber_records
			 SET status = ?, number_enc = NULL, disposed_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			StatusDisposed, in.RecordID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("mynumber: dispose update record: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		disposal = Disposal{
			ID:             uuid.New(),
			TenantID:       in.TenantID,
			RecordID:       in.RecordID,
			Reason:         in.Reason,
			Method:         in.Method,
			DisposedBy:     &in.ActorID,
			CertificateRef: in.CertificateRef,
		}
		if err := tx.Exec(
			`INSERT INTO mynumber_disposals
			   (id, tenant_id, record_id, reason, method, disposed_by, certificate_ref)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			disposal.ID, disposal.TenantID, disposal.RecordID, disposal.Reason,
			disposal.Method, disposal.DisposedBy, disposal.CertificateRef,
		).Error; err != nil {
			return fmt.Errorf("mynumber: dispose insert disposal: %w", err)
		}

		idStr := in.RecordID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "mynumber.disposed",
			ResourceType: "mynumber_record",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &disposal, nil
}

// ---------------------------------------------------------------------------
// Status transition (expire) — 保管期限到来等の論理失効
// ---------------------------------------------------------------------------

// ExpireInput holds fields for marking a record as expired (保管期限到来).
type ExpireInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	RecordID uuid.UUID
	IP       *string
}

// Expire transitions a record active → expired.  This does not destroy the
// ciphertext; it flags the record for disposal.  Invalid transitions are
// rejected with ErrInvalidTransition.
func (s *Service) Expire(ctx context.Context, in ExpireInput) error {
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Expiry mutates record state — requires mynumber:write (authoritative gate).
		if err := requirePermission(tx, in.TenantID, in.ActorID, writePermission); err != nil {
			return err
		}
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM mynumber_records WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RecordID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("mynumber: expire read status: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isRecordTransitionAllowed(current.Status, StatusExpired) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, StatusExpired)
		}

		res := tx.Exec(
			`UPDATE mynumber_records SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			StatusExpired, in.RecordID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("mynumber: expire update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := in.RecordID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "mynumber.expired",
			ResourceType: "mynumber_record",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// ---------------------------------------------------------------------------
// Access-log query + verification
// ---------------------------------------------------------------------------

// ListAccessLogs returns the use/provision log entries for a record.
//
// Authorization: the actor must hold mynumber:read.  This service-layer gate is
// authoritative (it does not depend on the HTTP route middleware being wired).
func (s *Service) ListAccessLogs(ctx context.Context, tenantID, actorID, recordID uuid.UUID) ([]AccessLog, error) {
	var logs []AccessLog
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := requirePermission(tx, tenantID, actorID, readPermission); err != nil {
			return err
		}
		return tx.Raw(
			`SELECT id, tenant_id, target_record_id, actor_user_id, action,
			        purpose, provided_to, occurred_at, prev_hash, hash, seq, created_at
			 FROM mynumber_access_logs
			 WHERE tenant_id = ? AND target_record_id = ?
			 ORDER BY seq ASC`,
			tenantID, recordID,
		).Scan(&logs).Error
	})
	if err != nil {
		return nil, err
	}
	return logs, nil
}

// VerifyAccessLogChain re-derives every hash in the tenant's access log
// (ordered by seq) and confirms linkage and strict seq monotonicity, returning
// true when the chain is intact (no tampering).
func (s *Service) VerifyAccessLogChain(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	var intact bool
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		ok, err := verifyAccessLogChain(tx, tenantID)
		if err != nil {
			return err
		}
		intact = ok
		return nil
	})
	if err != nil {
		return false, err
	}
	return intact, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// loadRevealableCiphertext fetches the ciphertext for a record after confirming
// it exists, is not disposed, and the requested purpose is registered.
// Returns ErrNotFound / ErrDisposed / ErrPurposeNotAllowed as appropriate.
func (s *Service) loadRevealableCiphertext(tx *gorm.DB, tenantID, recordID uuid.UUID, purpose string) ([]byte, error) {
	var rec struct {
		Status    string `gorm:"column:status"`
		NumberEnc []byte `gorm:"column:number_enc"`
		Found     bool   `gorm:"column:found"`
	}
	if err := tx.Raw(
		`SELECT status, number_enc, true AS found
		 FROM mynumber_records
		 WHERE id = ? AND tenant_id = ? LIMIT 1`,
		recordID, tenantID,
	).Scan(&rec).Error; err != nil {
		return nil, fmt.Errorf("mynumber: load record: %w", err)
	}
	if !rec.Found {
		return nil, ErrNotFound
	}
	if rec.Status == StatusDisposed {
		return nil, ErrDisposed
	}

	// 目的外利用拒否: the requested purpose must be registered for this record.
	var purposeCount int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM mynumber_purposes
		 WHERE tenant_id = ? AND record_id = ? AND purpose = ?`,
		tenantID, recordID, purpose,
	).Scan(&purposeCount).Error; err != nil {
		return nil, fmt.Errorf("mynumber: load purpose: %w", err)
	}
	if purposeCount == 0 {
		return nil, ErrPurposeNotAllowed
	}

	return rec.NumberEnc, nil
}

// accessLogEntry holds the caller-supplied fields for an access-log row.
// The hash-chain fields (prev_hash / hash / seq) are derived internally.
type accessLogEntry struct {
	TenantID       uuid.UUID
	TargetRecordID uuid.UUID
	ActorUserID    *uuid.UUID
	Action         string
	Purpose        string
	ProvidedTo     *string
}

// appendAccessLog writes a tamper-evident use/provision log row within tx.
//
// Concurrency: a tenant-scoped advisory lock (matching the platform/audit
// approach) serialises chain writes so the hash chain stays linear.  tx must be
// inside WithinTenant for e.TenantID.  PII / number values are never stored.
func (s *Service) appendAccessLog(tx *gorm.DB, e accessLogEntry) error {
	// Serialise per-tenant chain writes.
	if err := tx.Exec(
		`SELECT pg_advisory_xact_lock(hashtext(?::text)::bigint)`,
		"mynumber_access_logs:"+e.TenantID.String(),
	).Error; err != nil {
		return fmt.Errorf("mynumber: access log advisory lock: %w", err)
	}

	// Fetch the latest hash for this tenant.
	var prevHash string
	{
		var rows []struct {
			Hash string `gorm:"column:hash"`
		}
		if err := tx.Raw(
			`SELECT hash FROM mynumber_access_logs
			 WHERE tenant_id = ? ORDER BY seq DESC LIMIT 1`,
			e.TenantID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("mynumber: access log fetch prev_hash: %w", err)
		}
		if len(rows) > 0 {
			prevHash = rows[0].Hash
		}
	}

	now := time.Now().UTC()
	rowID := uuid.New()
	canonical := accessLogCanonical(
		prevHash, rowID, e.Action, e.Purpose, e.TargetRecordID,
		e.ProvidedTo, e.ActorUserID, now,
	)
	sum := sha256.Sum256([]byte(canonical))
	hash := hex.EncodeToString(sum[:])

	if err := tx.Exec(
		`INSERT INTO mynumber_access_logs
		   (id, tenant_id, target_record_id, actor_user_id, action, purpose,
		    provided_to, occurred_at, prev_hash, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rowID, e.TenantID, e.TargetRecordID, e.ActorUserID, e.Action, e.Purpose,
		e.ProvidedTo, now, prevHash, hash,
	).Error; err != nil {
		return fmt.Errorf("mynumber: access log insert: %w", err)
	}
	return nil
}

// accessLogCanonical builds the deterministic canonical string hashed for a row.
// Format: prevHash|id|action|purpose|target_record_id|provided_to|actor_user_id|occurred_at
func accessLogCanonical(
	prevHash string,
	id uuid.UUID,
	action, purpose string,
	targetRecordID uuid.UUID,
	providedTo *string,
	actorUserID *uuid.UUID,
	occurredAt time.Time,
) string {
	providedToStr := ""
	if providedTo != nil {
		providedToStr = *providedTo
	}
	actorStr := ""
	if actorUserID != nil {
		actorStr = actorUserID.String()
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s",
		prevHash,
		id.String(),
		action,
		purpose,
		targetRecordID.String(),
		providedToStr,
		actorStr,
		occurredAt.Format(time.RFC3339Nano),
	)
}

// verifyAccessLogChain re-derives the hash chain for tenantID and verifies
// linkage + strict seq monotonicity.  tx must be inside WithinTenant.
func verifyAccessLogChain(tx *gorm.DB, tenantID uuid.UUID) (bool, error) {
	var rows []AccessLog
	if err := tx.Raw(
		`SELECT id, tenant_id, target_record_id, actor_user_id, action, purpose,
		        provided_to, occurred_at, prev_hash, hash, seq
		 FROM mynumber_access_logs
		 WHERE tenant_id = ? ORDER BY seq ASC`,
		tenantID,
	).Scan(&rows).Error; err != nil {
		return false, fmt.Errorf("mynumber: verify chain fetch: %w", err)
	}

	var expectedPrevHash string
	var expectedSeq int64 = -1
	for _, row := range rows {
		if expectedSeq == -1 {
			expectedSeq = row.Seq
		} else {
			if row.Seq <= expectedSeq {
				return false, nil
			}
			expectedSeq = row.Seq
		}
		if row.PrevHash != expectedPrevHash {
			return false, nil
		}
		canonical := accessLogCanonical(
			row.PrevHash, row.ID, row.Action, row.Purpose, row.TargetRecordID,
			row.ProvidedTo, row.ActorUserID, row.OccurredAt.UTC(),
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
