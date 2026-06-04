// Package retention implements background data-retention and disposal jobs
// required by:
//   - LM-021: マイナンバー廃棄 (retention_until 到来 / 退職者)
//   - LM-054: 法定三帳簿の保存年限管理
//   - NFR-011: 退職者データ削除ポリシー (法定保存年限を優先)
//   - CORE-009: ファイル保持期間 (selfservice documents)
//
// Design principles:
//   - Each runner function is a pure Go function with a clear signature.
//     There is no global state; callers inject *tenantdb.TenantDB.
//   - Jobs record their execution in retention_job_runs within a
//     WithinTenant transaction for audit traceability (no PII stored).
//   - PII / 個人番号の平文・復号値はこのパッケージのいかなる箇所にも
//     記録・ログ・返却しない。
//   - Disposal operations call the existing service-layer functions
//     (mynumber.Service.Dispose) which enforce RBAC, hash chains, and
//     ciphertext destruction.  This job layer does NOT bypass service logic.
//   - Legal note: retention periods are configuration values (not hardcoded).
//     All period thresholds must be confirmed against the latest statutory
//     guidance and 社労士/弁護士 review.  This package is not legal advice.
//
// Cron scaffold:
//   - Run() is the entry point for a single sweep of all four sub-jobs.
//   - A real cron infrastructure (OS cron, Kubernetes CronJob, pg_cron, or
//     a job queue) must be wired by the operator.  cmd/retention/main.go
//     provides a one-shot binary entry point.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/mynumber"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// ---------------------------------------------------------------------------
// Job name constants — must match the CHECK constraint in migration 00030.
// ---------------------------------------------------------------------------

const (
	JobMyNumberDisposal   = "mynumber_disposal"
	JobLedgerRetention    = "ledger_retention"
	JobEmployeeDataPolicy = "employee_data_policy"
	JobDocumentExpiry     = "document_expiry"
)

// Run statuses — must match the CHECK constraint in migration 00030.
const (
	runStatusRunning   = "running"
	runStatusCompleted = "completed"
	runStatusFailed    = "failed"
	runStatusSkipped   = "skipped"
)

// ---------------------------------------------------------------------------
// Config — external policy values (never hardcoded legal periods).
// ---------------------------------------------------------------------------

// Config holds the legal-policy thresholds for the retention jobs.
//
// All duration values are legal/operational policy — they MUST be supplied
// by the operator via configuration and confirmed with 社労士/弁護士 review.
// Zero values are treated as "use per-tenant ledger_settings" where available.
type Config struct {
	// MyNumber disposal options.

	// DisposalMethod is the method used when auto-disposing マイナンバー records
	// (ciphertext_deleted | key_destroyed).  Required.
	DisposalMethod string

	// Ledger retention: fallback if per-tenant ledger_settings is not found
	// (e.g. tenant has not configured ledger_settings yet).
	// 0 means "do not fallback — skip tenant".
	LedgerRetentionFallbackYears int

	// Employee data policy: if an employee has status terminated/left/leaving
	// and left the organisation more than EmployeeDataGracePeriod ago (measured
	// from the latest contract end_date), schedule a disposal audit entry.
	// Law: ledger retention takes priority — we only flag employees whose
	// ledger retention has also expired.  Grace period is configurable.
	// 0 means "do not enforce — skip".
	EmployeeDataGracePeriod time.Duration
}

// DefaultConfig returns a safe Config with sane defaults for CI/test.
// Operators MUST override these values for production deployments after
// 社労士/弁護士 review.
func DefaultConfig() Config {
	return Config{
		DisposalMethod:               mynumber.MethodCiphertextDeleted,
		LedgerRetentionFallbackYears: 0, // safe default: skip unknown tenants
		EmployeeDataGracePeriod:      0, // safe default: disabled
	}
}

// ---------------------------------------------------------------------------
// Runner — top-level orchestrator
// ---------------------------------------------------------------------------

// Runner holds the dependencies for all retention sub-jobs.
type Runner struct {
	tdb           *tenantdb.TenantDB
	mynumberSvc   *mynumber.Service
	cfg           Config
	logger        *slog.Logger
	// systemActorID is used as the "actor" for automated disposal audit rows.
	// It MUST be a valid user ID in the users table (a dedicated service account)
	// that holds the mynumber:reveal permission for the relevant tenant(s).
	// If uuid.Nil is passed, disposal operations will fail with ErrForbidden
	// (the RBAC check requires a valid actor; the job must be given a real
	// service-account ID).
	//
	// SECURITY: this ID must NOT be a super-admin with blanket permissions.
	// Provision a dedicated, purpose-limited service account.
	systemActorID uuid.UUID
}

// NewRunner constructs a Runner.
//
// systemActorID must be the UUID of a system service-account user that has
// been granted mynumber:reveal and ledger:write in every tenant that this
// job will run against.
func NewRunner(
	tdb *tenantdb.TenantDB,
	mynumberSvc *mynumber.Service,
	cfg Config,
	logger *slog.Logger,
	systemActorID uuid.UUID,
) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		tdb:           tdb,
		mynumberSvc:   mynumberSvc,
		cfg:           cfg,
		logger:        logger,
		systemActorID: systemActorID,
	}
}

// Run executes all four sub-jobs for a single tenantID.  It is the entry
// point for cron-driven or one-shot invocation.
//
// Error handling: each sub-job records its own failure in retention_job_runs
// and returns a descriptive error.  Run returns the first non-nil error but
// continues executing the remaining sub-jobs so a failure in one does not
// prevent the others from running.
func (r *Runner) Run(ctx context.Context, tenantID uuid.UUID) error {
	r.logger.Info("retention run started", "tenant_id", tenantID)

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	record(r.RunMyNumberDisposal(ctx, tenantID))
	record(r.RunLedgerRetention(ctx, tenantID))
	record(r.RunEmployeeDataPolicy(ctx, tenantID))
	record(r.RunDocumentExpiry(ctx, tenantID))

	if firstErr != nil {
		r.logger.Error("retention run finished with errors", "tenant_id", tenantID, "first_error", firstErr)
	} else {
		r.logger.Info("retention run completed", "tenant_id", tenantID)
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// Sub-job: MyNumber disposal (LM-021)
// ---------------------------------------------------------------------------

// RunMyNumberDisposal finds all active/expired マイナンバー records whose
// retention_until has passed (保管期限到来) and disposes them:
//   - Updates status to disposed.
//   - Destroys the ciphertext (number_enc → NULL / 復号不能化).
//   - Inserts a disposal certificate row.
//   - Writes a tamper-evident mynumber_access_logs entry via Service.Dispose.
//
// SECURITY:
//   - Disposal is delegated to mynumber.Service.Dispose which re-validates
//     RBAC (mynumber:reveal required), enforces transitions, and handles
//     ciphertext destruction atomically.  No ciphertext is touched here.
//   - The systemActorID must hold mynumber:reveal for this tenant.
//   - 個人番号の平文・復号値はここでは一切扱わない。
//
// LEGAL NOTE: retention_until values are set at collection time from
// tenant-configured policy.  Operators must ensure they are correct.
func (r *Runner) RunMyNumberDisposal(ctx context.Context, tenantID uuid.UUID) error {
	runID := uuid.New()
	startedAt := time.Now().UTC()

	if err := r.insertJobRun(ctx, tenantID, runID, JobMyNumberDisposal); err != nil {
		r.logger.Error("retention: failed to create job run record",
			"job", JobMyNumberDisposal, "tenant_id", tenantID, "error", err)
		// Do not abort — proceed with the actual work even if run recording fails.
	}

	affected, skipped, runErr := r.doMyNumberDisposal(ctx, tenantID)

	status := runStatusCompleted
	if runErr != nil {
		status = runStatusFailed
		r.logger.Error("retention: mynumber disposal failed",
			"tenant_id", tenantID, "error", runErr)
	} else {
		r.logger.Info("retention: mynumber disposal done",
			"tenant_id", tenantID, "disposed", affected, "skipped", skipped)
	}

	_ = r.finaliseJobRun(ctx, tenantID, runID, status, affected, skipped, startedAt, runErr)
	return runErr
}

// doMyNumberDisposal is the inner implementation (separated for testability).
// Returns (affected, skipped, error).
//
// SECURITY: The plaintext 個人番号 is never loaded here.  We only handle
// opaque record IDs and pass them to mynumber.Service.Dispose which owns
// all crypto operations and audit logging.
func (r *Runner) doMyNumberDisposal(ctx context.Context, tenantID uuid.UUID) (affected, skipped int, _ error) {
	// Fetch IDs of records that should be disposed:
	//   - status IN ('active', 'expired')
	//   - retention_until IS NOT NULL AND retention_until < now()
	//
	// We fetch only the ID and employee_id — no NumberEnc, no PII.
	type candidateRow struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	var candidates []candidateRow

	err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id
			 FROM mynumber_records
			 WHERE tenant_id = ?
			   AND status IN ('active', 'expired')
			   AND retention_until IS NOT NULL
			   AND retention_until < now()
			 ORDER BY retention_until
			 LIMIT 1000`,
			tenantID,
		).Scan(&candidates).Error
	})
	if err != nil {
		return 0, 0, fmt.Errorf("retention: mynumber disposal query: %w", err)
	}

	for _, c := range candidates {
		in := mynumber.DisposeInput{
			TenantID: tenantID,
			ActorID:  r.systemActorID,
			RecordID: c.ID,
			Reason:   mynumber.ReasonRetentionExpired,
			Method:   r.cfg.DisposalMethod,
		}
		if _, err := r.mynumberSvc.Dispose(ctx, in); err != nil {
			// Log the opaque ID only — never log PII or the number.
			r.logger.Warn("retention: mynumber disposal skipped for record",
				"tenant_id", tenantID,
				"record_id", c.ID, // opaque UUID only
				"error", err)
			skipped++
			continue
		}
		affected++
	}

	return affected, skipped, nil
}

// ---------------------------------------------------------------------------
// Sub-job: Ledger retention (LM-054)
// ---------------------------------------------------------------------------

// RunLedgerRetention finds all ledger records (worker_rosters, wage_ledgers,
// attendance_books) whose retention_until has passed, marks them logically
// expired in a retention_expired flag (not physical deletion — 真実性保持),
// and writes an audit entry.
//
// LEGAL NOTE: 法定三帳簿の保存年限 is currently 5 years (経過措置3年あり).
// retention_until is stored per-record at finalise time based on
// ledger_settings.default_retention_years.  Operators must keep
// ledger_settings current to follow amendments.
func (r *Runner) RunLedgerRetention(ctx context.Context, tenantID uuid.UUID) error {
	runID := uuid.New()
	startedAt := time.Now().UTC()

	if err := r.insertJobRun(ctx, tenantID, runID, JobLedgerRetention); err != nil {
		r.logger.Error("retention: failed to create job run record",
			"job", JobLedgerRetention, "tenant_id", tenantID, "error", err)
	}

	affected, skipped, runErr := r.doLedgerRetention(ctx, tenantID)

	status := runStatusCompleted
	if runErr != nil {
		status = runStatusFailed
		r.logger.Error("retention: ledger retention failed",
			"tenant_id", tenantID, "error", runErr)
	} else {
		r.logger.Info("retention: ledger retention done",
			"tenant_id", tenantID, "expired", affected, "skipped", skipped)
	}

	_ = r.finaliseJobRun(ctx, tenantID, runID, status, affected, skipped, startedAt, runErr)
	return runErr
}

// doLedgerRetention marks ledger records as retention-expired.
//
// For each of the three ledger tables (worker_rosters, wage_ledgers,
// attendance_books) the job:
//  1. Fetches rows where retention_until < now() AND retention_expired = false
//     (idempotent: already-expired rows are not reprocessed).
//  2. Sets retention_expired = true in a single UPDATE within a tenant-scoped
//     transaction (migration 00035 adds the column).
//  3. Writes a tamper-evident audit entry via audit.Record.
//
// Physical deletion is NOT performed — 真実性 (電子帳簿保存法) requires the
// records to remain readable; only the logical expiry flag is set.
//
// IDEMPOTENCY: the query filters on retention_expired = false so that
// re-running the job after a partial failure does not re-audit already-expired
// rows.  Each (id, table) pair is processed at most once per row lifetime.
func (r *Runner) doLedgerRetention(ctx context.Context, tenantID uuid.UUID) (affected, skipped int, _ error) {
	type expiredRow struct {
		ID    uuid.UUID `gorm:"column:id"`
		Table string
	}

	// Collect expired-but-not-yet-flagged records from each of the three tables.
	// Filter: retention_expired = false ensures idempotency across re-runs.
	var allExpired []expiredRow

	tables := []string{"worker_rosters", "wage_ledgers", "attendance_books"}
	for _, tbl := range tables {
		var rows []struct {
			ID uuid.UUID `gorm:"column:id"`
		}
		err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			//nolint:gosec // table name is from a hardcoded slice above, not user input
			return tx.Raw(
				fmt.Sprintf(
					`SELECT id FROM %s
					 WHERE tenant_id = ?
					   AND retention_until IS NOT NULL
					   AND retention_until < now()
					   AND retention_expired = false
					 ORDER BY retention_until
					 LIMIT 500`,
					tbl,
				),
				tenantID,
			).Scan(&rows).Error
		})
		if err != nil {
			r.logger.Warn("retention: ledger retention query failed",
				"table", tbl, "tenant_id", tenantID, "error", err)
			skipped++
			continue
		}
		for _, row := range rows {
			allExpired = append(allExpired, expiredRow{ID: row.ID, Table: tbl})
		}
	}

	// For each expired row: set retention_expired = true and record an audit event.
	// Both operations run within a single WithinTenant transaction so that the flag
	// update and the audit entry are always consistent (atomic commit-or-rollback).
	for _, row := range allExpired {
		idStr := row.ID.String()
		err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			// Set the retention_expired flag.
			// The WHERE clause re-checks tenant_id and retention_expired = false
			// to guard against a concurrent run updating the same row (TOCTOU).
			//nolint:gosec // table name is from a hardcoded slice above, not user input
			res := tx.Exec(
				fmt.Sprintf(
					`UPDATE %s
					 SET retention_expired = true, updated_at = now()
					 WHERE id = ? AND tenant_id = ?
					   AND retention_expired = false`,
					row.Table,
				),
				row.ID, tenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("retention: ledger retention update: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				// Row was already processed by a concurrent run — skip silently.
				return nil
			}

			// Write tamper-evident audit entry.
			// SECURITY: only the opaque row ID is stored — no PII / wage data.
			return audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &r.systemActorID,
				Action:       "retention.ledger_retention_expired",
				ResourceType: row.Table,
				ResourceID:   &idStr,
			})
		})
		if err != nil {
			r.logger.Warn("retention: ledger retention update failed",
				"tenant_id", tenantID, "table", row.Table, "id", row.ID, "error", err)
			skipped++
			continue
		}
		affected++
	}

	return affected, skipped, nil
}

// ---------------------------------------------------------------------------
// Sub-job: Employee data policy (NFR-011)
// ---------------------------------------------------------------------------

// RunEmployeeDataPolicy identifies terminated/left employees whose
// associated ledger retention has also expired, and emits an audit event to
// signal that the record is eligible for operator review / anonymisation.
//
// POLICY: physical deletion of employee master data requires manual operator
// confirmation and depends on the full ledger retention sweep above having
// completed.  This job only flags eligibility; it does NOT delete rows.
//
// LEGAL NOTE: NFR-011 states that statutory retention takes priority over
// internal privacy preferences.  Operators must configure
// EmployeeDataGracePeriod after consulting 社労士/弁護士.
func (r *Runner) RunEmployeeDataPolicy(ctx context.Context, tenantID uuid.UUID) error {
	if r.cfg.EmployeeDataGracePeriod == 0 {
		r.logger.Info("retention: employee data policy disabled (grace_period=0), skipping",
			"tenant_id", tenantID)
		return r.insertAndFinaliseJobRun(ctx, tenantID, JobEmployeeDataPolicy,
			runStatusSkipped, 0, 0, nil)
	}

	runID := uuid.New()
	startedAt := time.Now().UTC()

	if err := r.insertJobRun(ctx, tenantID, runID, JobEmployeeDataPolicy); err != nil {
		r.logger.Error("retention: failed to create job run record",
			"job", JobEmployeeDataPolicy, "tenant_id", tenantID, "error", err)
	}

	affected, skipped, runErr := r.doEmployeeDataPolicy(ctx, tenantID)

	status := runStatusCompleted
	if runErr != nil {
		status = runStatusFailed
		r.logger.Error("retention: employee data policy failed",
			"tenant_id", tenantID, "error", runErr)
	} else {
		r.logger.Info("retention: employee data policy done",
			"tenant_id", tenantID, "flagged", affected, "skipped", skipped)
	}

	_ = r.finaliseJobRun(ctx, tenantID, runID, status, affected, skipped, startedAt, runErr)
	return runErr
}

// doEmployeeDataPolicy flags terminated/left employees eligible for review.
//
// Eligibility criteria:
//   - employees.status IN ('terminated', 'left')
//   - The employee's most recent employment contract has end_date < (now() - grace_period)
//   - No active ledger records remain (all retention_until < now() or no ledgers)
//
// Only an opaque employee ID appears in audit logs — never name, email, or PII.
func (r *Runner) doEmployeeDataPolicy(ctx context.Context, tenantID uuid.UUID) (affected, skipped int, _ error) {
	cutoff := time.Now().UTC().Add(-r.cfg.EmployeeDataGracePeriod)

	type candidateRow struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	var candidates []candidateRow

	// Find terminated/left employees whose latest contract ended before cutoff.
	// We join on employment_contracts to get the latest end_date.
	err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT e.id
			 FROM employees e
			 LEFT JOIN LATERAL (
			     SELECT MAX(end_date) AS latest_end
			     FROM employment_contracts
			     WHERE tenant_id = e.tenant_id AND employee_id = e.id
			       AND end_date IS NOT NULL
			 ) c ON true
			 WHERE e.tenant_id = ?
			   AND e.status IN ('terminated', 'left')
			   AND (c.latest_end IS NULL OR c.latest_end < ?)
			 ORDER BY e.id
			 LIMIT 500`,
			tenantID, cutoff,
		).Scan(&candidates).Error
	})
	if err != nil {
		return 0, 0, fmt.Errorf("retention: employee data policy query: %w", err)
	}

	for _, c := range candidates {
		// Further check: no ledger records with active retention.
		var activeLedgerCount int64
		err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			return tx.Raw(
				`SELECT COUNT(*)
				 FROM (
				     SELECT id FROM worker_rosters
				     WHERE tenant_id = ? AND employee_id = ?
				       AND (retention_until IS NULL OR retention_until >= now())
				     UNION ALL
				     SELECT id FROM wage_ledgers
				     WHERE tenant_id = ? AND employee_id = ?
				       AND (retention_until IS NULL OR retention_until >= now())
				     UNION ALL
				     SELECT id FROM attendance_books
				     WHERE tenant_id = ? AND employee_id = ?
				       AND (retention_until IS NULL OR retention_until >= now())
				 ) sub`,
				tenantID, c.ID,
				tenantID, c.ID,
				tenantID, c.ID,
			).Scan(&activeLedgerCount).Error
		})
		if err != nil {
			r.logger.Warn("retention: employee data policy ledger check failed",
				"tenant_id", tenantID, "employee_id", c.ID, "error", err)
			skipped++
			continue
		}
		if activeLedgerCount > 0 {
			// Active ledgers remain — data must be retained; skip.
			skipped++
			continue
		}

		// Eligible: emit audit entry flagging this employee for review.
		// SECURITY: only the opaque employee ID is stored — never name/email/PII.
		idStr := c.ID.String()
		err = r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			return audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &r.systemActorID,
				Action:       "retention.employee_data_eligible_for_review",
				ResourceType: "employee",
				ResourceID:   &idStr,
			})
		})
		if err != nil {
			r.logger.Warn("retention: employee data policy audit failed",
				"tenant_id", tenantID, "employee_id", c.ID, "error", err)
			skipped++
			continue
		}
		affected++
	}

	return affected, skipped, nil
}

// ---------------------------------------------------------------------------
// Sub-job: Document expiry (CORE-009)
// ---------------------------------------------------------------------------

// RunDocumentExpiry finds documents in the selfservice documents table whose
// retention_expires_on has passed and legal_hold is false, marks them
// logically expired (logically_expired = true), and writes an audit entry.
//
// Physical deletion is NOT performed — documents are only logically expired,
// preserving 真実性 (truthfulness).  The object storage key is retained for
// operator-initiated GC.
//
// Legal-hold documents are never touched regardless of retention_expires_on.
func (r *Runner) RunDocumentExpiry(ctx context.Context, tenantID uuid.UUID) error {
	runID := uuid.New()
	startedAt := time.Now().UTC()

	if err := r.insertJobRun(ctx, tenantID, runID, JobDocumentExpiry); err != nil {
		r.logger.Error("retention: failed to create job run record",
			"job", JobDocumentExpiry, "tenant_id", tenantID, "error", err)
	}

	affected, skipped, runErr := r.doDocumentExpiry(ctx, tenantID)

	status := runStatusCompleted
	if runErr != nil {
		status = runStatusFailed
		r.logger.Error("retention: document expiry failed",
			"tenant_id", tenantID, "error", runErr)
	} else {
		r.logger.Info("retention: document expiry done",
			"tenant_id", tenantID, "expired", affected, "skipped", skipped)
	}

	_ = r.finaliseJobRun(ctx, tenantID, runID, status, affected, skipped, startedAt, runErr)
	return runErr
}

// doDocumentExpiry sets logically_expired = true for documents past their
// retention date.
func (r *Runner) doDocumentExpiry(ctx context.Context, tenantID uuid.UUID) (affected, skipped int, _ error) {
	type candidateRow struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	var candidates []candidateRow

	err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id
			 FROM documents
			 WHERE tenant_id = ?
			   AND legal_hold = false
			   AND logically_expired = false
			   AND retention_expires_on IS NOT NULL
			   AND retention_expires_on < now()
			 ORDER BY retention_expires_on
			 LIMIT 500`,
			tenantID,
		).Scan(&candidates).Error
	})
	if err != nil {
		return 0, 0, fmt.Errorf("retention: document expiry query: %w", err)
	}

	for _, c := range candidates {
		err := r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
			res := tx.Exec(
				`UPDATE documents
				 SET logically_expired = true, updated_at = now()
				 WHERE id = ? AND tenant_id = ?
				   AND legal_hold = false
				   AND logically_expired = false`,
				c.ID, tenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("retention: document expire update: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				// Race: another process expired it, or legal_hold was set — skip.
				return nil
			}

			// Audit: only the opaque document ID is recorded — never title/PII.
			idStr := c.ID.String()
			return audit.Record(tx, audit.Entry{
				TenantID:     tenantID,
				UserID:       &r.systemActorID,
				Action:       "retention.document_logically_expired",
				ResourceType: "document",
				ResourceID:   &idStr,
			})
		})
		if err != nil {
			r.logger.Warn("retention: document expiry failed for document",
				"tenant_id", tenantID, "document_id", c.ID, "error", err)
			skipped++
			continue
		}
		affected++
	}

	return affected, skipped, nil
}

// ---------------------------------------------------------------------------
// Internal helpers — job-run record management
// ---------------------------------------------------------------------------

// jobRunRow is the GORM model for retention_job_runs.
type jobRunRow struct {
	ID            uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID      uuid.UUID  `gorm:"column:tenant_id"`
	JobName       string     `gorm:"column:job_name"`
	Status        string     `gorm:"column:status"`
	AffectedCount int        `gorm:"column:affected_count"`
	SkippedCount  int        `gorm:"column:skipped_count"`
	Notes         *string    `gorm:"column:notes"`
	StartedAt     time.Time  `gorm:"column:started_at"`
	FinishedAt    *time.Time `gorm:"column:finished_at"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at"`
}

// TableName maps jobRunRow to retention_job_runs.
func (jobRunRow) TableName() string { return "retention_job_runs" }

// insertJobRun creates a "running" job run record.
func (r *Runner) insertJobRun(ctx context.Context, tenantID, runID uuid.UUID, jobName string) error {
	return r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`INSERT INTO retention_job_runs (id, tenant_id, job_name, status, started_at)
			 VALUES (?, ?, ?, ?, now())`,
			runID, tenantID, jobName, runStatusRunning,
		).Error
	})
}

// finaliseJobRun updates the job run record with completion status.
// errors from this helper are logged but do not propagate (best-effort).
func (r *Runner) finaliseJobRun(
	ctx context.Context,
	tenantID, runID uuid.UUID,
	status string,
	affected, skipped int,
	startedAt time.Time,
	jobErr error,
) error {
	var notes *string
	if jobErr != nil {
		s := jobErr.Error()
		notes = &s
	}
	_ = startedAt // kept for potential latency logging expansion
	return r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`UPDATE retention_job_runs
			 SET status = ?, affected_count = ?, skipped_count = ?,
			     notes = ?, finished_at = now(), updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			status, affected, skipped, notes, runID, tenantID,
		).Error
	})
}

// insertAndFinaliseJobRun creates and immediately finalises a run record.
// Used for skipped runs that need no separate start/finish tracking.
func (r *Runner) insertAndFinaliseJobRun(
	ctx context.Context,
	tenantID uuid.UUID,
	jobName, status string,
	affected, skipped int,
	jobErr error,
) error {
	var notes *string
	if jobErr != nil {
		s := jobErr.Error()
		notes = &s
	}
	return r.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Exec(
			`INSERT INTO retention_job_runs
			   (id, tenant_id, job_name, status, affected_count, skipped_count, notes,
			    started_at, finished_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, now(), now())`,
			uuid.New(), tenantID, jobName, status, affected, skipped, notes,
		).Error
	})
}
