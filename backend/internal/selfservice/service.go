package selfservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/platform/audit"
	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/crypto"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("selfservice: not found")
	ErrInvalidTransition = errors.New("selfservice: invalid status transition")
	ErrForbidden         = errors.New("selfservice: permission denied")
	ErrValidation        = errors.New("selfservice: validation error")
	ErrLegalHold         = errors.New("selfservice: document is under legal hold and cannot be expired")
)

// ---------------------------------------------------------------------------
// Permission constants (service-layer defence-in-depth re-checks)
// ---------------------------------------------------------------------------

const (
	permReadSensitive = "selfservice:read_sensitive"
)

// ---------------------------------------------------------------------------
// State-transition allow-lists
// ---------------------------------------------------------------------------

// allowedChangeTransitions defines legal change-request status moves.
// Terminal states: approved, rejected, cancelled.
var allowedChangeTransitions = map[string]map[string]bool{
	ChangeStatusDraft: {
		ChangeStatusPending:   true,
		ChangeStatusCancelled: true,
	},
	ChangeStatusPending: {
		ChangeStatusApproved:  true,
		ChangeStatusRejected:  true,
		ChangeStatusCancelled: true,
	},
}

func isChangeTransitionAllowed(from, to string) bool {
	if next, ok := allowedChangeTransitions[from]; ok {
		return next[to]
	}
	return false
}

// ---------------------------------------------------------------------------
// Document upload validation policy (法令値ではない技術的 allowlist)
// ---------------------------------------------------------------------------

// maxDocumentBytes caps the inline content size accepted by the document store.
// The real binary normally lives in object storage; this guards inline payloads.
const maxDocumentBytes = 10 * 1024 * 1024 // 10 MB

// allowedMIMETypes is the document upload MIME allow-list.
var allowedMIMETypes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"text/plain":      true,
	"text/csv":        true,
}

// allowedExtensions is the document upload extension allow-list.
var allowedExtensions = map[string]bool{
	".pdf":  true,
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".txt":  true,
	".csv":  true,
}

// ---------------------------------------------------------------------------
// VirusScanner extension point
// ---------------------------------------------------------------------------

// VirusScanner is the extension point for malware scanning of uploaded content.
// The default NoopScanner always passes; production wires a real scanner.
type VirusScanner interface {
	// Scan returns a non-nil error when the content is rejected.
	Scan(content []byte) error
}

// NoopScanner is a VirusScanner that accepts all content.
type NoopScanner struct{}

// Scan always returns nil.
func (NoopScanner) Scan([]byte) error { return nil }

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// Service provides business logic for the self-service, CSV import and document
// store domains.
type Service struct {
	tdb         *tenantdb.TenantDB
	approvalSvc *approval.Service
	scanner     VirusScanner
}

// NewService constructs a Service.  The approval engine is built internally so
// that the RegisterRoutes signature stays uniform across stories.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{
		tdb:         tdb,
		approvalSvc: approval.NewService(tdb),
		scanner:     NoopScanner{},
	}
}

// ===========================================================================
// Self-service change requests
// ===========================================================================

// SubmitChangeRequestInput holds fields for submitting a self-service change.
//
// SensitivePlaintext, when non-empty, is encrypted before storage and never
// persisted, logged, or written to audit as plaintext.
type SubmitChangeRequestInput struct {
	TenantID           uuid.UUID
	ActorID            uuid.UUID
	EmployeeID         uuid.UUID
	TargetType         string
	ChangesJSON        []byte
	SensitivePlaintext []byte
	IP                 *string
}

// SubmitChangeRequest records a self-service change request and submits it to
// the approval engine within a single transaction.  The change is NEVER written
// directly to the master — it is reflected only on approval (ApproveChangeRequest).
//
// If no approval route is configured the request remains pending without an
// approval link (manual review fallback), mirroring the leave package.
func (s *Service) SubmitChangeRequest(ctx context.Context, in SubmitChangeRequestInput) (*ChangeRequest, error) {
	if !isValidTargetType(in.TargetType) {
		return nil, fmt.Errorf("%w: invalid target_type %q", ErrValidation, in.TargetType)
	}

	// Encrypt sensitive diff BEFORE opening the transaction (fail fast; the
	// plaintext never appears in an error message).
	var sensitiveEnc []byte
	if len(in.SensitivePlaintext) > 0 {
		var err error
		sensitiveEnc, err = crypto.Encrypt(in.SensitivePlaintext)
		if err != nil {
			return nil, fmt.Errorf("selfservice: encrypt sensitive change: %w", err)
		}
	}

	changes := in.ChangesJSON
	if len(changes) == 0 || string(changes) == "null" {
		changes = []byte(`{}`)
	}

	req := ChangeRequest{
		ID:                  uuid.New(),
		TenantID:            in.TenantID,
		EmployeeID:          in.EmployeeID,
		RequestedByUserID:   in.ActorID,
		TargetType:          in.TargetType,
		ChangesJSON:         changes,
		ChangesSensitiveEnc: sensitiveEnc,
		Status:              ChangeStatusPending,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the employee belongs to this tenant.
		var empCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&empCount).Error; err != nil {
			return fmt.Errorf("selfservice: submit change verify employee: %w", err)
		}
		if empCount == 0 {
			return ErrNotFound
		}

		// Verify the requesting user belongs to this tenant (users has no
		// composite FK; defence-in-depth COUNT check).
		var userCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
			in.ActorID, in.TenantID,
		).Scan(&userCount).Error; err != nil {
			return fmt.Errorf("selfservice: submit change verify user: %w", err)
		}
		if userCount == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO self_service_change_requests
			   (id, tenant_id, employee_id, requested_by_user_id, target_type,
			    changes_json, changes_sensitive_enc, status)
			 VALUES (?, ?, ?, ?, ?, ?::jsonb, ?, ?)`,
			req.ID, req.TenantID, req.EmployeeID, req.RequestedByUserID,
			req.TargetType, req.ChangesJSON, req.ChangesSensitiveEnc, req.Status,
		).Error; err != nil {
			return fmt.Errorf("selfservice: submit change insert: %w", err)
		}

		// Audit: opaque request ID only — never PII or decrypted values.
		idStr := req.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "selfservice_change.submitted",
			ResourceType: "self_service_change_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("selfservice: submit change audit: %w", err)
		}

		// Submit to the approval engine in the SAME transaction so the approval
		// request and the link UPDATE are atomic (no orphan on rollback).
		// PayloadJSON contains reference IDs only — no PII.
		approvalReq, submitErr := s.approvalSvc.SubmitTx(tx, approval.SubmitInput{
			TenantID:    in.TenantID,
			ActorID:     in.ActorID,
			RequestType: "selfservice_" + in.TargetType,
			SubjectRef:  req.ID.String(),
			PayloadJSON: []byte(`{"change_request_id":"` + req.ID.String() + `"}`),
			IP:          in.IP,
		})
		if submitErr != nil {
			// No route configured — request stays pending without a link.
			if errors.Is(submitErr, approval.ErrRouteNotFound) || errors.Is(submitErr, approval.ErrRouteEmpty) {
				return nil
			}
			return fmt.Errorf("selfservice: submit change to approval: %w", submitErr)
		}

		if err := tx.Exec(
			`UPDATE self_service_change_requests
			 SET approval_request_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			approvalReq.ID, req.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("selfservice: submit change link approval: %w", err)
		}
		req.ApprovalRequestID = &approvalReq.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Never expose ciphertext to callers.
	req.ChangesSensitiveEnc = nil
	return &req, nil
}

// ApproveChangeRequestInput holds fields for approving a change request and
// reflecting it to the master.
type ApproveChangeRequestInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	RequestID uuid.UUID
	IP        *string
}

// ApproveChangeRequest transitions a pending change request to approved and
// reflects the (non-sensitive) profile changes to the employees master in the
// SAME transaction.  Rejected/returned requests are never reflected.
//
// Only employee_profile changes (last_name/first_name) are reflected to the
// employees master here; other target types are recorded as approved for the
// downstream domain to consume.  This keeps the master write narrow and audited.
func (s *Service) ApproveChangeRequest(ctx context.Context, in ApproveChangeRequestInput) (*ChangeRequest, error) {
	var out ChangeRequest
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var req ChangeRequest
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, requested_by_user_id, target_type,
			        changes_json, approval_request_id, status, reflected_at,
			        created_at, updated_at
			 FROM self_service_change_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("selfservice: approve load request: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrNotFound
		}
		if !isChangeTransitionAllowed(req.Status, ChangeStatusApproved) {
			return fmt.Errorf("%w: %s → approved", ErrInvalidTransition, req.Status)
		}

		// Reflect employee_profile changes to the master (single tx).
		if req.TargetType == TargetEmployeeProfile {
			if err := reflectEmployeeProfile(tx, in.TenantID, req.EmployeeID, req.ChangesJSON); err != nil {
				return err
			}
		}

		res := tx.Exec(
			`UPDATE self_service_change_requests
			 SET status = ?, reflected_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			ChangeStatusApproved, in.RequestID, in.TenantID, ChangeStatusPending,
		)
		if res.Error != nil {
			return fmt.Errorf("selfservice: approve update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrInvalidTransition
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, requested_by_user_id, target_type,
			        changes_json, approval_request_id, status, reflected_at,
			        created_at, updated_at
			 FROM self_service_change_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("selfservice: approve re-read: %w", err)
		}

		idStr := in.RequestID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "selfservice_change.approved",
			ResourceType: "self_service_change_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// reflectEmployeeProfile applies last_name/first_name updates from changes_json
// to the employees master.  Only present keys are updated.
func reflectEmployeeProfile(tx *gorm.DB, tenantID, employeeID uuid.UUID, changesJSON []byte) error {
	var changes struct {
		LastName  *string `json:"last_name"`
		FirstName *string `json:"first_name"`
	}
	if len(changesJSON) > 0 {
		if err := json.Unmarshal(changesJSON, &changes); err != nil {
			return fmt.Errorf("%w: changes_json: %v", ErrValidation, err)
		}
	}
	if changes.LastName == nil && changes.FirstName == nil {
		return nil
	}

	// Build a parameterised UPDATE touching only the present columns.
	sets := make([]string, 0, 2)
	args := make([]any, 0, 4)
	if changes.LastName != nil {
		sets = append(sets, "last_name = ?")
		args = append(args, *changes.LastName)
	}
	if changes.FirstName != nil {
		sets = append(sets, "first_name = ?")
		args = append(args, *changes.FirstName)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, employeeID, tenantID)

	res := tx.Exec(
		`UPDATE employees SET `+strings.Join(sets, ", ")+
			` WHERE id = ? AND tenant_id = ?`,
		args...,
	)
	if res.Error != nil {
		return fmt.Errorf("selfservice: reflect employee profile: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// RejectChangeRequestInput holds fields for rejecting a change request.
type RejectChangeRequestInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	RequestID uuid.UUID
	IP        *string
}

// RejectChangeRequest transitions a pending change request to rejected.
// The master is NEVER modified for a rejected request.
func (s *Service) RejectChangeRequest(ctx context.Context, in RejectChangeRequestInput) (*ChangeRequest, error) {
	var out ChangeRequest
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var current struct {
			Status string `gorm:"column:status"`
		}
		if err := tx.Raw(
			`SELECT status FROM self_service_change_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&current).Error; err != nil {
			return fmt.Errorf("selfservice: reject read: %w", err)
		}
		if current.Status == "" {
			return ErrNotFound
		}
		if !isChangeTransitionAllowed(current.Status, ChangeStatusRejected) {
			return fmt.Errorf("%w: %s → rejected", ErrInvalidTransition, current.Status)
		}

		res := tx.Exec(
			`UPDATE self_service_change_requests
			 SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND status = ?`,
			ChangeStatusRejected, in.RequestID, in.TenantID, ChangeStatusPending,
		)
		if res.Error != nil {
			return fmt.Errorf("selfservice: reject update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrInvalidTransition
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, requested_by_user_id, target_type,
			        changes_json, approval_request_id, status, reflected_at,
			        created_at, updated_at
			 FROM self_service_change_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("selfservice: reject re-read: %w", err)
		}

		idStr := in.RequestID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "selfservice_change.rejected",
			ResourceType: "self_service_change_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetChangeRequestInput holds parameters for fetching a change request.
// ReadSensitive must be true to receive the decrypted sensitive diff; the
// service re-validates the selfservice:read_sensitive permission.
type GetChangeRequestInput struct {
	TenantID      uuid.UUID
	ActorID       uuid.UUID
	RequestID     uuid.UUID
	ReadSensitive bool
	IP            *string
}

// GetChangeRequest fetches a change request.  The decrypted sensitive diff is
// returned as a separate value only when the caller holds
// selfservice:read_sensitive (multi-layer permission enforcement).
func (s *Service) GetChangeRequest(ctx context.Context, in GetChangeRequestInput) (*ChangeRequest, []byte, error) {
	var req ChangeRequest
	var permitted bool
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if in.ReadSensitive {
			perms, err := platformauth.LoadUserPermissions(tx, in.TenantID, in.ActorID)
			if err != nil {
				return fmt.Errorf("selfservice: get change load perms: %w", err)
			}
			if !platformauth.HasPermission(perms, permReadSensitive) {
				return ErrForbidden
			}
			permitted = true
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, requested_by_user_id, target_type,
			        changes_json, changes_sensitive_enc, approval_request_id,
			        status, reflected_at, created_at, updated_at
			 FROM self_service_change_requests
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.RequestID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("selfservice: get change: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	var sensitivePlaintext []byte
	if permitted && len(req.ChangesSensitiveEnc) > 0 {
		plain, err := crypto.Decrypt(req.ChangesSensitiveEnc)
		if err != nil {
			return nil, nil, fmt.Errorf("selfservice: decrypt sensitive change: %w", err)
		}
		sensitivePlaintext = plain
	}
	req.ChangesSensitiveEnc = nil
	return &req, sensitivePlaintext, nil
}

// ListChangeRequests returns change requests for a tenant filtered by optional
// employee and status.
func (s *Service) ListChangeRequests(ctx context.Context, tenantID uuid.UUID, employeeID *uuid.UUID, status string) ([]ChangeRequest, error) {
	var reqs []ChangeRequest
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, employee_id, requested_by_user_id, target_type,
		             changes_json, approval_request_id, status, reflected_at,
		             created_at, updated_at
		      FROM self_service_change_requests
		      WHERE tenant_id = ?`
		args := []any{tenantID}
		if employeeID != nil {
			q += ` AND employee_id = ?`
			args = append(args, *employeeID)
		}
		if status != "" {
			q += ` AND status = ?`
			args = append(args, status)
		}
		q += ` ORDER BY created_at DESC`
		return tx.Raw(q, args...).Scan(&reqs).Error
	})
	if err != nil {
		return nil, err
	}
	return reqs, nil
}

// ===========================================================================
// CSV bulk import
// ===========================================================================

// RowError is one validation error for a CSV row.
type RowError struct {
	RowNumber int    `json:"row_number"`
	Field     string `json:"field"`
	Message   string `json:"message"`
}

// ImportResult summarises a validation or apply run.
type ImportResult struct {
	Job         ImportJob
	RowErrors   []RowError
	TotalRows   int
	ValidRows   int
	ErrorRows   int
	AppliedRows int
}

// ValidateCSVInput holds fields for a CSV dry-run validation.
type ValidateCSVInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	ImportType string
	Encoding   string
	// CSVData is the raw CSV bytes (in the declared Encoding).
	CSVData []byte
	IP      *string
}

// ValidateCSV runs the dry-run (validate-only) phase.  It records a job with
// mode=dry_run and per-row validation results but makes NO change to the
// employees/departments masters.  A row-numbered error report is returned.
func (s *Service) ValidateCSV(ctx context.Context, in ValidateCSVInput) (*ImportResult, error) {
	return s.runImport(ctx, importParams{
		tenantID:   in.TenantID,
		actorID:    in.ActorID,
		importType: in.ImportType,
		mode:       ModeDryRun,
		encoding:   in.Encoding,
		csvData:    in.CSVData,
		ip:         in.IP,
	})
}

// ApplyCSVInput holds fields for a CSV apply run.
type ApplyCSVInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	ImportType  string
	Encoding    string
	ApplyPolicy string
	CSVData     []byte
	IP          *string
}

// ApplyCSV runs the apply phase.  With apply_policy=all_or_nothing the whole
// import is rejected (no rows applied) when any row is invalid.  With
// skip_errors the valid rows are applied and invalid rows are skipped.  Upsert
// is keyed by the tenant-unique code; cross-tenant keys cannot be created
// (RLS + explicit tenant_id).  The entire run is atomic within one transaction.
func (s *Service) ApplyCSV(ctx context.Context, in ApplyCSVInput) (*ImportResult, error) {
	policy := in.ApplyPolicy
	if policy == "" {
		policy = PolicyAllOrNothing
	}
	if policy != PolicyAllOrNothing && policy != PolicySkipErrors {
		return nil, fmt.Errorf("%w: invalid apply_policy %q", ErrValidation, policy)
	}
	return s.runImport(ctx, importParams{
		tenantID:    in.TenantID,
		actorID:     in.ActorID,
		importType:  in.ImportType,
		mode:        ModeApply,
		encoding:    in.Encoding,
		applyPolicy: policy,
		csvData:     in.CSVData,
		ip:          in.IP,
	})
}

type importParams struct {
	tenantID    uuid.UUID
	actorID     uuid.UUID
	importType  string
	mode        string
	encoding    string
	applyPolicy string
	csvData     []byte
	ip          *string
}

// parsedRow is one decoded CSV record with its 1-based data-row number.
type parsedRow struct {
	rowNumber int
	values    map[string]string
}

// runImport is the shared implementation for ValidateCSV and ApplyCSV.
func (s *Service) runImport(ctx context.Context, p importParams) (*ImportResult, error) {
	if p.importType != ImportTypeEmployees && p.importType != ImportTypeDepartments {
		return nil, fmt.Errorf("%w: invalid import_type %q", ErrValidation, p.importType)
	}
	encoding := p.encoding
	if encoding == "" {
		encoding = EncodingUTF8
	}
	if encoding != EncodingUTF8 && encoding != EncodingShiftJIS {
		return nil, fmt.Errorf("%w: invalid encoding %q", ErrValidation, encoding)
	}

	// Decode bytes → UTF-8 string and parse CSV up-front (no DB resources yet).
	utf8Data, err := decodeCSVBytes(p.csvData, encoding)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	rows, err := parseCSV(utf8Data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	result := &ImportResult{TotalRows: len(rows)}

	err = s.tdb.WithinTenant(ctx, p.tenantID, func(tx *gorm.DB) error {
		// Verify the uploading user belongs to this tenant.
		var userCount int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
			p.actorID, p.tenantID,
		).Scan(&userCount).Error; err != nil {
			return fmt.Errorf("selfservice: import verify user: %w", err)
		}
		if userCount == 0 {
			return ErrNotFound
		}

		jobID := uuid.New()
		job := ImportJob{
			ID:               jobID,
			TenantID:         p.tenantID,
			ImportType:       p.importType,
			Mode:             p.mode,
			ApplyPolicy:      orDefault(p.applyPolicy, PolicyAllOrNothing),
			Encoding:         encoding,
			Status:           JobStatusValidating,
			TotalRows:        len(rows),
			UploadedByUserID: p.actorID,
		}
		if err := tx.Exec(
			`INSERT INTO csv_import_jobs
			   (id, tenant_id, import_type, mode, apply_policy, encoding, status,
			    total_rows, uploaded_by_user_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			job.ID, job.TenantID, job.ImportType, job.Mode, job.ApplyPolicy,
			job.Encoding, job.Status, job.TotalRows, job.UploadedByUserID,
		).Error; err != nil {
			return fmt.Errorf("selfservice: import insert job: %w", err)
		}

		// Validate every row and persist per-row results.
		seenCodes := map[string]bool{}
		for _, r := range rows {
			rowErrs := validateRow(p.importType, r, seenCodes)
			status := RowValid
			if len(rowErrs) > 0 {
				status = RowInvalid
				result.ErrorRows++
			} else {
				result.ValidRows++
			}
			result.RowErrors = append(result.RowErrors, rowErrs...)

			rawJSON, _ := json.Marshal(r.values)
			errsJSON, _ := json.Marshal(rowErrs)
			if err := tx.Exec(
				`INSERT INTO csv_import_rows
				   (id, tenant_id, job_id, row_number, raw_data_json,
				    validation_status, errors_json, applied)
				 VALUES (?, ?, ?, ?, ?::jsonb, ?, ?::jsonb, false)`,
				uuid.New(), p.tenantID, jobID, r.rowNumber, rawJSON, status, errsJSON,
			).Error; err != nil {
				return fmt.Errorf("selfservice: import insert row %d: %w", r.rowNumber, err)
			}
		}

		// Dry-run: never touch the master; just finalise the job as validated.
		if p.mode == ModeDryRun {
			return finaliseJob(tx, p.tenantID, jobID, JobStatusValidated, result, p.actorID, p.ip, "csv_import.validated")
		}

		// Apply: all_or_nothing rejects when any row is invalid.
		if job.ApplyPolicy == PolicyAllOrNothing && result.ErrorRows > 0 {
			// Mark job failed; do NOT apply any row (atomic rejection).
			return finaliseJob(tx, p.tenantID, jobID, JobStatusFailed, result, p.actorID, p.ip, "csv_import.rejected")
		}

		// Apply valid rows (skip invalid). Cross-tenant keys are impossible
		// because every write is scoped to p.tenantID under RLS.
		for _, r := range rows {
			rowErrs := validateRow(p.importType, r, nil) // re-validate cheaply (no dup map needed for apply decisions)
			if len(rowErrs) > 0 {
				continue // skip invalid rows
			}
			if err := applyRow(tx, p.tenantID, p.importType, r); err != nil {
				return err
			}
			result.AppliedRows++
			if err := tx.Exec(
				`UPDATE csv_import_rows SET applied = true, updated_at = now()
				 WHERE tenant_id = ? AND job_id = ? AND row_number = ?`,
				p.tenantID, jobID, r.rowNumber,
			).Error; err != nil {
				return fmt.Errorf("selfservice: import mark applied row %d: %w", r.rowNumber, err)
			}
		}

		return finaliseJob(tx, p.tenantID, jobID, JobStatusCompleted, result, p.actorID, p.ip, "csv_import.applied")
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// finaliseJob updates the job counters/status, re-reads it into the result, and
// records the audit entry — all within the caller's transaction.
func finaliseJob(tx *gorm.DB, tenantID, jobID uuid.UUID, status string, result *ImportResult, actorID uuid.UUID, ip *string, action string) error {
	if err := tx.Exec(
		`UPDATE csv_import_jobs
		 SET status = ?, success_rows = ?, error_rows = ?, completed_at = now()
		 WHERE id = ? AND tenant_id = ?`,
		status, result.AppliedRowsOrValid(), result.ErrorRows, jobID, tenantID,
	).Error; err != nil {
		return fmt.Errorf("selfservice: import finalise job: %w", err)
	}
	if err := tx.Raw(
		`SELECT id, tenant_id, import_type, mode, apply_policy, encoding, status,
		        total_rows, success_rows, error_rows, uploaded_by_user_id,
		        created_at, completed_at
		 FROM csv_import_jobs WHERE id = ? AND tenant_id = ? LIMIT 1`,
		jobID, tenantID,
	).Scan(&result.Job).Error; err != nil {
		return fmt.Errorf("selfservice: import re-read job: %w", err)
	}
	idStr := jobID.String()
	return audit.Record(tx, audit.Entry{
		TenantID:     tenantID,
		UserID:       &actorID,
		Action:       action,
		ResourceType: "csv_import_job",
		ResourceID:   &idStr,
		IP:           ip,
	})
}

// AppliedRowsOrValid returns AppliedRows for apply runs, ValidRows for dry-runs.
func (r *ImportResult) AppliedRowsOrValid() int {
	if r.AppliedRows > 0 {
		return r.AppliedRows
	}
	return r.ValidRows
}

// applyRow upserts one validated CSV row into the target master.  Upsert is keyed
// by the tenant-unique code (employee_code / department code) so re-imports update
// in place.  Writes are tenant-scoped, preventing cross-tenant key creation.
func applyRow(tx *gorm.DB, tenantID uuid.UUID, importType string, r parsedRow) error {
	switch importType {
	case ImportTypeEmployees:
		code := strings.TrimSpace(r.values["employee_code"])
		lastName := strings.TrimSpace(r.values["last_name"])
		firstName := strings.TrimSpace(r.values["first_name"])
		empType := orDefault(strings.TrimSpace(r.values["employment_type"]), "full_time")
		if err := tx.Exec(
			`INSERT INTO employees
			   (id, tenant_id, employee_code, last_name, first_name, employment_type, status)
			 VALUES (?, ?, ?, ?, ?, ?, 'active')
			 ON CONFLICT (tenant_id, employee_code) DO UPDATE
			   SET last_name = EXCLUDED.last_name,
			       first_name = EXCLUDED.first_name,
			       employment_type = EXCLUDED.employment_type,
			       updated_at = now()`,
			uuid.New(), tenantID, code, lastName, firstName, empType,
		).Error; err != nil {
			return fmt.Errorf("selfservice: apply employee row: %w", err)
		}
	case ImportTypeDepartments:
		code := strings.TrimSpace(r.values["code"])
		name := strings.TrimSpace(r.values["name"])
		// departments has no UNIQUE(tenant_id, code) constraint, so ON CONFLICT
		// cannot be used.  Manual upsert keyed by (tenant_id, code): update when
		// an existing row matches, otherwise insert.  Writes are tenant-scoped,
		// preventing cross-tenant key creation.
		res := tx.Exec(
			`UPDATE departments SET name = ?, updated_at = now()
			 WHERE tenant_id = ? AND code = ?`,
			name, tenantID, code,
		)
		if res.Error != nil {
			return fmt.Errorf("selfservice: apply department update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			if err := tx.Exec(
				`INSERT INTO departments (id, tenant_id, code, name)
				 VALUES (?, ?, ?, ?)`,
				uuid.New(), tenantID, code, name,
			).Error; err != nil {
				return fmt.Errorf("selfservice: apply department insert: %w", err)
			}
		}
	}
	return nil
}

// ===========================================================================
// Document store
// ===========================================================================

// CreateDocumentInput holds fields for creating a logical document plus its
// first version.
//
// Content, when non-empty, is encrypted before storage and never persisted as
// plaintext.  StorageKey references the object-storage location of the real
// binary (the DB stores metadata + key reference only).
type CreateDocumentInput struct {
	TenantID           uuid.UUID
	ActorID            uuid.UUID
	OwnerEmployeeID    *uuid.UUID
	Category           string
	Title              string
	RetentionLabel     string
	RetentionExpiresOn *time.Time
	LegalHold          bool
	// Version fields:
	StorageKey string
	Filename   string
	MimeType   string
	EncKeyRef  string
	Content    []byte
	IP         *string
}

// CreateDocument creates a document and its first version (version_no=1),
// setting current_version_id.  MIME/size/extension are validated and the content
// hash is computed for tamper detection (CMP-006).
func (s *Service) CreateDocument(ctx context.Context, in CreateDocumentInput) (*Document, *DocumentVersion, error) {
	if !allowedDocumentCategory(in.Category) {
		return nil, nil, fmt.Errorf("%w: invalid category %q", ErrValidation, in.Category)
	}
	if err := validateUpload(in.MimeType, in.Filename, in.Content); err != nil {
		return nil, nil, err
	}
	if err := s.scanner.Scan(in.Content); err != nil {
		return nil, nil, fmt.Errorf("%w: virus scan rejected upload: %v", ErrValidation, err)
	}

	// Compute hash over the original content (tamper detection / 真実性).
	contentHash := sha256Hex(in.Content)

	// Encrypt inline content before opening the tx.
	var contentEnc []byte
	if len(in.Content) > 0 {
		var err error
		contentEnc, err = crypto.Encrypt(in.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("selfservice: encrypt document content: %w", err)
		}
	}

	retentionLabel := orDefault(in.RetentionLabel, "unspecified")

	doc := Document{
		ID:                 uuid.New(),
		TenantID:           in.TenantID,
		OwnerEmployeeID:    in.OwnerEmployeeID,
		Category:           in.Category,
		Title:              in.Title,
		RetentionLabel:     retentionLabel,
		RetentionExpiresOn: in.RetentionExpiresOn,
		LegalHold:          in.LegalHold,
	}
	ver := DocumentVersion{
		ID:               uuid.New(),
		TenantID:         in.TenantID,
		DocumentID:       doc.ID,
		VersionNo:        1,
		StorageKey:       in.StorageKey,
		ContentHash:      contentHash,
		MimeType:         in.MimeType,
		SizeBytes:        int64(len(in.Content)),
		EncKeyRef:        in.EncKeyRef,
		ContentEnc:       contentEnc,
		UploadedByUserID: in.ActorID,
	}

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify owner employee (when set) and uploader belong to this tenant.
		if in.OwnerEmployeeID != nil {
			var cnt int64
			if err := tx.Raw(
				`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
				*in.OwnerEmployeeID, in.TenantID,
			).Scan(&cnt).Error; err != nil {
				return fmt.Errorf("selfservice: create doc verify owner: %w", err)
			}
			if cnt == 0 {
				return ErrNotFound
			}
		}
		var userCnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM users WHERE id = ? AND tenant_id = ?`,
			in.ActorID, in.TenantID,
		).Scan(&userCnt).Error; err != nil {
			return fmt.Errorf("selfservice: create doc verify user: %w", err)
		}
		if userCnt == 0 {
			return ErrNotFound
		}

		if err := tx.Exec(
			`INSERT INTO documents
			   (id, tenant_id, owner_employee_id, category, title, retention_label,
			    retention_expires_on, legal_hold)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			doc.ID, doc.TenantID, doc.OwnerEmployeeID, doc.Category, doc.Title,
			doc.RetentionLabel, doc.RetentionExpiresOn, doc.LegalHold,
		).Error; err != nil {
			return fmt.Errorf("selfservice: create doc insert: %w", err)
		}
		if err := tx.Exec(
			`INSERT INTO document_versions
			   (id, tenant_id, document_id, version_no, storage_key, content_hash,
			    mime_type, size_bytes, enc_key_ref, content_enc, uploaded_by_user_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ver.ID, ver.TenantID, ver.DocumentID, ver.VersionNo, ver.StorageKey,
			ver.ContentHash, ver.MimeType, ver.SizeBytes, ver.EncKeyRef,
			ver.ContentEnc, ver.UploadedByUserID,
		).Error; err != nil {
			return fmt.Errorf("selfservice: create doc version insert: %w", err)
		}
		if err := tx.Exec(
			`UPDATE documents SET current_version_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			ver.ID, doc.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("selfservice: create doc set current: %w", err)
		}
		doc.CurrentVersionID = &ver.ID

		idStr := doc.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "document.created",
			ResourceType: "document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	ver.ContentEnc = nil // never expose ciphertext
	return &doc, &ver, nil
}

// AddVersionInput holds fields for uploading a new version of a document.
type AddVersionInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	DocumentID uuid.UUID
	StorageKey string
	Filename   string
	MimeType   string
	EncKeyRef  string
	Content    []byte
	IP         *string
}

// AddVersion appends a new version to a document, retains all prior versions,
// and switches current_version_id to the new version.  Each version records the
// reviser (uploaded_by_user_id) and timestamp.
func (s *Service) AddVersion(ctx context.Context, in AddVersionInput) (*DocumentVersion, error) {
	if err := validateUpload(in.MimeType, in.Filename, in.Content); err != nil {
		return nil, err
	}
	if err := s.scanner.Scan(in.Content); err != nil {
		return nil, fmt.Errorf("%w: virus scan rejected upload: %v", ErrValidation, err)
	}
	contentHash := sha256Hex(in.Content)

	var contentEnc []byte
	if len(in.Content) > 0 {
		var err error
		contentEnc, err = crypto.Encrypt(in.Content)
		if err != nil {
			return nil, fmt.Errorf("selfservice: encrypt document content: %w", err)
		}
	}

	var ver DocumentVersion
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify the document exists in this tenant and lock to avoid version race.
		var doc struct {
			ID               uuid.UUID `gorm:"column:id"`
			LogicallyExpired bool      `gorm:"column:logically_expired"`
		}
		if err := tx.Raw(
			`SELECT id, logically_expired FROM documents
			 WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.DocumentID, in.TenantID,
		).Scan(&doc).Error; err != nil {
			return fmt.Errorf("selfservice: add version load doc: %w", err)
		}
		if doc.ID == uuid.Nil {
			return ErrNotFound
		}
		if doc.LogicallyExpired {
			return fmt.Errorf("%w: cannot add version to logically expired document", ErrInvalidTransition)
		}

		// Next version number = max(version_no)+1 under the row lock (TOCTOU-safe).
		var maxVer int
		if err := tx.Raw(
			`SELECT COALESCE(MAX(version_no), 0) FROM document_versions
			 WHERE document_id = ? AND tenant_id = ?`,
			in.DocumentID, in.TenantID,
		).Scan(&maxVer).Error; err != nil {
			return fmt.Errorf("selfservice: add version max: %w", err)
		}

		ver = DocumentVersion{
			ID:               uuid.New(),
			TenantID:         in.TenantID,
			DocumentID:       in.DocumentID,
			VersionNo:        maxVer + 1,
			StorageKey:       in.StorageKey,
			ContentHash:      contentHash,
			MimeType:         in.MimeType,
			SizeBytes:        int64(len(in.Content)),
			EncKeyRef:        in.EncKeyRef,
			ContentEnc:       contentEnc,
			UploadedByUserID: in.ActorID,
		}
		if err := tx.Exec(
			`INSERT INTO document_versions
			   (id, tenant_id, document_id, version_no, storage_key, content_hash,
			    mime_type, size_bytes, enc_key_ref, content_enc, uploaded_by_user_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ver.ID, ver.TenantID, ver.DocumentID, ver.VersionNo, ver.StorageKey,
			ver.ContentHash, ver.MimeType, ver.SizeBytes, ver.EncKeyRef,
			ver.ContentEnc, ver.UploadedByUserID,
		).Error; err != nil {
			return fmt.Errorf("selfservice: add version insert: %w", err)
		}
		// Switch current version; old versions remain as history.
		if err := tx.Exec(
			`UPDATE documents SET current_version_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			ver.ID, in.DocumentID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("selfservice: add version set current: %w", err)
		}

		idStr := in.DocumentID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "document.version_added",
			ResourceType: "document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	ver.ContentEnc = nil
	return &ver, nil
}

// GetDocument fetches a single document by ID within the tenant.
func (s *Service) GetDocument(ctx context.Context, tenantID, id uuid.UUID) (*Document, error) {
	var doc Document
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, owner_employee_id, category, title,
			        current_version_id, retention_label, retention_expires_on,
			        logically_expired, legal_hold, created_at, updated_at
			 FROM documents WHERE id = ? AND tenant_id = ? LIMIT 1`,
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

// ListDocuments returns documents for a tenant filtered by optional category.
// Logically expired documents are excluded unless includeExpired is true.
func (s *Service) ListDocuments(ctx context.Context, tenantID uuid.UUID, category string, includeExpired bool) ([]Document, error) {
	var docs []Document
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		q := `SELECT id, tenant_id, owner_employee_id, category, title,
		             current_version_id, retention_label, retention_expires_on,
		             logically_expired, legal_hold, created_at, updated_at
		      FROM documents WHERE tenant_id = ?`
		args := []any{tenantID}
		if category != "" {
			q += ` AND category = ?`
			args = append(args, category)
		}
		if !includeExpired {
			q += ` AND logically_expired = false`
		}
		q += ` ORDER BY created_at DESC`
		return tx.Raw(q, args...).Scan(&docs).Error
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// ListVersions returns all versions of a document, newest first.
func (s *Service) ListVersions(ctx context.Context, tenantID, documentID uuid.UUID) ([]DocumentVersion, error) {
	var vers []DocumentVersion
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Verify document belongs to tenant first.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM documents WHERE id = ? AND tenant_id = ?`,
			documentID, tenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("selfservice: list versions verify doc: %w", err)
		}
		if cnt == 0 {
			return ErrNotFound
		}
		return tx.Raw(
			`SELECT id, tenant_id, document_id, version_no, storage_key, content_hash,
			        mime_type, size_bytes, enc_key_ref, uploaded_by_user_id, uploaded_at
			 FROM document_versions
			 WHERE document_id = ? AND tenant_id = ?
			 ORDER BY version_no DESC`,
			documentID, tenantID,
		).Scan(&vers).Error
	})
	if err != nil {
		return nil, err
	}
	return vers, nil
}

// DownloadVersionInput holds fields for downloading (reading) a document version.
type DownloadVersionInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	VersionID uuid.UUID
	IP        *string
}

// DownloadResult is returned by DownloadVersion.
type DownloadResult struct {
	Version DocumentVersion
	// Content is the decrypted inline content (when content_enc was stored).
	Content []byte
	// HashVerified reports whether content_hash matches the decrypted content.
	HashVerified bool
}

// DownloadVersion reads a document version with access control and records the
// access in the audit log (resource_id = document opaque UUID).  When inline
// content is present it is decrypted and its hash is verified (tamper detection).
// A logically expired document is not downloadable.
func (s *Service) DownloadVersion(ctx context.Context, in DownloadVersionInput) (*DownloadResult, error) {
	var ver DocumentVersion
	var docID uuid.UUID
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		if err := tx.Raw(
			`SELECT id, tenant_id, document_id, version_no, storage_key, content_hash,
			        mime_type, size_bytes, enc_key_ref, content_enc,
			        uploaded_by_user_id, uploaded_at
			 FROM document_versions
			 WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.VersionID, in.TenantID,
		).Scan(&ver).Error; err != nil {
			return fmt.Errorf("selfservice: download load version: %w", err)
		}
		if ver.ID == uuid.Nil {
			return ErrNotFound
		}
		docID = ver.DocumentID

		// Reject download of logically expired documents (retention enforcement).
		var expired bool
		if err := tx.Raw(
			`SELECT logically_expired FROM documents WHERE id = ? AND tenant_id = ? LIMIT 1`,
			ver.DocumentID, in.TenantID,
		).Scan(&expired).Error; err != nil {
			return fmt.Errorf("selfservice: download check expiry: %w", err)
		}
		if expired {
			return fmt.Errorf("%w: document is logically expired", ErrForbidden)
		}

		// Audit the access — resource_id is the opaque document UUID, no PII.
		idStr := ver.DocumentID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "document.downloaded",
			ResourceType: "document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	_ = docID

	res := &DownloadResult{Version: ver}
	if len(ver.ContentEnc) > 0 {
		plain, err := crypto.Decrypt(ver.ContentEnc)
		if err != nil {
			return nil, fmt.Errorf("selfservice: decrypt document content: %w", err)
		}
		res.Content = plain
		res.HashVerified = sha256Hex(plain) == ver.ContentHash
	}
	res.Version.ContentEnc = nil
	return res, nil
}

// ExpireDocumentInput holds fields for logically expiring a document.
type ExpireDocumentInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	DocumentID uuid.UUID
	IP         *string
}

// ExpireDocument marks a document logically expired (access-restricted).  It
// NEVER deletes any row.  A legal-hold document cannot be expired (ErrLegalHold)
// to prevent erroneous removal of legally required records.
func (s *Service) ExpireDocument(ctx context.Context, in ExpireDocumentInput) (*Document, error) {
	var out Document
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		var doc struct {
			ID        uuid.UUID `gorm:"column:id"`
			LegalHold bool      `gorm:"column:legal_hold"`
		}
		if err := tx.Raw(
			`SELECT id, legal_hold FROM documents WHERE id = ? AND tenant_id = ? FOR UPDATE`,
			in.DocumentID, in.TenantID,
		).Scan(&doc).Error; err != nil {
			return fmt.Errorf("selfservice: expire load doc: %w", err)
		}
		if doc.ID == uuid.Nil {
			return ErrNotFound
		}
		// Legal-hold guard: legally required documents cannot be expired.
		if doc.LegalHold {
			return ErrLegalHold
		}

		res := tx.Exec(
			`UPDATE documents SET logically_expired = true, updated_at = now()
			 WHERE id = ? AND tenant_id = ? AND legal_hold = false`,
			in.DocumentID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("selfservice: expire update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, owner_employee_id, category, title,
			        current_version_id, retention_label, retention_expires_on,
			        logically_expired, legal_hold, created_at, updated_at
			 FROM documents WHERE id = ? AND tenant_id = ? LIMIT 1`,
			in.DocumentID, in.TenantID,
		).Scan(&out).Error; err != nil {
			return fmt.Errorf("selfservice: expire re-read: %w", err)
		}

		idStr := in.DocumentID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "document.expired",
			ResourceType: "document",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ===========================================================================
// Helpers
// ===========================================================================

func isValidTargetType(t string) bool {
	switch t {
	case TargetEmployeeProfile, TargetEmergencyError, TargetCommute, TargetBankAccount, TargetDependents:
		return true
	default:
		return false
	}
}

func allowedDocumentCategory(c string) bool {
	switch c {
	case CategoryContract, CategoryCertificate, CategoryPayslip, CategoryMisc:
		return true
	default:
		return false
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// validateUpload enforces the MIME / size / extension allow-lists.
func validateUpload(mimeType, filename string, content []byte) error {
	if len(content) > maxDocumentBytes {
		return fmt.Errorf("%w: content exceeds %d bytes", ErrValidation, maxDocumentBytes)
	}
	if mimeType != "" && !allowedMIMETypes[mimeType] {
		return fmt.Errorf("%w: MIME type %q not allowed", ErrValidation, mimeType)
	}
	if filename != "" {
		ext := strings.ToLower(filename[strings.LastIndex(filename, "."):])
		if strings.LastIndex(filename, ".") < 0 || !allowedExtensions[ext] {
			return fmt.Errorf("%w: file extension not allowed", ErrValidation)
		}
	}
	return nil
}

// decodeCSVBytes converts raw CSV bytes in the given encoding to UTF-8.
func decodeCSVBytes(data []byte, encoding string) ([]byte, error) {
	switch encoding {
	case EncodingShiftJIS:
		dec := japanese.ShiftJIS.NewDecoder()
		out, _, err := transform.Bytes(dec, data)
		if err != nil {
			return nil, fmt.Errorf("shift_jis decode: %w", err)
		}
		return out, nil
	default:
		return data, nil
	}
}

// parseCSV parses UTF-8 CSV bytes into header-mapped rows.  The first record is
// treated as the header; subsequent records become parsedRow with 1-based
// row numbers.
func parseCSV(data []byte) ([]parsedRow, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // allow ragged rows; reported as validation errors
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	for i := range header {
		header[i] = strings.TrimSpace(strings.ToLower(header[i]))
	}

	var rows []parsedRow
	rowNum := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read row: %w", err)
		}
		rowNum++
		values := make(map[string]string, len(header))
		for i, h := range header {
			if i < len(rec) {
				values[h] = rec[i]
			} else {
				values[h] = ""
			}
		}
		rows = append(rows, parsedRow{rowNumber: rowNum, values: values})
	}
	return rows, nil
}

// validateRow validates one CSV row for the given import type and accumulates
// row-numbered errors.  seenCodes (when non-nil) is used to detect duplicate
// codes within the same file.
func validateRow(importType string, r parsedRow, seenCodes map[string]bool) []RowError {
	var errs []RowError
	switch importType {
	case ImportTypeEmployees:
		code := strings.TrimSpace(r.values["employee_code"])
		if code == "" {
			errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "employee_code", Message: "required"})
		} else if seenCodes != nil {
			if seenCodes[code] {
				errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "employee_code", Message: "duplicate code in file"})
			}
			seenCodes[code] = true
		}
		if strings.TrimSpace(r.values["last_name"]) == "" {
			errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "last_name", Message: "required"})
		}
		if strings.TrimSpace(r.values["first_name"]) == "" {
			errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "first_name", Message: "required"})
		}
		if et := strings.TrimSpace(r.values["employment_type"]); et != "" && !validEmploymentType(et) {
			errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "employment_type", Message: "invalid value"})
		}
	case ImportTypeDepartments:
		code := strings.TrimSpace(r.values["code"])
		if code == "" {
			errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "code", Message: "required"})
		} else if seenCodes != nil {
			if seenCodes[code] {
				errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "code", Message: "duplicate code in file"})
			}
			seenCodes[code] = true
		}
		if strings.TrimSpace(r.values["name"]) == "" {
			errs = append(errs, RowError{RowNumber: r.rowNumber, Field: "name", Message: "required"})
		}
	}
	return errs
}

func validEmploymentType(t string) bool {
	switch t {
	case "full_time", "part_time", "contract", "dispatch":
		return true
	default:
		return false
	}
}
