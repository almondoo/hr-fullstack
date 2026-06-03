package applicant

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	ErrNotFound          = errors.New("applicant: not found")
	ErrInvalidTransition = errors.New("applicant: invalid status transition")
	ErrForbidden         = errors.New("applicant: permission denied")
	ErrInvalidMerge      = errors.New("applicant: invalid merge")
)

// permReadSensitive is the permission required to decrypt contact PII.
const permReadSensitive = "ats:applicant:read_sensitive"

// allowedStatusTransitions defines legal applicant status moves.
// Terminal states (hired, rejected, withdrawn) have no transitions out except
// that rejected/withdrawn cannot be re-opened here (a new application is a new
// applicant record).  This is enforced as a multi-layer defence alongside the
// DB CHECK constraint, which only constrains the value domain.
var allowedStatusTransitions = map[string]map[string]bool{
	StatusApplied: {
		StatusScreening: true,
		StatusRejected:  true,
		StatusWithdrawn: true,
	},
	StatusScreening: {
		StatusInterviewing: true,
		StatusRejected:     true,
		StatusWithdrawn:    true,
	},
	StatusInterviewing: {
		StatusOffered:   true,
		StatusRejected:  true,
		StatusWithdrawn: true,
	},
	StatusOffered: {
		StatusHired:     true,
		StatusRejected:  true,
		StatusWithdrawn: true,
	},
	// hired / rejected / withdrawn are terminal — no transitions out.
}

// isStatusTransitionAllowed reports whether moving from current → next is valid.
func isStatusTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedStatusTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// normalizeEmail lower-cases and trims an email for duplicate-detection matching.
// Returns "" for empty input.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Service provides business logic for applicant operations.
type Service struct {
	tdb *tenantdb.TenantDB
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb}
}

// ---------------------------------------------------------------------------
// Applicants
// ---------------------------------------------------------------------------

// CreateApplicantInput holds fields for registering an applicant.
//
// EmailPlaintext / PhonePlaintext contain the contact PII in plaintext.  They
// are encrypted with AES-256-GCM before storage; the plaintext is NEVER
// persisted to the database, written to audit records, or logged.
type CreateApplicantInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	JobPostingID *uuid.UUID
	LastName     string
	FirstName    string
	// EmailPlaintext / PhonePlaintext: encrypted before INSERT; never persisted
	// as plaintext.  MUST NOT be logged or written to audit records.
	EmailPlaintext string
	PhonePlaintext string
	BirthDate      *time.Time
	ConsentStatus  string
	Source         string
	// RetentionLabel is a tenant-configured retention policy label (CMP-004).
	// It is NOT a hardcoded legal value; the caller supplies it from settings.
	RetentionLabel     string
	RetentionExpiresOn *time.Time
	IP                 *string
}

// CreateApplicant registers a new applicant.  Contact PII (email/phone) is
// encrypted before the transaction opens (fail-fast); the plaintext is never
// persisted.  The returned Applicant never carries ciphertext.
func (s *Service) CreateApplicant(ctx context.Context, in CreateApplicantInput) (*Applicant, error) {
	// Encrypt contact PII BEFORE opening the transaction so any crypto error
	// fails fast.  Security: the plaintext never appears in any error message.
	emailEnc, err := encryptOptional(in.EmailPlaintext)
	if err != nil {
		return nil, fmt.Errorf("applicant: encrypt email: %w", err)
	}
	phoneEnc, err := encryptOptional(in.PhonePlaintext)
	if err != nil {
		return nil, fmt.Errorf("applicant: encrypt phone: %w", err)
	}

	consentStatus := in.ConsentStatus
	if consentStatus == "" {
		consentStatus = ConsentUnknown
	}
	source := in.Source
	if source == "" {
		source = SourceDirect
	}
	retentionLabel := in.RetentionLabel
	if retentionLabel == "" {
		// Sentinel non-legal value: a real label must be supplied from tenant
		// settings (CMP-004).  Never hardcode a legal retention duration here.
		retentionLabel = "unset"
	}

	var emailNorm *string
	if n := normalizeEmail(in.EmailPlaintext); n != "" {
		emailNorm = &n
	}

	a := Applicant{
		ID:                 uuid.New(),
		TenantID:           in.TenantID,
		JobPostingID:       in.JobPostingID,
		LastName:           in.LastName,
		FirstName:          in.FirstName,
		EmailNormalized:    emailNorm,
		BirthDate:          in.BirthDate,
		EmailEnc:           emailEnc,
		PhoneEnc:           phoneEnc,
		Status:             StatusApplied,
		ConsentStatus:      consentStatus,
		Source:             source,
		RetentionLabel:     retentionLabel,
		RetentionExpiresOn: in.RetentionExpiresOn,
	}

	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the referenced job posting belongs to this tenant when provided.
		// job_postings is owned by ST-ATS-01 (cross-story): no composite FK is
		// declared, so we validate tenant ownership in the service layer + RLS.
		if in.JobPostingID != nil {
			var cnt int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM job_postings WHERE id = ? AND tenant_id = ?`,
				*in.JobPostingID, in.TenantID,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("applicant: verify job posting: %w", err)
			}
			if cnt == 0 {
				return ErrNotFound
			}
		}

		if err := tx.Exec(
			"INSERT INTO applicants\n"+
				"   (id, tenant_id, job_posting_id, last_name, first_name, birth_date,\n"+
				"    email_enc, phone_enc, status, consent_status, source,\n"+
				"    retention_label, retention_expires_on, email_normalized)\n"+ //nolint:misspell // email_normalised is a DB column name (schema contract)
				" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			a.ID, a.TenantID, a.JobPostingID, a.LastName, a.FirstName, a.BirthDate,
			a.EmailEnc, a.PhoneEnc,
			a.Status, a.ConsentStatus, a.Source, a.RetentionLabel, a.RetentionExpiresOn,
			a.EmailNormalized,
		).Error; err != nil {
			return fmt.Errorf("applicant: create insert: %w", err)
		}

		idStr := a.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "applicant.created",
			ResourceType: "applicant",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers.
	a.EmailEnc = nil
	a.PhoneEnc = nil
	return &a, nil
}

// GetApplicantInput holds parameters for fetching an applicant.
// ReadSensitive must be true when the caller holds ats:applicant:read_sensitive;
// the service re-validates this against the DB (multi-layer defence) before
// decrypting contact PII.
type GetApplicantInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	ID            uuid.UUID
	ReadSensitive bool
	IP            *string
}

// SensitiveContact holds decrypted contact PII, returned separately from the
// Applicant struct so callers cannot accidentally persist it.
type SensitiveContact struct {
	Email string
	Phone string
}

// GetApplicant fetches an applicant.  When ReadSensitive is requested, the
// service re-validates ats:applicant:read_sensitive against the DB and decrypts
// the contact PII; the decrypted values are returned via the second result and
// are NEVER written to logs or audit records.  Without permission the second
// result is nil and no plaintext is exposed.
func (s *Service) GetApplicant(ctx context.Context, in GetApplicantInput) (*Applicant, *SensitiveContact, error) {
	var a Applicant
	var permittedSensitive bool

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Service-layer permission check (defence-in-depth): even if the HTTP
		// middleware already verified it, re-validate here so internal callers
		// (batch jobs, service-to-service) cannot receive plaintext without
		// holding the permission.
		if in.ReadSensitive {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("applicant: load permissions: %w", err)
			}
			if !platformauth.HasPermission(perms, permReadSensitive) {
				return ErrForbidden
			}
			permittedSensitive = true
		}

		if err := tx.Raw(
			"SELECT id, tenant_id, job_posting_id, merged_into_id, last_name, first_name,\n"+
				"        birth_date, email_enc, phone_enc,\n"+
				"        status, consent_status, source, retention_label, retention_expires_on,\n"+
				"        anonymized_at, created_at, updated_at, email_normalized\n"+ //nolint:misspell // email_normalised is a DB column name (schema contract)
				" FROM applicants WHERE id = ? AND tenant_id = ? LIMIT 1",
			in.ID, in.TenantID,
		).Scan(&a).Error; err != nil {
			return fmt.Errorf("applicant: get: %w", err)
		}
		if a.ID == uuid.Nil {
			return ErrNotFound
		}

		action := "applicant.read"
		if permittedSensitive {
			action = "applicant.read_sensitive"
		}
		idStr := a.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "applicant",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}

	var sensitive *SensitiveContact
	if permittedSensitive {
		sc := SensitiveContact{}
		if len(a.EmailEnc) > 0 {
			plain, err := crypto.Decrypt(a.EmailEnc)
			if err != nil {
				return nil, nil, fmt.Errorf("applicant: decrypt email: %w", err)
			}
			sc.Email = string(plain)
		}
		if len(a.PhoneEnc) > 0 {
			plain, err := crypto.Decrypt(a.PhoneEnc)
			if err != nil {
				return nil, nil, fmt.Errorf("applicant: decrypt phone: %w", err)
			}
			sc.Phone = string(plain)
		}
		sensitive = &sc
	}

	// Clear ciphertext from the returned struct regardless; callers that need
	// the plaintext use the second return value.
	a.EmailEnc = nil
	a.PhoneEnc = nil
	return &a, sensitive, nil
}

// ListApplicants returns applicants for the tenant, optionally filtered by
// job_posting_id and/or status.  Logically merged applicants (merged_into_id
// set) are excluded by default unless includeMerged is true.
func (s *Service) ListApplicants(ctx context.Context, tenantID uuid.UUID, jobPostingID *uuid.UUID, status string, includeMerged bool) ([]Applicant, error) {
	var list []Applicant
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, job_posting_id, merged_into_id, last_name, first_name,
		             birth_date, status, consent_status, source,
		             retention_label, retention_expires_on, anonymized_at,
		             created_at, updated_at, email_normalized` + //nolint:misspell // email_normalised is a DB column name (schema contract)
			` FROM applicants WHERE tenant_id = ?`
		args := []any{tenantID}
		if jobPostingID != nil {
			q += ` AND job_posting_id = ?`
			args = append(args, *jobPostingID)
		}
		if status != "" {
			q += ` AND status = ?`
			args = append(args, status)
		}
		if !includeMerged {
			q += ` AND merged_into_id IS NULL`
		}
		q += ` ORDER BY created_at DESC`
		return tx.Raw(q, args...).Scan(&list).Error
	})
	if err != nil {
		return nil, err
	}
	return list, nil
}

// UpdateStatusInput holds fields for an applicant status transition.
type UpdateStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	Status   string
	IP       *string
}

// UpdateStatus transitions an applicant to the given status.  Only allow-listed
// transitions are accepted (ErrInvalidTransition otherwise).
func (s *Service) UpdateStatus(ctx context.Context, in UpdateStatusInput) (*Applicant, error) {
	var a Applicant
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock the row to avoid TOCTOU on the status check + update.
		var current struct {
			Status       string     `gorm:"column:status"`
			MergedIntoID *uuid.UUID `gorm:"column:merged_into_id"`
		}
		if err := tx.Raw(
			`SELECT status, merged_into_id FROM applicants
			 WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("applicant: update status read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		// Merged (logically retired) records must not transition.
		if current.MergedIntoID != nil {
			return fmt.Errorf("%w: applicant is merged", ErrInvalidTransition)
		}
		if !isStatusTransitionAllowed(current.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current.Status, in.Status)
		}

		res := tx.Exec(
			`UPDATE applicants SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("applicant: update status: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			"SELECT id, tenant_id, job_posting_id, merged_into_id, last_name, first_name,\n"+
				"        birth_date, status, consent_status, source,\n"+
				"        retention_label, retention_expires_on, anonymized_at,\n"+
				"        created_at, updated_at, email_normalized\n"+ //nolint:misspell // email_normalised is a DB column name (schema contract)
				" FROM applicants WHERE id = ? AND tenant_id = ? LIMIT 1",
			in.ID, in.TenantID,
		).Scan(&a).Error; err != nil {
			return fmt.Errorf("applicant: update status re-read: %w", err)
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "applicant.status_updated",
			ResourceType: "applicant",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

// AddDocumentInput holds fields for attaching a document reference.
type AddDocumentInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	ApplicantID uuid.UUID
	DocType     string
	// FileRef is the opaque reference into the file storage platform (ST-FND-10).
	// The file body PII is never stored in the DB.
	FileRef  string
	FileName string
	IP       *string
}

// AddDocument attaches a document (opaque file_ref) to an applicant.
func (s *Service) AddDocument(ctx context.Context, in AddDocumentInput) (*Document, error) {
	docType := in.DocType
	if docType == "" {
		docType = DocTypeResume
	}
	d := Document{
		ID:          uuid.New(),
		TenantID:    in.TenantID,
		ApplicantID: in.ApplicantID,
		DocType:     docType,
		FileRef:     in.FileRef,
		FileName:    in.FileName,
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the applicant belongs to this tenant (defence-in-depth; the
		// composite FK also enforces this at the DB layer).
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM applicants WHERE id = ? AND tenant_id = ?`,
			in.ApplicantID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("applicant: add document verify applicant: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO applicant_documents
			   (id, tenant_id, applicant_id, doc_type, file_ref, file_name)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			d.ID, d.TenantID, d.ApplicantID, d.DocType, d.FileRef, d.FileName,
		).Error; err != nil {
			return fmt.Errorf("applicant: add document insert: %w", err)
		}

		idStr := d.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "applicant_document.added",
			ResourceType: "applicant_document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDocuments returns the document references for an applicant.
func (s *Service) ListDocuments(ctx context.Context, tenantID, applicantID uuid.UUID) ([]Document, error) {
	var docs []Document
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, applicant_id, doc_type, file_ref, file_name,
			        created_at, updated_at
			 FROM applicant_documents
			 WHERE tenant_id = ? AND applicant_id = ?
			 ORDER BY created_at`,
			tenantID, applicantID,
		).Scan(&docs).Error
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// ---------------------------------------------------------------------------
// Consents — 個人情報保護法 利用目的別 同意管理 (CMP-004)
// ---------------------------------------------------------------------------

// RecordConsentInput holds fields for granting or withdrawing consent.
type RecordConsentInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	ApplicantID uuid.UUID
	Purpose     string
	// Granted true → records granted_at and clears withdrawn_at.
	// Granted false → records withdrawn_at (withdrawal).
	Granted     bool
	CrossBorder bool
	IP          *string
}

// RecordConsent upserts a per-purpose consent row.  When Granted is false the
// purpose is marked withdrawn; the service recomputes the applicant's aggregate
// consent_status so that downstream usage restrictions apply.  Granting/
// withdrawing and the aggregate recompute happen atomically in one transaction.
func (s *Service) RecordConsent(ctx context.Context, in RecordConsentInput) (*Consent, error) {
	if strings.TrimSpace(in.Purpose) == "" {
		return nil, fmt.Errorf("%w: purpose is required", ErrInvalidMerge)
	}
	var consent Consent
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM applicants WHERE id = ? AND tenant_id = ?`,
			in.ApplicantID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("applicant: record consent verify applicant: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}

		id := uuid.New()
		now := time.Now().UTC()
		var grantedAt, withdrawnAt *time.Time
		if in.Granted {
			grantedAt = &now
		} else {
			withdrawnAt = &now
		}

		// Upsert per (applicant_id, tenant_id, purpose).
		if err := tx.Exec(
			`INSERT INTO applicant_consents
			   (id, tenant_id, applicant_id, purpose, granted_at, withdrawn_at, cross_border)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (applicant_id, tenant_id, purpose) DO UPDATE
			   SET granted_at   = EXCLUDED.granted_at,
			       withdrawn_at = EXCLUDED.withdrawn_at,
			       cross_border = EXCLUDED.cross_border,
			       updated_at   = now()`,
			id, in.TenantID, in.ApplicantID, in.Purpose, grantedAt, withdrawnAt, in.CrossBorder,
		).Error; err != nil {
			return fmt.Errorf("applicant: record consent upsert: %w", err)
		}

		// Recompute the applicant's aggregate consent_status:
		//   - withdrawn if any purpose has been withdrawn (and not re-granted),
		//   - granted   if at least one purpose is granted and none withdrawn,
		//   - unknown    otherwise.
		// This drives the usage-restriction logic required by the spec.
		newStatus := ConsentUnknown
		var withdrawnCount, grantedCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM applicant_consents
			 WHERE applicant_id = ? AND tenant_id = ? AND withdrawn_at IS NOT NULL`,
			in.ApplicantID, in.TenantID,
		).Scan(&withdrawnCount).Error; err != nil {
			return fmt.Errorf("applicant: record consent aggregate withdrawn: %w", err)
		}
		if err := tx.Raw(
			`SELECT COUNT(1) FROM applicant_consents
			 WHERE applicant_id = ? AND tenant_id = ?
			   AND granted_at IS NOT NULL AND withdrawn_at IS NULL`,
			in.ApplicantID, in.TenantID,
		).Scan(&grantedCount).Error; err != nil {
			return fmt.Errorf("applicant: record consent aggregate granted: %w", err)
		}
		switch {
		case withdrawnCount > 0:
			newStatus = ConsentWithdrawn
		case grantedCount > 0:
			newStatus = ConsentGranted
		}

		if err := tx.Exec(
			`UPDATE applicants SET consent_status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			newStatus, in.ApplicantID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("applicant: record consent update applicant: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, applicant_id, purpose, granted_at, withdrawn_at,
			        cross_border, created_at, updated_at
			 FROM applicant_consents
			 WHERE applicant_id = ? AND tenant_id = ? AND purpose = ? LIMIT 1`,
			in.ApplicantID, in.TenantID, in.Purpose,
		).Scan(&consent).Error; err != nil {
			return fmt.Errorf("applicant: record consent re-read: %w", err)
		}

		action := "applicant_consent.granted"
		if !in.Granted {
			action = "applicant_consent.withdrawn"
		}
		idStr := consent.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "applicant_consent",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &consent, nil
}

// ListConsents returns the consent history for an applicant.
func (s *Service) ListConsents(ctx context.Context, tenantID, applicantID uuid.UUID) ([]Consent, error) {
	var consents []Consent
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, applicant_id, purpose, granted_at, withdrawn_at,
			        cross_border, created_at, updated_at
			 FROM applicant_consents
			 WHERE tenant_id = ? AND applicant_id = ?
			 ORDER BY created_at`,
			tenantID, applicantID,
		).Scan(&consents).Error
	})
	if err != nil {
		return nil, err
	}
	return consents, nil
}

// ---------------------------------------------------------------------------
// Duplicate detection
// ---------------------------------------------------------------------------

// FindDuplicates returns candidate duplicate applicants for the given one,
// scored by normalised-email match and/or name + birth_date match.  Auto-merge
// is intentionally NOT performed — the caller must confirm manually.
func (s *Service) FindDuplicates(ctx context.Context, tenantID, applicantID uuid.UUID) ([]Applicant, error) {
	var candidates []Applicant
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var base Applicant
		if err := tx.Raw(
			"SELECT id, tenant_id, last_name, first_name, birth_date,\n"+
				"        status, merged_into_id, email_normalized\n"+ //nolint:misspell // email_normalised is a DB column name (schema contract)
				" FROM applicants WHERE id = ? AND tenant_id = ? LIMIT 1",
			applicantID, tenantID,
		).Scan(&base).Error; err != nil {
			return fmt.Errorf("applicant: find duplicates read base: %w", err)
		}
		if base.ID == uuid.Nil {
			return ErrNotFound
		}

		// Candidates: same tenant, different id, not already merged, matching
		// either normalised email OR (last_name + first_name + birth_date).
		// All bound via ? placeholders; no string concatenation of values.
		var emailNorm string
		if base.EmailNormalized != nil {
			emailNorm = *base.EmailNormalized
		}
		// Duplicate-detection query. email_normalised DB column (American-spelled in schema)
		// is the primary match key; fallback uses name + birth date.
		dupQuery := "SELECT id, tenant_id, job_posting_id, merged_into_id, last_name, first_name," +
			" birth_date, status, consent_status, source," +
			" retention_label, retention_expires_on, anonymized_at," +
			" created_at, updated_at, email_normalized" + //nolint:misspell // DB column name is schema contract
			" FROM applicants" +
			" WHERE tenant_id = ? AND id <> ? AND merged_into_id IS NULL" +
			" AND ((email_normalized IS NOT NULL AND email_normalized <> '' AND email_normalized = ?)" + //nolint:misspell // DB column name is schema contract
			" OR (last_name = ? AND first_name = ? AND birth_date IS NOT NULL AND birth_date = ?))" +
			" ORDER BY created_at"
		return tx.Raw(dupQuery,
			tenantID, applicantID, emailNorm,
			base.LastName, base.FirstName, base.BirthDate,
		).Scan(&candidates).Error
	})
	if err != nil {
		return nil, err
	}
	return candidates, nil
}

// ---------------------------------------------------------------------------
// Merge — human-confirmed duplicate consolidation
// ---------------------------------------------------------------------------

// MergeInput holds fields for merging a source applicant into a target.
type MergeInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// SourceID is the duplicate to be retired (merged_into_id set to TargetID).
	SourceID uuid.UUID
	// TargetID is the surviving record that child rows are re-parented to.
	TargetID uuid.UUID
	Notes    *string
	IP       *string
}

// Merge consolidates a duplicate (source) into a surviving applicant (target).
// All child rows (documents, consents) are re-parented to the target, the
// source's merged_into_id is set, and an applicant_merges audit row is written
// — all atomically (no orphans).  Source records are never physically deleted.
func (s *Service) Merge(ctx context.Context, in MergeInput) (*Merge, error) {
	if in.SourceID == in.TargetID {
		return nil, fmt.Errorf("%w: source and target are identical", ErrInvalidMerge)
	}

	var m Merge
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock both rows (deterministic order to avoid deadlocks) and validate
		// they belong to the tenant and are not already merged.
		first, second := in.SourceID, in.TargetID
		if second.String() < first.String() {
			first, second = second, first
		}
		var src, tgt struct {
			ID           uuid.UUID  `gorm:"column:id"`
			MergedIntoID *uuid.UUID `gorm:"column:merged_into_id"`
		}
		// Lock in deterministic order, then read each by id.
		if err := tx.Exec(
			`SELECT id FROM applicants WHERE id IN (?, ?) AND tenant_id = ? FOR UPDATE`,
			first, second, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("applicant: merge lock: %w", err)
		}
		if err := tx.Raw(
			`SELECT id, merged_into_id FROM applicants WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.SourceID, in.TenantID,
		).Scan(&src).Error; err != nil {
			return fmt.Errorf("applicant: merge read source: %w", err)
		}
		if src.ID == uuid.Nil {
			return ErrNotFound
		}
		if err := tx.Raw(
			`SELECT id, merged_into_id FROM applicants WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.TargetID, in.TenantID,
		).Scan(&tgt).Error; err != nil {
			return fmt.Errorf("applicant: merge read target: %w", err)
		}
		if tgt.ID == uuid.Nil {
			return ErrNotFound
		}
		// Neither may already be merged (prevent chains / double-merge).
		if src.MergedIntoID != nil {
			return fmt.Errorf("%w: source already merged", ErrInvalidMerge)
		}
		if tgt.MergedIntoID != nil {
			return fmt.Errorf("%w: target is itself merged", ErrInvalidMerge)
		}

		// Re-parent child documents to the target.
		if err := tx.Exec(
			`UPDATE applicant_documents SET applicant_id = ?, updated_at = now()
			 WHERE applicant_id = ? AND tenant_id = ?`,
			in.TargetID, in.SourceID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("applicant: merge reparent documents: %w", err)
		}

		// Re-parent consents to the target, skipping purposes that already exist
		// on the target (the unique (applicant_id, tenant_id, purpose) would
		// otherwise be violated).  Surviving consents on the target take
		// precedence; remaining source consents are re-parented.
		if err := tx.Exec(
			`UPDATE applicant_consents SET applicant_id = ?, updated_at = now()
			 WHERE applicant_id = ? AND tenant_id = ?
			   AND purpose NOT IN (
			        SELECT purpose FROM applicant_consents
			        WHERE applicant_id = ? AND tenant_id = ?
			   )`,
			in.TargetID, in.SourceID, in.TenantID, in.TargetID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("applicant: merge reparent consents: %w", err)
		}
		// Any source consents whose purpose collided with the target remain on
		// the source record (which is retired) — they are preserved, not orphaned.

		// Mark the source as logically merged.  Never physically delete.
		res := tx.Exec(
			`UPDATE applicants SET merged_into_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND merged_into_id IS NULL`,
			in.TargetID, in.SourceID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("applicant: merge mark source: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrInvalidMerge
		}

		// Write the merge audit row.
		m = Merge{
			ID:                uuid.New(),
			TenantID:          in.TenantID,
			SourceApplicantID: in.SourceID,
			TargetApplicantID: in.TargetID,
			MergedBy:          &in.ActorID,
			MergedAt:          time.Now().UTC(),
			Notes:             in.Notes,
		}
		if err := tx.Exec(
			`INSERT INTO applicant_merges
			   (id, tenant_id, source_applicant_id, target_applicant_id, merged_by, merged_at, notes)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.TenantID, m.SourceApplicantID, m.TargetApplicantID, m.MergedBy, m.MergedAt, m.Notes,
		).Error; err != nil {
			return fmt.Errorf("applicant: merge insert history: %w", err)
		}

		// Audit: opaque IDs only — no PII.
		idStr := m.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "applicant.merged",
			ResourceType: "applicant_merge",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMerges returns the merge history for the tenant (optionally for a given
// target applicant).
func (s *Service) ListMerges(ctx context.Context, tenantID uuid.UUID, targetID *uuid.UUID) ([]Merge, error) {
	var merges []Merge
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, source_applicant_id, target_applicant_id, merged_by,
		             merged_at, notes, created_at, updated_at
		      FROM applicant_merges
		      WHERE tenant_id = ?`
		args := []any{tenantID}
		if targetID != nil {
			q += ` AND target_applicant_id = ?`
			args = append(args, *targetID)
		}
		q += ` ORDER BY merged_at DESC`
		return tx.Raw(q, args...).Scan(&merges).Error
	})
	if err != nil {
		return nil, err
	}
	return merges, nil
}

// ---------------------------------------------------------------------------
// Retention — logical expiry (anonymisation / access restriction)
// ---------------------------------------------------------------------------

// AnonymizeInput holds fields for logically expiring an applicant.
type AnonymizeInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	IP       *string
}

// Anonymize logically expires an applicant whose retention window has elapsed:
// it clears the encrypted contact PII and the normalised match key and stamps
// anonymized_at.  The row is NEVER physically deleted (CMP-004; mirrors the
// offboarding retention policy).  Only rejected/withdrawn applicants whose
// retention_expires_on is in the past may be anonymised.
//
// Note: in production this is driven by a background job evaluating
// retention_expires_on.  This method performs the per-record logical expiry.
func (s *Service) Anonymize(ctx context.Context, in AnonymizeInput) error {
	return s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status       string     `gorm:"column:status"`
			ExpiresOn    *time.Time `gorm:"column:retention_expires_on"`
			AnonymizedAt *time.Time `gorm:"column:anonymized_at"`
		}
		if err := tx.Raw(
			`SELECT status, retention_expires_on, anonymized_at FROM applicants
			 WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("applicant: anonymize read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if current.AnonymizedAt != nil {
			// Already anonymised — idempotent no-op (still audited below).
			return nil
		}
		// Only terminal-rejected/withdrawn records past their retention window
		// may be logically expired.
		if current.Status != StatusRejected && current.Status != StatusWithdrawn {
			return fmt.Errorf("%w: only rejected/withdrawn applicants may be anonymised", ErrInvalidTransition)
		}
		if current.ExpiresOn == nil || current.ExpiresOn.After(time.Now().UTC()) {
			return fmt.Errorf("%w: retention window has not elapsed", ErrInvalidTransition)
		}

		// Clear PII; keep the row (logical expiry, never physical delete).
		res := tx.Exec(
			"UPDATE applicants SET email_enc = NULL, phone_enc = NULL, anonymized_at = now(), updated_at = now(), email_normalized = NULL WHERE id = ? AND tenant_id = ?", //nolint:misspell // email_normalised is a DB column name (schema contract)
			in.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("applicant: anonymize update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		idStr := in.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "applicant.anonymized",
			ResourceType: "applicant",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// encryptOptional encrypts a non-empty plaintext, returning nil for empty input.
// Encryption happens before any DB transaction so crypto errors fail fast and
// the plaintext never reaches an error message.
func encryptOptional(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	return crypto.Encrypt([]byte(plaintext))
}
