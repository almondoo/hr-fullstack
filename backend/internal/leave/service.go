package leave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/approval"
	"github.com/your-org/hr-saas/internal/notification"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrSettingNotFound     = errors.New("leave: settings not found for this tenant")
	ErrRequestNotFound     = errors.New("leave: request not found")
	ErrGrantNotFound       = errors.New("leave: grant not found")
	ErrEmployeeNotFound    = errors.New("leave: employee not found in this tenant")
	ErrInsufficientBalance = errors.New("leave: insufficient annual leave balance")
	ErrInvalidTransition   = errors.New("leave: invalid request status transition")
	ErrInvalidLeaveType    = errors.New("leave: invalid leave type")
	ErrInvalidDates        = errors.New("leave: start_date must not be after end_date")
)

// validLeaveTypes is the allow-list for leave_type values.
var validLeaveTypes = map[string]bool{
	LeaveTypeAnnual:     true,
	LeaveTypeSpecial:    true,
	LeaveTypeCondolence: true,
	LeaveTypeMaternity:  true,
	LeaveTypeChildcare:  true,
	LeaveTypeCare:       true,
	LeaveTypeAbsence:    true,
}

// allowedRequestTransitions defines legal status moves for leave_requests.
// Only allow-listed transitions are accepted.
var allowedRequestTransitions = map[string]map[string]bool{
	RequestStatusPending: {
		RequestStatusApproved:  true,
		RequestStatusRejected:  true,
		RequestStatusCancelled: true,
	},
	// An approved leave request may be cancelled (e.g. employee changes plans
	// before the leave starts).  Cancellation reverses the balance allocation.
	RequestStatusApproved: {
		RequestStatusCancelled: true,
		RequestStatusRejected:  true,
	},
}

func isRequestTransitionAllowed(from, to string) bool {
	if next, ok := allowedRequestTransitions[from]; ok {
		return next[to]
	}
	return false
}

// Service provides business logic for the leave domain.
type Service struct {
	tdb         *tenantdb.TenantDB
	approvalSvc *approval.Service
}

// NewService constructs a Service.
func NewService(tdb *tenantdb.TenantDB, approvalSvc *approval.Service) *Service {
	return &Service{tdb: tdb, approvalSvc: approvalSvc}
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// GetSettings returns the leave settings for a tenant, returning
// ErrSettingNotFound when no row exists.
func (s *Service) GetSettings(ctx context.Context, tenantID uuid.UUID) (*Setting, error) {
	var setting Setting
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, base_date_rule, grant_table_json,
			        proportional_table_json, five_day_obligation_threshold,
			        expiry_months, updated_at
			 FROM leave_settings
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

// UpsertSettingsInput holds the validated fields for creating or updating settings.
type UpsertSettingsInput struct {
	TenantID                   uuid.UUID
	ActorID                    uuid.UUID
	BaseDateRule               string
	GrantTableJSON             []byte
	ProportionalTableJSON      []byte
	FiveDayObligationThreshold int
	ExpiryMonths               int
	IP                         *string
}

// UpsertSettings creates or updates leave settings for a tenant.
func (s *Service) UpsertSettings(ctx context.Context, in UpsertSettingsInput) (*Setting, error) {
	var setting Setting
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Check whether a row already exists.
		var existing Setting
		if err := tx.Raw(
			`SELECT id FROM leave_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("leave: upsert settings read: %w", err)
		}

		// Determine whether the caller supplied non-empty JSON tables.
		// When omitted (nil / "null" / "[]"), preserve the DB default on INSERT
		// and leave the column unchanged on UPDATE.  This prevents callers from
		// accidentally overwriting carefully-maintained grant tables by omitting
		// the field.
		grantJSONSupplied := len(in.GrantTableJSON) > 0 &&
			string(in.GrantTableJSON) != "null" &&
			string(in.GrantTableJSON) != "[]"
		propJSONSupplied := len(in.ProportionalTableJSON) > 0 &&
			string(in.ProportionalTableJSON) != "null" &&
			string(in.ProportionalTableJSON) != "[]"

		if existing.ID == uuid.Nil {
			// INSERT: include grant_table_json / proportional_table_json only when
			// supplied; otherwise let the column DEFAULT (migration values) apply.
			newID := uuid.New()
			var insertErr error
			switch {
			case grantJSONSupplied && propJSONSupplied:
				insertErr = tx.Exec(
					`INSERT INTO leave_settings
					   (id, tenant_id, base_date_rule, grant_table_json,
					    proportional_table_json, five_day_obligation_threshold,
					    expiry_months)
					 VALUES (?, ?, ?, ?::jsonb, ?::jsonb, ?, ?)`,
					newID, in.TenantID, in.BaseDateRule,
					in.GrantTableJSON, in.ProportionalTableJSON,
					in.FiveDayObligationThreshold, in.ExpiryMonths,
				).Error
			case grantJSONSupplied:
				insertErr = tx.Exec(
					`INSERT INTO leave_settings
					   (id, tenant_id, base_date_rule, grant_table_json,
					    five_day_obligation_threshold, expiry_months)
					 VALUES (?, ?, ?, ?::jsonb, ?, ?)`,
					newID, in.TenantID, in.BaseDateRule,
					in.GrantTableJSON,
					in.FiveDayObligationThreshold, in.ExpiryMonths,
				).Error
			default:
				// Use column DEFAULTs for both JSON tables.
				insertErr = tx.Exec(
					`INSERT INTO leave_settings
					   (id, tenant_id, base_date_rule,
					    five_day_obligation_threshold, expiry_months)
					 VALUES (?, ?, ?, ?, ?)`,
					newID, in.TenantID, in.BaseDateRule,
					in.FiveDayObligationThreshold, in.ExpiryMonths,
				).Error
			}
			if insertErr != nil {
				return fmt.Errorf("leave: upsert settings insert: %w", insertErr)
			}
		} else {
			// UPDATE: build the SET clause conditionally.
			var updateErr error
			if grantJSONSupplied && propJSONSupplied {
				updateErr = tx.Exec(
					`UPDATE leave_settings
					 SET base_date_rule = ?, grant_table_json = ?::jsonb,
					     proportional_table_json = ?::jsonb,
					     five_day_obligation_threshold = ?,
					     expiry_months = ?, updated_at = now()
					 WHERE tenant_id = ?`,
					in.BaseDateRule, in.GrantTableJSON, in.ProportionalTableJSON,
					in.FiveDayObligationThreshold, in.ExpiryMonths,
					in.TenantID,
				).Error
			} else {
				// Skip JSON columns that were not supplied.
				updateErr = tx.Exec(
					`UPDATE leave_settings
					 SET base_date_rule = ?,
					     five_day_obligation_threshold = ?,
					     expiry_months = ?, updated_at = now()
					 WHERE tenant_id = ?`,
					in.BaseDateRule,
					in.FiveDayObligationThreshold, in.ExpiryMonths,
					in.TenantID,
				).Error
			}
			if updateErr != nil {
				return fmt.Errorf("leave: upsert settings update: %w", updateErr)
			}
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, base_date_rule, grant_table_json,
			        proportional_table_json, five_day_obligation_threshold,
			        expiry_months, updated_at
			 FROM leave_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&setting).Error; err != nil {
			return fmt.Errorf("leave: upsert settings re-read: %w", err)
		}

		idStr := setting.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "leave_settings.upserted",
			ResourceType: "leave_settings",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("leave: upsert settings audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &setting, nil
}

// ---------------------------------------------------------------------------
// Grant management (LM-040)
// ---------------------------------------------------------------------------

// GrantLeaveInput holds the parameters for a manual grant operation.
type GrantLeaveInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	GrantDate  time.Time
	Days       float64
	Source     string
	IP         *string
}

// GrantLeave records a leave grant using the tenant's settings for expiry computation.
// This covers both manual grants and the result of computing annual/proportional grants.
func (s *Service) GrantLeave(ctx context.Context, in GrantLeaveInput) (*Grant, error) {
	if in.Days <= 0 {
		return nil, fmt.Errorf("leave: grant days must be positive")
	}

	var grant Grant
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("leave: grant verify employee: %w", err)
		}
		if cnt == 0 {
			return ErrEmployeeNotFound
		}

		// Load expiry from settings.  The ID == uuid.Nil check reliably detects
		// a missing row regardless of what expiry_months is configured to (including
		// future cases where 0 might be a valid value).
		var setting Setting
		if err := tx.Raw(
			`SELECT id, expiry_months FROM leave_settings WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&setting).Error; err != nil {
			return fmt.Errorf("leave: grant load settings: %w", err)
		}
		if setting.ID == uuid.Nil {
			return ErrSettingNotFound
		}

		expiresOn := in.GrantDate.AddDate(0, setting.ExpiryMonths, 0)
		source := in.Source
		if source == "" {
			source = GrantSourceAnnual
		}

		grant = Grant{
			ID:         uuid.New(),
			TenantID:   in.TenantID,
			EmployeeID: in.EmployeeID,
			GrantDate:  in.GrantDate,
			Days:       in.Days,
			Source:     source,
			ExpiresOn:  expiresOn,
		}

		if err := tx.Exec(
			`INSERT INTO leave_grants
			   (id, tenant_id, employee_id, grant_date, days, source, expires_on)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			grant.ID, grant.TenantID, grant.EmployeeID,
			grant.GrantDate, grant.Days, grant.Source, grant.ExpiresOn,
		).Error; err != nil {
			return fmt.Errorf("leave: grant insert: %w", err)
		}

		idStr := grant.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "leave_grant.created",
			ResourceType: "leave_grant",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("leave: grant audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &grant, nil
}

// ComputeAnnualGrantInput holds parameters for computing a grant from the table.
type ComputeAnnualGrantInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	HiredOn    time.Time
	GrantDate  time.Time
	WeeklyDays *float64 // nil = standard (full-time); set for proportional grant
	IP         *string
}

// ComputeAndGrantAnnual computes the appropriate grant (standard or proportional)
// from the tenant's settings tables and calls GrantLeave.
//
// LEGAL NOTICE: The grant table values come from leave_settings.grant_table_json
// and proportional_table_json.  Correctness depends on the tenant having
// configured tables that reflect current Labour Standards Law.  Nothing here
// constitutes legal advice.
func (s *Service) ComputeAndGrantAnnual(ctx context.Context, in ComputeAndGrantAnnualInput) (*Grant, error) {
	// Load settings to decode the grant tables.
	setting, err := s.GetSettings(ctx, in.TenantID)
	if err != nil {
		return nil, err
	}

	tenureMonths := monthsBetween(in.HiredOn, in.GrantDate)

	var days float64

	if in.WeeklyDays != nil && *in.WeeklyDays < 5 {
		// Proportional grant path.  Pass the standard grant table so that when
		// weekly_days has no exact entry in the proportional table the fallback
		// correctly uses the full-time grant table rather than returning
		// ErrSettingNotFound (which would happen if nil were passed).
		days, err = lookupProportionalDays(setting.ProportionalTableJSON, setting.GrantTableJSON, *in.WeeklyDays, tenureMonths)
		if err != nil {
			return nil, err
		}
		if days <= 0 {
			// Below minimum tenure threshold — no grant due yet.
			return nil, nil
		}
		return s.GrantLeave(ctx, GrantLeaveInput{
			TenantID:   in.TenantID,
			ActorID:    in.ActorID,
			EmployeeID: in.EmployeeID,
			GrantDate:  in.GrantDate,
			Days:       days,
			Source:     GrantSourceProportional,
			IP:         in.IP,
		})
	}

	// Standard (full-time) grant path.
	days, err = lookupGrantDays(setting.GrantTableJSON, tenureMonths)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		// Below minimum tenure threshold — no grant due yet.
		return nil, nil
	}
	return s.GrantLeave(ctx, GrantLeaveInput{
		TenantID:   in.TenantID,
		ActorID:    in.ActorID,
		EmployeeID: in.EmployeeID,
		GrantDate:  in.GrantDate,
		Days:       days,
		Source:     GrantSourceAnnual,
		IP:         in.IP,
	})
}

// ComputeAndGrantAnnualInput holds parameters for computing and issuing a grant.
type ComputeAndGrantAnnualInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	HiredOn    time.Time
	GrantDate  time.Time
	WeeklyDays *float64
	IP         *string
}

// ListGrants returns all grants for an employee within the tenant.
func (s *Service) ListGrants(ctx context.Context, tenantID, employeeID uuid.UUID) ([]Grant, error) {
	var grants []Grant
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, grant_date, days, source, expires_on, created_at
			 FROM leave_grants
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY grant_date DESC`,
			tenantID, employeeID,
		).Scan(&grants).Error
	})
	if err != nil {
		return nil, err
	}
	return grants, nil
}

// ---------------------------------------------------------------------------
// Balance & 5-day obligation (LM-041)
// ---------------------------------------------------------------------------

// GetBalance computes the leave balance for an employee as of asOf.
// Expired grants are excluded.  Usage rows link approved requests to grants.
func (s *Service) GetBalance(ctx context.Context, tenantID, employeeID uuid.UUID, asOf time.Time) (*Balance, error) {
	var balance Balance
	balance.EmployeeID = employeeID
	balance.AsOf = asOf

	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			employeeID, tenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("leave: balance verify employee: %w", err)
		}
		if cnt == 0 {
			return ErrEmployeeNotFound
		}

		// Load all non-expired grants.
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, grant_date, days, source, expires_on, created_at
			 FROM leave_grants
			 WHERE tenant_id = ? AND employee_id = ? AND expires_on > ?
			 ORDER BY grant_date ASC`,
			tenantID, employeeID, asOf,
		).Scan(&balance.Grants).Error; err != nil {
			return fmt.Errorf("leave: balance load grants: %w", err)
		}

		for _, g := range balance.Grants {
			balance.TotalGranted += g.Days
		}

		// Sum used days via leave_usages for non-expired grants only.
		var usedRow struct {
			Total float64 `gorm:"column:total"`
		}
		if err := tx.Raw(
			`SELECT COALESCE(SUM(lu.days_used), 0) AS total
			 FROM leave_usages lu
			 JOIN leave_grants lg ON lg.id = lu.leave_grant_id AND lg.tenant_id = lu.tenant_id
			 WHERE lu.tenant_id = ?
			   AND lg.employee_id = ?
			   AND lg.expires_on > ?`,
			tenantID, employeeID, asOf,
		).Scan(&usedRow).Error; err != nil {
			return fmt.Errorf("leave: balance compute used: %w", err)
		}
		balance.TotalUsed = usedRow.Total

		if balance.TotalGranted > balance.TotalUsed {
			balance.Remaining = balance.TotalGranted - balance.TotalUsed
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return &balance, nil
}

// GetFiveDayObligation computes the 5-day obligation status for an employee
// for the grant year that contains asOf.
//
// LEGAL NOTICE: The obligation threshold (default 10 days) and required minimum
// (5 days) come from leave_settings.  Both values MUST be verified against
// current Labour Standards Law (労基法第39条第7項 等) by a qualified professional.
// The obligation year runs from the base date used for the most recent grant.
func (s *Service) GetFiveDayObligation(ctx context.Context, tenantID, employeeID uuid.UUID, asOf time.Time) (*FiveDayObligation, error) {
	setting, err := s.GetSettings(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	var obl FiveDayObligation
	obl.EmployeeID = employeeID

	err = s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		// Verify employee.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			employeeID, tenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("leave: five-day verify employee: %w", err)
		}
		if cnt == 0 {
			return ErrEmployeeNotFound
		}

		// Find the most recent grant on or before asOf to establish the grant year
		// (GrantYearStart).  The year window is grant_date to grant_date + 1 year - 1 day,
		// respecting hire_date_anniversary / fixed base_date_rule ordering.
		var latestGrant Grant
		if err := tx.Raw(
			`SELECT id, grant_date, days, expires_on
			 FROM leave_grants
			 WHERE tenant_id = ? AND employee_id = ? AND grant_date <= ?
			 ORDER BY grant_date DESC LIMIT 1`,
			tenantID, employeeID, asOf,
		).Scan(&latestGrant).Error; err != nil {
			return fmt.Errorf("leave: five-day load latest grant: %w", err)
		}

		if latestGrant.ID == uuid.Nil {
			// No grant yet — not obligated.
			obl.Obligated = false
			return nil
		}

		// Grant year: latestGrant.GrantDate to GrantDate + 1 year - 1 day
		obl.GrantYearStart = latestGrant.GrantDate
		obl.GrantYearEnd = latestGrant.GrantDate.AddDate(1, 0, -1)

		// Sum all grants whose grant_date falls within this grant year.
		// Multiple grants (e.g. fresh + carry-over on the same anniversary date,
		// or split grants) must be totalled to determine whether the employee
		// crosses the obligation threshold.  Using only latestGrant.Days would
		// under-count when more than one grant was issued in the year.
		var grantYearTotal struct {
			Total float64 `gorm:"column:total"`
		}
		if err := tx.Raw(
			`SELECT COALESCE(SUM(days), 0) AS total
			 FROM leave_grants
			 WHERE tenant_id = ? AND employee_id = ?
			   AND grant_date >= ? AND grant_date <= ?`,
			tenantID, employeeID, obl.GrantYearStart, obl.GrantYearEnd,
		).Scan(&grantYearTotal).Error; err != nil {
			return fmt.Errorf("leave: five-day sum grant year: %w", err)
		}
		obl.GrantDays = grantYearTotal.Total

		// Check whether the obligation threshold is met.
		// LEGAL NOTICE: five_day_obligation_threshold from settings; verify with amendments.
		obl.Obligated = obl.GrantDays >= float64(setting.FiveDayObligationThreshold)

		if !obl.Obligated {
			return nil
		}

		// Sum annual-leave usage within the grant year.
		var usedRow struct {
			Total float64 `gorm:"column:total"`
		}
		if err := tx.Raw(
			`SELECT COALESCE(SUM(lu.days_used), 0) AS total
			 FROM leave_usages lu
			 JOIN leave_requests lr ON lr.id = lu.leave_request_id AND lr.tenant_id = lu.tenant_id
			 JOIN leave_grants lg ON lg.id = lu.leave_grant_id AND lg.tenant_id = lu.tenant_id
			 WHERE lu.tenant_id = ?
			   AND lg.employee_id = ?
			   AND lr.leave_type = 'annual'
			   AND lr.status = 'approved'
			   AND lr.start_date >= ?
			   AND lr.start_date <= ?`,
			tenantID, employeeID, obl.GrantYearStart, obl.GrantYearEnd,
		).Scan(&usedRow).Error; err != nil {
			return fmt.Errorf("leave: five-day compute used: %w", err)
		}
		obl.UsedDays = usedRow.Total

		// The required minimum (5 days) — LEGAL NOTICE: verify this value
		// against current law.  We use a constant 5 here because it is
		// the statutory minimum; however tenants should verify this is correct.
		const fiveDayMinimum = 5.0
		if obl.UsedDays >= fiveDayMinimum {
			obl.Met = true
		} else {
			obl.ShortfallDays = fiveDayMinimum - obl.UsedDays
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return &obl, nil
}

// ---------------------------------------------------------------------------
// Leave requests (LM-042)
// ---------------------------------------------------------------------------

// CreateRequestInput holds validated parameters for a new leave request.
type CreateRequestInput struct {
	TenantID   uuid.UUID
	ActorID    uuid.UUID
	EmployeeID uuid.UUID
	LeaveType  string
	StartDate  time.Time
	EndDate    time.Time
	Days       float64
	Reason     *string
	IP         *string
}

// CreateRequest creates a leave request and submits it to the approval engine.
// For annual leave, the balance is checked as an advisory pre-check before the
// transaction; the authoritative balance enforcement occurs inside
// allocateAnnualLeave (on UpdateRequestStatus) with row-level locking.
//
// Atomicity guarantee: the leave_request INSERT, approval.SubmitTx, and the
// approval_request_id link UPDATE all execute within a single WithinTenant
// transaction.  If any step fails the entire transaction rolls back, so no
// orphaned approval_request rows or permanently-pending unlinked leave_requests
// can result.  When no approval route is configured, the request is left in
// pending status without an approval_request_id (manually approvable via
// UpdateRequestStatus).
func (s *Service) CreateRequest(ctx context.Context, in CreateRequestInput) (*Request, error) {
	if !validLeaveTypes[in.LeaveType] {
		return nil, ErrInvalidLeaveType
	}
	if in.EndDate.Before(in.StartDate) {
		return nil, ErrInvalidDates
	}
	if in.Days <= 0 {
		return nil, fmt.Errorf("leave: days must be positive")
	}

	// Advisory pre-check for annual leave.  This runs outside the main
	// transaction and may be stale under concurrent approvals; the authoritative
	// balance check happens inside allocateAnnualLeave with FOR UPDATE locking.
	// The pre-check exists to surface an obvious ErrInsufficientBalance early
	// (before inserting any rows) so callers get fast feedback in the common case.
	if in.LeaveType == LeaveTypeAnnual {
		balance, err := s.GetBalance(ctx, in.TenantID, in.EmployeeID, in.StartDate)
		if err != nil {
			return nil, err
		}
		if balance.Remaining < in.Days {
			return nil, ErrInsufficientBalance
		}
	}

	var req Request
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Verify employee belongs to this tenant.
		var cnt int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM employees WHERE id = ? AND tenant_id = ?`,
			in.EmployeeID, in.TenantID,
		).Scan(&cnt).Error; err != nil {
			return fmt.Errorf("leave: create request verify employee: %w", err)
		}
		if cnt == 0 {
			return ErrEmployeeNotFound
		}

		req = Request{
			ID:         uuid.New(),
			TenantID:   in.TenantID,
			EmployeeID: in.EmployeeID,
			LeaveType:  in.LeaveType,
			StartDate:  in.StartDate,
			EndDate:    in.EndDate,
			Days:       in.Days,
			Status:     RequestStatusPending,
			Reason:     in.Reason,
		}

		if err := tx.Exec(
			`INSERT INTO leave_requests
			   (id, tenant_id, employee_id, leave_type, start_date, end_date,
			    days, status, reason)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			req.ID, req.TenantID, req.EmployeeID,
			req.LeaveType, req.StartDate, req.EndDate,
			req.Days, req.Status, req.Reason,
		).Error; err != nil {
			return fmt.Errorf("leave: create request insert: %w", err)
		}

		idStr := req.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "leave_request.created",
			ResourceType: "leave_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("leave: create request audit: %w", err)
		}

		// Submit to the approval engine within the same transaction so that
		// the approval_request INSERT and the leave_request approval_request_id
		// link UPDATE are atomic.  If the route is not configured (ErrRouteNotFound
		// or ErrRouteEmpty) the request remains pending without a link — this is
		// the intended fallback for manually-managed tenants.
		approvalReq, submitErr := s.approvalSvc.SubmitTx(tx, approval.SubmitInput{
			TenantID:    in.TenantID,
			ActorID:     in.ActorID,
			RequestType: "leave_" + in.LeaveType,
			SubjectRef:  req.ID.String(),
			PayloadJSON: []byte(`{"leave_request_id":"` + req.ID.String() + `"}`),
			IP:          in.IP,
		})
		if submitErr != nil {
			// No route configured — leave request remains pending without a link.
			// This is not an error; the request can be approved manually.
			if errors.Is(submitErr, approval.ErrRouteNotFound) || errors.Is(submitErr, approval.ErrRouteEmpty) {
				return nil
			}
			return fmt.Errorf("leave: create request submit approval: %w", submitErr)
		}

		// Link the approval_request_id on the leave_request row.
		if err := tx.Exec(
			`UPDATE leave_requests
			 SET approval_request_id = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			approvalReq.ID, req.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("leave: create request link approval: %w", err)
		}
		req.ApprovalRequestID = &approvalReq.ID
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &req, nil
}

// GetRequest fetches a single leave request by ID.
func (s *Service) GetRequest(ctx context.Context, tenantID, id uuid.UUID) (*Request, error) {
	var req Request
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, leave_type, start_date, end_date,
			        days, status, approval_request_id, reason, created_at, updated_at
			 FROM leave_requests
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			id, tenantID,
		).Scan(&req).Error
	})
	if err != nil {
		return nil, err
	}
	if req.ID == uuid.Nil {
		return nil, ErrRequestNotFound
	}
	return &req, nil
}

// ListRequests returns all leave requests for an employee.
func (s *Service) ListRequests(ctx context.Context, tenantID, employeeID uuid.UUID) ([]Request, error) {
	var reqs []Request
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, employee_id, leave_type, start_date, end_date,
			        days, status, approval_request_id, reason, created_at, updated_at
			 FROM leave_requests
			 WHERE tenant_id = ? AND employee_id = ?
			 ORDER BY start_date DESC`,
			tenantID, employeeID,
		).Scan(&reqs).Error
	})
	if err != nil {
		return nil, err
	}
	return reqs, nil
}

// UpdateRequestStatusInput holds parameters for a status change.
type UpdateRequestStatusInput struct {
	TenantID uuid.UUID
	ID       uuid.UUID
	ActorID  uuid.UUID
	Status   string
	IP       *string
}

// UpdateRequestStatus changes the status of a leave request.
// On approval of annual leave: leave_usages rows are written to link the
// request to the FIFO-consumed grants (oldest non-expired first).
// On rejection or cancellation of an approved annual request: usages are deleted.
func (s *Service) UpdateRequestStatus(ctx context.Context, in UpdateRequestStatusInput) (*Request, error) {
	var req Request
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Load current request.
		if err := tx.Raw(
			`SELECT id, tenant_id, employee_id, leave_type, start_date, end_date,
			        days, status, approval_request_id, reason, created_at, updated_at
			 FROM leave_requests
			 WHERE id = ? AND tenant_id = ?
			 LIMIT 1`,
			in.ID, in.TenantID,
		).Scan(&req).Error; err != nil {
			return fmt.Errorf("leave: update status read: %w", err)
		}
		if req.ID == uuid.Nil {
			return ErrRequestNotFound
		}

		if !isRequestTransitionAllowed(req.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, req.Status, in.Status)
		}

		// On approval of annual leave: allocate from FIFO grants.
		if in.Status == RequestStatusApproved && req.LeaveType == LeaveTypeAnnual {
			if err := allocateAnnualLeave(tx, in.TenantID, req, req.StartDate); err != nil {
				return err
			}
		}

		// On cancel/reject of an approved annual request: release the usages.
		if req.Status == RequestStatusApproved &&
			(in.Status == RequestStatusCancelled || in.Status == RequestStatusRejected) &&
			req.LeaveType == LeaveTypeAnnual {
			if err := releaseAnnualLeave(tx, in.TenantID, req.ID); err != nil {
				return err
			}
		}

		if err := tx.Exec(
			`UPDATE leave_requests
			 SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, in.ID, in.TenantID,
		).Error; err != nil {
			return fmt.Errorf("leave: update status exec: %w", err)
		}
		req.Status = in.Status

		idStr := in.ID.String()
		var auditAction string
		switch in.Status {
		case RequestStatusApproved:
			auditAction = "leave_request.approved"
		case RequestStatusRejected:
			auditAction = "leave_request.rejected"
		case RequestStatusCancelled:
			auditAction = "leave_request.cancelled"
		default:
			auditAction = "leave_request.status_updated"
		}
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       auditAction,
			ResourceType: "leave_request",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return fmt.Errorf("leave: update status audit: %w", err)
		}

		// Outbox hook: notify the actor (HR/approver) about the leave status change.
		// The actor is the user performing the status update (approval decision, etc.).
		outboxEventType := "leave.status_updated"
		switch in.Status {
		case RequestStatusApproved:
			outboxEventType = "leave.approved"
		case RequestStatusRejected:
			outboxEventType = "leave.rejected"
		case RequestStatusCancelled:
			outboxEventType = "leave.cancelled"
		}
		if err := notification.InsertOutbox(tx, notification.InsertOutboxEntry{
			TenantID:        in.TenantID,
			EventType:       outboxEventType,
			ActorUserID:     &in.ActorID,
			RecipientUserID: in.ActorID,
			ResourceType:    "leave_request",
			ResourceID:      &in.ID,
		}); err != nil {
			return fmt.Errorf("leave: update status outbox: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// allocateAnnualLeave writes leave_usages rows consuming days FIFO from the
// oldest non-expired grant, preventing double-allocation for idempotency
// (existing usage rows for this request are checked first).
func allocateAnnualLeave(tx *gorm.DB, tenantID uuid.UUID, req Request, asOf time.Time) error {
	// Idempotency: skip if usages already exist for this request.
	var existingCnt int64
	if err := tx.Raw(
		`SELECT COUNT(1) FROM leave_usages WHERE leave_request_id = ? AND tenant_id = ?`,
		req.ID, tenantID,
	).Scan(&existingCnt).Error; err != nil {
		return fmt.Errorf("leave: allocate check existing: %w", err)
	}
	if existingCnt > 0 {
		return nil
	}

	// Load non-expired grants ordered by expires_on ASC (FIFO expiry) then grant_date ASC.
	// FOR UPDATE serialises concurrent approvals: only one transaction allocates
	// from a given grant row at a time, preventing over-allocation under race conditions.
	// The remaining_days column is the only days-related field returned so that
	// the scan unambiguously reads the net-remaining balance, not the gross grant.
	var grants []struct {
		ID            uuid.UUID `gorm:"column:id"`
		RemainingDays float64   `gorm:"column:remaining_days"`
		ExpiresOn     time.Time `gorm:"column:expires_on"`
	}
	if err := tx.Raw(
		`SELECT lg.id,
		        lg.days - COALESCE(
		            (SELECT SUM(lu2.days_used) FROM leave_usages lu2
		             WHERE lu2.leave_grant_id = lg.id AND lu2.tenant_id = lg.tenant_id),
		            0
		        ) AS remaining_days,
		        lg.expires_on
		 FROM leave_grants lg
		 WHERE lg.tenant_id = ? AND lg.employee_id = ? AND lg.expires_on > ?
		 ORDER BY lg.expires_on ASC, lg.grant_date ASC
		 FOR UPDATE`,
		tenantID, req.EmployeeID, asOf,
	).Scan(&grants).Error; err != nil {
		return fmt.Errorf("leave: allocate load grants: %w", err)
	}

	remaining := req.Days
	for _, g := range grants {
		if remaining <= 0 {
			break
		}
		if g.RemainingDays <= 0 {
			continue
		}
		consume := g.RemainingDays
		if consume > remaining {
			consume = remaining
		}
		usageID := uuid.New()
		if err := tx.Exec(
			`INSERT INTO leave_usages (id, tenant_id, leave_request_id, leave_grant_id, days_used)
			 VALUES (?, ?, ?, ?, ?)`,
			usageID, tenantID, req.ID, g.ID, consume,
		).Error; err != nil {
			return fmt.Errorf("leave: allocate insert usage: %w", err)
		}
		remaining -= consume
	}

	if remaining > 0 {
		return ErrInsufficientBalance
	}
	return nil
}

// releaseAnnualLeave deletes leave_usages rows for a request (reversal on
// rejection or cancellation).
func releaseAnnualLeave(tx *gorm.DB, tenantID, requestID uuid.UUID) error {
	if err := tx.Exec(
		`DELETE FROM leave_usages WHERE leave_request_id = ? AND tenant_id = ?`,
		requestID, tenantID,
	).Error; err != nil {
		return fmt.Errorf("leave: release annual leave: %w", err)
	}
	return nil
}

// monthsBetween returns the number of complete months between from and to.
func monthsBetween(from, to time.Time) int {
	years := to.Year() - from.Year()
	months := int(to.Month()) - int(from.Month())
	total := years*12 + months
	// Adjust if the day-of-month in to has not reached from's day-of-month.
	if to.Day() < from.Day() {
		total--
	}
	if total < 0 {
		return 0
	}
	return total
}

// lookupGrantDays finds the grant days for a given tenure from grant_table_json.
func lookupGrantDays(raw []byte, tenureMonths int) (float64, error) {
	if len(raw) == 0 {
		return 0, ErrSettingNotFound
	}
	var entries []GrantEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return 0, fmt.Errorf("leave: decode grant_table_json: %w", err)
	}
	for _, e := range entries {
		if tenureMonths < e.TenureMonthsMin {
			continue
		}
		if e.TenureMonthsMax != nil && tenureMonths > *e.TenureMonthsMax {
			continue
		}
		return e.GrantDays, nil
	}
	// Tenure is below the minimum threshold (e.g. < 6 months) — no grant.
	return 0, nil
}

// lookupProportionalDays finds the proportional grant days for the given
// weekly_days and tenure from proportional_table_json.
// fallbackGrantTableJSON is the standard (full-time) grant table used when
// weekly_days has no exact entry in the proportional table.
func lookupProportionalDays(raw []byte, fallbackGrantTableJSON []byte, weeklyDays float64, tenureMonths int) (float64, error) {
	if len(raw) == 0 {
		return 0, ErrSettingNotFound
	}
	var groups []ProportionalGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return 0, fmt.Errorf("leave: decode proportional_table_json: %w", err)
	}
	// Find the group whose weekly_days matches exactly.
	var bestGroup *ProportionalGroup
	for i := range groups {
		g := &groups[i]
		if g.WeeklyDays == weeklyDays {
			bestGroup = g
			break
		}
	}
	if bestGroup == nil {
		// No exact match in proportional table — fall back to the standard
		// (full-time) grant table.  Passing the actual fallbackGrantTableJSON
		// (not nil) ensures lookupGrantDays can decode it; passing nil would
		// always return ErrSettingNotFound.
		return lookupGrantDays(fallbackGrantTableJSON, tenureMonths)
	}
	for _, e := range bestGroup.Entries {
		if tenureMonths < e.TenureMonthsMin {
			continue
		}
		if e.TenureMonthsMax != nil && tenureMonths > *e.TenureMonthsMax {
			continue
		}
		return e.GrantDays, nil
	}
	return 0, nil
}
