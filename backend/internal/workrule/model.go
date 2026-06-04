// Package workrule implements the work rule (就業規則) and labour agreement
// (労使協定 / 36協定) lifecycle management domain (ST-LM-11).
//
// Features:
//   - LM-055: Work rule version management (改定履歴・現行版/旧版), employee
//     notification (周知) and read/consent (同意) tracking.
//   - LM-056: Labour agreement (36協定/その他) document version management,
//     electronic filing status machine (draft→filed→accepted), validity period
//     and renewal-alert management.
//   - CORE-009 / CMP-009: retention policy and statute-reflection (要専門家確認)
//     handled as new versions.
//
// Scope boundary: the statutory UPPER LIMIT values of an 36協定 (月/年/特別条項)
// are owned by the existing attendance.labor_agreements table (American-spelled DB name). //nolint:misspell // DB table name is schema contract
// This package only manages the agreement DOCUMENT / version / filing / validity metadata
// and links to that source-of-truth row via LinkedLaborAgreementID — it never
// duplicates the limit values.
//
// Legal note: statutory values (renewal alert lead time, retention period,
// filing forms, etc.) are NOT hardcoded.  They are read from per-tenant
// workrule_settings so the system can follow legal amendments.  Statutory
// values require confirmation from the latest official sources and a labour /
// legal specialist; this implementation is not legal advice.
package workrule

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Status / type constants
// ---------------------------------------------------------------------------

// Work rule version statuses.
const (
	VersionStatusDraft      = "draft"
	VersionStatusPublished  = "published"
	VersionStatusSuperseded = "superseded"
)

// Acknowledgement consent kinds.
const (
	ConsentRead   = "read"
	ConsentAgreed = "agreed"
)

// Labour agreement types.
const (
	AgreementTypeArticle36 = "article36"
	AgreementTypeOther     = "other"
)

// Labour agreement filing statuses.
const (
	FilingStatusDraft    = "draft"
	FilingStatusFiled    = "filed"
	FilingStatusAccepted = "accepted"
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// Settings is the GORM model for workrule_settings.
//
// Holds per-tenant legal/operational configuration so statutory values are not
// hardcoded: the renewal alert lead time and retention policy label.
type Settings struct {
	ID                     uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID               uuid.UUID `gorm:"column:tenant_id"`
	AgreementAlertLeadDays int       `gorm:"column:agreement_alert_lead_days"`
	RetentionPolicy        string    `gorm:"column:retention_policy"`
	TemplatesJSON          []byte    `gorm:"column:templates_json;type:jsonb"`
	CreatedAt              time.Time `gorm:"column:created_at"`
	UpdatedAt              time.Time `gorm:"column:updated_at"`
}

// TableName maps Settings to workrule_settings.
func (Settings) TableName() string { return "workrule_settings" }

// WorkRule is the GORM model for work_rules (就業規則ドキュメントの論理単位).
type WorkRule struct {
	ID               uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id"`
	Title            string     `gorm:"column:title"`
	Category         string     `gorm:"column:category"`
	CurrentVersionID *uuid.UUID `gorm:"column:current_version_id"`
	RetentionPolicy  string     `gorm:"column:retention_policy"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName maps WorkRule to work_rules.
func (WorkRule) TableName() string { return "work_rules" }

// WorkRuleVersion is the GORM model for work_rule_versions (改定履歴の中核).
type WorkRuleVersion struct { //nolint:revive // name intentionally includes package prefix for clarity
	ID                   uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID             uuid.UUID  `gorm:"column:tenant_id"`
	WorkRuleID           uuid.UUID  `gorm:"column:work_rule_id"`
	Version              int        `gorm:"column:version"`
	RevisedOn            *time.Time `gorm:"column:revised_on"`
	RevisionReason       string     `gorm:"column:revision_reason"`
	DocumentRef          *string    `gorm:"column:document_ref"`
	Status               string     `gorm:"column:status"`
	PublishedAt          *time.Time `gorm:"column:published_at"`
	RequiresExpertReview bool       `gorm:"column:requires_expert_review"`
	CreatedBy            *uuid.UUID `gorm:"column:created_by"`
	CreatedAt            time.Time  `gorm:"column:created_at"`
	UpdatedAt            time.Time  `gorm:"column:updated_at"`
}

// TableName maps WorkRuleVersion to work_rule_versions.
func (WorkRuleVersion) TableName() string { return "work_rule_versions" }

// Acknowledgement is the GORM model for work_rule_acknowledgements
// (従業員ごとの周知既読・同意取得の証跡).
type Acknowledgement struct {
	ID                uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID          uuid.UUID `gorm:"column:tenant_id"`
	WorkRuleVersionID uuid.UUID `gorm:"column:work_rule_version_id"`
	EmployeeID        uuid.UUID `gorm:"column:employee_id"`
	Consent           string    `gorm:"column:consent"`
	AcknowledgedAt    time.Time `gorm:"column:acknowledged_at"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

// TableName maps Acknowledgement to work_rule_acknowledgements.
func (Acknowledgement) TableName() string { return "work_rule_acknowledgements" }

// LaborAgreementDocument is the GORM model for the labour agreement documents DB table. //nolint:misspell // type name matches DB schema contract
//
// LinkedLaborAgreementID references attendance.labor_agreements(id) — the //nolint:misspell // DB table name is schema contract
// source of truth for 36協定 upper-limit values.  This package never duplicates
// those limit values; it only stores the document/version/filing/validity meta.
//
// GovFilingID references gov_filings(id) once InitiateEGovFiling has been
// called (Issue #21 足場).  NULL until the e-Gov filing flow is initiated.
type LaborAgreementDocument struct { //nolint:misspell // type name matches DB table (schema contract)
	ID                     uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID               uuid.UUID  `gorm:"column:tenant_id"`
	Title                  string     `gorm:"column:title"`
	AgreementType          string     `gorm:"column:agreement_type"`
	Version                int        `gorm:"column:version"`
	ValidFrom              time.Time  `gorm:"column:valid_from"`
	ValidTo                time.Time  `gorm:"column:valid_to"`
	FilingStatus           string     `gorm:"column:filing_status"`
	FiledAt                *time.Time `gorm:"column:filed_at"`
	AcceptedAt             *time.Time `gorm:"column:accepted_at"`
	DocumentRef            *string    `gorm:"column:document_ref"`
	LinkedLaborAgreementID *uuid.UUID `gorm:"column:linked_labor_agreement_id"` //nolint:misspell // DB column name is schema contract
	RenewalAlertAt         *time.Time `gorm:"column:renewal_alert_at"`
	// GovFilingID links to gov_filings(id) once InitiateEGovFiling is called.
	// NULL until the 36協定 e-Gov filing flow is initiated (Issue #21 足場).
	// Composite FK (govfiling_id, tenant_id) → gov_filings(id, tenant_id).
	GovFilingID *uuid.UUID `gorm:"column:govfiling_id"`
	CreatedBy   *uuid.UUID `gorm:"column:created_by"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	UpdatedAt   time.Time  `gorm:"column:updated_at"`
}

// TableName maps LaborAgreementDocument to the labour_agreement_documents table. //nolint:misspell // type name matches DB schema
func (LaborAgreementDocument) TableName() string { return "labor_agreement_documents" } //nolint:misspell // DB table name is schema contract
