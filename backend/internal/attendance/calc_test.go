package attendance

// calc_test.go — pure unit tests for the calculation layer.
//
// These tests do NOT require Docker / database access. They test the
// statutory boundary logic to verify that the configurable values drive
// results correctly (i.e. changing a setting changes the outcome).
//
// LEGAL NOTICE: Test values are representative examples used to verify
// the calculation machinery. They do NOT constitute authoritative legal
// thresholds. Always confirm applicable rates/limits with a qualified
// labor-law professional.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultSetting returns a canonical AttendanceSetting for unit tests.
// All values come from attendance_settings defaults (migration 00005) which
// themselves mirror statutory minimums as of 2026-06-02. Tests that verify
// "changing the setting changes the result" deliberately mutate a copy.
func defaultSetting() AttendanceSetting {
	return AttendanceSetting{
		RoundingUnitMinutes:   1,
		OvertimeRate:          1.25,
		NightRate:             0.25,
		HolidayRate:           1.35,
		Over60Rate:            1.50,
		NightStart:            "22:00:00",
		NightEnd:              "05:00:00",
		BreakAutoMinutes:      0,
		DeviationAlertMinutes: 30,
		Over60BoundaryMinutes: 3600, // 60h × 60min — statutory default; 要専門家確認
	}
}

// ---------------------------------------------------------------------------
// RoundMinutes
// ---------------------------------------------------------------------------

func TestRoundMinutes_NoRounding(t *testing.T) {
	assert.Equal(t, 97, RoundMinutes(97, 0))
	assert.Equal(t, 97, RoundMinutes(97, 1))
}

func TestRoundMinutes_15MinUnit(t *testing.T) {
	assert.Equal(t, 90, RoundMinutes(97, 15), "97 minutes → truncated to 90")
	assert.Equal(t, 90, RoundMinutes(90, 15))
	assert.Equal(t, 105, RoundMinutes(119, 15), "119 minutes → truncated to 105")
}

func TestRoundMinutes_SettingDrivesResult(t *testing.T) {
	// Changing rounding_unit from 1 to 30 changes the result — demonstrates
	// that the value is configurable.
	assert.Equal(t, 97, RoundMinutes(97, 1))
	assert.Equal(t, 90, RoundMinutes(97, 30), "30-min unit: 97→90")
}

// ---------------------------------------------------------------------------
// nightMinutesInRange
// ---------------------------------------------------------------------------

func TestNightMinutesInRange_NonWrapping(t *testing.T) {
	// A hypothetical day-shift night zone: 10:00-14:00 (just for arithmetic)
	// 10*60=600, 14*60=840
	start := 0  // 00:00 absolute (day 1)
	end := 1440 // 24:00 same day
	got := nightMinutesInRange(start, end, 600, 840)
	assert.Equal(t, 240, got, "4h zone × 1 day = 240 min")
}

func TestNightMinutesInRange_WrappingMidnight(t *testing.T) {
	// Statutory night zone: 22:00-05:00 (1320-300, wrapping)
	// Work 20:00-06:00 → spans 10h
	// Night overlap: 22:00-05:00 = 7h = 420 min
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 20, 0, 0, 0, loc)
	co := time.Date(2024, 1, 16, 6, 0, 0, 0, loc)
	startMin := int(ci.Unix() / 60)
	endMin := int(co.Unix() / 60)
	got := nightMinutesInRange(startMin, endMin, 22*60, 5*60)
	assert.Equal(t, 420, got, "22:00-05:00 overlap in 20:00-06:00 shift")
}

func TestNightMinutesInRange_NoOverlap(t *testing.T) {
	// Day shift 08:00-17:00, night zone 22:00-05:00
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 8, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 17, 0, 0, 0, loc)
	startMin := int(ci.Unix() / 60)
	endMin := int(co.Unix() / 60)
	got := nightMinutesInRange(startMin, endMin, 22*60, 5*60)
	assert.Equal(t, 0, got, "day shift has no night overlap")
}

func TestNightMinutesInRange_FullyWithinNight(t *testing.T) {
	// Work 23:00-01:00 (2h), all within 22:00-05:00
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 23, 0, 0, 0, loc)
	co := time.Date(2024, 1, 16, 1, 0, 0, 0, loc)
	startMin := int(ci.Unix() / 60)
	endMin := int(co.Unix() / 60)
	got := nightMinutesInRange(startMin, endMin, 22*60, 5*60)
	assert.Equal(t, 120, got, "2h fully inside night zone = 120 min")
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — regular overtime split
// ---------------------------------------------------------------------------

func TestComputeBreakdown_NoOvertime(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 18, 0, 0, 0, loc) // 9h gross
	bd, err := ComputeBreakdown(ci, co, 60, 480, false, 0, defaultSetting())
	require.NoError(t, err)
	// 9h - 1h break = 8h = 480 actual, scheduled 480 → no overtime
	assert.Equal(t, 480, bd.RegularMinutes)
	assert.Equal(t, 0, bd.OvertimeMinutes)
	assert.Equal(t, 0, bd.Over60Minutes)
	assert.Equal(t, 0, bd.NightMinutes)
}

func TestComputeBreakdown_WithOvertime(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 21, 0, 0, 0, loc) // 12h gross
	bd, err := ComputeBreakdown(ci, co, 60, 480, false, 0, defaultSetting())
	require.NoError(t, err)
	// 12h - 1h break = 11h = 660 actual, scheduled 480 → 180 OT
	assert.Equal(t, 480, bd.RegularMinutes)
	assert.Equal(t, 180, bd.OvertimeMinutes)
	assert.Equal(t, 0, bd.Over60Minutes)
}

// TestComputeBreakdown_NightOverlap verifies that night minutes are correctly
// calculated for work that crosses 22:00.
func TestComputeBreakdown_NightOverlap(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 20, 0, 0, 0, loc)
	co := time.Date(2024, 1, 16, 1, 0, 0, 0, loc) // 5h total, 22:00-01:00 = 3h night
	bd, err := ComputeBreakdown(ci, co, 0, 480, false, 0, defaultSetting())
	require.NoError(t, err)
	// Night minutes: 22:00-01:00 = 180 min
	assert.Equal(t, 180, bd.NightMinutes)
}

// TestComputeBreakdown_NightZoneConfigurable demonstrates that changing the
// night zone in attendance_settings changes the calculated night minutes.
func TestComputeBreakdown_NightZoneConfigurable(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 20, 0, 0, 0, loc)
	co := time.Date(2024, 1, 16, 1, 0, 0, 0, loc)

	st := defaultSetting()
	bd1, err := ComputeBreakdown(ci, co, 0, 480, false, 0, st)
	require.NoError(t, err)

	// Change night zone to 20:00-06:00 → more minutes covered
	st.NightStart = "20:00:00"
	st.NightEnd = "06:00:00"
	bd2, err := ComputeBreakdown(ci, co, 0, 480, false, 0, st)
	require.NoError(t, err)

	assert.Greater(t, bd2.NightMinutes, bd1.NightMinutes,
		"wider night zone produces more night minutes")
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — monthly 60h boundary (LM-033)
// ---------------------------------------------------------------------------

// TestComputeBreakdown_Over60Boundary verifies the 60h monthly boundary split.
// LEGAL NOTE: The 60h threshold and 50% rate are subject to transitional
// provisions for small/medium enterprises. Verify applicability with a
// qualified professional.
func TestComputeBreakdown_Over60Boundary_JustBelow(t *testing.T) {
	loc := time.UTC
	// accOT = 3540 min (59h), today adds 120 min OT → total 3660 > 3600
	// Under boundary: 3600-3540=60 min at normal OT, 60 min at over60
	ci := time.Date(2024, 1, 30, 8, 0, 0, 0, loc)
	co := time.Date(2024, 1, 30, 16, 0, 0, 0, loc) // 8h, scheduled 6h → 120 OT
	bd, err := ComputeBreakdown(ci, co, 0, 360, false, 3540, defaultSetting())
	require.NoError(t, err)
	assert.Equal(t, 60, bd.OvertimeMinutes, "60 min under the boundary")
	assert.Equal(t, 60, bd.Over60Minutes, "60 min over the boundary")
}

func TestComputeBreakdown_Over60Boundary_AtExact60h(t *testing.T) {
	// accOT = 3600 min (exactly 60h), any further OT goes to over60.
	loc := time.UTC
	ci := time.Date(2024, 1, 31, 8, 0, 0, 0, loc)
	co := time.Date(2024, 1, 31, 10, 0, 0, 0, loc) // 2h, scheduled 0h → 120 OT
	bd, err := ComputeBreakdown(ci, co, 0, 0, false, 3600, defaultSetting())
	require.NoError(t, err)
	assert.Equal(t, 0, bd.OvertimeMinutes)
	assert.Equal(t, 120, bd.Over60Minutes, "all 120 min above 60h boundary")
}

func TestComputeBreakdown_Over60_RateConfigurable(t *testing.T) {
	// Changing over60_rate in settings changes the PremiumResult — not the
	// minute count, but the rate. This test verifies the rate propagation.
	st := defaultSetting()
	loc := time.UTC
	ci := time.Date(2024, 1, 31, 8, 0, 0, 0, loc)
	co := time.Date(2024, 1, 31, 10, 0, 0, 0, loc)
	bd, err := ComputeBreakdown(ci, co, 0, 0, false, 3600, st)
	require.NoError(t, err)

	pr1 := ComputePremiumResult(bd, st)
	assert.Equal(t, 1.50, pr1.Over60Rate)

	// Change rate to 1.75 (hypothetical future amendment)
	st.Over60Rate = 1.75
	pr2 := ComputePremiumResult(bd, st)
	assert.Equal(t, 1.75, pr2.Over60Rate,
		"changing over60_rate in settings changes the resulting rate")
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — legal holiday (LM-033)
// ---------------------------------------------------------------------------

func TestComputeBreakdown_LegalHoliday(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 14, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 14, 17, 0, 0, 0, loc)
	bd, err := ComputeBreakdown(ci, co, 60, 0, true, 0, defaultSetting())
	require.NoError(t, err)
	// Legal holiday: all actual minutes → HolidayMinutes (no OT bucket)
	expected := 7 * 60 // 7h × 60 = 420 min net
	assert.Equal(t, expected, bd.HolidayMinutes)
	assert.Equal(t, 0, bd.OvertimeMinutes, "no overtime on legal holiday")
	assert.Equal(t, 0, bd.Over60Minutes)
}

func TestComputeBreakdown_LegalHoliday_WithNight(t *testing.T) {
	loc := time.UTC
	// Legal holiday with night-zone overlap
	ci := time.Date(2024, 1, 14, 20, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 2, 0, 0, 0, loc) // 6h, 22:00-02:00 = 4h night
	bd, err := ComputeBreakdown(ci, co, 0, 0, true, 0, defaultSetting())
	require.NoError(t, err)
	assert.Equal(t, 360, bd.HolidayMinutes)
	assert.Equal(t, 240, bd.NightMinutes, "night portion still attributed on legal holiday")
}

func TestComputeBreakdown_HolidayRate_Configurable(t *testing.T) {
	st := defaultSetting()
	loc := time.UTC
	ci := time.Date(2024, 1, 14, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 14, 17, 0, 0, 0, loc)
	bd, _ := ComputeBreakdown(ci, co, 60, 0, true, 0, st)

	pr1 := ComputePremiumResult(bd, st)
	assert.Equal(t, 1.35, pr1.HolidayRate)

	st.HolidayRate = 1.60 // hypothetical amendment
	pr2 := ComputePremiumResult(bd, st)
	assert.Equal(t, 1.60, pr2.HolidayRate,
		"changing holiday_rate in settings propagates correctly")
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — overnight (日跨ぎ)
// ---------------------------------------------------------------------------

func TestComputeBreakdown_OvernightShift(t *testing.T) {
	loc := time.UTC
	// Night shift: 22:00 → 06:00 next day = 8h
	ci := time.Date(2024, 1, 15, 22, 0, 0, 0, loc)
	co := time.Date(2024, 1, 16, 6, 0, 0, 0, loc)
	bd, err := ComputeBreakdown(ci, co, 60, 420, false, 0, defaultSetting())
	require.NoError(t, err)
	// 8h - 1h break = 7h = 420 actual = scheduled → no OT
	// Night zone 22:00-05:00 = 7h = 420 raw night; minus break proportional (ignored) = ~420
	assert.Equal(t, 0, bd.OvertimeMinutes)
	assert.Greater(t, bd.NightMinutes, 0, "overnight shift produces night minutes")
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — break deduction / rounding interaction
// ---------------------------------------------------------------------------

func TestComputeBreakdown_BreakDeduction(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 18, 0, 0, 0, loc) // 9h gross
	bd, err := ComputeBreakdown(ci, co, 60, 480, false, 0, defaultSetting())
	require.NoError(t, err)
	assert.Equal(t, 480, bd.RegularMinutes+bd.OvertimeMinutes, "break correctly deducted")
}

func TestComputeBreakdown_Rounding15Min(t *testing.T) {
	loc := time.UTC
	// 9h2m gross - 60m break = 482min actual. With 15-min rounding → 480min.
	ci := time.Date(2024, 1, 15, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 18, 2, 0, 0, loc)

	st := defaultSetting()
	st.RoundingUnitMinutes = 15
	bd, err := ComputeBreakdown(ci, co, 60, 480, false, 0, st)
	require.NoError(t, err)
	assert.Equal(t, 480, bd.RegularMinutes, "15-min rounding truncates 482→480")
}

func TestComputeBreakdown_Rounding_UnitChangesDrivesResult(t *testing.T) {
	loc := time.UTC
	ci := time.Date(2024, 1, 15, 9, 0, 0, 0, loc)
	co := time.Date(2024, 1, 15, 18, 7, 0, 0, loc) // 9h7m gross - 60m = 487min

	st1 := defaultSetting()
	st1.RoundingUnitMinutes = 1
	bd1, _ := ComputeBreakdown(ci, co, 60, 480, false, 0, st1)

	st2 := defaultSetting()
	st2.RoundingUnitMinutes = 30
	bd2, _ := ComputeBreakdown(ci, co, 60, 480, false, 0, st2)

	assert.NotEqual(t, bd1.OvertimeMinutes, bd2.OvertimeMinutes,
		"rounding unit in settings drives overtime minute result")
}

// ---------------------------------------------------------------------------
// CheckAgreementAlerts — 36協定 boundary tests (LM-032)
// ---------------------------------------------------------------------------

func makeAgreement(monthlyMin, yearlyMin int, special bool, specialMonthlyMin, specialCount, multiAvgMin *int) LaborAgreement {
	return LaborAgreement{
		MonthlyLimitMinutes:        monthlyMin,
		YearlyLimitMinutes:         yearlyMin,
		SpecialClause:              special,
		SpecialMonthlyLimitMinutes: specialMonthlyMin,
		SpecialCountLimit:          specialCount,
		MultiMonthAvgLimitMinutes:  multiAvgMin,
	}
}

// LEGAL NOTE: The monthly/yearly limits below (2700/21600) mirror default
// statutory values as of 2026-06-02. They are configured per-tenant in
// labor_agreements and are NOT hard-coded thresholds. Verify with a qualified
// professional after each amendment.
func TestCheckAgreementAlerts_MonthlyBelowLimit(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	// 2000 is well below 2700 and also below 90% of 2700 (=2430), so no alert at all.
	alerts := CheckAgreementAlerts(2000, 10000, 0, nil, ag, 0.9)
	assert.Empty(t, alerts, "well below monthly limit and approaching threshold → no alert")
}

func TestCheckAgreementAlerts_MonthlyJustBelowLimit_NoApproaching(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	// 2699 is just below 2700 but is above 90% threshold → approaching alert, not exceeded.
	alerts := CheckAgreementAlerts(2699, 10000, 0, nil, ag, 0)
	assert.Empty(t, alerts, "approaching=0 disables approaching alerts; 2699 < 2700 not exceeded")
}

func TestCheckAgreementAlerts_MonthlyAtLimit(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	alerts := CheckAgreementAlerts(2700, 10000, 0, nil, ag, 0)
	assert.Empty(t, alerts, "at limit = not exceeded")
}

func TestCheckAgreementAlerts_MonthlyExceeded(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	alerts := CheckAgreementAlerts(2701, 10000, 0, nil, ag, 0)
	require.Len(t, alerts, 1)
	assert.Equal(t, "exceeded", alerts[0].Level)
	assert.Equal(t, "monthly", alerts[0].Rule)
}

func TestCheckAgreementAlerts_YearlyBelowLimit(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	alerts := CheckAgreementAlerts(100, 21599, 0, nil, ag, 0)
	assert.Empty(t, alerts)
}

func TestCheckAgreementAlerts_YearlyExceeded(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	alerts := CheckAgreementAlerts(100, 21601, 0, nil, ag, 0)
	require.Len(t, alerts, 1)
	assert.Equal(t, "exceeded", alerts[0].Level)
	assert.Equal(t, "yearly", alerts[0].Rule)
}

func TestCheckAgreementAlerts_Approaching(t *testing.T) {
	ag := makeAgreement(2700, 21600, false, nil, nil, nil)
	// 90% of 2700 = 2430; 2430 exactly triggers "approaching"
	alerts := CheckAgreementAlerts(2430, 10000, 0, nil, ag, 0.9)
	require.Len(t, alerts, 1)
	assert.Equal(t, "approaching", alerts[0].Level)
	assert.Equal(t, "monthly", alerts[0].Rule)
}

func TestCheckAgreementAlerts_SpecialCountExceeded(t *testing.T) {
	limit := 6
	specialMonthly := 4800
	ag := makeAgreement(2700, 21600, true, &specialMonthly, &limit, nil)
	// 7 special months > 6 allowed
	alerts := CheckAgreementAlerts(100, 10000, 7, nil, ag, 0)
	// should have at least one "special_count" exceeded alert
	var found bool
	for _, a := range alerts {
		if a.Rule == "special_count" && a.Level == "exceeded" {
			found = true
		}
	}
	assert.True(t, found, "special_count exceeded alert expected")
}

func TestCheckAgreementAlerts_SpecialCountAtLimit(t *testing.T) {
	limit := 6
	specialMonthly := 4800
	ag := makeAgreement(2700, 21600, true, &specialMonthly, &limit, nil)
	alerts := CheckAgreementAlerts(100, 10000, 6, nil, ag, 0)
	// exactly at limit → no exceeded
	for _, a := range alerts {
		assert.NotEqual(t, "special_count", a.Rule, "at limit should not trigger exceeded")
	}
}

func TestCheckAgreementAlerts_MultiMonthAvg_2Month(t *testing.T) {
	// 2-month average: (4000 + 4000) / 2 = 4000 < 4800 → no alert
	avgLimit := 4800
	ag := makeAgreement(2700, 21600, false, nil, nil, &avgLimit)
	alerts := CheckAgreementAlerts(100, 10000, 0, []int{4000, 4000}, ag, 0)
	assert.Empty(t, alerts)
}

func TestCheckAgreementAlerts_MultiMonthAvg_Exceeded(t *testing.T) {
	// 3-month average: (5000+5000+4800)/3 = 4933 > 4800 → exceeded
	avgLimit := 4800
	ag := makeAgreement(2700, 21600, false, nil, nil, &avgLimit)
	alerts := CheckAgreementAlerts(100, 10000, 0, []int{5000, 5000, 4800}, ag, 0)
	var found bool
	for _, a := range alerts {
		if a.Rule == "multi_month_avg" && a.Level == "exceeded" {
			found = true
		}
	}
	assert.True(t, found, "multi-month average exceeded alert expected")
}

// LEGAL NOTE: The 80h / 100h thresholds for multi-month average are statutory
// health-care provisions subject to revision. The actual limits come from the
// labor_agreements table; these tests use the default (4800 min = 80h).
func TestCheckAgreementAlerts_MultiMonthAvg_6Month(t *testing.T) {
	// 6-month window: average 3000 (50h) < 4800 → no alert
	avgLimit := 4800
	ag := makeAgreement(2700, 21600, false, nil, nil, &avgLimit)
	alerts := CheckAgreementAlerts(100, 10000, 0, []int{3000, 3000, 3000, 3000, 3000, 3000}, ag, 0)
	assert.Empty(t, alerts)
}

func TestCheckAgreementAlerts_MultiMonthAvg_Below2Month_Skip(t *testing.T) {
	// With fewer than 2 months, multi-month average is not evaluated.
	avgLimit := 4800
	ag := makeAgreement(2700, 21600, false, nil, nil, &avgLimit)
	alerts := CheckAgreementAlerts(100, 10000, 0, []int{99999}, ag, 0) // 1 month only
	for _, a := range alerts {
		assert.NotEqual(t, "multi_month_avg", a.Rule, "single month → no avg check")
	}
}

func TestCheckAgreementAlerts_LimitsAreConfigurable(t *testing.T) {
	// Key test: changing the limit values in the agreement changes the result.
	ag1 := makeAgreement(2700, 21600, false, nil, nil, nil)
	ag2 := makeAgreement(3000, 21600, false, nil, nil, nil) // higher monthly limit

	alertsWithStrictLimit := CheckAgreementAlerts(2800, 10000, 0, nil, ag1, 0)
	alertsWithLooseLimit := CheckAgreementAlerts(2800, 10000, 0, nil, ag2, 0)

	assert.NotEmpty(t, alertsWithStrictLimit, "2800 exceeds 2700")
	assert.Empty(t, alertsWithLooseLimit, "2800 does not exceed 3000")
}

// ---------------------------------------------------------------------------
// CheckDeviationAlert (LM-031)
// ---------------------------------------------------------------------------

// testEmployeeID is a synthetic UUID used in CheckDeviationAlert unit tests.
// It contains no real PII.
var testEmployeeID = uuid.MustParse("00000000-0000-0000-0000-000000000001")
var testWorkDate = time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)

func TestCheckDeviationAlert_BelowThreshold(t *testing.T) {
	st := defaultSetting() // threshold = 30 min
	da := CheckDeviationAlert(testEmployeeID, testWorkDate, 29, 0, st)
	assert.Nil(t, da)
}

func TestCheckDeviationAlert_AtThreshold(t *testing.T) {
	st := defaultSetting()
	da := CheckDeviationAlert(testEmployeeID, testWorkDate, 30, 0, st) // diff == threshold → alert
	assert.NotNil(t, da)
}

func TestCheckDeviationAlert_AboveThreshold(t *testing.T) {
	st := defaultSetting()
	da := CheckDeviationAlert(testEmployeeID, testWorkDate, 60, 0, st)
	assert.NotNil(t, da)
	assert.Equal(t, 60, da.DeviationMinutes)
}

func TestCheckDeviationAlert_ThresholdConfigurable(t *testing.T) {
	st := defaultSetting()
	st.DeviationAlertMinutes = 90

	da60 := CheckDeviationAlert(testEmployeeID, testWorkDate, 60, 0, st)
	da90 := CheckDeviationAlert(testEmployeeID, testWorkDate, 90, 0, st)
	assert.Nil(t, da60, "60 min < threshold 90 → no alert")
	assert.NotNil(t, da90, "90 min == threshold → alert")
}

func TestCheckDeviationAlert_PopulatesFields(t *testing.T) {
	st := defaultSetting()
	da := CheckDeviationAlert(testEmployeeID, testWorkDate, 540, 480, st)
	require.NotNil(t, da)
	assert.Equal(t, testEmployeeID, da.EmployeeID)
	assert.Equal(t, testWorkDate, da.WorkDate)
	assert.Equal(t, 540, da.ActualMinutes)
	assert.Equal(t, 480, da.ScheduledMinutes)
	assert.Equal(t, 60, da.DeviationMinutes)
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — over60 boundary configurability (MUSTFIX 1)
// ---------------------------------------------------------------------------

// TestComputeBreakdown_Over60Boundary_Configurable verifies that changing
// Over60BoundaryMinutes in AttendanceSetting changes the over60 split result.
// LEGAL NOTE: 要専門家確認・改正追従 — 月60h境界は labor_standards 改正で変わりうる。
// 設定値を変えると判定が変わることを確認するテスト。
func TestComputeBreakdown_Over60Boundary_Configurable(t *testing.T) {
	loc := time.UTC
	// Work 8h with 0 scheduled → all 480 min is overtime.
	// accOT = 3000 min already accumulated this month.
	ci := time.Date(2024, 1, 28, 8, 0, 0, 0, loc)
	co := time.Date(2024, 1, 28, 16, 0, 0, 0, loc)

	// Default boundary 3600: remaining = 3600-3000 = 600 > 480, so all OT is ≤60h.
	st := defaultSetting() // Over60BoundaryMinutes = 3600
	bd1, err := ComputeBreakdown(ci, co, 0, 0, false, 3000, st)
	require.NoError(t, err)
	assert.Equal(t, 480, bd1.OvertimeMinutes, "all OT under 3600 boundary")
	assert.Equal(t, 0, bd1.Over60Minutes)

	// Lower boundary to 3300 (55h): remaining = 3300-3000 = 300; 480-300 = 180 over60.
	st.Over60BoundaryMinutes = 3300
	bd2, err := ComputeBreakdown(ci, co, 0, 0, false, 3000, st)
	require.NoError(t, err)
	assert.Equal(t, 300, bd2.OvertimeMinutes, "300 min under 3300 boundary")
	assert.Equal(t, 180, bd2.Over60Minutes, "180 min over the lower boundary")

	// Verify the two results differ — changing the setting changed the outcome.
	assert.NotEqual(t, bd1.Over60Minutes, bd2.Over60Minutes,
		"Over60BoundaryMinutes in settings drives the over-60h split result")
}

// ---------------------------------------------------------------------------
// ComputeBreakdown — night break proportional deduction (Imp 3)
// ---------------------------------------------------------------------------

// TestComputeBreakdown_NightMinutes_BreakProportionalDeduction verifies that a
// break occurring during a shift that overlaps the night zone causes night_minutes
// to be less than the raw (undeducted) overlap. This prevents over-counting when
// break time falls within the night zone.
//
// LEGAL NOTE: 要専門家確認 — 深夜帯の休憩控除は法令上の明示規定なし;
// 比例控除による保守的な実装であることを確認すること。
func TestComputeBreakdown_NightMinutes_BreakProportionalDeduction(t *testing.T) {
	loc := time.UTC
	// Shift: 20:00–02:00 (6h gross), 60 min break.
	// Raw night zone overlap (22:00-02:00) = 4h = 240 min (of 6h gross).
	// Without break deduction: nightMinutes = 240.
	// With proportional deduction: breakDeduction = 60 × (240/360) = 40 min.
	// Expected nightMinutes ≤ 240 (should be 200 after deduction).
	ci := time.Date(2024, 1, 15, 20, 0, 0, 0, loc)
	co := time.Date(2024, 1, 16, 2, 0, 0, 0, loc)

	st := defaultSetting()
	bdWithBreak, err := ComputeBreakdown(ci, co, 60, 480, false, 0, st)
	require.NoError(t, err)

	bdNoBreak, err := ComputeBreakdown(ci, co, 0, 480, false, 0, st)
	require.NoError(t, err)

	assert.Less(t, bdWithBreak.NightMinutes, bdNoBreak.NightMinutes,
		"break during a night-zone shift should reduce night_minutes (proportional deduction)")
	assert.LessOrEqual(t, bdWithBreak.NightMinutes, 240,
		"night_minutes must not exceed raw night zone overlap")
}

// ---------------------------------------------------------------------------
// parseTimeOfDay
// ---------------------------------------------------------------------------

func TestParseTimeOfDay_Valid(t *testing.T) {
	v, err := parseTimeOfDay("22:00:00")
	require.NoError(t, err)
	assert.Equal(t, 22*60, v)

	v2, err := parseTimeOfDay("05:30:00")
	require.NoError(t, err)
	assert.Equal(t, 5*60+30, v2)
}

func TestParseTimeOfDay_Invalid(t *testing.T) {
	_, err := parseTimeOfDay("not-a-time")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// multiMonthAvg helper
// ---------------------------------------------------------------------------

func TestMultiMonthAvg(t *testing.T) {
	assert.Equal(t, 0, multiMonthAvg(nil))
	assert.Equal(t, 3000, multiMonthAvg([]int{3000}))
	assert.Equal(t, 3000, multiMonthAvg([]int{2000, 4000}))
	assert.Equal(t, 3333, multiMonthAvg([]int{2000, 3000, 5000})) // (10000/3=3333.3→3333)
}
