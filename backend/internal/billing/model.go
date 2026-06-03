// Package billing implements the tenant billing domain (ST-FND-04):
// plan catalog, subscriptions, metered (seat) + flat-rate billing, invoices,
// mock payment processing, and tenant provisioning.
//
// Design notes:
//   - plans is a GLOBAL catalog (no tenant_id, no RLS).  All other tables are
//     tenant-scoped and protected by RLS + explicit tenant_id predicates.
//   - Money is stored as numeric(12,2) and modelled in Go as a fixed-point
//     integer count of minor units (e.g. 1 yen = 100 minor units) to avoid
//     binary float rounding.  See money.go-style helpers in service.go.
//   - Payments are MOCK only.  Card numbers / PAN / raw tokens are never
//     received or stored; only an opaque provider_ref is persisted.
//   - Legal/accounting values (tax rate, seat pricing, trial days, cycle,
//     rounding, etc.) are configuration (plans / subscriptions), never
//     hardcoded.  This is not legal or accounting advice; correctness of tax
//     and qualified-invoice (インボイス制度) handling is out of scope (mock).
package billing

import (
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Status / kind constants
// ---------------------------------------------------------------------------

// Subscription statuses.
const (
	SubStatusTrialing = "trialing" //nolint:misspell // Stripe-compatible status value: "trialing" is the established API contract
	SubStatusActive   = "active"
	SubStatusPastDue  = "past_due"
	SubStatusCanceled = "canceled"
	SubStatusExpired  = "expired"
)

// Enforcement modes for plan-limit overage handling.
const (
	EnforcementSoft = "soft"
	EnforcementHard = "hard"
)

// Invoice statuses.
const (
	InvoiceStatusDraft         = "draft"
	InvoiceStatusOpen          = "open"
	InvoiceStatusPaid          = "paid"
	InvoiceStatusVoid          = "void"
	InvoiceStatusUncollectible = "uncollectible"
)

// Invoice line-item kinds.
const (
	LineKindBaseFee     = "base_fee"
	LineKindSeatOverage = "seat_overage"
	LineKindDiscount    = "discount"
	LineKindTax         = "tax"
	LineKindCredit      = "credit"
)

// Payment attempt statuses.
const (
	PaymentPending   = "pending"
	PaymentSucceeded = "succeeded"
	PaymentFailed    = "failed"
)

// Provisioning statuses.
const (
	ProvisionPending    = "pending"
	ProvisionInProgress = "in_progress"
	ProvisionCompleted  = "completed"
	ProvisionFailed     = "failed"
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// Plan is the GORM model for the global plans catalog (no tenant_id, no RLS).
//
// Monetary columns (numeric(12,2)) are scanned as int64 minor units
// (e.g. 1.00 -> 100) by the service-layer queries, which cast them with
// (column * 100)::bigint so that no floating-point arithmetic is used.
type Plan struct {
	ID               uuid.UUID `gorm:"column:id;primaryKey"`
	PlanCode         string    `gorm:"column:plan_code"`
	Name             string    `gorm:"column:name"`
	MonthlyBaseFee   int64     `gorm:"column:monthly_base_fee"` // minor units
	PerSeatFee       int64     `gorm:"column:per_seat_fee"`     // minor units
	IncludedSeats    int       `gorm:"column:included_seats"`
	TrialDays        int       `gorm:"column:trial_days"`
	FeatureFlagsJSON []byte    `gorm:"column:feature_flags_json;type:jsonb"`
	Currency         string    `gorm:"column:currency"`
	Active           bool      `gorm:"column:active"`
	CreatedAt        time.Time `gorm:"column:created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at"`
}

// TableName maps Plan to the plans table.
func (Plan) TableName() string { return "plans" }

// Subscription is the GORM model for subscriptions (tenant-scoped).
type Subscription struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID           uuid.UUID  `gorm:"column:tenant_id"`
	PlanCode           string     `gorm:"column:plan_code"`
	Status             string     `gorm:"column:status"`
	TrialEndsOn        *time.Time `gorm:"column:trial_ends_on"`
	CurrentPeriodStart time.Time  `gorm:"column:current_period_start"`
	CurrentPeriodEnd   time.Time  `gorm:"column:current_period_end"`
	CanceledAt         *time.Time `gorm:"column:canceled_at"`
	CancelAtPeriodEnd  bool       `gorm:"column:cancel_at_period_end"`
	EnforcementMode    string     `gorm:"column:enforcement_mode"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName maps Subscription to the subscriptions table.
func (Subscription) TableName() string { return "subscriptions" }

// SeatUsageSnapshot is the GORM model for seat_usage_snapshots (tenant-scoped).
type SeatUsageSnapshot struct {
	ID              uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID        uuid.UUID `gorm:"column:tenant_id"`
	SubscriptionID  uuid.UUID `gorm:"column:subscription_id"`
	PeriodStart     time.Time `gorm:"column:period_start"`
	PeriodEnd       time.Time `gorm:"column:period_end"`
	BillableSeats   int       `gorm:"column:billable_seats"`
	ActiveUserCount int       `gorm:"column:active_user_count"`
	CapturedAt      time.Time `gorm:"column:captured_at"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// TableName maps SeatUsageSnapshot to the seat_usage_snapshots table.
func (SeatUsageSnapshot) TableName() string { return "seat_usage_snapshots" }

// Invoice is the GORM model for invoices (tenant-scoped).
// Monetary columns are scanned as int64 minor units.
type Invoice struct {
	ID             uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID       uuid.UUID  `gorm:"column:tenant_id"`
	SubscriptionID uuid.UUID  `gorm:"column:subscription_id"`
	InvoiceNumber  string     `gorm:"column:invoice_number"`
	PeriodStart    time.Time  `gorm:"column:period_start"`
	PeriodEnd      time.Time  `gorm:"column:period_end"`
	Subtotal       int64      `gorm:"column:subtotal"`   // minor units
	TaxAmount      int64      `gorm:"column:tax_amount"` // minor units
	Total          int64      `gorm:"column:total"`      // minor units
	Currency       string     `gorm:"column:currency"`
	Status         string     `gorm:"column:status"`
	IssuedOn       *time.Time `gorm:"column:issued_on"`
	DueOn          *time.Time `gorm:"column:due_on"`
	PaidAt         *time.Time `gorm:"column:paid_at"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	UpdatedAt      time.Time  `gorm:"column:updated_at"`
}

// TableName maps Invoice to the invoices table.
func (Invoice) TableName() string { return "invoices" }

// InvoiceLineItem is the GORM model for invoice_line_items (tenant-scoped).
// amount may be negative (discount / credit).  Monetary columns are int64
// minor units.
type InvoiceLineItem struct {
	ID          uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID    uuid.UUID `gorm:"column:tenant_id"`
	InvoiceID   uuid.UUID `gorm:"column:invoice_id"`
	Kind        string    `gorm:"column:kind"`
	Description string    `gorm:"column:description"`
	Quantity    int       `gorm:"column:quantity"`
	UnitPrice   int64     `gorm:"column:unit_price"` // minor units
	Amount      int64     `gorm:"column:amount"`     // minor units (signed)
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

// TableName maps InvoiceLineItem to the invoice_line_items table.
func (InvoiceLineItem) TableName() string { return "invoice_line_items" }

// PaymentAttempt is the GORM model for payment_attempts (tenant-scoped).
//
// SECURITY: this struct intentionally has no field for card number / PAN /
// raw token.  Only the opaque provider_ref is stored.
type PaymentAttempt struct {
	ID            uuid.UUID `gorm:"column:id;primaryKey"`
	TenantID      uuid.UUID `gorm:"column:tenant_id"`
	InvoiceID     uuid.UUID `gorm:"column:invoice_id"`
	Provider      string    `gorm:"column:provider"`
	ProviderRef   *string   `gorm:"column:provider_ref"`
	Amount        int64     `gorm:"column:amount"` // minor units
	Status        string    `gorm:"column:status"`
	FailureReason *string   `gorm:"column:failure_reason"`
	AttemptedAt   time.Time `gorm:"column:attempted_at"`
	CreatedAt     time.Time `gorm:"column:created_at"`
	UpdatedAt     time.Time `gorm:"column:updated_at"`
}

// TableName maps PaymentAttempt to the payment_attempts table.
func (PaymentAttempt) TableName() string { return "payment_attempts" }

// TenantProvisioning is the GORM model for tenant_provisioning (tenant-scoped).
type TenantProvisioning struct {
	ID               uuid.UUID  `gorm:"column:id;primaryKey"`
	TenantID         uuid.UUID  `gorm:"column:tenant_id"`
	Status           string     `gorm:"column:status"`
	StepsJSON        []byte     `gorm:"column:steps_json;type:jsonb"`
	SampleDataLoaded bool       `gorm:"column:sample_data_loaded"`
	CompletedAt      *time.Time `gorm:"column:completed_at"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName maps TenantProvisioning to the tenant_provisioning table.
func (TenantProvisioning) TableName() string { return "tenant_provisioning" }
