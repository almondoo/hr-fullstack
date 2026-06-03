// Package applicant implements the applicant (応募者/候補者) database for the
// ATS (Applicant Tracking System) domain — ST-ATS-02.
//
// Features:
//   - Applicant CRUD with tenant-scoped RLS isolation.
//   - Sensitive contact PII (email / phone) stored AES-256-GCM encrypted
//     (*_enc bytea) and decrypted in the service layer only when the caller
//     holds the ats:applicant:read_sensitive permission (multi-layer defence,
//     mirroring the onboarding intake-form pattern).
//   - Document references (履歴書/職務経歴書) held as opaque file_ref values only;
//     the file body PII is never expanded into a DB column (ST-FND-10 file store
//     is the system of record).
//   - Consent management (利用目的別の同意取得/撤回) for 個人情報保護法 (CMP-004).
//   - Duplicate detection (正規化メール突合) and human-confirmed merge with an
//     auditable source→target merge history.  Source records are marked
//     logically merged (merged_into_id set); rows are never physically deleted.
//   - Retention: rejected applicants past their retention window are logically
//     expired (anonymised / access-restricted), never physically deleted.
//
// 法令値(保持期間ラベル等)はハードコードせずテナント設定参照とする。最新の
// 官公庁情報・社労士/弁護士確認のうえ設定化して改正に追従すること。本実装は
// 法的助言ではない (CMP-004)。
package applicant

import (
	"time"

	"github.com/google/uuid"
)

// Applicant status values.
const (
	StatusApplied      = "applied"
	StatusScreening    = "screening"
	StatusInterviewing = "interviewing"
	StatusOffered      = "offered"
	StatusHired        = "hired"
	StatusRejected     = "rejected"
	StatusWithdrawn    = "withdrawn"
)

// Consent status values.
const (
	ConsentGranted   = "granted"
	ConsentWithdrawn = "withdrawn"
	ConsentUnknown   = "unknown"
)

// Source values (応募媒体/経路).
const (
	SourceDirect   = "direct"
	SourceAgent    = "agent"
	SourceReferral = "referral"
	SourceJobBoard = "job_board"
	SourceOther    = "other"
)

// Document type values.
const (
	DocTypeResume    = "resume"
	DocTypeCV        = "cv"
	DocTypePortfolio = "portfolio"
	DocTypeOther     = "other"
)

// Applicant is the GORM model for applicants.
//
// Security note on EmailEnc / PhoneEnc:
//   - These fields hold the AES-256-GCM ciphertext of the applicant's email and
//     phone number.  The plaintext is NEVER stored or returned to callers
//     without the ats:applicant:read_sensitive permission check.
//   - Callers that do not hold that permission receive nil/omitted fields.
type Applicant struct {
	ID           uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID  `gorm:"column:tenant_id"`
	JobPostingID *uuid.UUID `gorm:"column:job_posting_id"`
	MergedIntoID *uuid.UUID `gorm:"column:merged_into_id"`
	LastName     string     `gorm:"column:last_name"`
	FirstName    string     `gorm:"column:first_name"`
	// EmailNormalized is a normalised duplicate-detection match key (lower-cased
	// email), not a plaintext store of the raw contact email.
	EmailNormalized *string    `gorm:"column:email_normalized"` //nolint:misspell // DB column name is API contract
	BirthDate       *time.Time `gorm:"column:birth_date"`
	// EmailEnc / PhoneEnc hold the encrypted ciphertext.  Use crypto.Decrypt to
	// obtain plaintext; only do so when the caller holds
	// ats:applicant:read_sensitive permission.
	EmailEnc           []byte     `gorm:"column:email_enc;type:bytea"`
	PhoneEnc           []byte     `gorm:"column:phone_enc;type:bytea"`
	Status             string     `gorm:"column:status"`
	ConsentStatus      string     `gorm:"column:consent_status"`
	Source             string     `gorm:"column:source"`
	RetentionLabel     string     `gorm:"column:retention_label"`
	RetentionExpiresOn *time.Time `gorm:"column:retention_expires_on"`
	AnonymizedAt       *time.Time `gorm:"column:anonymized_at"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps Applicant to applicants.
func (Applicant) TableName() string { return "applicants" }

// Document is the GORM model for applicant_documents.
// FileRef is an opaque reference into the file storage platform (ST-FND-10);
// the file body PII is never expanded into a DB column.
type Document struct {
	ID          uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	ApplicantID uuid.UUID `gorm:"column:applicant_id"`
	DocType     string    `gorm:"column:doc_type"`
	FileRef     string    `gorm:"column:file_ref"`
	FileName    string    `gorm:"column:file_name"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps Document to applicant_documents.
func (Document) TableName() string { return "applicant_documents" }

// Consent is the GORM model for applicant_consents.
// Tracks per-purpose consent grant/withdrawal for 個人情報保護法 (CMP-004).
type Consent struct {
	ID          uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID  `gorm:"column:tenant_id"`
	ApplicantID uuid.UUID  `gorm:"column:applicant_id"`
	Purpose     string     `gorm:"column:purpose"`
	GrantedAt   *time.Time `gorm:"column:granted_at"`
	WithdrawnAt *time.Time `gorm:"column:withdrawn_at"`
	CrossBorder bool       `gorm:"column:cross_border"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps Consent to applicant_consents.
func (Consent) TableName() string { return "applicant_consents" }

// Merge is the GORM model for applicant_merges.
// Records an auditable source→target duplicate-merge event.  Source records are
// marked logically merged (applicants.merged_into_id); rows are never deleted.
type Merge struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID  `gorm:"column:tenant_id"`
	SourceApplicantID uuid.UUID  `gorm:"column:source_applicant_id"`
	TargetApplicantID uuid.UUID  `gorm:"column:target_applicant_id"`
	MergedBy          *uuid.UUID `gorm:"column:merged_by"`
	MergedAt          time.Time  `gorm:"column:merged_at"`
	Notes             *string    `gorm:"column:notes"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

// TableName maps Merge to applicant_merges.
func (Merge) TableName() string { return "applicant_merges" }
