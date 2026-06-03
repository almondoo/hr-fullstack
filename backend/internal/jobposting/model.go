// Package jobposting implements the ATS (Applicant Tracking System) foundation
// for ST-ATS-01: 求人票の作成・公開と求人ステータス管理.
//
// Features:
//   - 求人票 (job_postings) の CRUD と状態遷移 (draft → open → on_hold ↔ open →
//     closed)。closed は終端状態。
//   - 求人へのリクルーター/採用マネージャ割当 (同一テナント内 user に限定)。
//   - 求人への面接官割当 (多対多、job_posting_interviewers)。
//   - 公開フラグ + opaque な public_slug (連番非露出) による公開識別。
//   - 想定年収レンジ・採用予算の項目レベル権限 (ats:read_budget) 制御。
//
// job_postings は ATS 全体 (応募者・選考パイプライン・面接・オファー) のルート
// エンティティであり、後続ストーリーが (id, tenant_id) 複合FKで参照する。
//
// 法令注意: 求人情報の保存年限 (retention_label) は法定値ではないが社内規程化
// が想定されるためテナント設定値として保持する (ハードコード禁止)。最新の官公庁
// 情報・社労士/弁護士確認のうえ設定化して改正に追従すること。本実装は法的助言
// ではない。
package jobposting

import (
	"time"

	"github.com/google/uuid"
)

// Job posting status constants.
const (
	StatusDraft  = "draft"
	StatusOpen   = "open"
	StatusOnHold = "on_hold"
	StatusClosed = "closed"
)

// JobPosting is the GORM model for job_postings.
//
// Security note on financial / budget fields:
//   - SalaryRangeMin, SalaryRangeMax and HiringBudget are item-level
//     restricted: they are only exposed to callers holding ats:read_budget.
//     The service layer clears them from the returned struct otherwise.
type JobPosting struct {
	ID                  uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID            uuid.UUID  `gorm:"column:tenant_id"`
	Title               string     `gorm:"column:title"`
	Status              string     `gorm:"column:status"`
	EmploymentType      string     `gorm:"column:employment_type"`
	DepartmentID        uuid.UUID  `gorm:"column:department_id"`
	RecruiterUserID     *uuid.UUID `gorm:"column:recruiter_user_id"`
	HiringManagerUserID *uuid.UUID `gorm:"column:hiring_manager_user_id"`
	RequirementsJSON    []byte     `gorm:"column:requirements_json;type:jsonb"`
	// SalaryRangeMin/Max and HiringBudget are budget-restricted (ats:read_budget).
	SalaryRangeMin  *int64    `gorm:"column:salary_range_min"`
	SalaryRangeMax  *int64    `gorm:"column:salary_range_max"`
	HiringBudget    *int64    `gorm:"column:hiring_budget"`
	RetentionLabel  string    `gorm:"column:retention_label"`
	PublicPublished bool      `gorm:"column:public_published"`
	PublicSlug      string    `gorm:"column:public_slug"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps JobPosting to job_postings.
func (JobPosting) TableName() string { return "job_postings" }

// Interviewer is the GORM model for job_posting_interviewers.
type Interviewer struct {
	ID           uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID     uuid.UUID `gorm:"column:tenant_id"`
	JobPostingID uuid.UUID `gorm:"column:job_posting_id"`
	UserID       uuid.UUID `gorm:"column:user_id"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

// TableName maps Interviewer to job_posting_interviewers.
func (Interviewer) TableName() string { return "job_posting_interviewers" }
