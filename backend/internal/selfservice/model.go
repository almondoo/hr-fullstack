// Package selfservice implements ST-FND-10: three related foundations for the
// HR SaaS platform:
//
//   - Employee self-service change requests (自己情報更新申請): an employee
//     submits a change to their own data as a request; it is never written
//     directly to the master.  Approval engine (ST-FND-08) review reflects the
//     change to employees/related masters in a single transaction at approval
//     time.
//   - CSV bulk import (CSV一括取込): masters (employees/departments) are imported
//     from CSV with per-row validation, a dry-run (validate-only) phase and an
//     apply phase (all-or-nothing or skip-errors), returning a row-numbered
//     error report.
//   - Document store (ファイル/書類保管): encrypted-at-rest storage with version
//     management and retention policy (logical expiry, never physical deletion).
//
// Security model:
//   - All tables are tenant-scoped with RLS; the service additionally filters by
//     tenant_id (defence in depth).
//   - Sensitive PII (bank account / dependents diffs in change requests, sensitive
//     CSV rows, small inline document content) is stored AES-256-GCM encrypted in
//     bytea columns; plaintext is never persisted, logged, or written to audit.
//   - Legal retention values are NOT hardcoded; retention labels are configurable
//     and legal-hold documents cannot be expired or deleted.
package selfservice

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Status / type constants
// ---------------------------------------------------------------------------

// Change request status values.
const (
	ChangeStatusDraft     = "draft"
	ChangeStatusPending   = "pending"
	ChangeStatusApproved  = "approved"
	ChangeStatusRejected  = "rejected"
	ChangeStatusCancelled = "cancelled"
)

// Change request target types (must match the chk_sscr_target_type constraint).
const (
	TargetEmployeeProfile = "employee_profile"
	TargetEmergencyError  = "emergency_contact"
	TargetCommute         = "commute"
	TargetBankAccount     = "bank_account"
	TargetDependents      = "dependents"
)

// CSV import job modes.
const (
	ModeDryRun = "dry_run"
	ModeApply  = "apply"
)

// CSV import apply policies.
const (
	PolicyAllOrNothing = "all_or_nothing"
	PolicySkipErrors   = "skip_errors"
)

// CSV import encodings.
const (
	EncodingUTF8     = "utf-8"
	EncodingShiftJIS = "shift_jis"
)

// CSV import job status values.
const (
	JobStatusPending    = "pending"
	JobStatusValidating = "validating"
	JobStatusValidated  = "validated"
	JobStatusApplying   = "applying"
	JobStatusCompleted  = "completed"
	JobStatusFailed     = "failed"
)

// CSV import types.
const (
	ImportTypeEmployees   = "employees"
	ImportTypeDepartments = "departments"
)

// CSV row validation status.
const (
	RowValid   = "valid"
	RowInvalid = "invalid"
)

// Document categories (must match the chk_documents_category constraint).
const (
	CategoryContract    = "contract"
	CategoryCertificate = "certificate"
	CategoryPayslip     = "payslip"
	CategoryMisc        = "misc"
)

// ---------------------------------------------------------------------------
// GORM models
// ---------------------------------------------------------------------------

// ChangeRequest is the GORM model for self_service_change_requests.
//
// Security note on ChangesSensitiveEnc:
//   - Holds the AES-256-GCM ciphertext of any sensitive change diff (bank
//     account, dependents, etc.).  The plaintext is NEVER stored or returned to
//     callers without the selfservice:read_sensitive permission check.
type ChangeRequest struct {
	ID                  uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID          uuid.UUID  `gorm:"column:employee_id"`
	RequestedByUserID   uuid.UUID  `gorm:"column:requested_by_user_id"`
	TargetType          string     `gorm:"column:target_type"`
	ChangesJSON         []byte     `gorm:"column:changes_json;type:jsonb"`
	ChangesSensitiveEnc []byte     `gorm:"column:changes_sensitive_enc;type:bytea"`
	ApprovalRequestID   *uuid.UUID `gorm:"column:approval_request_id"`
	Status              string     `gorm:"column:status"`
	ReflectedAt         *time.Time `gorm:"column:reflected_at"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	UpdatedAt           time.Time  `gorm:"column:updated_at"`
}

// TableName maps ChangeRequest to self_service_change_requests.
func (ChangeRequest) TableName() string { return "self_service_change_requests" }

// ImportJob is the GORM model for csv_import_jobs.
type ImportJob struct {
	ID               uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id"`
	ImportType       string     `gorm:"column:import_type"`
	Mode             string     `gorm:"column:mode"`
	ApplyPolicy      string     `gorm:"column:apply_policy"`
	Encoding         string     `gorm:"column:encoding"`
	Status           string     `gorm:"column:status"`
	TotalRows        int        `gorm:"column:total_rows"`
	SuccessRows      int        `gorm:"column:success_rows"`
	ErrorRows        int        `gorm:"column:error_rows"`
	UploadedByUserID uuid.UUID  `gorm:"column:uploaded_by_user_id"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	CompletedAt      *time.Time `gorm:"column:completed_at"`
}

// TableName maps ImportJob to csv_import_jobs.
func (ImportJob) TableName() string { return "csv_import_jobs" }

// ImportRow is the GORM model for csv_import_rows.
//
// Security note on RawDataEnc:
//   - Holds the AES-256-GCM ciphertext of row data that may contain sensitive
//     PII.  Plaintext row data with sensitive fields is never persisted.
type ImportRow struct {
	ID               uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID `gorm:"column:tenant_id"`
	JobID            uuid.UUID `gorm:"column:job_id"`
	RowNumber        int       `gorm:"column:row_number"`
	RawDataJSON      []byte    `gorm:"column:raw_data_json;type:jsonb"`
	RawDataEnc       []byte    `gorm:"column:raw_data_enc;type:bytea"`
	ValidationStatus string    `gorm:"column:validation_status"`
	ErrorsJSON       []byte    `gorm:"column:errors_json;type:jsonb"`
	Applied          bool      `gorm:"column:applied"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// TableName maps ImportRow to csv_import_rows.
func (ImportRow) TableName() string { return "csv_import_rows" }

// Document is the GORM model for documents (logical document = version parent).
type Document struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	OwnerEmployeeID    *uuid.UUID `gorm:"column:owner_employee_id"`
	Category           string     `gorm:"column:category"`
	Title              string     `gorm:"column:title"`
	CurrentVersionID   *uuid.UUID `gorm:"column:current_version_id"`
	RetentionLabel     string     `gorm:"column:retention_label"`
	RetentionExpiresOn *time.Time `gorm:"column:retention_expires_on"`
	LogicallyExpired   bool       `gorm:"column:logically_expired"`
	LegalHold          bool       `gorm:"column:legal_hold"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps Document to documents.
func (Document) TableName() string { return "documents" }

// DocumentVersion is the GORM model for document_versions.
//
// Security note on ContentEnc:
//   - Holds the AES-256-GCM ciphertext of small inline document content.  The
//     real binary normally lives in object storage (StorageKey); ContentEnc is
//     used only for small inline payloads and plaintext is never persisted.
type DocumentVersion struct {
	ID               uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID `gorm:"column:tenant_id"`
	DocumentID       uuid.UUID `gorm:"column:document_id"`
	VersionNo        int       `gorm:"column:version_no"`
	StorageKey       string    `gorm:"column:storage_key"`
	ContentHash      string    `gorm:"column:content_hash"`
	MimeType         string    `gorm:"column:mime_type"`
	SizeBytes        int64     `gorm:"column:size_bytes"`
	EncKeyRef        string    `gorm:"column:enc_key_ref"`
	ContentEnc       []byte    `gorm:"column:content_enc;type:bytea"`
	UploadedByUserID uuid.UUID `gorm:"column:uploaded_by_user_id"`
	UploadedAt       time.Time `gorm:"column:uploaded_at"`
}

// TableName maps DocumentVersion to document_versions.
func (DocumentVersion) TableName() string { return "document_versions" }
