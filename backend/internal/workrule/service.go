package workrule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("workrule: not found")
	ErrInvalidTransition = errors.New("workrule: invalid status transition")
	ErrForbidden         = errors.New("workrule: permission denied")
	ErrInvalidInput      = errors.New("workrule: invalid input")
)

// defaultAlertLeadDays is the fallback renewal-alert lead time (days) used only
// when no tenant workrule_settings row exists yet.  The authoritative value is
// stored in workrule_settings.agreement_alert_lead_days and is configurable so
// the system can follow operational / legal changes (legalConfigPoints).
// Statutory and operational values must NOT be hardcoded in business logic; this
// constant exists solely as a safe bootstrap default mirrored from the DB column
// default and is overridden as soon as settings are persisted.
const defaultAlertLeadDays = 60

// defaultRetentionPolicy mirrors the workrule_settings.retention_policy column
// default; used only when no settings row exists yet.  See note above.
const defaultRetentionPolicy = "5years"

// ---------------------------------------------------------------------------
// State machines
// ---------------------------------------------------------------------------

// allowedFilingTransitions defines legal labour-agreement filing-status moves.
// draft → filed → accepted.  Terminal: accepted.
var allowedFilingTransitions = map[string]map[string]bool{
	FilingStatusDraft: {
		FilingStatusFiled: true,
	},
	FilingStatusFiled: {
		FilingStatusAccepted: true,
		// allow correcting a premature filing back to draft
		FilingStatusDraft: true,
	},
}

// isFilingTransitionAllowed reports whether moving filing status current → next is valid.
func isFilingTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedFilingTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// Service provides business logic for the workrule domain.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Settings (legal/operational config — legalConfigPoints)
// ---------------------------------------------------------------------------

// UpsertSettingsInput holds fields for setting the tenant workrule config.
type UpsertSettingsInput struct {
	TenantID               uuid.UUID
	ActorID                uuid.UUID
	AgreementAlertLeadDays int
	RetentionPolicy        string
	TemplatesJSON          []byte
	IP                     *string
}

// UpsertSettings creates or updates the per-tenant workrule settings.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*Settings, error) {
	if in.AgreementAlertLeadDays < 0 || in.AgreementAlertLeadDays > 3650 {
		return nil, fmt.Errorf("%w: agreement_alert_lead_days out of range", ErrInvalidInput)
	}
	retention := in.RetentionPolicy
	if retention == "" {
		retention = defaultRetentionPolicy
	}
	templates := in.TemplatesJSON
	if len(templates) == 0 {
		templates = []byte(`{}`)
	}

	var out Settings
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		id := uuid.New()
		if err := tx.Exec(
			`INSERT INTO workrule_settings
			   (id, tenant_id, agreement_alert_lead_days, retention_policy, templates_json)
			 VALUES (?, ?, ?, ?, ?::jsonb)
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET agreement_alert_lead_days = EXCLUDED.agreement_alert_lead_days,
			       retention_policy          = EXCLUDED.retention_policy,
			       templates_json            = EXCLUDED.templates_json,
			       updated_at                = now()`,
			id, in.TenantID, in.AgreementAlertLeadDays, retention, templates,
		).Error; err != nil {
			return fmt.Errorf("workrule: upsert settings: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, agreement_alert_lead_days, retention_policy,
			        templates_json, created_at, updated_at
			 FROM workrule_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("workrule: upsert settings re-read: %w", err)
		}

		idStr := out.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "workrule_settings.updated",
			ResourceType: "workrule_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSettings returns the tenant's workrule settings, or ErrNotFound if unset.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*Settings, error) {
	var out Settings
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, agreement_alert_lead_days, retention_policy,
			        templates_json, created_at, updated_at
			 FROM workrule_settings WHERE tenant_id = ? LIMIT 1`,
			tenantID,
		).Scan(&out).Error
	})
	if err != nil {
		return nil, err
	}
	if out.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &out, nil
}

// resolveAlertLeadDays returns the tenant's configured alert lead days within
// the given transaction, falling back to defaultAlertLeadDays when no settings
// row exists.  Used to compute renewal_alert_at without hardcoding the value.
func resolveAlertLeadDays(tx *gorm.DB, tenantID uuid.UUID) (int, error) {
	var row struct {
		AgreementAlertLeadDays *int `gorm:"column:agreement_alert_lead_days"`
	}
	if err := tx.Raw(
		`SELECT agreement_alert_lead_days FROM workrule_settings
		 WHERE tenant_id = ? LIMIT 1`,
		tenantID,
	).Scan(&row).Error; err != nil {
		return 0, fmt.Errorf("workrule: resolve alert lead days: %w", err)
	}
	if row.AgreementAlertLeadDays == nil {
		return defaultAlertLeadDays, nil
	}
	return *row.AgreementAlertLeadDays, nil
}

// ---------------------------------------------------------------------------
// Work rules (就業規則ドキュメント) — LM-055
// ---------------------------------------------------------------------------

// CreateWorkRuleInput holds fields for creating a work rule document.
type CreateWorkRuleInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	Title           string
	Category        string
	RetentionPolicy string
	IP              *string
}

// CreateWorkRule creates a new work rule document (no versions yet).
func (s *Service) CreateWorkRule(ctx context.Context, in CreateWorkRuleInput) (*WorkRule, error) {
	category := in.Category
	if category == "" {
		category = "main"
	}
	retention := in.RetentionPolicy

	wr := WorkRule{
		ID:       uuid.New(),
		TenantID: in.TenantID,
		Title:    in.Title,
		Category: category,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Default the retention policy from settings when not supplied.
		if retention == "" {
			var srow struct {
				RetentionPolicy *string `gorm:"column:retention_policy"`
			}
			if err := tx.Raw(
				`SELECT retention_policy FROM workrule_settings WHERE tenant_id = ? LIMIT 1`,
				in.TenantID,
			).Scan(&srow).Error; err != nil {
				return fmt.Errorf("workrule: create rule read settings: %w", err)
			}
			if srow.RetentionPolicy != nil && *srow.RetentionPolicy != "" {
				retention = *srow.RetentionPolicy
			} else {
				retention = defaultRetentionPolicy
			}
		}
		wr.RetentionPolicy = retention

		if err := tx.Exec(
			`INSERT INTO work_rules (id, tenant_id, title, category, retention_policy)
			 VALUES (?, ?, ?, ?, ?)`,
			wr.ID, wr.TenantID, wr.Title, wr.Category, wr.RetentionPolicy,
		).Error; err != nil {
			return fmt.Errorf("workrule: create rule insert: %w", err)
		}

		idStr := wr.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "work_rule.created",
			ResourceType: "work_rule",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &wr, nil
}

// GetWorkRule fetches a work rule by ID within the tenant.
func (s *Service) GetWorkRule(ctx context.Context, tenantID, id uuid.UUID) (*WorkRule, error) {
	var wr WorkRule
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, title, category, current_version_id,
			        retention_policy, created_at, updated_at
			 FROM work_rules WHERE id = ? AND tenant_id = ? LIMIT 1`,
			id, tenantID,
		).Scan(&wr).Error
	})
	if err != nil {
		return nil, err
	}
	if wr.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &wr, nil
}

// ListWorkRules returns all work rules for a tenant, optionally filtered by category.
func (s *Service) ListWorkRules(ctx context.Context, tenantID uuid.UUID, category string) ([]WorkRule, error) {
	var rules []WorkRule
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, title, category, current_version_id,
		             retention_policy, created_at, updated_at
		      FROM work_rules WHERE tenant_id = ?`
		args := []any{tenantID}
		if category != "" {
			q += ` AND category = ?`
			args = append(args, category)
		}
		q += ` ORDER BY title`
		return tx.Raw(q, args...).Scan(&rules).Error
	})
	if err != nil {
		return nil, err
	}
	return rules, nil
}

// ---------------------------------------------------------------------------
// Work rule versions (改定履歴・現行版) — LM-055 / CORE-009 / CMP-009
// ---------------------------------------------------------------------------

// CreateVersionInput holds fields for creating a new work rule version (draft).
type CreateVersionInput struct {
	TenantID             uuid.UUID
	ActorID              uuid.UUID
	WorkRuleID           uuid.UUID
	RevisedOn            *time.Time
	RevisionReason       string
	DocumentRef          *string
	RequiresExpertReview bool
	IP                   *string
}

// CreateVersion adds a new draft version to a work rule.  The version number is
// derived as max(existing)+1 under a row lock to avoid TOCTOU races.  The new
// version starts in "draft" status and is NOT yet the current version.
func (s *Service) CreateVersion(ctx context.Context, in CreateVersionInput) (*WorkRuleVersion, error) {
	var v WorkRuleVersion
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the parent work rule belongs to this tenant and lock that row so
		// the version-number computation is serialised per rule (TOCTOU-safe).
		// FOR UPDATE cannot be combined with an aggregate, so we lock the rule row
		// by selecting its id.
		var lockedRule struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Raw(
			`SELECT id FROM work_rules
			 WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.WorkRuleID, in.TenantID,
		).Scan(&lockedRule).Error; err != nil {
			return fmt.Errorf("workrule: create version lock rule: %w", err)
		}
		if lockedRule.ID == uuid.Nil {
			return ErrNotFound
		}

		// Next version number = max(version)+1 for this rule.
		var maxRow struct {
			MaxVersion *int `gorm:"column:max_version"`
		}
		if err := tx.Raw(
			`SELECT MAX(version) AS max_version FROM work_rule_versions
			 WHERE work_rule_id = ? AND tenant_id = ?`,
			in.WorkRuleID, in.TenantID,
		).Scan(&maxRow).Error; err != nil {
			return fmt.Errorf("workrule: create version compute number: %w", err)
		}
		next := 1
		if maxRow.MaxVersion != nil {
			next = *maxRow.MaxVersion + 1
		}

		v = WorkRuleVersion{
			ID:                   uuid.New(),
			TenantID:             in.TenantID,
			WorkRuleID:           in.WorkRuleID,
			Version:              next,
			RevisedOn:            in.RevisedOn,
			RevisionReason:       in.RevisionReason,
			DocumentRef:          in.DocumentRef,
			Status:               VersionStatusDraft,
			RequiresExpertReview: in.RequiresExpertReview,
			CreatedBy:            &in.ActorID,
		}
		if err := tx.Exec(
			`INSERT INTO work_rule_versions
			   (id, tenant_id, work_rule_id, version, revised_on, revision_reason,
			    document_ref, status, requires_expert_review, created_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			v.ID, v.TenantID, v.WorkRuleID, v.Version, v.RevisedOn, v.RevisionReason,
			v.DocumentRef, v.Status, v.RequiresExpertReview, v.CreatedBy,
		).Error; err != nil {
			return fmt.Errorf("workrule: create version insert: %w", err)
		}

		idStr := v.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "work_rule_version.created",
			ResourceType: "work_rule_version",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// PublishVersionInput holds fields for publishing a draft version.
type PublishVersionInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	VersionID uuid.UUID
	IP        *string
}

// PublishVersion publishes a draft version: it marks any currently-published
// version of the same rule as superseded, sets this version to published, and
// updates work_rules.current_version_id so the current version is uniquely
// identifiable.  All writes occur in one transaction.
func (s *Service) PublishVersion(ctx context.Context, in PublishVersionInput) (*WorkRuleVersion, error) {
	var v WorkRuleVersion
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Read the target version (lock it) and its parent rule.
		var target struct {
			WorkRuleID uuid.UUID `gorm:"column:work_rule_id"`
			Status     string    `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT work_rule_id, status FROM work_rule_versions
			 WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.VersionID, in.TenantID,
		).Scan(&target).Error; err != nil {
			return fmt.Errorf("workrule: publish read version: %w", err)
		}
		if target.WorkRuleID == uuid.Nil {
			return ErrNotFound
		}
		// Only a draft may be published.
		if target.Status != VersionStatusDraft {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, target.Status, VersionStatusPublished)
		}

		// Supersede any currently-published version of the same rule.
		if err := tx.Exec(
			`UPDATE work_rule_versions
			 SET status = ?, updated_at = now()
			 WHERE work_rule_id = ? AND tenant_id = ? AND status = ?`,
			VersionStatusSuperseded, target.WorkRuleID, in.TenantID, VersionStatusPublished,
		).Error; err != nil {
			return fmt.Errorf("workrule: publish supersede old: %w", err)
		}

		// Publish the target version.
		res := tx.Exec(
			`UPDATE work_rule_versions
			 SET status = ?, published_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			VersionStatusPublished, in.VersionID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("workrule: publish set published: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		// Point the rule at the new current version.
		if err := tx.Exec(
			`UPDATE work_rules SET current_version_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.VersionID, target.WorkRuleID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("workrule: publish update current: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, work_rule_id, version, revised_on, revision_reason,
			        document_ref, status, published_at, requires_expert_review,
			        created_by, created_at, updated_at
			 FROM work_rule_versions WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.VersionID, in.TenantID,
		).Scan(&v).Error; err != nil {
			return fmt.Errorf("workrule: publish re-read: %w", err)
		}

		idStr := in.VersionID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "work_rule_version.published",
			ResourceType: "work_rule_version",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// ListVersions returns the version history of a work rule (newest first).
func (s *Service) ListVersions(ctx context.Context, tenantID, workRuleID uuid.UUID) ([]WorkRuleVersion, error) {
	var versions []WorkRuleVersion
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, work_rule_id, version, revised_on, revision_reason,
			        document_ref, status, published_at, requires_expert_review,
			        created_by, created_at, updated_at
			 FROM work_rule_versions
			 WHERE work_rule_id = ? AND tenant_id = ?
			 ORDER BY version DESC`,
			workRuleID, tenantID,
		).Scan(&versions).Error
	})
	if err != nil {
		return nil, err
	}
	return versions, nil
}

// ---------------------------------------------------------------------------
// Acknowledgements (周知/同意) — LM-055
// ---------------------------------------------------------------------------

// AcknowledgeInput holds fields for recording an employee read/consent.
type AcknowledgeInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	VersionID uuid.UUID
	// EmployeeID is the employee whose read/consent is being recorded.
	EmployeeID uuid.UUID
	Consent    string
	IP         *string
}

// Acknowledge records (or updates) an employee's read/consent for a published
// version.  Only a published version can be acknowledged.  Re-acknowledging
// upgrades read → agreed via ON CONFLICT.
func (s *Service) Acknowledge(ctx context.Context, in AcknowledgeInput) (*Acknowledgement, error) {
	consent := in.Consent
	if consent == "" {
		consent = ConsentRead
	}
	if consent != ConsentRead && consent != ConsentAgreed {
		return nil, fmt.Errorf("%w: consent must be read or agreed", ErrInvalidInput)
	}

	var ack Acknowledgement
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the version exists in this tenant and is published.
		var vrow struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM work_rule_versions
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.VersionID, in.TenantID,
		).Scan(&vrow).Error; err != nil {
			return fmt.Errorf("workrule: acknowledge read version: %w", err)
		}
		if vrow.Status == "" {
			return ErrNotFound
		}
		if vrow.Status != VersionStatusPublished {
			return fmt.Errorf("%w: cannot acknowledge a %s version", ErrInvalidTransition, vrow.Status)
		}

		// Verify the employee belongs to this tenant (defence-in-depth on top of
		// the composite FK).
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("workrule: acknowledge verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		ack = Acknowledgement{
			ID:                uuid.New(),
			TenantID:          in.TenantID,
			WorkRuleVersionID: in.VersionID,
			EmployeeID:        in.EmployeeID,
			Consent:           consent,
		}
		if err := tx.Exec(
			`INSERT INTO work_rule_acknowledgements
			   (id, tenant_id, work_rule_version_id, employee_id, consent, acknowledged_at)
			 VALUES (?, ?, ?, ?, ?, now())
			 ON CONFLICT (work_rule_version_id, employee_id) DO UPDATE
			   SET consent         = EXCLUDED.consent,
			       acknowledged_at = now(),
			       updated_at      = now()`,
			ack.ID, ack.TenantID, ack.WorkRuleVersionID, ack.EmployeeID, ack.Consent,
		).Error; err != nil {
			return fmt.Errorf("workrule: acknowledge insert: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, work_rule_version_id, employee_id, consent,
			        acknowledged_at, created_at, updated_at
			 FROM work_rule_acknowledgements
			 WHERE work_rule_version_id = ? AND employee_id = ? AND tenant_id = ? LIMIT 1`,
			in.VersionID, in.EmployeeID, in.TenantID,
		).Scan(&ack).Error; err != nil {
			return fmt.Errorf("workrule: acknowledge re-read: %w", err)
		}

		idStr := ack.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "work_rule.acknowledged",
			ResourceType: "work_rule_acknowledgement",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &ack, nil
}

// ListAcknowledgements returns all acknowledgements for a version.
func (s *Service) ListAcknowledgements(ctx context.Context, tenantID, versionID uuid.UUID) ([]Acknowledgement, error) {
	var acks []Acknowledgement
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, work_rule_version_id, employee_id, consent,
			        acknowledged_at, created_at, updated_at
			 FROM work_rule_acknowledgements
			 WHERE work_rule_version_id = ? AND tenant_id = ?
			 ORDER BY acknowledged_at`,
			versionID, tenantID,
		).Scan(&acks).Error
	})
	if err != nil {
		return nil, err
	}
	return acks, nil
}

// ListUnacknowledgedEmployees returns the IDs of active employees who have NOT
// acknowledged the given published version — the 未同意者の可視化 requirement.
func (s *Service) ListUnacknowledgedEmployees(ctx context.Context, tenantID, versionID uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Confirm the version belongs to the tenant first.
		var vCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM work_rule_versions WHERE id = ? AND tenant_id = ?`,
			versionID, tenantID,
		).Scan(&vCount).Error; err != nil {
			return fmt.Errorf("workrule: unacked verify version: %w", err)
		}
		if vCount == 0 {
			return ErrNotFound
		}

		var rows []struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		if err := tx.Raw(
			`SELECT e.id AS id FROM employees e
			 WHERE e.tenant_id = ? AND e.status = 'active'
			   AND NOT EXISTS (
			     SELECT 1 FROM work_rule_acknowledgements a
			     WHERE a.work_rule_version_id = ?
			       AND a.tenant_id = ?
			       AND a.employee_id = e.id
			   )
			 ORDER BY e.id`,
			tenantID, versionID, tenantID,
		).Scan(&rows).Error; err != nil {
			return fmt.Errorf("workrule: unacked query: %w", err)
		}
		ids = make([]uuid.UUID, len(rows))
		for i := range rows {
			ids[i] = rows[i].ID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ---------------------------------------------------------------------------
// Labour agreement documents (労使協定/36協定) — LM-056
// ---------------------------------------------------------------------------

// CreateAgreementInput holds fields for creating a labour agreement document.
type CreateAgreementInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	Title     string
	Type      string
	ValidFrom time.Time
	ValidTo   time.Time
	// LinkedLaborAgreementID references attendance.labor_agreements(id) — the //nolint:misspell // DB table name is schema contract
	// source of truth for 36協定 upper-limit values (no duplication here).
	LinkedLaborAgreementID *uuid.UUID
	DocumentRef            *string
	IP                     *string
}

// CreateAgreement creates a draft labour agreement document.  The renewal alert
// date is computed from valid_to minus the tenant's configured lead time.  When
// the type is article36 and LinkedLaborAgreementID is provided, the link is
// validated to belong to the same tenant (its limit values stay the source of
// truth — never copied).
func (s *Service) CreateAgreement(ctx context.Context, in CreateAgreementInput) (*LaborAgreementDocument, error) {
	agType := in.Type
	if agType == "" {
		agType = AgreementTypeOther
	}
	if agType != AgreementTypeArticle36 && agType != AgreementTypeOther {
		return nil, fmt.Errorf("%w: invalid agreement_type", ErrInvalidInput)
	}
	if in.ValidTo.Before(in.ValidFrom) {
		return nil, fmt.Errorf("%w: valid_to before valid_from", ErrInvalidInput)
	}

	doc := LaborAgreementDocument{
		ID:                     uuid.New(),
		TenantID:               in.TenantID,
		Title:                  in.Title,
		AgreementType:          agType,
		Version:                1,
		ValidFrom:              in.ValidFrom,
		ValidTo:                in.ValidTo,
		FilingStatus:           FilingStatusDraft,
		DocumentRef:            in.DocumentRef,
		LinkedLaborAgreementID: in.LinkedLaborAgreementID,
		CreatedBy:              &in.ActorID,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Validate the cross-package link belongs to the same tenant (RLS is on
		// labour_agreements too, so this read is tenant-scoped).
		if in.LinkedLaborAgreementID != nil {
			var laCount int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM labor_agreements WHERE id = ? AND tenant_id = ?`, //nolint:misspell // DB table name is schema contract
				*in.LinkedLaborAgreementID, in.TenantID,
			).Scan(&laCount).Error; err != nil {
				return fmt.Errorf("workrule: create agreement verify link: %w", err)
			}
			if laCount == 0 {
				return ErrNotFound
			}
		}

		leadDays, err := resolveAlertLeadDays(tx, in.TenantID)
		if err != nil {
			return err
		}
		alertAt := in.ValidTo.AddDate(0, 0, -leadDays)
		doc.RenewalAlertAt = &alertAt

		if err := tx.Exec(
			"INSERT INTO labor_agreement_documents\n"+ //nolint:misspell // DB table name is schema contract
				"   (id, tenant_id, title, agreement_type, version, valid_from, valid_to,\n"+
				"    filing_status, document_ref, renewal_alert_at, created_by, linked_labor_agreement_id)\n"+ //nolint:misspell // DB column name is schema contract
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			doc.ID, doc.TenantID, doc.Title, doc.AgreementType, doc.Version,
			doc.ValidFrom, doc.ValidTo, doc.FilingStatus, doc.DocumentRef,
			doc.RenewalAlertAt, doc.CreatedBy, doc.LinkedLaborAgreementID,
		).Error; err != nil {
			return fmt.Errorf("workrule: create agreement insert: %w", err)
		}

		idStr := doc.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "labor_agreement.created",  //nolint:misspell // audit event name is schema contract
			ResourceType: "labor_agreement_document", //nolint:misspell // audit resource type is schema contract
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// GetAgreement fetches a labour agreement document by ID within the tenant.
func (s *Service) GetAgreement(ctx context.Context, tenantID, id uuid.UUID) (*LaborAgreementDocument, error) {
	var doc LaborAgreementDocument
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, title, agreement_type, version, valid_from, valid_to,\n"+
				"        filing_status, filed_at, accepted_at, document_ref,\n"+
				"        linked_labor_agreement_id, renewal_alert_at, created_by,\n"+ //nolint:misspell // DB column name is schema contract
				"        created_at, updated_at FROM labor_agreement_documents WHERE id = ? AND tenant_id = ? LIMIT 1", //nolint:misspell // DB table name is schema contract
			id, tenantID,
		).Scan(&doc).Error
	})
	if err != nil {
		return nil, err
	}
	if doc.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &doc, nil
}

// ListAgreements returns labour agreement documents for a tenant, optionally
// filtered by agreement type.
func (s *Service) ListAgreements(ctx context.Context, tenantID uuid.UUID, agreementType string) ([]LaborAgreementDocument, error) {
	var docs []LaborAgreementDocument
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := "SELECT id, tenant_id, title, agreement_type, version, valid_from, valid_to,\n" +
			"             filing_status, filed_at, accepted_at, document_ref,\n" +
			"             linked_labor_agreement_id, renewal_alert_at, created_by,\n" + //nolint:misspell // DB column name is schema contract
			"             created_at, updated_at FROM labor_agreement_documents WHERE tenant_id = ?" //nolint:misspell // DB table name is schema contract
		args := []any{tenantID}
		if agreementType != "" {
			q += ` AND agreement_type = ?`
			args = append(args, agreementType)
		}
		q += ` ORDER BY valid_to DESC`
		return tx.Raw(q, args...).Scan(&docs).Error
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// UpdateFilingStatusInput holds fields for a filing-status transition.
type UpdateFilingStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	Status   string
	IP       *string
}

// UpdateFilingStatus transitions a labour agreement's electronic filing status
// (draft → filed → accepted).  Only allow-listed transitions are accepted.
// Entering "filed" stamps filed_at; entering "accepted" stamps accepted_at.
func (s *Service) UpdateFilingStatus(ctx context.Context, in UpdateFilingStatusInput) (*LaborAgreementDocument, error) {
	var doc LaborAgreementDocument
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:filing_status"`
		}
		if err := tx.Raw(
			`SELECT filing_status FROM labor_agreement_documents WHERE id = ? AND tenant_id = ? FOR UPDATE`, //nolint:misspell // DB table name is schema contract
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("workrule: filing status read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isFilingTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		switch in.Status {
		case FilingStatusFiled:
			if err := tx.Exec(
				`UPDATE labor_agreement_documents SET filing_status = ?, filed_at = now(), updated_at = now() WHERE id = ? AND tenant_id = ?`, //nolint:misspell // DB table name is schema contract
				in.Status, in.ID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("workrule: filing status set filed: %w", err)
			}
		case FilingStatusAccepted:
			if err := tx.Exec(
				`UPDATE labor_agreement_documents SET filing_status = ?, accepted_at = now(), updated_at = now() WHERE id = ? AND tenant_id = ?`, //nolint:misspell // DB table name is schema contract
				in.Status, in.ID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("workrule: filing status set accepted: %w", err)
			}
		default: // draft (correction)
			if err := tx.Exec(
				`UPDATE labor_agreement_documents SET filing_status = ?, filed_at = NULL, accepted_at = NULL, updated_at = now() WHERE id = ? AND tenant_id = ?`, //nolint:misspell // DB table name is schema contract
				in.Status, in.ID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("workrule: filing status set draft: %w", err)
			}
		}

		if err := tx.Raw(
			"SELECT id, tenant_id, title, agreement_type, version, valid_from, valid_to,\n"+
				"        filing_status, filed_at, accepted_at, document_ref,\n"+
				"        linked_labor_agreement_id, renewal_alert_at, created_by,\n"+ //nolint:misspell // DB column name is schema contract
				"        created_at, updated_at FROM labor_agreement_documents WHERE id = ? AND tenant_id = ? LIMIT 1", //nolint:misspell // DB table name is schema contract
			in.ID, in.TenantID,
		).Scan(&doc).Error; err != nil {
			return fmt.Errorf("workrule: filing status re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "labor_agreement.filing_status_updated", //nolint:misspell // audit event name is schema contract
			ResourceType: "labor_agreement_document",              //nolint:misspell // audit resource type is schema contract
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// ListExpiringAgreements returns labour agreement documents whose renewal alert
// date is on or before asOf — the basis for renewal-alert generation that the
// notification slice (ST-FND-09) consumes.
func (s *Service) ListExpiringAgreements(ctx context.Context, tenantID uuid.UUID, asOf time.Time) ([]LaborAgreementDocument, error) {
	var docs []LaborAgreementDocument
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			"SELECT id, tenant_id, title, agreement_type, version, valid_from, valid_to,\n"+
				"        filing_status, filed_at, accepted_at, document_ref,\n"+
				"        linked_labor_agreement_id, renewal_alert_at, created_by,\n"+ //nolint:misspell // DB column name is schema contract
				"        created_at, updated_at FROM labor_agreement_documents\n"+ //nolint:misspell // DB table name is schema contract
				" WHERE tenant_id = ? AND renewal_alert_at IS NOT NULL AND renewal_alert_at <= ?\n"+
				" ORDER BY renewal_alert_at",
			tenantID, asOf,
		).Scan(&docs).Error
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// LinkedLimits holds the 36協定 upper-limit values read (not copied) from the
// linked attendance.labor_agreements row (American-spelled DB table). //nolint:misspell // DB table name is schema contract
// This proves the linkage works while keeping the source of truth in the attendance package.
type LinkedLimits struct {
	LaborAgreementID           uuid.UUID //nolint:misspell // field name matches DB table (schema contract)
	MonthlyLimitMinutes        int
	YearlyLimitMinutes         int
	SpecialClause              bool
	SpecialMonthlyLimitMinutes *int
}

// GetLinkedLimits reads the upper-limit values from the attendance.labor_agreements //nolint:misspell // DB table name is schema contract
// row linked to the given agreement document.  Returns ErrNotFound when the
// document has no link or the linked row is absent.  The values are NOT stored
// in this package — they remain owned by attendance (source of truth).
func (s *Service) GetLinkedLimits(ctx context.Context, tenantID, docID uuid.UUID) (*LinkedLimits, error) {
	var out *LinkedLimits
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var linkRow struct {
			LinkedLaborAgreementID *uuid.UUID `gorm:"column:linked_labor_agreement_id"` //nolint:misspell // DB column name is schema contract
		}
		if err := tx.Raw(
			`SELECT linked_labor_agreement_id FROM labor_agreement_documents WHERE id = ? AND tenant_id = ? LIMIT 1`, //nolint:misspell // DB table/column names are schema contract
			docID, tenantID,
		).Scan(&linkRow).Error; err != nil {
			return fmt.Errorf("workrule: get linked limits read doc: %w", err)
		}
		if linkRow.LinkedLaborAgreementID == nil {
			return ErrNotFound
		}

		var la struct {
			ID                         uuid.UUID `gorm:"column:id"`
			MonthlyLimitMinutes        int       `gorm:"column:monthly_limit_minutes"`
			YearlyLimitMinutes         int       `gorm:"column:yearly_limit_minutes"`
			SpecialClause              bool      `gorm:"column:special_clause"`
			SpecialMonthlyLimitMinutes *int      `gorm:"column:special_monthly_limit_minutes"`
		}
		if err := tx.Raw(
			`SELECT id, monthly_limit_minutes, yearly_limit_minutes, special_clause,
			        special_monthly_limit_minutes FROM labor_agreements WHERE id = ? AND tenant_id = ? LIMIT 1`, //nolint:misspell // DB table name is schema contract
			*linkRow.LinkedLaborAgreementID, tenantID,
		).Scan(&la).Error; err != nil {
			return fmt.Errorf("workrule: get linked limits read agreement: %w", err)
		}
		if la.ID == uuid.Nil {
			return ErrNotFound
		}
		out = &LinkedLimits{
			LaborAgreementID:           la.ID,
			MonthlyLimitMinutes:        la.MonthlyLimitMinutes,
			YearlyLimitMinutes:         la.YearlyLimitMinutes,
			SpecialClause:              la.SpecialClause,
			SpecialMonthlyLimitMinutes: la.SpecialMonthlyLimitMinutes,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
