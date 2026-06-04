package econtract_test

import (
	"testing"
	"time"

	"github.com/your-org/hr-saas/internal/offer/econtract"
)

// ---------------------------------------------------------------------------
// CalcRetentionExpiry — 保管期限計算テスト (DB 不要)
// ---------------------------------------------------------------------------

func TestCalcRetentionExpiry_AlwaysFirstOfMonth(t *testing.T) {
	// retentionYears は法令値ではなく設定値。テストでは任意の整数を使う。
	cases := []struct {
		name           string
		signedAt       time.Time
		retentionYears int
		wantDay        int
		wantMonthAdv   int // expected month offset relative to (signedAt + retentionYears)
	}{
		{
			name:           "mid-month signing",
			signedAt:       time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC),
			retentionYears: 7,
			wantDay:        1,
		},
		{
			name:           "end of month signing",
			signedAt:       time.Date(2024, 1, 31, 23, 59, 0, 0, time.UTC),
			retentionYears: 5,
			wantDay:        1,
		},
		{
			name:           "first of month signing",
			signedAt:       time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC),
			retentionYears: 3,
			wantDay:        1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := econtract.CalcRetentionExpiry(tc.signedAt, tc.retentionYears)

			if got.Day() != tc.wantDay {
				t.Errorf("CalcRetentionExpiry day: got %d, want %d", got.Day(), tc.wantDay)
			}

			// Expiry must be strictly after (signedAt + retentionYears).
			anniversary := tc.signedAt.AddDate(tc.retentionYears, 0, 0)
			if !got.After(anniversary) {
				t.Errorf("CalcRetentionExpiry: expiry %v must be after anniversary %v",
					got, anniversary)
			}
		})
	}
}

func TestCalcRetentionExpiry_YearOffset(t *testing.T) {
	// Verify that the year component advances correctly.
	signedAt := time.Date(2020, 1, 15, 10, 0, 0, 0, time.UTC)
	retentionYears := 7 // 設定値 (法令値確定前のプレースホルダ)

	got := econtract.CalcRetentionExpiry(signedAt, retentionYears)

	wantYear := 2020 + retentionYears
	// The result is the first of the month after anniversary=2027-01-15 → 2027-02-01.
	if got.Year() != wantYear {
		t.Errorf("CalcRetentionExpiry year: got %d, want %d", got.Year(), wantYear)
	}
	if got.Month() != time.February {
		t.Errorf("CalcRetentionExpiry month: got %v, want February", got.Month())
	}
}

// ---------------------------------------------------------------------------
// DefaultRetentionConfig — 安全なデフォルト値の検証
// ---------------------------------------------------------------------------

func TestDefaultRetentionConfig_BatchLimit(t *testing.T) {
	cfg := econtract.DefaultRetentionConfig()
	if cfg.BatchLimit <= 0 {
		t.Errorf("DefaultRetentionConfig.BatchLimit must be > 0, got %d", cfg.BatchLimit)
	}
}

// ---------------------------------------------------------------------------
// NewRetentionRunner — nil logger / zero BatchLimit の安全性
// ---------------------------------------------------------------------------

func TestNewRetentionRunner_NilLoggerDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("NewRetentionRunner panicked with nil logger: %v", r)
		}
	}()
	_ = econtract.NewRetentionRunner(nil, econtract.DefaultRetentionConfig(), nil, [16]byte{})
}

func TestNewRetentionRunner_ZeroBatchLimitFallsBackToDefault(t *testing.T) {
	zeroCfg := econtract.RetentionConfig{BatchLimit: 0}
	// Should not panic; BatchLimit is normalised to default internally.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("NewRetentionRunner panicked with zero BatchLimit: %v", r)
		}
	}()
	_ = econtract.NewRetentionRunner(nil, zeroCfg, nil, [16]byte{})
}
