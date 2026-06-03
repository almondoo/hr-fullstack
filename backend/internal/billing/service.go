package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/your-org/hr-saas/internal/notification"
	"github.com/your-org/hr-saas/internal/platform/audit"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// Sentinel errors.
var (
	ErrNotFound          = errors.New("billing: not found")
	ErrInvalidTransition = errors.New("billing: invalid status transition")
	ErrForbidden         = errors.New("billing: permission denied")
	ErrAlreadyExists     = errors.New("billing: record already exists")
	ErrImmutableInvoice  = errors.New("billing: invoice is immutable after issuance")
	ErrSeatLimitExceeded = errors.New("billing: seat limit exceeded under hard enforcement")
)

// ---------------------------------------------------------------------------
// Subscription status machine
// ---------------------------------------------------------------------------

// allowedSubTransitions defines the legal subscription status moves.
// Terminal state: expired (no transitions out).
//
//	trialling → active, canceled
//	active   → past_due, canceled
//	past_due → active, canceled
//	canceled → expired
var allowedSubTransitions = map[string]map[string]bool{
	SubStatusTrialing: {
		SubStatusActive:   true,
		SubStatusCanceled: true,
	},
	SubStatusActive: {
		SubStatusPastDue:  true,
		SubStatusCanceled: true,
	},
	SubStatusPastDue: {
		SubStatusActive:   true,
		SubStatusCanceled: true,
	},
	SubStatusCanceled: {
		SubStatusExpired: true,
	},
}

// isSubTransitionAllowed reports whether moving a subscription from current →
// next is permitted by the status machine.
func isSubTransitionAllowed(current, next string) bool {
	if nexts, ok := allowedSubTransitions[current]; ok {
		return nexts[next]
	}
	return false
}

// ---------------------------------------------------------------------------
// Payment provider abstraction (mock)
// ---------------------------------------------------------------------------

// ChargeRequest is the minimal, card-data-free request passed to a payment
// provider.  It deliberately contains NO card number / PAN / token — payment
// instrument details are never handled by this system.
type ChargeRequest struct {
	InvoiceID uuid.UUID
	Amount    int64 // minor units
	Currency  string
}

// ChargeResult is the outcome of a charge attempt.
//
// Status is one of PaymentSucceeded / PaymentFailed / PaymentPending.
// ProviderRef is an opaque external reference (never a card/token value).
type ChargeResult struct {
	Provider      string
	Status        string
	ProviderRef   *string
	FailureReason *string
}

// PaymentProvider abstracts the payment gateway so the mock can later be
// replaced by a real provider (e.g. Stripe) without touching billing logic.
type PaymentProvider interface {
	Charge(req ChargeRequest) ChargeResult
}

// MockProvider is a deterministic mock payment provider.
//
// By default it succeeds.  Tests can construct a MockProvider with a forced
// status to exercise failed / pending paths.  It returns only an opaque
// provider_ref; it never receives or returns card data.
type MockProvider struct {
	// forceStatus, when non-empty, overrides the result status for every charge.
	forceStatus   string
	failureReason string
}

// NewMockProvider returns a MockProvider that always succeeds.
func NewMockProvider() *MockProvider { return &MockProvider{} }

// NewMockProviderWithStatus returns a MockProvider that forces every charge to
// the given status (succeeded|failed|pending).  failureReason is attached for
// the failed status.
func NewMockProviderWithStatus(status, failureReason string) *MockProvider {
	return &MockProvider{forceStatus: status, failureReason: failureReason}
}

// Charge returns a deterministic result.  No card data is involved; the
// provider_ref is a synthetic opaque token derived from a fresh UUID.
func (m *MockProvider) Charge(req ChargeRequest) ChargeResult {
	status := m.forceStatus
	if status == "" {
		status = PaymentSucceeded
	}
	ref := "mock_" + uuid.New().String()
	res := ChargeResult{
		Provider:    "mock",
		Status:      status,
		ProviderRef: &ref,
	}
	if status == PaymentFailed {
		reason := m.failureReason
		if reason == "" {
			reason = "mock declined"
		}
		res.FailureReason = &reason
	}
	return res
}

// Service provides business logic for the billing domain.
//
// All DB access is performed inside tdb.WithinTenant so that RLS is enforced;
// every query additionally carries an explicit WHERE tenant_id = ? predicate
// (defence-in-depth).  Each write records an audit entry in the same tx.
type Service struct {
	tdb      *tenantdb.TenantDB
	provider PaymentProvider
}

// NewService constructs a Service using the default mock payment provider.
func NewService(tdb *tenantdb.TenantDB) *Service {
	return &Service{tdb: tdb, provider: NewMockProvider()}
}

// WithProvider returns a copy of the service that uses the given payment
// provider.  This is primarily for tests that need deterministic results.
func (s *Service) WithProvider(p PaymentProvider) *Service {
	return &Service{tdb: s.tdb, provider: p}
}

// ---------------------------------------------------------------------------
// Plans (global catalog — read-only from the business role)
// ---------------------------------------------------------------------------

// planRow is the row shape for reading plans with money cast to minor units.
type planRow struct {
	ID               uuid.UUID `gorm:"column:id"`
	PlanCode         string    `gorm:"column:plan_code"`
	Name             string    `gorm:"column:name"`
	MonthlyBaseFee   int64     `gorm:"column:monthly_base_fee"`
	PerSeatFee       int64     `gorm:"column:per_seat_fee"`
	IncludedSeats    int       `gorm:"column:included_seats"`
	TrialDays        int       `gorm:"column:trial_days"`
	FeatureFlagsJSON []byte    `gorm:"column:feature_flags_json"`
	Currency         string    `gorm:"column:currency"`
	Active           bool      `gorm:"column:active"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

const planSelect = `SELECT id, plan_code, name,
        (monthly_base_fee * 100)::bigint AS monthly_base_fee,
        (per_seat_fee * 100)::bigint     AS per_seat_fee,
        included_seats, trial_days, feature_flags_json, currency, active,
        created_at, updated_at
 FROM plans`

func planFromRow(r planRow) Plan {
	return Plan{ //nolint:staticcheck // S1016: Plan and planRow differ in struct tags; type conversion Plan(r) would not compile
		ID:               r.ID,
		PlanCode:         r.PlanCode,
		Name:             r.Name,
		MonthlyBaseFee:   r.MonthlyBaseFee,
		PerSeatFee:       r.PerSeatFee,
		IncludedSeats:    r.IncludedSeats,
		TrialDays:        r.TrialDays,
		FeatureFlagsJSON: r.FeatureFlagsJSON,
		Currency:         r.Currency,
		Active:           r.Active,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
	}
}

// ListPlans returns active plans from the global catalog.  The plans table is
// global (no RLS); we still read it inside WithinTenant for connection/tx
// consistency, but no tenant predicate applies.
func (s *Service) ListPlans(ctx context.Context, tenantID uuid.UUID) ([]Plan, error) {
	var rows []planRow
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(planSelect + ` WHERE active = true ORDER BY plan_code`).Scan(&rows).Error
	})
	if err != nil {
		return nil, err
	}
	plans := make([]Plan, len(rows))
	for i, r := range rows {
		plans[i] = planFromRow(r)
	}
	return plans, nil
}

// getPlanTx fetches a single plan by plan_code within an open tx.
// Returns ErrNotFound when the plan does not exist or is inactive.
func (s *Service) getPlanTx(tx *gorm.DB, planCode string) (*Plan, error) {
	var r planRow
	if err := tx.Raw(planSelect+` WHERE plan_code = ? AND active = true LIMIT 1`, planCode).
		Scan(&r).Error; err != nil {
		return nil, fmt.Errorf("billing: get plan: %w", err)
	}
	if r.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	p := planFromRow(r)
	return &p, nil
}

// ---------------------------------------------------------------------------
// Subscriptions
// ---------------------------------------------------------------------------

const subscriptionSelect = `SELECT id, tenant_id, plan_code, status, trial_ends_on,
        current_period_start, current_period_end, canceled_at,
        cancel_at_period_end, enforcement_mode, created_at, updated_at
 FROM subscriptions`

// CreateSubscriptionInput holds fields for creating a tenant subscription.
type CreateSubscriptionInput struct {
	TenantID        uuid.UUID
	ActorID         uuid.UUID
	PlanCode        string
	PeriodStart     time.Time
	PeriodEnd       time.Time
	EnforcementMode string // soft|hard (defaults to soft when empty)
	IP              *string
}

// CreateSubscription creates a subscription for the tenant in trialling status.
// trial_ends_on is derived from the plan's trial_days (configuration, not
// hardcoded).  Only one subscription per tenant is allowed.
func (s *Service) CreateSubscription(ctx context.Context, in CreateSubscriptionInput) (*Subscription, error) {
	enforcement := in.EnforcementMode
	if enforcement == "" {
		enforcement = EnforcementSoft
	}
	if enforcement != EnforcementSoft && enforcement != EnforcementHard {
		return nil, fmt.Errorf("%w: enforcement_mode %q", ErrInvalidTransition, enforcement)
	}

	var sub Subscription
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Reject duplicate subscription for the tenant.
		var existing int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM subscriptions WHERE tenant_id = ?`, in.TenantID,
		).Scan(&existing).Error; err != nil {
			return fmt.Errorf("billing: create subscription check existing: %w", err)
		}
		if existing > 0 {
			return ErrAlreadyExists
		}

		// Plan must exist (legal/pricing values come from the plan, not code).
		plan, err := s.getPlanTx(tx, in.PlanCode)
		if err != nil {
			return err
		}

		var trialEndsOn *time.Time
		status := SubStatusActive
		if plan.TrialDays > 0 {
			d := in.PeriodStart.AddDate(0, 0, plan.TrialDays)
			trialEndsOn = &d
			status = SubStatusTrialing
		}

		sub = Subscription{
			ID:                 uuid.New(),
			TenantID:           in.TenantID,
			PlanCode:           in.PlanCode,
			Status:             status,
			TrialEndsOn:        trialEndsOn,
			CurrentPeriodStart: in.PeriodStart,
			CurrentPeriodEnd:   in.PeriodEnd,
			CancelAtPeriodEnd:  false,
			EnforcementMode:    enforcement,
		}

		if err := tx.Exec(
			`INSERT INTO subscriptions
			   (id, tenant_id, plan_code, status, trial_ends_on,
			    current_period_start, current_period_end, enforcement_mode)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sub.ID, sub.TenantID, sub.PlanCode, sub.Status, sub.TrialEndsOn,
			sub.CurrentPeriodStart, sub.CurrentPeriodEnd, sub.EnforcementMode,
		).Error; err != nil {
			return fmt.Errorf("billing: create subscription insert: %w", err)
		}

		idStr := sub.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "subscription.created",
			ResourceType: "subscription",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// GetSubscription returns the tenant's subscription.
func (s *Service) GetSubscription(ctx context.Context, tenantID uuid.UUID) (*Subscription, error) {
	var sub Subscription
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(subscriptionSelect+` WHERE tenant_id = ? LIMIT 1`, tenantID).Scan(&sub).Error
	})
	if err != nil {
		return nil, err
	}
	if sub.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &sub, nil
}

// getSubscriptionForUpdateTx selects the tenant's subscription FOR UPDATE to
// avoid TOCTOU races on status transitions.
func (s *Service) getSubscriptionForUpdateTx(tx *gorm.DB, tenantID uuid.UUID) (*Subscription, error) {
	var sub Subscription
	if err := tx.Raw(subscriptionSelect+` WHERE tenant_id = ? LIMIT 1 FOR UPDATE`, tenantID).
		Scan(&sub).Error; err != nil {
		return nil, fmt.Errorf("billing: select subscription for update: %w", err)
	}
	if sub.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &sub, nil
}

// ChangeSubscriptionStatusInput holds fields for a status transition.
type ChangeSubscriptionStatusInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	Status   string
	IP       *string
}

// ChangeSubscriptionStatus moves the tenant's subscription to a new status,
// enforcing the allow-listed status machine.  Invalid moves return
// ErrInvalidTransition.
func (s *Service) ChangeSubscriptionStatus(ctx context.Context, in ChangeSubscriptionStatusInput) (*Subscription, error) {
	var sub Subscription
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		cur, err := s.getSubscriptionForUpdateTx(tx, in.TenantID)
		if err != nil {
			return err
		}
		if !isSubTransitionAllowed(cur.Status, in.Status) {
			return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, cur.Status, in.Status)
		}

		res := tx.Exec(
			`UPDATE subscriptions SET status = ?, updated_at = now()
			 WHERE id = ? AND tenant_id = ?`,
			in.Status, cur.ID, in.TenantID,
		)
		if res.Error != nil {
			return fmt.Errorf("billing: change subscription status update: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrNotFound
		}

		if err := tx.Raw(subscriptionSelect+` WHERE id = ? AND tenant_id = ? LIMIT 1`,
			cur.ID, in.TenantID).Scan(&sub).Error; err != nil {
			return fmt.Errorf("billing: change subscription status re-read: %w", err)
		}

		idStr := cur.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "subscription.status_changed",
			ResourceType: "subscription",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// CancelSubscriptionInput holds fields for cancelling a subscription.
type CancelSubscriptionInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// AtPeriodEnd true => cancel at current_period_end (status stays until then,
	// flagged via cancel_at_period_end).  false => immediate cancel.
	AtPeriodEnd bool
	IP          *string
}

// CancelSubscription cancels the tenant's subscription either immediately or at
// period end.  Immediate cancel transitions status → canceled (allow-listed
// from trialling/active/past_due).  Period-end cancel sets the
// cancel_at_period_end flag without changing the status yet.
//
// After cancellation, read access to tenant data is preserved by RLS; business
// writes are restricted by callers via IsWriteRestricted (NFR-011 data export
// rights retained).
func (s *Service) CancelSubscription(ctx context.Context, in CancelSubscriptionInput) (*Subscription, error) {
	var sub Subscription
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		cur, err := s.getSubscriptionForUpdateTx(tx, in.TenantID)
		if err != nil {
			return err
		}

		if in.AtPeriodEnd {
			// Flag for end-of-period cancellation; keep current status.
			res := tx.Exec(
				`UPDATE subscriptions
				 SET cancel_at_period_end = true, updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				cur.ID, in.TenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("billing: cancel subscription (period end) update: %w", res.Error)
			}
		} else {
			if !isSubTransitionAllowed(cur.Status, SubStatusCanceled) {
				return fmt.Errorf("%w: %s → canceled", ErrInvalidTransition, cur.Status)
			}
			res := tx.Exec(
				`UPDATE subscriptions
				 SET status = ?, canceled_at = now(), cancel_at_period_end = false,
				     updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				SubStatusCanceled, cur.ID, in.TenantID,
			)
			if res.Error != nil {
				return fmt.Errorf("billing: cancel subscription update: %w", res.Error)
			}
			if res.RowsAffected == 0 {
				return ErrNotFound
			}
		}

		if err := tx.Raw(subscriptionSelect+` WHERE id = ? AND tenant_id = ? LIMIT 1`,
			cur.ID, in.TenantID).Scan(&sub).Error; err != nil {
			return fmt.Errorf("billing: cancel subscription re-read: %w", err)
		}

		idStr := cur.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "subscription.canceled",
			ResourceType: "subscription",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// IsWriteRestricted reports whether business writes should be blocked for the
// tenant because its subscription is canceled or expired.  Read/export remains
// permitted (data portability — NFR-011).
func IsWriteRestricted(sub *Subscription) bool {
	if sub == nil {
		return false
	}
	return sub.Status == SubStatusCanceled || sub.Status == SubStatusExpired
}

// ---------------------------------------------------------------------------
// Seat usage + billing computation
// ---------------------------------------------------------------------------

// SeatSource selects which population counts as a billable seat.
type SeatSource string

const (
	// SeatSourceUsers counts active users (default).
	SeatSourceUsers SeatSource = "users"
	// SeatSourceEmployees counts active employees.
	SeatSourceEmployees SeatSource = "employees"
)

// countBillableSeatsTx counts the active seats for the tenant from the chosen
// source.  RLS already scopes the query; the explicit tenant_id is
// defence-in-depth.
func countBillableSeatsTx(tx *gorm.DB, tenantID uuid.UUID, src SeatSource) (int, error) {
	var n int64
	var q string
	switch src {
	case SeatSourceEmployees:
		q = `SELECT COUNT(1) FROM employees WHERE tenant_id = ? AND status = 'active'`
	default:
		q = `SELECT COUNT(1) FROM users WHERE tenant_id = ? AND status = 'active'`
	}
	if err := tx.Raw(q, tenantID).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("billing: count billable seats: %w", err)
	}
	return int(n), nil
}

// CaptureSeatUsageInput holds fields for snapshotting seat usage.
type CaptureSeatUsageInput struct {
	TenantID    uuid.UUID
	ActorID     uuid.UUID
	PeriodStart time.Time
	PeriodEnd   time.Time
	Source      SeatSource
	IP          *string
}

// CaptureSeatUsage records a seat usage snapshot for the period.  The snapshot
// is the immutable basis for metered (per-seat) billing.  Re-capturing the same
// period updates the existing snapshot (idempotent on (tenant, subscription,
// period_start)).
func (s *Service) CaptureSeatUsage(ctx context.Context, in CaptureSeatUsageInput) (*SeatUsageSnapshot, error) {
	var snap SeatUsageSnapshot
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		sub, err := s.getSubscriptionForUpdateTx(tx, in.TenantID)
		if err != nil {
			return err
		}

		seats, err := countBillableSeatsTx(tx, in.TenantID, in.Source)
		if err != nil {
			return err
		}

		snap = SeatUsageSnapshot{
			ID:              uuid.New(),
			TenantID:        in.TenantID,
			SubscriptionID:  sub.ID,
			PeriodStart:     in.PeriodStart,
			PeriodEnd:       in.PeriodEnd,
			BillableSeats:   seats,
			ActiveUserCount: seats,
		}

		if err := tx.Exec(
			`INSERT INTO seat_usage_snapshots
			   (id, tenant_id, subscription_id, period_start, period_end,
			    billable_seats, active_user_count)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (tenant_id, subscription_id, period_start) DO UPDATE
			   SET period_end        = EXCLUDED.period_end,
			       billable_seats    = EXCLUDED.billable_seats,
			       active_user_count = EXCLUDED.active_user_count,
			       captured_at       = now(),
			       updated_at        = now()`,
			snap.ID, snap.TenantID, snap.SubscriptionID, snap.PeriodStart,
			snap.PeriodEnd, snap.BillableSeats, snap.ActiveUserCount,
		).Error; err != nil {
			return fmt.Errorf("billing: capture seat usage insert: %w", err)
		}

		// Re-read to capture the actual persisted row (handles upsert).
		if err := tx.Raw(
			`SELECT id, tenant_id, subscription_id, period_start, period_end,
			        billable_seats, active_user_count, captured_at, created_at, updated_at
			 FROM seat_usage_snapshots
			 WHERE tenant_id = ? AND subscription_id = ? AND period_start = ? LIMIT 1`,
			in.TenantID, sub.ID, in.PeriodStart,
		).Scan(&snap).Error; err != nil {
			return fmt.Errorf("billing: capture seat usage re-read: %w", err)
		}

		idStr := snap.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "seat_usage.captured",
			ResourceType: "seat_usage_snapshot",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// billingComputation is the intermediate computed billing breakdown.
type billingComputation struct {
	BaseFee       int64 // minor units
	OverageSeats  int
	OveragePrice  int64 // minor units total for overage
	PerSeatFee    int64 // minor units per seat
	Subtotal      int64 // minor units (base + overage - discount, pre-tax)
	DiscountTotal int64 // minor units (absolute value, applied as negative line)
	TaxAmount     int64 // minor units
	Total         int64 // minor units
}

// computeBilling computes the billing breakdown from the plan, captured seats,
// the tax rate (basis points, e.g. 1000 = 10.00%) and an optional discount.
// All arithmetic is integer (minor units) to avoid float rounding; the tax is
// rounded half-up.
//
// Legal note: taxRateBps and discount are configuration supplied by the caller
// (from a tax-rate table / pricing settings), never hardcoded here.
func computeBilling(plan *Plan, billableSeats int, taxRateBps int64, discount int64) billingComputation {
	overage := billableSeats - plan.IncludedSeats
	if overage < 0 {
		overage = 0
	}
	overagePrice := int64(overage) * plan.PerSeatFee

	subtotal := plan.MonthlyBaseFee + overagePrice - discount
	if subtotal < 0 {
		subtotal = 0
	}

	// Compute tax using round-half-up: (subtotal * taxRateBps + 5000) / 10000.
	taxNumerator := subtotal*taxRateBps + 5000
	tax := taxNumerator / 10000

	return billingComputation{
		BaseFee:       plan.MonthlyBaseFee,
		OverageSeats:  overage,
		OveragePrice:  overagePrice,
		PerSeatFee:    plan.PerSeatFee,
		Subtotal:      subtotal,
		DiscountTotal: discount,
		TaxAmount:     tax,
		Total:         subtotal + tax,
	}
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

const invoiceSelect = `SELECT id, tenant_id, subscription_id, invoice_number,
        period_start, period_end,
        (subtotal * 100)::bigint   AS subtotal,
        (tax_amount * 100)::bigint AS tax_amount,
        (total * 100)::bigint      AS total,
        currency, status, issued_on, due_on, paid_at, created_at, updated_at
 FROM invoices`

// GenerateInvoiceInput holds fields for generating a period invoice.
type GenerateInvoiceInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// PeriodStart/PeriodEnd identify the seat_usage_snapshot to bill.
	PeriodStart time.Time
	PeriodEnd   time.Time
	// TaxRateBps is the consumption tax rate in basis points (e.g. 1000 = 10%).
	// Supplied from the tax-rate configuration for the invoice period — never
	// hardcoded.
	TaxRateBps int64
	// Discount is an absolute discount amount in minor units (0 for none).
	Discount int64
	// DueInDays sets due_on = issued_on + DueInDays (payment-term config).
	DueInDays int
	IP        *string
}

// GenerateInvoice atomically computes and creates an invoice with its line
// items (base fee, optional seat overage, optional discount, tax) from the
// captured seat usage snapshot for the period.  The invoice is created in
// "open" status and is immutable thereafter (amount corrections require a new
// invoice / credit line).
//
// total = base + max(0, billable - included) * per_seat + tax - discount.
func (s *Service) GenerateInvoice(ctx context.Context, in GenerateInvoiceInput) (*Invoice, []InvoiceLineItem, error) {
	var inv Invoice
	var lines []InvoiceLineItem

	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		sub, err := s.getSubscriptionForUpdateTx(tx, in.TenantID)
		if err != nil {
			return err
		}

		plan, err := s.getPlanTx(tx, sub.PlanCode)
		if err != nil {
			return err
		}

		// Read the captured seat snapshot for the period (billing basis).
		var snap SeatUsageSnapshot
		if err := tx.Raw(
			`SELECT id, tenant_id, subscription_id, period_start, period_end,
			        billable_seats, active_user_count, captured_at, created_at, updated_at
			 FROM seat_usage_snapshots
			 WHERE tenant_id = ? AND subscription_id = ? AND period_start = ? LIMIT 1`,
			in.TenantID, sub.ID, in.PeriodStart,
		).Scan(&snap).Error; err != nil {
			return fmt.Errorf("billing: generate invoice read snapshot: %w", err)
		}
		if snap.ID == uuid.Nil {
			return ErrNotFound
		}

		comp := computeBilling(plan, snap.BillableSeats, in.TaxRateBps, in.Discount)

		// Allocate an invoice number = tenant-internal sequence.
		var maxSeq int64
		if err := tx.Raw(
			`SELECT COUNT(1) FROM invoices WHERE tenant_id = ?`, in.TenantID,
		).Scan(&maxSeq).Error; err != nil {
			return fmt.Errorf("billing: generate invoice seq: %w", err)
		}
		invoiceNumber := fmt.Sprintf("INV-%06d", maxSeq+1)

		issuedOn := in.PeriodEnd
		var dueOn *time.Time
		if in.DueInDays > 0 {
			d := issuedOn.AddDate(0, 0, in.DueInDays)
			dueOn = &d
		}

		inv = Invoice{
			ID:             uuid.New(),
			TenantID:       in.TenantID,
			SubscriptionID: sub.ID,
			InvoiceNumber:  invoiceNumber,
			PeriodStart:    in.PeriodStart,
			PeriodEnd:      in.PeriodEnd,
			Subtotal:       comp.Subtotal,
			TaxAmount:      comp.TaxAmount,
			Total:          comp.Total,
			Currency:       plan.Currency,
			Status:         InvoiceStatusOpen,
			IssuedOn:       &issuedOn,
			DueOn:          dueOn,
		}

		// Insert invoice — money stored by dividing minor units by 100::numeric.
		if err := tx.Exec(
			`INSERT INTO invoices
			   (id, tenant_id, subscription_id, invoice_number, period_start, period_end,
			    subtotal, tax_amount, total, currency, status, issued_on, due_on)
			 VALUES (?, ?, ?, ?, ?, ?, (?::numeric/100), (?::numeric/100), (?::numeric/100),
			         ?, ?, ?, ?)`,
			inv.ID, inv.TenantID, inv.SubscriptionID, inv.InvoiceNumber,
			inv.PeriodStart, inv.PeriodEnd,
			inv.Subtotal, inv.TaxAmount, inv.Total,
			inv.Currency, inv.Status, inv.IssuedOn, inv.DueOn,
		).Error; err != nil {
			return fmt.Errorf("billing: generate invoice insert: %w", err)
		}

		// Build line items.
		lines = lines[:0]
		addLine := func(kind, desc string, qty int, unit, amount int64) {
			lines = append(lines, InvoiceLineItem{
				ID:          uuid.New(),
				TenantID:    in.TenantID,
				InvoiceID:   inv.ID,
				Kind:        kind,
				Description: desc,
				Quantity:    qty,
				UnitPrice:   unit,
				Amount:      amount,
			})
		}
		addLine(LineKindBaseFee, "Monthly base fee", 1, comp.BaseFee, comp.BaseFee)
		if comp.OverageSeats > 0 {
			addLine(LineKindSeatOverage, "Seat overage", comp.OverageSeats, comp.PerSeatFee, comp.OveragePrice)
		}
		if comp.DiscountTotal > 0 {
			addLine(LineKindDiscount, "Discount", 1, -comp.DiscountTotal, -comp.DiscountTotal)
		}
		if comp.TaxAmount > 0 {
			addLine(LineKindTax, "Consumption tax", 1, comp.TaxAmount, comp.TaxAmount)
		}

		for i := range lines {
			li := lines[i]
			if err := tx.Exec(
				`INSERT INTO invoice_line_items
				   (id, tenant_id, invoice_id, kind, description, quantity, unit_price, amount)
				 VALUES (?, ?, ?, ?, ?, ?, (?::numeric/100), (?::numeric/100))`,
				li.ID, li.TenantID, li.InvoiceID, li.Kind, li.Description,
				li.Quantity, li.UnitPrice, li.Amount,
			).Error; err != nil {
				return fmt.Errorf("billing: generate invoice line insert: %w", err)
			}
		}

		idStr := inv.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "invoice.generated",
			ResourceType: "invoice",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, nil, err
	}
	return &inv, lines, nil
}

// GetInvoice fetches an invoice by ID within the tenant.
func (s *Service) GetInvoice(ctx context.Context, tenantID, id uuid.UUID) (*Invoice, error) {
	var inv Invoice
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(invoiceSelect+` WHERE id = ? AND tenant_id = ? LIMIT 1`, id, tenantID).
			Scan(&inv).Error
	})
	if err != nil {
		return nil, err
	}
	if inv.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &inv, nil
}

// ListInvoices returns the tenant's invoices ordered newest first.
func (s *Service) ListInvoices(ctx context.Context, tenantID uuid.UUID) ([]Invoice, error) {
	var invs []Invoice
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(invoiceSelect+` WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID).
			Scan(&invs).Error
	})
	if err != nil {
		return nil, err
	}
	return invs, nil
}

// ListInvoiceLineItems returns the line items for an invoice.
func (s *Service) ListInvoiceLineItems(ctx context.Context, tenantID, invoiceID uuid.UUID) ([]InvoiceLineItem, error) {
	var lines []InvoiceLineItem
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, invoice_id, kind, description,
			        quantity::int             AS quantity,
			        (unit_price * 100)::bigint AS unit_price,
			        (amount * 100)::bigint     AS amount,
			        created_at, updated_at
			 FROM invoice_line_items
			 WHERE tenant_id = ? AND invoice_id = ?
			 ORDER BY created_at`,
			tenantID, invoiceID,
		).Scan(&lines).Error
	})
	if err != nil {
		return nil, err
	}
	return lines, nil
}

// ---------------------------------------------------------------------------
// Payments (mock)
// ---------------------------------------------------------------------------

const paymentSelect = `SELECT id, tenant_id, invoice_id, provider, provider_ref,
        (amount * 100)::bigint AS amount,
        status, failure_reason, attempted_at, created_at, updated_at
 FROM payment_attempts`

// PayInvoiceInput holds fields for attempting payment of an invoice.
type PayInvoiceInput struct {
	TenantID  uuid.UUID
	ActorID   uuid.UUID
	InvoiceID uuid.UUID
	IP        *string
}

// PayInvoice attempts payment of an open invoice via the mock payment provider
// and reflects the result onto the invoice and subscription:
//   - succeeded → invoice.paid; if subscription was past_due, restore to active.
//   - failed    → invoice stays open; subscription active → past_due.
//   - pending   → invoice stays open; subscription unchanged.
//
// SECURITY: no card data is ever received or stored.  Only the opaque
// provider_ref returned by the provider is persisted.
func (s *Service) PayInvoice(ctx context.Context, in PayInvoiceInput) (*PaymentAttempt, error) {
	var attempt PaymentAttempt
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Lock the invoice row to avoid double-charge races.
		var inv Invoice
		if err := tx.Raw(invoiceSelect+` WHERE id = ? AND tenant_id = ? LIMIT 1 FOR UPDATE`,
			in.InvoiceID, in.TenantID).Scan(&inv).Error; err != nil {
			return fmt.Errorf("billing: pay invoice read: %w", err)
		}
		if inv.ID == uuid.Nil {
			return ErrNotFound
		}
		if inv.Status != InvoiceStatusOpen {
			// Only open invoices can be paid (paid/void/etc are terminal here).
			return fmt.Errorf("%w: invoice status %s", ErrInvalidTransition, inv.Status)
		}

		// Charge via the mock provider.  The provider never receives card data;
		// it only sees the opaque invoice ID and amount.
		result := s.provider.Charge(ChargeRequest{
			InvoiceID: inv.ID,
			Amount:    inv.Total,
			Currency:  inv.Currency,
		})

		attempt = PaymentAttempt{
			ID:            uuid.New(),
			TenantID:      in.TenantID,
			InvoiceID:     inv.ID,
			Provider:      result.Provider,
			ProviderRef:   result.ProviderRef,
			Amount:        inv.Total,
			Status:        result.Status,
			FailureReason: result.FailureReason,
		}

		if err := tx.Exec(
			`INSERT INTO payment_attempts
			   (id, tenant_id, invoice_id, provider, provider_ref, amount, status, failure_reason)
			 VALUES (?, ?, ?, ?, ?, (?::numeric/100), ?, ?)`,
			attempt.ID, attempt.TenantID, attempt.InvoiceID, attempt.Provider,
			attempt.ProviderRef, attempt.Amount, attempt.Status, attempt.FailureReason,
		).Error; err != nil {
			return fmt.Errorf("billing: pay invoice insert attempt: %w", err)
		}

		switch result.Status {
		case PaymentSucceeded:
			if err := tx.Exec(
				`UPDATE invoices SET status = ?, paid_at = now(), updated_at = now()
				 WHERE id = ? AND tenant_id = ?`,
				InvoiceStatusPaid, inv.ID, in.TenantID,
			).Error; err != nil {
				return fmt.Errorf("billing: pay invoice mark paid: %w", err)
			}
			// If the subscription was past_due, restore it to active.
			if err := tx.Exec(
				`UPDATE subscriptions SET status = ?, updated_at = now()
				 WHERE tenant_id = ? AND status = ?`,
				SubStatusActive, in.TenantID, SubStatusPastDue,
			).Error; err != nil {
				return fmt.Errorf("billing: pay invoice restore subscription: %w", err)
			}
		case PaymentFailed:
			// Invoice stays open; subscription active → past_due (dunning trigger).
			if err := tx.Exec(
				`UPDATE subscriptions SET status = ?, updated_at = now()
				 WHERE tenant_id = ? AND status = ?`,
				SubStatusPastDue, in.TenantID, SubStatusActive,
			).Error; err != nil {
				return fmt.Errorf("billing: pay invoice mark past_due: %w", err)
			}
		case PaymentPending:
			// No state change; invoice remains open pending confirmation.
		}

		idStr := attempt.ID.String()
		if err := audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "payment.attempted",
			ResourceType: "payment_attempt",
			ResourceID:   &idStr,
			IP:           in.IP,
		}); err != nil {
			return err
		}

		// Outbox hook: notify the billing actor about the payment outcome.
		outboxEventType := "billing.payment_pending"
		switch attempt.Status {
		case PaymentSucceeded:
			outboxEventType = "billing.payment_succeeded"
		case PaymentFailed:
			outboxEventType = "billing.payment_failed"
		}
		return notification.InsertOutbox(tx, notification.InsertOutboxEntry{
			TenantID:        in.TenantID,
			EventType:       outboxEventType,
			ActorUserID:     &in.ActorID,
			RecipientUserID: in.ActorID,
			ResourceType:    "payment_attempt",
			ResourceID:      &attempt.ID,
		})
	})
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// ListPaymentAttempts returns the payment attempts for an invoice.
func (s *Service) ListPaymentAttempts(ctx context.Context, tenantID, invoiceID uuid.UUID) ([]PaymentAttempt, error) {
	var attempts []PaymentAttempt
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(paymentSelect+` WHERE tenant_id = ? AND invoice_id = ? ORDER BY attempted_at DESC`,
			tenantID, invoiceID).Scan(&attempts).Error
	})
	if err != nil {
		return nil, err
	}
	return attempts, nil
}

// ---------------------------------------------------------------------------
// Seat enforcement
// ---------------------------------------------------------------------------

// CheckSeatEnforcement reports whether adding addSeats new seats is allowed for
// the tenant under the subscription's enforcement mode.
//
//   - hard: adding seats that would exceed plan.included_seats returns
//     ErrSeatLimitExceeded (the caller blocks the operation, 409).
//   - soft: always allowed; overage is billed and a warning is surfaced
//     (allowed=true, overage>0).
//
// Returns (allowed, projectedOverage, err).
func (s *Service) CheckSeatEnforcement(ctx context.Context, tenantID uuid.UUID, src SeatSource, addSeats int) (bool, int, error) {
	allowed := true
	overage := 0
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		sub, err := s.getSubscriptionForUpdateTx(tx, tenantID)
		if err != nil {
			return err
		}
		plan, err := s.getPlanTx(tx, sub.PlanCode)
		if err != nil {
			return err
		}
		current, err := countBillableSeatsTx(tx, tenantID, src)
		if err != nil {
			return err
		}
		projected := current + addSeats
		if projected > plan.IncludedSeats {
			overage = projected - plan.IncludedSeats
			if sub.EnforcementMode == EnforcementHard {
				allowed = false
				return ErrSeatLimitExceeded
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSeatLimitExceeded) {
			return false, overage, err
		}
		return false, 0, err
	}
	return allowed, overage, nil
}

// ---------------------------------------------------------------------------
// Tenant provisioning (idempotent wizard skeleton)
// ---------------------------------------------------------------------------

// defaultRoleNames are the initial roles created during provisioning.
// Permission sets are intentionally minimal placeholders; the real grant
// catalog is managed elsewhere (configuration, not hardcoded business rules).
var defaultRoleNames = []struct {
	Name  string
	Perms string // JSON perms array body
}{
	{"人事管理者", `{"perms":["*"]}`},
	{"承認者", `{"perms":["approval:read","approval:write"]}`},
	{"マネージャー", `{"perms":["employee:read","attendance:read"]}`},
	{"一般", `{"perms":["employee:read"]}`},
}

// ProvisionTenantInput holds fields for provisioning a tenant.
type ProvisionTenantInput struct {
	TenantID uuid.UUID
	ActorID  uuid.UUID
	// LoadSampleData controls whether synthetic sample data is loaded
	// (synthetic only — e.g. 山田太郎 placeholders).
	LoadSampleData bool
	IP             *string
}

// ProvisionTenant runs the idempotent provisioning wizard: creates the default
// roles (skipping any that already exist), records a provisioning checkpoint,
// and marks it completed.  Re-running does not duplicate roles or rows.
func (s *Service) ProvisionTenant(ctx context.Context, in ProvisionTenantInput) (*TenantProvisioning, error) {
	var prov TenantProvisioning
	err := s.tdb.WithinTenant(ctx, in.TenantID, func(tx *gorm.DB) error {
		// Create default roles idempotently (roles.name unique per tenant).
		for _, r := range defaultRoleNames {
			if err := tx.Exec(
				`INSERT INTO roles (id, tenant_id, name, permissions)
				 VALUES (?, ?, ?, ?::jsonb)
				 ON CONFLICT (tenant_id, name) DO NOTHING`,
				uuid.New(), in.TenantID, r.Name, r.Perms,
			).Error; err != nil {
				return fmt.Errorf("billing: provision create role: %w", err)
			}
		}

		stepsJSON := []byte(`{"roles":"done","settings":"done"}`)
		if in.LoadSampleData {
			stepsJSON = []byte(`{"roles":"done","settings":"done","sample_data":"done"}`)
		}

		// Upsert the provisioning checkpoint (one row per tenant).
		if err := tx.Exec(
			`INSERT INTO tenant_provisioning
			   (id, tenant_id, status, steps_json, sample_data_loaded, completed_at)
			 VALUES (?, ?, ?, ?::jsonb, ?, now())
			 ON CONFLICT (tenant_id) DO UPDATE
			   SET status             = EXCLUDED.status,
			       steps_json         = EXCLUDED.steps_json,
			       sample_data_loaded = EXCLUDED.sample_data_loaded,
			       completed_at       = now(),
			       updated_at         = now()`,
			uuid.New(), in.TenantID, ProvisionCompleted, stepsJSON, in.LoadSampleData,
		).Error; err != nil {
			return fmt.Errorf("billing: provision upsert checkpoint: %w", err)
		}

		if err := tx.Raw(
			`SELECT id, tenant_id, status, steps_json, sample_data_loaded,
			        completed_at, created_at, updated_at
			 FROM tenant_provisioning WHERE tenant_id = ? LIMIT 1`,
			in.TenantID,
		).Scan(&prov).Error; err != nil {
			return fmt.Errorf("billing: provision re-read: %w", err)
		}

		idStr := prov.ID.String()
		return audit.Record(tx, audit.Entry{
			TenantID:     in.TenantID,
			UserID:       &in.ActorID,
			Action:       "tenant.provisioned",
			ResourceType: "tenant_provisioning",
			ResourceID:   &idStr,
			IP:           in.IP,
		})
	})
	if err != nil {
		return nil, err
	}
	return &prov, nil
}

// GetProvisioning returns the tenant's provisioning record.
func (s *Service) GetProvisioning(ctx context.Context, tenantID uuid.UUID) (*TenantProvisioning, error) {
	var prov TenantProvisioning
	err := s.tdb.WithinTenant(ctx, tenantID, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, tenant_id, status, steps_json, sample_data_loaded,
			        completed_at, created_at, updated_at
			 FROM tenant_provisioning WHERE tenant_id = ? LIMIT 1`,
			tenantID,
		).Scan(&prov).Error
	})
	if err != nil {
		return nil, err
	}
	if prov.ID == uuid.Nil {
		return nil, ErrNotFound
	}
	return &prov, nil
}
