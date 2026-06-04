package offer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/offer/econtract"
	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("offer: not found")
	ErrInvalidTransition = errors.New("offer: invalid offer status transition")
	ErrForbidden         = errors.New("offer: permission denied")
	ErrNotApproved       = errors.New("offer: approval not granted; cannot send")
	ErrSettingNotFound   = errors.New("offer: settings not found for this tenant")
)

// allowedOfferTransitions defines legal offer status moves.
//
//	draft → sent
//	sent  → accepted | declined | expired | rescinded
//
// Terminal states (accepted / declined / expired / rescinded) have no outward
// transitions.
var allowedOfferTransitions = map[string]map[string]bool{
	StatusDraft: {
		StatusSent:      true,
		StatusRescinded: true, // a draft offer may be withdrawn before being sent
	},
	StatusSent: {
		StatusAccepted:  true,
		StatusDeclined:  true,
		StatusExpired:   true,
		StatusRescinded: true,
	},
}

// isOfferTransitionAllowed reports whether moving an offer from current → next
// is valid per the allow-list.
func isOfferTransitionAllowed(current, next string) bool {
	if allowed, ok := allowedOfferTransitions[current]; ok {
		return allowed[next]
	}
	return false
}

// EmployeeCreator is the integration point for ST-ATS-06 (candidate → employee
// master creation). When an offer is accepted, the service invokes Create so a
// downstream package (or the central wiring) can generate the employee master
// from the offer. The implementation must be idempotent (it may be called more
// than once for the same offer) and must NOT receive any decrypted PII — only
// opaque IDs are passed.
//
// The call runs inside the same transaction as the acceptance write so that a
// failure rolls back the whole acceptance (no orphaned state). A nil creator is
// a no-op, keeping the offer package self-contained and decoupled from ATS-06.
type EmployeeCreator interface {
	// Create is called within tx (already tenant-scoped) on offer acceptance.
	// It receives only opaque reference IDs (tenant, offer, application).
	Create(tx *gorm.DB, tenantID, offerID, applicationID uuid.UUID) error
}

// Service provides business logic for the offer domain.
type Service struct {
	tdb         *tenantdb.TenantDB
	approvalSvc *approval.Service
	// employeeCreator is the optional ST-ATS-06 trigger invoked on acceptance.
	// nil keeps the package decoupled (the acceptance simply records the
	// response without generating an employee master).
	employeeCreator EmployeeCreator
	// econtractAdapter is the optional INT-009 electronic-contract service
	// adapter. nil (or stub) keeps the package decoupled until real provider
	// credentials are configured (Issue #14).
	econtractAdapter econtract.Adapter
}

// NewService constructs a Service. The approval engine is built internally so
// that the RegisterRoutes signature stays uniform across all stories.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{
		tdb:              tdb,
		approvalSvc:      approval.NewService(tdb),
		econtractAdapter: econtract.NewStubAdapter(),
	}
}

// WithEmployeeCreator returns a copy of the service with the ST-ATS-06 trigger
// wired in. Used by the central wiring (or tests) without changing the public
// constructor signature.
func (s *Service) WithEmployeeCreator(c EmployeeCreator) *Service {
	clone := *s
	clone.employeeCreator = c
	return &clone
}

// WithEContractAdapter returns a copy of the service with the INT-009
// electronic-contract adapter wired in. Used by the central wiring (or tests)
// to replace the default stub with a real provider adapter.
func (s *Service) WithEContractAdapter(a econtract.Adapter) *Service {
	clone := *s
	clone.econtractAdapter = a
	return &clone
}

// ---------------------------------------------------------------------------
// Electronic-contract service (INT-009 / LM-002)
// ---------------------------------------------------------------------------

// InitiateSigningInput holds fields for dispatching an offer letter to the
// electronic-contract service for signature.
//
// SECURITY: SignerRef MUST be an opaque identifier — no PII (name / email)
// should be placed here. Resolve and pass the actual signer contact info to
// the external service via a separate privileged channel only.
type InitiateSigningInput struct {
	TenantID       uuid.UUID
	ActorID        uuid.UUID
	LetterID       uuid.UUID
	IdempotencyKey string
	IP             *string
}

// InitiateSigning dispatches the offer letter to the configured
// electronic-contract service for signature and records the returned envelope
// reference in offer_letters.esign_envelope_id.
//
// Behaviour when the adapter is the stub (no real provider configured):
// the stub returns a deterministic mock envelope ID. The row is still updated
// so callers can test the flow end-to-end without real provider credentials.
//
// An INT-009 failure (non-nil error from the adapter) does NOT update the DB
// and is returned directly. A manual signing path (IssueLetter with explicit
// SignedAt) is always available as a fallback (loosely coupled — CMP-006).
func (s *Service) InitiateSigning(ctx context.Context, in InitiateSigningInput) (*Letter, error) {
	// Fetch the letter first to obtain the file_ref and content_hash needed
	// by the adapter. The fetch is intentionally outside the write transaction
	// to keep the external API call outside a DB transaction boundary.
	var letter Letter
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, offer_id, file_ref, version, esign_provider,
			        esign_envelope_id, content_hash, signer_ref, signed_at,
			        created_at, updated_at
			 FROM offer_letters
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.LetterID, in.TenantID,
		).Scan(&letter).Error
	})
	if err != nil {
		return nil, fmt.Errorf("offer: initiate signing read letter: %w", err)
	}
	if letter.ID == uuid.Nil {
		return nil, ErrNotFound
	}

	// Dispatch to the external service outside the DB transaction.
	signerRef := ""
	if letter.SignerRef != nil {
		signerRef = *letter.SignerRef
	}
	result, adapterErr := s.econtractAdapter.SendSignRequest(ctx, econtract.SendSignRequest{
		TenantID:       in.TenantID,
		OfferLetterID:  in.LetterID,
		IdempotencyKey: in.IdempotencyKey,
		FileRef:        letter.FileRef,
		ContentHash:    letter.ContentHash,
		SignerRef:      signerRef,
	})
	if adapterErr != nil {
		return nil, fmt.Errorf("offer: initiate signing adapter: %w", adapterErr)
	}

	// Persist the envelope reference and provider label.
	writeErr := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE offer_letters
			 SET    esign_provider    = ?,
			        esign_envelope_id = ?,
			        updated_at        = now()
			 WHERE  id = ? AND tenant_id = ?`,
			result.Provider, result.EnvelopeID, in.LetterID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("offer: initiate signing update: %w", res.Error)
		}

		idStr := in.LetterID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "offer_letter.esign_initiated",
			ResourceType: "offer_letter",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if writeErr != nil {
		return nil, writeErr
	}

	letter.EsignProvider = result.Provider
	letter.EsignEnvelopeID = result.EnvelopeID
	return &letter, nil
}

// PollSigningStatusInput holds parameters for polling the current signing
// status from the external provider.
type PollSigningStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	LetterID uuid.UUID
	IP       *string
}

// PollSigningStatus retrieves the current signing status from the configured
// electronic-contract service adapter and, when the status is completed,
// records signed_at on the offer_letters row.
//
// Behaviour when the adapter is the stub: the stub returns completed for
// STUB-prefixed envelope IDs so the flow can be tested end-to-end.
func (s *Service) PollSigningStatus(ctx context.Context, in PollSigningStatusInput) (*Letter, econtract.SignStatus, error) {
	var letter Letter
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, offer_id, file_ref, version, esign_provider,
			        esign_envelope_id, content_hash, signer_ref, signed_at,
			        created_at, updated_at
			 FROM offer_letters
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.LetterID, in.TenantID,
		).Scan(&letter).Error
	})
	if err != nil {
		return nil, econtract.SignStatus{}, fmt.Errorf("offer: poll signing status read letter: %w", err)
	}
	if letter.ID == uuid.Nil {
		return nil, econtract.SignStatus{}, ErrNotFound
	}
	if letter.EsignEnvelopeID == "" {
		return nil, econtract.SignStatus{}, fmt.Errorf("offer: poll signing status: no esign_envelope_id on letter")
	}

	status, adapterErr := s.econtractAdapter.GetSignStatus(ctx, letter.EsignEnvelopeID)
	if adapterErr != nil {
		return nil, econtract.SignStatus{}, fmt.Errorf("offer: poll signing status adapter: %w", adapterErr)
	}

	// When completed, persist signed_at and record an audit entry.
	if status.Status == econtract.SignStatusCompleted && letter.SignedAt == nil && status.SignedAt != nil {
		writeErr := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
			res := tx.Exec(
				`UPDATE offer_letters
				 SET    signed_at  = ?,
				        updated_at = now()
				 WHERE  id = ? AND tenant_id = ? AND signed_at IS NULL`,
				status.SignedAt, in.LetterID, in.TenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("offer: poll signing status update: %w", res.Error)
			}

			idStr := in.LetterID.String()
			return audit.Record(tx, audit.Entry{
				TenantID:     in.TenantID,
				UserID:       &in.ActorID,
				Action:       "offer_letter.esign_completed",
				ResourceType: "offer_letter",
				ResourceID:   &idStr,
				IP:           in.IP,
			})
		})
		if writeErr != nil {
			return nil, econtract.SignStatus{}, writeErr
		}
		letter.SignedAt = status.SignedAt
	}

	return &letter, status, nil
}

// ---------------------------------------------------------------------------
// Settings (legal configuration — CMP-006 / CMP-001)
// ---------------------------------------------------------------------------

// GetSettings returns the offer settings for a tenant, returning
// ErrSettingNotFound when no row exists.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*Setting, error) {
	var setting Setting
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, required_fields_json, retention_years,
			        esign_storage_mode, default_expiry_lead_days,
			        created_at, updated_at
			 FROM offer_settings
			 WHERE tenant_id = ?
			 LIMIT 1`,
			tenantID,
		).Scan(&setting).Error
	})
	if err != nil {
		return nil, err
	}
	if setting.ID == uuid.Nil {
		return nil, ErrSettingNotFound
	}
	return &setting, nil
}

// UpsertSettingsInput holds validated fields for creating or updating settings.
//
// Legal values are configured here (not hardcoded) so they can follow legal
// revisions. The latest values require expert confirmation (CMP-001 / CMP-006).
type UpsertSettingsInput struct {
	TenantID              uuid.UUID
	ActorID               uuid.UUID
	RequiredFieldsJSON    []byte
	RetentionYears        int
	EsignStorageMode      string
	DefaultExpiryLeadDays int
	IP                    *string
}

// UpsertSettings creates or updates the offer settings for a tenant.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*Setting, error) {
	var setting Setting
	requiredFields := in.RequiredFieldsJSON
	if len(requiredFields) == 0 || string(requiredFields) == "null" {
		requiredFields = []byte(`{"fields":[]}`)
	}
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO offer_settings
			   (id, tenant_id, required_fields_json, retention_years,
			    esign_storage_mode, default_expiry_lead_days)
			 VALUES (?, ?, ?::jsonb, ?, ?, ?)
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET required_fields_json     = EXCLUDED.required_fields_json,
			       retention_years          = EXCLUDED.retention_years,
			       esign_storage_mode       = EXCLUDED.esign_storage_mode,
			       default_expiry_lead_days = EXCLUDED.default_expiry_lead_days,
			       updated_at               = now()`,
			uuid.New(), in.TenantID, requiredFields, in.RetentionYears,
			in.EsignStorageMode, in.DefaultExpiryLeadDays,
		).Error; err != nil {
			return fmt.Errorf("offer: upsert settings: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, required_fields_json, retention_years,
			        esign_storage_mode, default_expiry_lead_days,
			        created_at, updated_at
			 FROM offer_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&setting).Error; err != nil {
			return fmt.Errorf("offer: upsert settings re-read: %w", err)
		}

		idStr := setting.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "offer_settings.upserted",
			ResourceType: "offer_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &setting, nil
}

// ---------------------------------------------------------------------------
// Offers
// ---------------------------------------------------------------------------

// CreateOfferInput holds fields for creating a draft offer.
//
// AnnualSalaryPlaintext / CompensationDetailPlaintext contain the sensitive
// offer terms in plaintext. They are encrypted with AES-256-GCM before storage;
// the plaintext is NEVER persisted, logged, or written to audit records.
type CreateOfferInput struct {
	TenantID                    uuid.UUID
	ActorID                     uuid.UUID
	ApplicationID               uuid.UUID
	Position                    string
	EmploymentType              string
	StartDate                   *time.Time
	ExpiryDate                  *time.Time
	AnnualSalaryPlaintext       []byte
	CompensationDetailPlaintext []byte
	IP                          *string
}

// CreateOffer creates a new offer in draft status against an application.
// Sensitive salary / compensation fields are encrypted before the transaction
// opens (fail-fast); the plaintext is never persisted.
func (s *Service) CreateOffer(ctx context.Context, in CreateOfferInput) (*Offer, error) {
	// Encrypt sensitive fields BEFORE opening the transaction so any crypto
	// error fails fast without acquiring DB resources. The plaintext value
	// does not appear in any error message.
	annualSalaryEnc, err := encryptOptional(in.AnnualSalaryPlaintext)
	if err != nil {
		return nil, fmt.Errorf("offer: encrypt annual salary: %w", err)
	}
	compensationEnc, err := encryptOptional(in.CompensationDetailPlaintext)
	if err != nil {
		return nil, fmt.Errorf("offer: encrypt compensation detail: %w", err)
	}

	off := Offer{
		ID:                    uuid.New(),
		TenantID:              in.TenantID,
		ApplicationID:         in.ApplicationID,
		Status:                StatusDraft,
		Position:              in.Position,
		EmploymentType:        in.EmploymentType,
		StartDate:             in.StartDate,
		ExpiryDate:            in.ExpiryDate,
		AnnualSalaryEnc:       annualSalaryEnc,
		CompensationDetailEnc: compensationEnc,
	}

	err = s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Exec(
			`INSERT INTO offers
			   (id, tenant_id, application_id, status, position, employment_type,
			    start_date, expiry_date, annual_salary_enc, compensation_detail_enc)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			off.ID, off.TenantID, off.ApplicationID, off.Status, off.Position,
			off.EmploymentType, off.StartDate, off.ExpiryDate,
			off.AnnualSalaryEnc, off.CompensationDetailEnc,
		).Error; err != nil {
			return fmt.Errorf("offer: create offer insert: %w", err)
		}

		idStr := off.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "offer.created",
			ResourceType: "offer",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers.
	off.AnnualSalaryEnc = nil
	off.CompensationDetailEnc = nil
	return &off, nil
}

// GetOfferInput holds parameters for fetching a single offer.
//
// ReadSensitive must be true only when the caller holds offer:read_sensitive.
// When false (or the service-layer check fails), the decrypted salary /
// compensation values are not returned.
type GetOfferInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	ID            uuid.UUID
	ReadSensitive bool
	IP            *string
}

// DecryptedTerms carries the decrypted sensitive offer terms, returned
// separately from the Offer struct so callers cannot accidentally persist them.
type DecryptedTerms struct {
	AnnualSalary       []byte
	CompensationDetail []byte
}

// GetOffer fetches an offer by ID. expiry_date is evaluated at read time so an
// elapsed sent offer is reported as expired without a physical delete.
//
// Multi-layer permission enforcement for ReadSensitive (mirrors onboarding):
//   - Layer 1 (HTTP): the sensitive route requires offer:read_sensitive via
//     RequirePermission middleware before ReadSensitive is set true.
//   - Layer 2 (service, here): when ReadSensitive is true the service
//     re-validates offer:read_sensitive via LoadUserPermissions inside the same
//     transaction, so callers that bypass the HTTP layer cannot obtain plaintext.
//
// The decrypted values are returned only when both layers grant access; they
// are NEVER written to the audit log or any other log.
func (s *Service) GetOffer(ctx context.Context, in GetOfferInput) (*Offer, *DecryptedTerms, error) {
	var off Offer
	var permittedSensitive bool
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if in.ReadSensitive {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("offer: get offer load permissions: %w", err)
			}
			if !platformauth.HasPermission(perms, "offer:read_sensitive") {
				return ErrForbidden
			}
			permittedSensitive = true
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, position, employment_type,
			        start_date, expiry_date, annual_salary_enc, compensation_detail_enc,
			        approval_request_id, created_at, updated_at
			 FROM offers WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&off).Error; err != nil {
			return fmt.Errorf("offer: get offer: %w", err)
		}
		if off.ID == uuid.Nil {
			return ErrNotFound
		}

		action := "offer.read"
		if permittedSensitive {
			action = "offer.read_sensitive"
		}
		idStr := off.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "offer",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}

	// Reflect expiry at read time (logical, no physical delete).
	off.Status = effectiveStatus(&off, time.Now())

	var terms *DecryptedTerms
	if permittedSensitive {
		t := DecryptedTerms{}
		if len(off.AnnualSalaryEnc) > 0 {
			plain, derr := crypto.Decrypt(off.AnnualSalaryEnc)
			if derr != nil {
				return nil, nil, fmt.Errorf("offer: decrypt annual salary: %w", derr)
			}
			t.AnnualSalary = plain
		}
		if len(off.CompensationDetailEnc) > 0 {
			plain, derr := crypto.Decrypt(off.CompensationDetailEnc)
			if derr != nil {
				return nil, nil, fmt.Errorf("offer: decrypt compensation detail: %w", derr)
			}
			t.CompensationDetail = plain
		}
		terms = &t
	}

	// Never expose ciphertext to callers regardless of permission.
	off.AnnualSalaryEnc = nil
	off.CompensationDetailEnc = nil
	return &off, terms, nil
}

// ListOffers returns offers for an application (or all in the tenant when
// applicationID is uuid.Nil). Ciphertext is never returned by this path.
// expiry is reflected at read time.
func (s *Service) ListOffers(ctx context.Context, tenantID, applicationID uuid.UUID) ([]Offer, error) {
	var offers []Offer
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, application_id, status, position, employment_type,
		             start_date, expiry_date, approval_request_id, created_at, updated_at
		      FROM offers
		      WHERE tenant_id = ?`
		args := []any{tenantID}
		if applicationID != uuid.Nil {
			q += ` AND application_id = ?`
			args = append(args, applicationID)
		}
		q += ` ORDER BY created_at DESC`
		return tx.Raw(q, args...).Scan(&offers).Error
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range offers {
		offers[i].Status = effectiveStatus(&offers[i], now)
		offers[i].AnnualSalaryEnc = nil
		offers[i].CompensationDetailEnc = nil
	}
	return offers, nil
}

// SubmitForApprovalInput holds fields for submitting an offer for issuance
// approval (ST-FND-08).
type SubmitForApprovalInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	IP       *string
}

// SubmitForApproval submits a draft offer to the approval engine for issuance
// approval. The approval_request INSERT and the offer.approval_request_id link
// UPDATE are atomic within one transaction (orphan prevention). When no route
// is configured (ErrRouteNotFound / ErrRouteEmpty) the offer remains in draft
// without a link — the intended fallback for manually-managed tenants.
func (s *Service) SubmitForApproval(ctx context.Context, in SubmitForApprovalInput) (*Offer, error) {
	var off Offer
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, approval_request_id
			 FROM offers WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&off).Error; err != nil {
			return fmt.Errorf("offer: submit approval read offer: %w", err)
		}
		if off.ID == uuid.Nil {
			return ErrNotFound
		}
		if off.Status != StatusDraft {
			return fmt.Errorf("%w: only draft offers may be submitted (current %s)", ErrInvalidTransition, off.Status)
		}

		approvalReq, submitErr := s.approvalSvc.SubmitTx(tx, approval.SubmitInput{
			TenantID:    in.TenantID,
			ActorID:     in.ActorID,
			RequestType: "offer_issue",
			SubjectRef:  off.ID.String(),
			PayloadJSON: []byte(`{"offer_id":"` + off.ID.String() + `"}`),
			IP:          in.IP,
		})
		if submitErr != nil {
			// No route configured — leave the offer in draft without a link.
			// This is not an error; the offer can be sent manually.
			if errors.Is(submitErr, approval.ErrRouteNotFound) || errors.Is(submitErr, approval.ErrRouteEmpty) {
				return recordAudit(tx, in.TenantID, in.ActorID, "offer.submit_approval_noroute", off.ID, in.IP)
			}
			return fmt.Errorf("offer: submit approval: %w", submitErr)
		}

		if err := tx.Exec(
			`UPDATE offers SET approval_request_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			approvalReq.ID, off.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("offer: submit approval link: %w", err)
		}
		off.ApprovalRequestID = &approvalReq.ID

		return recordAudit(tx, in.TenantID, in.ActorID, "offer.submitted_for_approval", off.ID, in.IP)
	})
	if err != nil {
		return nil, err
	}
	off.AnnualSalaryEnc = nil
	off.CompensationDetailEnc = nil
	return &off, nil
}

// SendOfferInput holds fields for transitioning a draft offer to sent.
type SendOfferInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	IP       *string
}

// SendOffer transitions a draft offer to sent. When an approval request is
// linked, it must be in the approved state before the offer can be sent
// (ErrNotApproved otherwise). Offers with no linked approval request (manual
// flow / no route configured) may be sent directly.
func (s *Service) SendOffer(ctx context.Context, in SendOfferInput) (*Offer, error) {
	var off Offer
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, approval_request_id
			 FROM offers WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.ID, in.TenantID,
		).Scan(&off).Error; err != nil {
			return fmt.Errorf("offer: send offer read: %w", err)
		}
		if off.ID == uuid.Nil {
			return ErrNotFound
		}
		if !isOfferTransitionAllowed(off.Status, StatusSent) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, off.Status, StatusSent)
		}

		// If an approval request is linked, it must be approved first.
		if off.ApprovalRequestID != nil {
			var approvalStatus struct {
				Status string `gorm:"column:status"`
			}
			if err := tx.Raw(
				`SELECT status FROM approval_requests WHERE id = ? AND tenant_id = ? LIMIT 1`,
				*off.ApprovalRequestID, in.TenantID,
			).Scan(&approvalStatus).Error; err != nil {
				return fmt.Errorf("offer: send offer read approval status: %w", err)
			}
			if approvalStatus.Status != "approved" {
				return ErrNotApproved
			}
		}

		res := tx.Exec(
			`UPDATE offers SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			StatusSent, in.ID, in.TenantID, StatusDraft,
		)
		if res.Error != nil {
			return fmt.Errorf("offer: send offer update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		off.Status = StatusSent

		return recordAudit(tx, in.TenantID, in.ActorID, "offer.sent", off.ID, in.IP)
	})
	if err != nil {
		return nil, err
	}
	off.AnnualSalaryEnc = nil
	off.CompensationDetailEnc = nil
	return &off, nil
}

// RescindOfferInput holds fields for withdrawing an offer.
type RescindOfferInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	ID       uuid.UUID
	IP       *string
}

// RescindOffer withdraws (rescinds) a draft or sent offer. Terminal offers
// cannot be rescinded (ErrInvalidTransition). Physical deletion is never done.
func (s *Service) RescindOffer(ctx context.Context, in RescindOfferInput) (*Offer, error) {
	return s.transitionTerminal(ctx, in.TenantID, in.ActorID, in.ID, StatusRescinded, "offer.rescinded", in.IP)
}

// ---------------------------------------------------------------------------
// Offer letters (CMP-006 signing evidence)
// ---------------------------------------------------------------------------

// IssueLetterInput holds fields for issuing / recording an offer letter and its
// signing evidence. esign fields and content_hash come from the LM-002 / INT-009
// flow; an INT-009 failure must not block recording a manual signing (loosely
// coupled — the caller may pass empty esign fields with a manual signed_at).
type IssueLetterInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	OfferID         uuid.UUID
	FileRef         string
	EsignProvider   string
	EsignEnvelopeID string
	ContentHash     string
	SignerRef       *string
	SignedAt        *time.Time
	IP              *string
}

// IssueLetter creates a new versioned offer letter row with signing evidence.
// The version is auto-incremented per offer. content_hash records the document
// hash for tamper detection (truthfulness — CMP-006); signed_at / signer_ref
// record the signing audit trail. The parent offer must exist in the tenant.
//
// When SignedAt is non-nil the retention expiry date is computed via
// econtract.CalcRetentionExpiry using the tenant's offer_settings.retention_years
// and stored in retention_expires_on.  This value is consumed by the
// econtract retention job (RunEContractRetention) to identify expired letters.
// When offer_settings does not exist for the tenant, retention_expires_on is
// left NULL — the job will skip such rows until settings are configured.
func (s *Service) IssueLetter(ctx context.Context, in IssueLetterInput) (*Letter, error) {
	// Compute retention expiry outside the transaction when SignedAt is provided
	// (no DB I/O; fail-fast on config read only).
	var retentionExpiresOn *time.Time
	if in.SignedAt != nil {
		settings, settingsErr := s.GetSettings(ctx, in.TenantID)
		if settingsErr == nil && settings.RetentionYears > 0 {
			expiry := econtract.CalcRetentionExpiry(*in.SignedAt, settings.RetentionYears)
			retentionExpiresOn = &expiry
		}
		// When settings are absent (ErrSettingNotFound) or RetentionYears == 0,
		// leave retention_expires_on as NULL.  The letter is still issued; the
		// retention job will skip it until settings are configured.
	}

	var letter Letter
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the parent offer belongs to this tenant (defence-in-depth).
		var offCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM offers WHERE id = ? AND tenant_id = ?`,
			in.OfferID, in.TenantID,
		).Scan(&offCount).Error; err != nil {
			return fmt.Errorf("offer: issue letter verify offer: %w", err)
		}
		if offCount == 0 {
			return ErrNotFound
		}

		// Compute the next version under FOR UPDATE-style serialisation via the
		// per-offer unique constraint; read current max and increment.
		var maxVersion struct {
			V *int `gorm:"column:v"`
		}
		if err := tx.Raw(
			`SELECT MAX(version) AS v FROM offer_letters
			 WHERE offer_id = ? AND tenant_id = ?`,
			in.OfferID, in.TenantID,
		).Scan(&maxVersion).Error; err != nil {
			return fmt.Errorf("offer: issue letter read version: %w", err)
		}
		nextVersion := 1
		if maxVersion.V != nil {
			nextVersion = *maxVersion.V + 1
		}

		letter = Letter{
			ID:                 uuid.New(),
			TenantID:           in.TenantID,
			OfferID:            in.OfferID,
			FileRef:            in.FileRef,
			Version:            nextVersion,
			EsignProvider:      in.EsignProvider,
			EsignEnvelopeID:    in.EsignEnvelopeID,
			ContentHash:        in.ContentHash,
			SignerRef:          in.SignerRef,
			SignedAt:           in.SignedAt,
			RetentionExpiresOn: retentionExpiresOn,
		}

		if err := tx.Exec(
			`INSERT INTO offer_letters
			   (id, tenant_id, offer_id, file_ref, version, esign_provider,
			    esign_envelope_id, content_hash, signer_ref, signed_at,
			    retention_expires_on)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			letter.ID, letter.TenantID, letter.OfferID, letter.FileRef,
			letter.Version, letter.EsignProvider, letter.EsignEnvelopeID,
			letter.ContentHash, letter.SignerRef, letter.SignedAt,
			letter.RetentionExpiresOn,
		).Error; err != nil {
			return fmt.Errorf("offer: issue letter insert: %w", err)
		}

		idStr := letter.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "offer_letter.issued",
			ResourceType: "offer_letter",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &letter, nil
}

// ListLetters returns the letter versions for an offer (newest version first).
func (s *Service) ListLetters(ctx context.Context, tenantID, offerID uuid.UUID) ([]Letter, error) {
	var letters []Letter
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, offer_id, file_ref, version, esign_provider,
			        esign_envelope_id, content_hash, signer_ref, signed_at,
			        created_at, updated_at
			 FROM offer_letters
			 WHERE tenant_id = ? AND offer_id = ?
			 ORDER BY version DESC`,
			tenantID, offerID,
		).Scan(&letters).Error
	})
	if err != nil {
		return nil, err
	}
	return letters, nil
}

// VerifyLetterContentHash re-checks a letter's stored content_hash against the
// provided expected hash, supporting tamper detection (truthfulness — CMP-006).
// Returns (true, nil) when they match, (false, nil) on mismatch, ErrNotFound
// when the letter does not exist in the tenant.
func (s *Service) VerifyLetterContentHash(ctx context.Context, tenantID, letterID uuid.UUID, expectedHash string) (bool, error) {
	var match bool
	var found bool
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		var row struct {
			ContentHash string `gorm:"column:content_hash"`
			ID          string `gorm:"column:id"`
		}
		if err := tx.Raw(
			`SELECT id, content_hash FROM offer_letters
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			letterID, tenantID,
		).Scan(&row).Error; err != nil {
			return fmt.Errorf("offer: verify letter hash read: %w", err)
		}
		if row.ID == "" {
			return ErrNotFound
		}
		found = true
		match = row.ContentHash == expectedHash
		return nil
	})
	if err != nil {
		return false, err
	}
	if !found {
		return false, ErrNotFound
	}
	return match, nil
}

// ---------------------------------------------------------------------------
// Responses (acceptance / decline — ST-ATS-06 trigger)
// ---------------------------------------------------------------------------

// RespondInput holds fields for recording a candidate acceptance / decline.
type RespondInput struct {
	TenantID     uuid.UUID
	ActorID      uuid.UUID
	OfferID      uuid.UUID
	Response     string // accepted | declined
	RespondedVia string // portal | esign | manual
	IP           *string
}

// Respond records the candidate's acceptance or decline of a sent offer,
// transitions the offer to accepted / declined, and (on acceptance) invokes the
// ST-ATS-06 employee-creation trigger within the same transaction.
//
// The offer must currently be in the sent state (terminal offers reject with
// ErrInvalidTransition). The acceptance trigger is idempotent: the employee
// creator (when wired) must handle being called for an already-created offer.
func (s *Service) Respond(ctx context.Context, in RespondInput) (*Response, error) {
	via := in.RespondedVia
	if via == "" {
		via = ViaManual
	}
	target := StatusAccepted
	if in.Response == ResponseDeclined {
		target = StatusDeclined
	}

	var resp Response
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var off Offer
		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status, expiry_date
			 FROM offers WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.OfferID, in.TenantID,
		).Scan(&off).Error; err != nil {
			return fmt.Errorf("offer: respond read offer: %w", err)
		}
		if off.ID == uuid.Nil {
			return ErrNotFound
		}

		// An offer whose expiry has elapsed is treated as expired; a response
		// to an expired/terminal offer is an invalid transition.
		current := effectiveStatus(&off, time.Now())
		if !isOfferTransitionAllowed(current, target) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current, target)
		}

		// Record the response history row.
		resp = Response{
			ID:           uuid.New(),
			TenantID:     in.TenantID,
			OfferID:      in.OfferID,
			Response:     in.Response,
			RespondedVia: via,
		}
		if err := tx.Exec(
			`INSERT INTO offer_responses
			   (id, tenant_id, offer_id, response, responded_via)
			 VALUES (?, ?, ?, ?, ?)`,
			resp.ID, resp.TenantID, resp.OfferID, resp.Response, resp.RespondedVia,
		).Error; err != nil {
			return fmt.Errorf("offer: respond insert: %w", err)
		}

		// Transition the offer. The status guard in the WHERE clause prevents a
		// concurrent double-transition (the FOR UPDATE above already serialises).
		res := tx.Exec(
			`UPDATE offers SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			target, in.OfferID, in.TenantID, StatusSent,
		)
		if res.Error != nil {
			return fmt.Errorf("offer: respond update offer: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// The offer was not in sent state at UPDATE time (race) — reject.
			return fmt.Errorf("%w: offer no longer in sent state", ErrInvalidTransition)
		}

		// On acceptance, fire the ST-ATS-06 employee-creation trigger within the
		// same transaction. Idempotent and decoupled (no-op when not wired).
		if target == StatusAccepted && s.employeeCreator != nil {
			if err := s.employeeCreator.Create(tx, in.TenantID, off.ID, off.ApplicationID); err != nil {
				return fmt.Errorf("offer: respond employee creation trigger: %w", err)
			}
		}

		respIDStr := resp.ID.String()
		action := "offer.accepted"
		if target == StatusDeclined {
			action = "offer.declined"
		}
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       action,
			ResourceType: "offer_response",
			ResourceID:   &respIDStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListResponses returns the response history for an offer (newest first).
func (s *Service) ListResponses(ctx context.Context, tenantID, offerID uuid.UUID) ([]Response, error) {
	var responses []Response
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, offer_id, response, responded_via, responded_at,
			        created_at, updated_at
			 FROM offer_responses
			 WHERE tenant_id = ? AND offer_id = ?
			 ORDER BY responded_at DESC`,
			tenantID, offerID,
		).Scan(&responses).Error
	})
	if err != nil {
		return nil, err
	}
	return responses, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// transitionTerminal moves an offer to a terminal status (e.g. rescinded) when
// allowed, records an audit entry, and never physically deletes the row.
func (s *Service) transitionTerminal(ctx context.Context, tenantID, actorID, id uuid.UUID, target, action string, ip *string) (*Offer, error) {
	var off Offer
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, application_id, status
			 FROM offers WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			id, tenantID,
		).Scan(&off).Error; err != nil {
			return fmt.Errorf("offer: transition read offer: %w", err)
		}
		if off.ID == uuid.Nil {
			return ErrNotFound
		}
		if !isOfferTransitionAllowed(off.Status, target) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, off.Status, target)
		}

		res := tx.Exec(
			`UPDATE offers SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			target, id, tenantID, off.Status,
		)
		if res.Error != nil {
			return fmt.Errorf("offer: transition update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}
		off.Status = target

		return recordAudit(tx, tenantID, actorID, action, id, ip)
	})
	if err != nil {
		return nil, err
	}
	off.AnnualSalaryEnc = nil
	off.CompensationDetailEnc = nil
	return &off, nil
}

// recordAudit is a small helper to record an offer-resource audit entry with an
// opaque UUID resource_id (never PII).
func recordAudit(tx *gorm.DB, tenantID, actorID uuid.UUID, action string, resourceID uuid.UUID, ip *string) error {
	idStr := resourceID.String()
	return audit.Record(tx, audit.Entry{
		TenantID:     tenantID,
		UserID:       &actorID,
		Action:       action,
		ResourceType: "offer",
		ResourceID:   &idStr,
		IP:           ip,
	})
}

// encryptOptional encrypts plaintext when non-empty; returns nil for empty input.
func encryptOptional(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, nil
	}
	return crypto.Encrypt(plaintext)
}

// effectiveStatus reflects expiry at read time: a sent offer whose expiry_date
// has elapsed is reported as expired. This is a logical view — the persisted
// status is unchanged here (no physical delete, never mutated by a read). A
// background job (outside this slice) may persist the expired status.
func effectiveStatus(o *Offer, now time.Time) string {
	if o.Status == StatusSent && o.ExpiryDate != nil {
		// expiry_date is a DATE; treat the offer as expired once the day after
		// the expiry date has begun (end-of-day expiry).
		if now.After(o.ExpiryDate.AddDate(0, 0, 1)) || now.Equal(o.ExpiryDate.AddDate(0, 0, 1)) {
			return StatusExpired
		}
	}
	return o.Status
}
