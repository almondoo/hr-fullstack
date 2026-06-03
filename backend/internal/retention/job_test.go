package retention_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/your-org/hr-saas/internal/retention"
)

// TestDefaultConfig verifies that DefaultConfig returns a well-formed,
// non-empty Config with safe defaults.  This is a compile-time and
// value-correctness smoke test; it does not require a database.
func TestDefaultConfig(t *testing.T) {
	cfg := retention.DefaultConfig()
	if cfg.DisposalMethod == "" {
		t.Error("DefaultConfig: DisposalMethod must not be empty")
	}
	// Safe defaults: disabled features should produce zero/no-op values.
	if cfg.LedgerRetentionFallbackYears != 0 {
		t.Errorf("DefaultConfig: LedgerRetentionFallbackYears want 0, got %d",
			cfg.LedgerRetentionFallbackYears)
	}
	if cfg.EmployeeDataGracePeriod != 0 {
		t.Errorf("DefaultConfig: EmployeeDataGracePeriod want 0, got %v",
			cfg.EmployeeDataGracePeriod)
	}
}

// TestNewRunnerNilLogger verifies that NewRunner does not panic when logger is
// nil (it falls back to slog.Default()).
func TestNewRunnerNilLogger(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("NewRunner panicked with nil logger: %v", r)
		}
	}()
	_ = retention.NewRunner(nil, nil, retention.DefaultConfig(), nil, uuid.Nil)
}

// TestJobNameConstants verifies that the exported job-name constants match
// the expected values (constraint in migration 00030).
func TestJobNameConstants(t *testing.T) {
	want := map[string]string{
		"JobMyNumberDisposal":   "mynumber_disposal",
		"JobLedgerRetention":    "ledger_retention",
		"JobEmployeeDataPolicy": "employee_data_policy",
		"JobDocumentExpiry":     "document_expiry",
	}
	got := map[string]string{
		"JobMyNumberDisposal":   retention.JobMyNumberDisposal,
		"JobLedgerRetention":    retention.JobLedgerRetention,
		"JobEmployeeDataPolicy": retention.JobEmployeeDataPolicy,
		"JobDocumentExpiry":     retention.JobDocumentExpiry,
	}
	for k, wantV := range want {
		if got[k] != wantV {
			t.Errorf("constant %s: want %q, got %q", k, wantV, got[k])
		}
	}
}
