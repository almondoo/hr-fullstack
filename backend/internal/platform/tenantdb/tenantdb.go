// Package tenantdb provides the single authorised entry point for all
// tenant-scoped database access.
//
// Architecture note — why this package exists:
//
// PostgreSQL Row-Level Security (RLS) is the first layer of tenant isolation.
// It works by filtering every query through a policy that compares each row's
// tenant_id column with the session-local variable app.tenant_id.  Because the
// application uses a connection pool (hr_app role), the variable MUST be set
// inside an explicit transaction using SET LOCAL so that it is automatically
// cleared when the transaction ends — preventing it from leaking to other
// requests that reuse the same connection.
//
// This package enforces that contract:
//
//  1. WithinTenant starts a GORM transaction.
//  2. It immediately sets app.tenant_id via a parameterised SET LOCAL call
//     (no string concatenation — SQL injection is impossible).
//  3. It passes the transaction handle (tx) to the caller-supplied fn.
//  4. On fn success: the transaction is committed.
//     On fn error or panic: the transaction is rolled back.
//
// Business / service code MUST use WithinTenant and operate exclusively on
// the tx handle passed to fn.  Passing the raw *gorm.DB from TenantDB.db to
// service code is a violation of this contract and defeats RLS.
//
// Multi-layer defence: even though RLS provides fail-closed behaviour at the
// DB level, application-layer queries should still include explicit tenant_id
// conditions (e.g. Where("tenant_id = ?", tenantID)) to make intent clear and
// to catch misconfiguration before the query reaches the database.
package tenantdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TenantDB wraps the application's *gorm.DB connection (hr_app role) and
// exposes WithinTenant as the only path to execute tenant-scoped queries.
//
// The embedded db field is intentionally unexported: callers must use
// WithinTenant; they cannot obtain the raw connection handle.
type TenantDB struct {
	db *gorm.DB
}

// New creates a TenantDB from the provided GORM connection.
// The connection should use the hr_app role (NOBYPASSRLS) so that RLS
// policies are enforced by PostgreSQL.
func New(gdb *gorm.DB) *TenantDB {
	return &TenantDB{db: gdb}
}

// WithinTenant executes fn inside a PostgreSQL transaction that has
// app.tenant_id set to tenantID.
//
// The sequence is:
//
//  1. Begin transaction (inherits ctx cancellation).
//  2. SET LOCAL app.tenant_id = '<tenantID>' — local scope means the variable
//     is automatically cleared when the transaction ends, so it never leaks
//     across connections in the pool.
//  3. Call fn(tx) — fn must use tx for all queries; do not use the outer db.
//  4. Commit if fn returns nil; rollback otherwise.
//
// Panics inside fn are recovered, the transaction is rolled back, and the
// panic is re-raised so the caller's recovery middleware can handle it.
// If rollback itself fails during a panic path, the rollback error is
// appended to the panic value as a formatted string and re-panicked.
//
// The tenantID parameter is validated to be a non-nil UUID before the
// transaction is opened.
func (t *TenantDB) WithinTenant(ctx context.Context, tenantID uuid.UUID, fn func(tx *gorm.DB) error) (retErr error) {
	if tenantID == uuid.Nil {
		return fmt.Errorf("tenantdb: WithinTenant called with nil UUID")
	}

	// Begin an explicit transaction with the caller's context so that
	// cancellation / deadline propagates to every query inside fn.
	tx := t.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("tenantdb: begin transaction: %w", tx.Error)
	}

	// Ensure rollback on any exit path that has not already committed.
	// The committed flag prevents a double-rollback after a successful commit.
	committed := false
	defer func() {
		if r := recover(); r != nil {
			// Panic path: attempt rollback and re-panic.
			// If rollback fails, attach the rollback error to the panic value
			// so it is visible in crash dumps / recovery middleware.
			if rbErr := tx.Rollback().Error; rbErr != nil {
				panic(fmt.Sprintf("tenantdb: panic during fn (%v); additionally, rollback failed: %v", r, rbErr))
			}
			panic(r)
		}
		if !committed {
			// Normal error path: surface rollback failure alongside the
			// original function error so callers can observe both.
			if rbErr := tx.Rollback().Error; rbErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("tenantdb: rollback: %w", rbErr))
			}
		}
	}()

	// SET LOCAL is transaction-scoped: the variable resets when the
	// transaction commits or rolls back.  Using a parameterised call
	// (? placeholder) prevents SQL injection regardless of UUID content.
	// set_config(key, value, is_local) with is_local=true is the
	// function-call equivalent of SET LOCAL and works correctly inside
	// an explicit transaction.
	if err := tx.Exec(
		"SELECT set_config('app.tenant_id', ?, true)",
		tenantID.String(),
	).Error; err != nil {
		return fmt.Errorf("tenantdb: set app.tenant_id: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit().Error; err != nil {
		return fmt.Errorf("tenantdb: commit: %w", err)
	}
	committed = true

	return nil
}
