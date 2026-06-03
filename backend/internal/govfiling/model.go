// Package govfiling implements ST-LM-08: 社会保険・労働保険 帳票生成と電子申請
// (e-Gov / マイナポータルAPI) 連携.
//
// Features:
//   - LM-010/011: 健保・厚年・雇用保険・労災 各届出の帳票データ生成 (insurance_settings
//     の料率/等級表 JSONB を参照。法令値はハードコードせず設定化し改正追従)。
//   - LM-012/INT-003: e-Gov / マイナポータル送信を Submitter インタフェースで抽象化し、
//     MVP ではモックアダプタで実装 (実送信は P3)。冪等キーで二重送信防止。
//   - LM-013: 申請ステータス機械 (draft→submitted→accepted/returned→completed/error) と、
//     公文書 (受付控・決定通知・返戻理由・生成帳票) の暗号化保存 (content_enc, AES-256-GCM)、
//     ステータス遷移履歴の保管。
//   - LM-014: 協会けんぽ/健保組合の書式差異を insurer_kind 設定で切替。
//
// Security:
//   - 機微情報 (マイナンバー等) の復号値は payload_json/external_ref/監査 resource_id の
//     いずれにも格納しない。マイナンバーは ST-LM-09 マイナンバーストアをトークン/別経路で参照する。
//   - 公文書本体 content_enc は AES-256-GCM。復号は service 層の RBAC 再検証
//     (filing:read_sensitive) 通過時のみ・別戻り値で返す。
//
// Legal note:
//   - 料率・等級表・保存年限・判定閾値・様式バージョン等の法令値は insurance_settings に
//     設定化する。具体値は最新の官公庁情報・社労士/弁護士確認が前提であり、本実装は法的助言で
//     はない。
package govfiling

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Status / type constants
// ---------------------------------------------------------------------------

// Filing statuses (申請ステータス機械の状態).
const (
	StatusDraft     = "draft"
	StatusSubmitted = "submitted"
	StatusAccepted  = "accepted"
	StatusReturned  = "returned"
	StatusCompleted = "completed"
	StatusError     = "error"
)

// Filing channels (送信先チャネル).
const (
	ChannelEgov = "egov"
	ChannelMyna = "myna"
)

// Filing types (届出種別).
const (
	FilingHealthInsuranceAcquire  = "health_insurance_acquire"
	FilingHealthInsuranceLose     = "health_insurance_lose"
	FilingPensionCalc             = "pension_calc"
	FilingPensionChange           = "pension_change"
	FilingEmploymentInsuranceAcq  = "employment_insurance_acquire"
	FilingEmploymentInsuranceLose = "employment_insurance_lose"
	FilingEmploymentInsuranceSep  = "employment_insurance_separation"
	FilingWorkersCompReport       = "workers_comp_report"
)

// Document kinds (公文書/帳票の種別).
const (
	DocKindReceipt       = "receipt"        // 受付控
	DocKindDecision      = "decision"       // 決定通知
	DocKindReturnReason  = "return_reason"  // 返戻理由
	DocKindGeneratedForm = "generated_form" // 生成帳票
)

// Insurer kinds (提出先区分 — 協会けんぽ / 健保組合).
const (
	InsurerKyokai = "kyokai" // 協会けんぽ
	InsurerKumiai = "kumiai" // 健保組合
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// InsuranceSettings is the GORM model for insurance_settings.
//
// All statutory values (料率・等級表・判定閾値・様式バージョン) are held in JSONB
// columns so they can be updated on legal revision without code changes
// (法令値ハードコード禁止・改正追従).
type InsuranceSettings struct {
	ID                     uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID               uuid.UUID `gorm:"column:tenant_id"`
	InsurerKind            string    `gorm:"column:insurer_kind"`
	RateTableJSON          []byte    `gorm:"column:rate_table_json;type:jsonb"`
	GradeTableJSON         []byte    `gorm:"column:grade_table_json;type:jsonb"`
	JudgementThresholdJSON []byte    `gorm:"column:judgement_threshold_json;type:jsonb"`
	FormVersionJSON        []byte    `gorm:"column:form_version_json;type:jsonb"`
	CreatedAt              time.Time `gorm:"column:created_at"`
	UpdatedAt              time.Time `gorm:"column:updated_at"`
}

// TableName maps InsuranceSettings to insurance_settings.
func (InsuranceSettings) TableName() string { return "insurance_settings" }

// Filing is the GORM model for gov_filings (電子申請ジョブ).
//
// Security note: PayloadJSON holds only reference IDs and non-sensitive filing
// data. Decrypted 機微情報 (マイナンバー等) is NEVER stored in PayloadJSON or any
// other column on this table.
type Filing struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	EmployeeID     uuid.UUID  `gorm:"column:employee_id"`
	FilingType     string     `gorm:"column:filing_type"`
	Channel        string     `gorm:"column:channel"`
	Status         string     `gorm:"column:status"`
	PayloadJSON    []byte     `gorm:"column:payload_json;type:jsonb"`
	ExternalRef    *string    `gorm:"column:external_ref"`
	IdempotencyKey string     `gorm:"column:idempotency_key"`
	SubmittedAt    *time.Time `gorm:"column:submitted_at"`
	LastError      *string    `gorm:"column:last_error"`
	CreatedBy      *uuid.UUID `gorm:"column:created_by"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Filing to gov_filings.
func (Filing) TableName() string { return "gov_filings" }

// FilingDocument is the GORM model for gov_filing_documents.
//
// Security note on ContentEnc:
//   - This field holds the AES-256-GCM ciphertext of the official document body
//     (公文書本体), which may contain 機微情報.
//   - The plaintext is NEVER stored or returned to callers without the
//     filing:read_sensitive permission check.
//   - Callers that do not hold filing:read_sensitive receive a nil/cleared field.
type FilingDocument struct {
	ID       uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID uuid.UUID `gorm:"column:tenant_id"`
	FilingID uuid.UUID `gorm:"column:filing_id"`
	DocKind  string    `gorm:"column:doc_kind"`
	// ContentEnc holds the encrypted document body ciphertext.
	// Use crypto.Decrypt to obtain plaintext; only do so when the caller
	// holds filing:read_sensitive permission.
	ContentEnc     []byte    `gorm:"column:content_enc;type:bytea"`
	RetentionLabel string    `gorm:"column:retention_label"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

// TableName maps FilingDocument to gov_filing_documents.
func (FilingDocument) TableName() string { return "gov_filing_documents" }

// StatusHistory is the GORM model for gov_filing_status_history.
type StatusHistory struct {
	ID              uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID  `gorm:"column:tenant_id"`
	FilingID        uuid.UUID  `gorm:"column:filing_id"`
	FromStatus      string     `gorm:"column:from_status"`
	ToStatus        string     `gorm:"column:to_status"`
	Note            *string    `gorm:"column:note"`
	ExternalMessage *string    `gorm:"column:external_message"`
	ChangedBy       *uuid.UUID `gorm:"column:changed_by"`
	ChangedAt       time.Time  `gorm:"column:changed_at"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	UpdatedAt       time.Time  `gorm:"column:updated_at"`
}

// TableName maps StatusHistory to gov_filing_status_history.
func (StatusHistory) TableName() string { return "gov_filing_status_history" }
