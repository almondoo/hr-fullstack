// Package offer implements the offer / 内定 management domain (ST-ATS-05).
//
// Features:
//   - ATS-015: Offer creation against a final-stage application; offer letter
//     issuance; candidate acceptance / decline / expiry / rescind management.
//   - CMP-006: Offer letter document + signing evidence (content_hash for
//     truthfulness, signed_at / signer_ref for the signing audit trail).
//     Electronic signing / contracting reuses the LM-002 method and the INT-009
//     integration interface (loosely coupled: an INT-009 failure must not block
//     a manual signing record).
//   - Sensitive offer terms (annual salary, compensation detail) are encrypted
//     with AES-256-GCM (bytea, *_enc) and require the offer:read_sensitive
//     permission to decrypt — same defence-in-depth as onboarding intake forms.
//
// Legal / compliance note:
//   - Legal values (electronic-storage method, retention years, required offer
//     fields, expiry lead time) are NOT hardcoded; they live in offer_settings
//     so they can follow legal revisions. The latest values require confirmation
//     by the competent authorities / a labour & social-security attorney or
//     lawyer (CMP-001 / CMP-006). This implementation is not legal advice.
package offer

import (
	"time"

	"github.com/google/uuid"
)

// Offer status constants. Allowed status machine:
//
//	draft → sent → accepted | declined
//	sent  → expired (expiry) | rescinded (withdrawal)
//
// Terminal states (accepted / declined / expired / rescinded) have no outward
// transitions.
const (
	StatusDraft     = "draft"
	StatusSent      = "sent"
	StatusAccepted  = "accepted"
	StatusDeclined  = "declined"
	StatusExpired   = "expired"
	StatusRescinded = "rescinded"
)

// Response constants for offer_responses.response.
const (
	ResponseAccepted = "accepted"
	ResponseDeclined = "declined"
)

// RespondedVia constants for offer_responses.responded_via.
const (
	ViaPortal = "portal"
	ViaEsign  = "esign"
	ViaManual = "manual"
)

// Setting is the GORM model for offer_settings.
//
// Holds per-tenant legal configuration so that legal values are not hardcoded.
// RequiredFieldsJSON: required offer fields set (労働条件通知 に準ずる, CMP-001).
// RetentionYears / EsignStorageMode: electronic-storage requirements (CMP-006).
// DefaultExpiryLeadDays: default expiry lead time for new offers.
type Setting struct {
	ID                    uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID              uuid.UUID `gorm:"column:tenant_id"`
	RequiredFieldsJSON    []byte    `gorm:"column:required_fields_json;type:jsonb"`
	RetentionYears        int       `gorm:"column:retention_years"`
	EsignStorageMode      string    `gorm:"column:esign_storage_mode"`
	DefaultExpiryLeadDays int       `gorm:"column:default_expiry_lead_days"`
	CreatedAt             time.Time `gorm:"column:created_at"`
	UpdatedAt             time.Time `gorm:"column:updated_at"`
}

// TableName maps Setting to offer_settings.
func (Setting) TableName() string { return "offer_settings" }

// Offer is the GORM model for offers.
//
// Security note on AnnualSalaryEnc / CompensationDetailEnc:
//   - These fields hold the AES-256-GCM ciphertext of the sensitive offer terms.
//   - The plaintext is NEVER stored or returned to callers without the
//     offer:read_sensitive permission check (re-validated in the service layer).
//   - Callers that do not hold offer:read_sensitive receive nil/omitted fields.
//
// ApplicationID is a logical reference to ST-ATS-03 applications (plain uuid,
// no FK — cross-story isolation). ApprovalRequestID composite-FK links to the
// ST-FND-08 approval_requests table.
type Offer struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	ApplicationID  uuid.UUID  `gorm:"column:application_id"`
	Status         string     `gorm:"column:status"`
	Position       string     `gorm:"column:position"`
	EmploymentType string     `gorm:"column:employment_type"`
	StartDate      *time.Time `gorm:"column:start_date"`
	ExpiryDate     *time.Time `gorm:"column:expiry_date"`
	// AnnualSalaryEnc holds the encrypted annual salary ciphertext.
	// Use crypto.Decrypt to obtain plaintext; only do so when the caller holds
	// offer:read_sensitive permission.
	AnnualSalaryEnc []byte `gorm:"column:annual_salary_enc;type:bytea"`
	// CompensationDetailEnc holds the encrypted compensation detail ciphertext.
	CompensationDetailEnc []byte     `gorm:"column:compensation_detail_enc;type:bytea"`
	ApprovalRequestID     *uuid.UUID `gorm:"column:approval_request_id"`
	CreatedAt             time.Time  `gorm:"column:created_at"`
	UpdatedAt             time.Time  `gorm:"column:updated_at"`
}

// TableName maps Offer to offers.
func (Offer) TableName() string { return "offers" }

// Letter is the GORM model for offer_letters.
//
// Holds the offer letter document reference and the signing evidence required
// for CMP-006 (truthfulness / visibility): content_hash for tamper detection,
// signed_at / signer_ref for the signing audit trail. signer_ref is an opaque
// reference — no PII (names etc.) is stored.
type Letter struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	OfferID         uuid.UUID  `gorm:"column:offer_id"`
	FileRef         string     `gorm:"column:file_ref"`
	Version         int        `gorm:"column:version"`
	EsignProvider   string     `gorm:"column:esign_provider"`
	EsignEnvelopeID string     `gorm:"column:esign_envelope_id"`
	ContentHash     string     `gorm:"column:content_hash"`
	SignerRef       *string    `gorm:"column:signer_ref"`
	SignedAt        *time.Time `gorm:"column:signed_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps Letter to offer_letters.
func (Letter) TableName() string { return "offer_letters" }

// Response is the GORM model for offer_responses.
//
// Records the candidate acceptance / decline history. An accepted response is
// the integration trigger for ST-ATS-06 (candidate → employee master creation).
type Response struct {
	ID           uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID `gorm:"column:tenant_id"`
	OfferID      uuid.UUID `gorm:"column:offer_id"`
	Response     string    `gorm:"column:response"`
	RespondedVia string    `gorm:"column:responded_via"`
	RespondedAt  time.Time `gorm:"column:responded_at"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName maps Response to offer_responses.
func (Response) TableName() string { return "offer_responses" }
