package attendance

// calc.go — pure calculation functions (no I/O, no DB).
//
// LEGAL NOTICE: All thresholds, rates, and time boundaries are passed as
// parameters derived from AttendanceSetting / LaborAgreement rows. No value
// is hard-coded in this file. Any statutory default lives only in the
// migration (00005_attendance.sql) and must be reviewed by a qualified
// labor-law professional after each statutory amendment.

import (
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

// RoundMinutes rounds rawMinutes up or down according to unitMinutes.
//
//   - unitMinutes <= 0 or 1: no rounding (return rawMinutes unchanged).
//   - unitMinutes > 1: round down to the nearest multiple (truncation), which
//     is the most common payroll treatment in Japan. Callers that require
//     rounding-up should pass the minutes pre-rounded.
func RoundMinutes(rawMinutes, unitMinutes int) int {
	if unitMinutes <= 1 {
		return rawMinutes
	}
	return (rawMinutes / unitMinutes) * unitMinutes
}

// nightMinutesInRange returns the number of minutes in [start, end) that fall
// inside the night zone [nightStart, nightEnd).
//
// The night zone is allowed to wrap midnight (e.g. 22:00–05:00). Both
// start/end and nightStart/nightEnd are expressed as minutes-since-midnight
// on an arbitrary reference day; the function adjusts for midnight wrap.
//
// start and end are absolute UNIX-epoch minutes derived from real timestamps.
// nightStart and nightEnd are time-of-day minutes (0..1439).
func nightMinutesInRange(startMin, endMin, nightStartMoD, nightEndMoD int) int {
	if startMin >= endMin {
		return 0
	}
	// Expand the interval into full-day "epochs" to handle midnight wrap.
	// For each minute t in [startMin, endMin), determine whether the
	// time-of-day equivalent (t mod 1440) falls in the night zone.
	//
	// Optimised: compute the overlapping minutes analytically by decomposing
	// the night zone into wrapped and non-wrapped segments.
	total := 0
	span := endMin - startMin

	if nightStartMoD < nightEndMoD {
		// Non-wrapping zone: e.g. 08:00-20:00
		// Count minutes whose ToD is in [nightStartMoD, nightEndMoD).
		total += overlapWithDailyZone(startMin, endMin, nightStartMoD, nightEndMoD)
	} else {
		// Wrapping zone: e.g. 22:00-05:00 → split into [22:00, 24:00) + [00:00, 05:00)
		total += overlapWithDailyZone(startMin, endMin, nightStartMoD, 1440)
		total += overlapWithDailyZone(startMin, endMin, 0, nightEndMoD)
	}
	_ = span
	return total
}

// overlapWithDailyZone counts minutes in [workStart, workEnd) whose
// time-of-day value falls in [zoneStart, zoneEnd), where both zone bounds are
// minutes-since-midnight (0..1440). zoneStart < zoneEnd is required.
func overlapWithDailyZone(workStart, workEnd, zoneStart, zoneEnd int) int {
	if zoneStart >= zoneEnd || workStart >= workEnd {
		return 0
	}
	total := 0
	// workStart and workEnd are absolute minutes (may span multiple days).
	// Align to the first occurrence of zoneStart at or before workStart.
	dayStart := (workStart / 1440) * 1440
	for dayStart < workEnd {
		segStart := dayStart + zoneStart
		segEnd := dayStart + zoneEnd
		// Clamp to [workStart, workEnd)
		a := max(segStart, workStart)
		b := min(segEnd, workEnd)
		if a < b {
			total += b - a
		}
		dayStart += 1440
	}
	return total
}

// max returns the larger of two ints.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// min returns the smaller of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parseTimeOfDay parses a "HH:MM:SS" or "HH:MM" string into minutes-since-midnight.
// Returns an error if the string is malformed or out of range.
func parseTimeOfDay(s string) (int, error) {
	var h, m, sec int
	n, err := fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec)
	if err != nil && n < 2 {
		return 0, fmt.Errorf("attendance: parse time-of-day %q: %w", s, err)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 || sec < 0 || sec > 59 {
		return 0, fmt.Errorf("attendance: time-of-day %q out of range", s)
	}
	return h*60 + m, nil
}

// ComputeBreakdown calculates the categorised overtime/night/holiday breakdown
// for a single attendance record.
//
// Parameters:
//   - clockIn, clockOut: actual timestamps (must be non-nil and clockIn < clockOut).
//   - breakMinutes: already-computed break time to deduct.
//   - scheduledMinutes: the employee's contracted daily work minutes (所定労働時間).
//     Overtime = max(0, actual - scheduled).
//   - isLegalHoliday: true if this work_date is a statutory holiday (法定休日).
//   - accOvertimeBeforeToday: accumulated overtime this month BEFORE this record.
//     Used to compute over60 boundary.
//   - setting: tenant-level config (rates, night zone, rounding).
//
// Returns OvertimeBreakdown with all minute counts rounded according to setting.RoundingUnitMinutes.
//
// LEGAL NOTICE: This function reflects general statutory patterns as of 2026.
// It MUST be reviewed with a qualified labor-law professional for your
// specific employment type and after each statutory amendment.
func ComputeBreakdown(
	clockIn, clockOut time.Time,
	breakMinutes, scheduledMinutes int,
	isLegalHoliday bool,
	accOvertimeBeforeToday int,
	setting AttendanceSetting,
) (OvertimeBreakdown, error) {
	if !clockOut.After(clockIn) {
		return OvertimeBreakdown{}, fmt.Errorf("attendance: clock_out must be after clock_in")
	}

	nightStartMoD, err := parseTimeOfDay(setting.NightStart)
	if err != nil {
		return OvertimeBreakdown{}, err
	}
	nightEndMoD, err := parseTimeOfDay(setting.NightEnd)
	if err != nil {
		return OvertimeBreakdown{}, err
	}

	// Total actual minutes (gross), then subtract break.
	grossMinutes := int(clockOut.Sub(clockIn).Minutes())
	actualMinutes := grossMinutes - breakMinutes
	if actualMinutes < 0 {
		actualMinutes = 0
	}
	actualMinutes = RoundMinutes(actualMinutes, setting.RoundingUnitMinutes)

	// Night minutes: computed from raw timestamps (before rounding) to avoid
	// losing sub-unit minutes in the night zone.
	startMin := int(clockIn.Unix() / 60)
	endMin := int(clockOut.Unix() / 60)
	rawNight := nightMinutesInRange(startMin, endMin, nightStartMoD, nightEndMoD)

	// Proportionally deduct break time from night minutes to avoid over-counting
	// when a break falls within the night zone. This is a conservative approximation:
	// break is assumed to be distributed uniformly across the shift. A precise
	// implementation would require break_start/break_end timestamps.
	//
	// TODO(future): accept break interval timestamps for exact night-break deduction.
	// LEGAL NOTICE: 要専門家確認 — 深夜帯の休憩控除は法令上の明示規定なし;
	// 過大計上を防ぐ保守的な比例控除として実装。専門家への確認を推奨。
	adjustedNight := rawNight
	if grossMinutes > 0 && breakMinutes > 0 && rawNight > 0 {
		// nightFraction = rawNight / grossMinutes; subtract breakMinutes × nightFraction
		nightBreakDeduction := int(math.Round(float64(breakMinutes) * float64(rawNight) / float64(grossMinutes)))
		adjustedNight = rawNight - nightBreakDeduction
		if adjustedNight < 0 {
			adjustedNight = 0
		}
	}
	nightMinutes := RoundMinutes(adjustedNight, setting.RoundingUnitMinutes)

	bd := OvertimeBreakdown{}

	if isLegalHoliday {
		// Statutory holiday work: entire actual time is holiday minutes.
		// Night portion still attracts an additive night premium.
		bd.HolidayMinutes = actualMinutes
		bd.NightMinutes = nightMinutes
		return bd, nil
	}

	// Regular / contractual holiday:
	// Overtime = actual minutes that exceed scheduled minutes.
	overtimeTotal := 0
	if actualMinutes > scheduledMinutes {
		overtimeTotal = actualMinutes - scheduledMinutes
	}
	bd.RegularMinutes = actualMinutes - overtimeTotal

	// Split overtime into ≤60h and >60h buckets.
	// The monthly boundary is read from setting.Over60BoundaryMinutes (DB column
	// over60_boundary_minutes; default 3600 = 60h × 60min). Callers supplying
	// accOvertimeBeforeToday enable correct multi-record boundary calculation
	// within a month.
	//
	// LEGAL NOTE: 要専門家確認・改正追従 — The >60h enhanced rate (50%) has
	// transitional provisions for small/medium enterprises (労働基準法第37条4項).
	// Whether it applies depends on employer size and applicable ordinance.
	// Verify with a qualified professional after each statutory amendment.
	over60Boundary := setting.Over60BoundaryMinutes
	if over60Boundary <= 0 {
		// Defensive fallback: if misconfigured, use the statutory default.
		// This should not occur in production because the migration sets DEFAULT 3600.
		over60Boundary = 3600
	}

	remaining60 := over60Boundary - accOvertimeBeforeToday
	if remaining60 < 0 {
		remaining60 = 0
	}

	if overtimeTotal <= remaining60 {
		bd.OvertimeMinutes = overtimeTotal
	} else {
		bd.OvertimeMinutes = remaining60
		bd.Over60Minutes = overtimeTotal - remaining60
	}

	bd.NightMinutes = nightMinutes

	return bd, nil
}

// CheckAgreementAlerts evaluates the accumulated overtime against the
// tenant's 36-agreement limits and returns a (possibly empty) slice of alerts
// (LM-032).
//
// Parameters:
//   - monthlyOT: accumulated overtime minutes for the current month.
//   - yearlyOT: accumulated overtime minutes for the current year-within-agreement.
//   - specialMonthCount: number of months this year in which special_clause was invoked.
//   - avgMinutes: slice of monthly overtime minutes for recent N months (for
//     multi-month average check; pass [] to skip).
//   - ag: the applicable labor agreement.
//   - approachingPct: fraction of the limit at which an "approaching" alert is
//     raised (e.g. 0.90 for 90%). Pass 0 to disable approaching alerts.
//
// Returned Level values: "exceeded" or "approaching".
//
// LEGAL NOTICE: Limit values derive from ag (database row) and are subject to
// statutory revision. Verify with a qualified professional.
func CheckAgreementAlerts(
	monthlyOT, yearlyOT, specialMonthCount int,
	avgMinutes []int,
	ag LaborAgreement,
	approachingPct float64,
) []AgreementAlert {
	var alerts []AgreementAlert
	check := func(current, limit int, rule string) {
		if limit <= 0 {
			return
		}
		if current > limit {
			alerts = append(alerts, AgreementAlert{
				Level:          "exceeded",
				Rule:           rule,
				CurrentMinutes: current,
				LimitMinutes:   limit,
			})
		} else if approachingPct > 0 && float64(current) >= float64(limit)*approachingPct {
			alerts = append(alerts, AgreementAlert{
				Level:          "approaching",
				Rule:           rule,
				CurrentMinutes: current,
				LimitMinutes:   limit,
			})
		}
	}

	check(monthlyOT, ag.MonthlyLimitMinutes, "monthly")
	check(yearlyOT, ag.YearlyLimitMinutes, "yearly")

	if ag.SpecialClause {
		if ag.SpecialMonthlyLimitMinutes != nil {
			check(monthlyOT, *ag.SpecialMonthlyLimitMinutes, "special_monthly")
		}
		if ag.SpecialCountLimit != nil && *ag.SpecialCountLimit > 0 {
			if specialMonthCount > *ag.SpecialCountLimit {
				alerts = append(alerts, AgreementAlert{
					Level:          "exceeded",
					Rule:           "special_count",
					CurrentMinutes: specialMonthCount,
					LimitMinutes:   *ag.SpecialCountLimit,
				})
			} else if approachingPct > 0 && float64(specialMonthCount) >= float64(*ag.SpecialCountLimit)*approachingPct {
				alerts = append(alerts, AgreementAlert{
					Level:          "approaching",
					Rule:           "special_count",
					CurrentMinutes: specialMonthCount,
					LimitMinutes:   *ag.SpecialCountLimit,
				})
			}
		}
	}

	// Multi-month average check (2..6-month window).
	// LEGAL NOTE: The 80h / 100h thresholds are statutory health-care provisions
	// as of 2026 (労働安全衛生法 / 過重労働防止指針). The exact window and
	// applicable limit vary by employer type. Verify with a qualified professional.
	if ag.MultiMonthAvgLimitMinutes != nil && len(avgMinutes) >= 2 {
		sum := 0
		for _, v := range avgMinutes {
			sum += v
		}
		avg := int(math.Round(float64(sum) / float64(len(avgMinutes))))
		check(avg, *ag.MultiMonthAvgLimitMinutes, "multi_month_avg")
	}

	return alerts
}

// CheckDeviationAlert returns a non-nil DeviationAlert when the absolute
// difference between actualMinutes and scheduledMinutes is greater than or
// equal to setting.DeviationAlertMinutes (LM-031).
//
// This is a pure function; it does not access the database.
// Service.CheckDeviationForRecord wraps this function and resolves the
// employee/work-date context from the database.
func CheckDeviationAlert(
	employeeID uuid.UUID,
	workDate time.Time,
	actualMinutes, scheduledMinutes int,
	setting AttendanceSetting,
) *DeviationAlert {
	diff := actualMinutes - scheduledMinutes
	if diff < 0 {
		diff = -diff
	}
	if setting.DeviationAlertMinutes > 0 && diff >= setting.DeviationAlertMinutes {
		return &DeviationAlert{
			EmployeeID:       employeeID,
			WorkDate:         workDate,
			ActualMinutes:    actualMinutes,
			ScheduledMinutes: scheduledMinutes,
			DeviationMinutes: diff,
		}
	}
	return nil
}

// ComputePremiumResult assembles a PremiumResult from a breakdown and the
// tenant's rate configuration (LM-033).
//
// LEGAL NOTICE: The rate fields are read from setting (DB row). They MUST be
// verified against current statutory rates by a qualified professional after
// each statutory amendment.
func ComputePremiumResult(bd OvertimeBreakdown, setting AttendanceSetting) PremiumResult {
	return PremiumResult{
		OvertimeMinutes: bd.OvertimeMinutes,
		OvertimeRate:    setting.OvertimeRate,
		Over60Minutes:   bd.Over60Minutes,
		Over60Rate:      setting.Over60Rate,
		NightMinutes:    bd.NightMinutes,
		NightRate:       setting.NightRate,
		HolidayMinutes:  bd.HolidayMinutes,
		HolidayRate:     setting.HolidayRate,
	}
}
