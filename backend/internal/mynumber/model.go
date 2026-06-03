// Package mynumber implements マイナンバー (個人番号) collection, encrypted
// storage in a physically separated store, strict access control, tamper-evident
// use/provision logging, retention management, and disposal (ST-LM-09).
//
// Security design:
//   - 個人番号 is stored ONLY in mynumber_records.number_enc as AES-256-GCM
//     ciphertext (bytea).  The plaintext is NEVER persisted, logged, or written
//     to audit / access-log rows.
//   - Reveal (decryption / display) requires the dedicated mynumber:reveal
//     permission AND a matching registered purpose, re-validated in the service
//     layer (defence-in-depth) — ordinary HR read permissions are insufficient.
//   - Every view / decrypt / provide is recorded in mynumber_access_logs with a
//     SHA-256 hash chain (tamper-evident), referencing only opaque IDs.
//   - Disposal logically expires the record (status=disposed) and destroys the
//     ciphertext (復号不能化); a disposal certificate row is recorded.  After
//     disposal, decrypt / provide / display are all rejected.
//
// Legal-value note: retention periods, the enumerated purpose list, key
// rotation policy, and disposal reasons/methods are legal/operational policy
// values.  They MUST be externalised as configuration and confirmed against the
// latest official guidance and 社労士/弁護士 review to follow law revisions.
// This implementation is not legal advice.
package mynumber

import (
	"time"

	"github.com/google/uuid"
)

// Subject types for a mynumber record.
const (
	SubjectSelf      = "self"
	SubjectDependent = "dependent"
)

// Record lifecycle statuses.
const (
	StatusActive   = "active"
	StatusExpired  = "expired"
	StatusDisposed = "disposed"
)

// Enumerated use purposes (限定列挙).  Legal value — externalise via config to
// support additions when the law / operations change.
const (
	PurposePayroll         = "payroll"
	PurposeSocialInsurance = "social_insurance"
	PurposeTax             = "tax"
)

// Access-log actions.
const (
	ActionView    = "view"
	ActionDecrypt = "decrypt"
	ActionProvide = "provide"
)

// Disposal reasons.
const (
	ReasonRetentionExpired = "retention_expired"
	ReasonResignation      = "resignation"
	ReasonManual           = "manual"
)

// Disposal methods.
const (
	MethodCiphertextDeleted = "ciphertext_deleted"
	MethodKeyDestroyed      = "key_destroyed"
)

// Record is the GORM model for mynumber_records (専用分離ストア).
//
// Security note on NumberEnc:
//   - This field holds the AES-256-GCM ciphertext of the 個人番号.
//   - The plaintext is NEVER stored or returned to callers without passing the
//     mynumber:reveal permission AND purpose checks in the service layer.
//   - Callers without reveal permission receive a nil NumberEnc field.
type Record struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID   uuid.UUID  `gorm:"column:employee_id"`
	SubjectType  string     `gorm:"column:subject_type"`
	DependentRef *uuid.UUID `gorm:"column:dependent_ref"`
	// NumberEnc holds the encrypted 個人番号 ciphertext.  Decrypt only after the
	// service-layer reveal-permission + purpose checks pass.
	NumberEnc      []byte     `gorm:"column:number_enc;type:bytea"`
	Status         string     `gorm:"column:status"`
	CollectedAt    time.Time  `gorm:"column:collected_at"`
	RetentionUntil *time.Time `gorm:"column:retention_until"`
	DisposedAt     *time.Time `gorm:"column:disposed_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Record to mynumber_records.
func (Record) TableName() string { return "mynumber_records" }

// Purpose is the GORM model for mynumber_purposes (利用目的).
type Purpose struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID  uuid.UUID `gorm:"column:tenant_id"`
	RecordID  uuid.UUID `gorm:"column:record_id"`
	Purpose   string    `gorm:"column:purpose"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

// TableName maps Purpose to mynumber_purposes.
func (Purpose) TableName() string { return "mynumber_purposes" }

// AccessLog is the GORM model for mynumber_access_logs (利用提供ログ).
//
// SECURITY: this row references only opaque IDs.  個人番号 values and decrypted
// plaintext are NEVER stored here.
type AccessLog struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	TargetRecordID uuid.UUID  `gorm:"column:target_record_id"`
	ActorUserID    *uuid.UUID `gorm:"column:actor_user_id"`
	Action         string     `gorm:"column:action"`
	Purpose        string     `gorm:"column:purpose"`
	ProvidedTo     *string    `gorm:"column:provided_to"`
	OccurredAt     time.Time  `gorm:"column:occurred_at"`
	PrevHash       string     `gorm:"column:prev_hash"`
	Hash           string     `gorm:"column:hash"`
	Seq            int64      `gorm:"column:seq"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
}

// TableName maps AccessLog to mynumber_access_logs.
func (AccessLog) TableName() string { return "mynumber_access_logs" }

// Disposal is the GORM model for mynumber_disposals (廃棄記録).
type Disposal struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	RecordID       uuid.UUID  `gorm:"column:record_id"`
	Reason         string     `gorm:"column:reason"`
	Method         string     `gorm:"column:method"`
	DisposedBy     *uuid.UUID `gorm:"column:disposed_by"`
	DisposedAt     time.Time  `gorm:"column:disposed_at"`
	CertificateRef *string    `gorm:"column:certificate_ref"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Disposal to mynumber_disposals.
func (Disposal) TableName() string { return "mynumber_disposals" }
