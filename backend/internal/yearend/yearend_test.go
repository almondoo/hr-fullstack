package yearend

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CalculateTax unit tests
// ---------------------------------------------------------------------------

// TestCalculateTax_Zero verifies zero income produces zero tax.
func TestCalculateTax_Zero(t *testing.T) {
	result := CalculateTax(TaxInput{})
	assert.Equal(t, int64(0), result.TaxableIncome)
	assert.Equal(t, int64(0), result.AnnualTax)
	assert.Equal(t, int64(0), result.Difference)
}

// TestCalculateTax_BasicCase verifies the calculation for a typical salary worker.
func TestCalculateTax_BasicCase(t *testing.T) {
	// 年収 5,000,000円, 給与所得控除 1,440,000円 (approx for 5M range)
	// 基礎控除 480,000円, 社会保険料控除 714,000円 (approx)
	in := TaxInput{
		GrossIncome:              5_000_000,
		EmploymentDeduction:      1_440_000,
		BasicDeduction:           480_000,
		SocialInsuranceDeduction: 714_000,
		WithheldTax:              150_000,
	}
	result := CalculateTax(in)

	// 給与所得 = 5,000,000 - 1,440,000 = 3,560,000
	// 所得控除 = 480,000 + 714,000 = 1,194,000
	// 課税所得 = 3,560,000 - 1,194,000 = 2,366,000 → 千円未満切捨 = 2,366,000
	assert.Equal(t, int64(2_366_000), result.TaxableIncome)
	assert.Greater(t, result.AnnualTax, int64(0))
	assert.Equal(t, result.AnnualTax-in.WithheldTax, result.Difference)
}

// TestCalculateTax_HousingLoan verifies housing loan deduction reduces tax.
func TestCalculateTax_HousingLoan(t *testing.T) {
	in := TaxInput{
		GrossIncome:          8_000_000,
		EmploymentDeduction:  1_950_000, // approximate
		BasicDeduction:       480_000,
		HousingLoanDeduction: 200_000,
		WithheldTax:          500_000,
	}
	inNoHousing := in
	inNoHousing.HousingLoanDeduction = 0

	withHousing := CalculateTax(in)
	withoutHousing := CalculateTax(inNoHousing)

	// Housing loan deduction reduces annual tax.
	assert.LessOrEqual(t, withHousing.AnnualTax, withoutHousing.AnnualTax)
}

// TestCalculateTax_NegativeTaxableIncome verifies that taxable income is clamped to 0.
func TestCalculateTax_NegativeTaxableIncome(t *testing.T) {
	in := TaxInput{
		GrossIncome:         1_000_000,
		EmploymentDeduction: 1_000_000,
		BasicDeduction:      480_000, // deductions exceed income
	}
	result := CalculateTax(in)
	assert.Equal(t, int64(0), result.TaxableIncome)
	assert.Equal(t, int64(0), result.AnnualTax)
}

// TestCalculateTax_ResultJSON verifies TaxResult marshals cleanly to JSON.
func TestCalculateTax_ResultJSON(t *testing.T) {
	result := CalculateTax(TaxInput{
		GrossIncome:         4_000_000,
		EmploymentDeduction: 1_240_000,
		BasicDeduction:      480_000,
		WithheldTax:         100_000,
	})
	b, err := json.Marshal(result)
	require.NoError(t, err)
	require.True(t, json.Valid(b))

	var decoded TaxResult
	require.NoError(t, json.Unmarshal(b, &decoded))
	assert.Equal(t, result.TaxableIncome, decoded.TaxableIncome)
	assert.Equal(t, result.AnnualTax, decoded.AnnualTax)
}

// ---------------------------------------------------------------------------
// BasicDeductionForYear — 年度依存・基礎控除 (令和7年度税制改正対応)
// ---------------------------------------------------------------------------

// TestBasicDeductionForYear_R6_Brackets verifies the 令和6年分以前の基礎控除段階。
// 出典: 国税庁 No.1199 https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1199.htm
func TestBasicDeductionForYear_R6_Brackets(t *testing.T) {
	tests := []struct {
		name        string
		taxYear     int
		totalIncome int64
		want        int64
	}{
		// 令和6年分 — 合計所得2,400万以下 → 48万
		{name: "R6_below24M", taxYear: 2024, totalIncome: 24_000_000, want: 480_000},
		{name: "R6_low",      taxYear: 2024, totalIncome: 5_000_000,  want: 480_000},
		{name: "R6_zero",     taxYear: 2024, totalIncome: 0,           want: 480_000},
		// 2,400万超2,450万以下 → 32万
		{name: "R6_24.1M",   taxYear: 2024, totalIncome: 24_100_000, want: 320_000},
		{name: "R6_24.5M",   taxYear: 2024, totalIncome: 24_500_000, want: 320_000},
		// 2,450万超2,500万以下 → 16万
		{name: "R6_24.6M",   taxYear: 2024, totalIncome: 24_600_000, want: 160_000},
		{name: "R6_25M",     taxYear: 2024, totalIncome: 25_000_000, want: 160_000},
		// 2,500万超 → 0
		{name: "R6_25.1M",   taxYear: 2024, totalIncome: 25_100_000, want: 0},
		// 令和5年分も同じ
		{name: "R5_low",     taxYear: 2023, totalIncome: 5_000_000,  want: 480_000},
		{name: "R5_high",    taxYear: 2023, totalIncome: 26_000_000, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BasicDeductionForYear(tc.taxYear, tc.totalIncome)
			assert.Equal(t, tc.want, got,
				"taxYear=%d totalIncome=%d", tc.taxYear, tc.totalIncome)
		})
	}
}

// TestBasicDeductionForYear_R7_Brackets verifies the 令和7・8年分の段階制基礎控除。
// 出典: 国税庁「令和7年分の基礎控除等の改正について」
// https://www.nta.go.jp/users/gensen/2025kiso/index.htm
// 国税庁 No.1199 https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1199.htm
func TestBasicDeductionForYear_R7_Brackets(t *testing.T) {
	tests := []struct {
		name        string
		taxYear     int
		totalIncome int64
		want        int64
	}{
		// 132万以下 → 95万
		{name: "R7_0",          taxYear: 2025, totalIncome: 0,           want: 950_000},
		{name: "R7_1.32M",      taxYear: 2025, totalIncome: 1_320_000,   want: 950_000},
		// 132万超336万以下 → 88万
		{name: "R7_1.32M+1",    taxYear: 2025, totalIncome: 1_320_001,   want: 880_000},
		{name: "R7_3.36M",      taxYear: 2025, totalIncome: 3_360_000,   want: 880_000},
		// 336万超489万以下 → 68万
		{name: "R7_3.36M+1",    taxYear: 2025, totalIncome: 3_360_001,   want: 680_000},
		{name: "R7_4.89M",      taxYear: 2025, totalIncome: 4_890_000,   want: 680_000},
		// 489万超655万以下 → 63万
		{name: "R7_4.89M+1",    taxYear: 2025, totalIncome: 4_890_001,   want: 630_000},
		{name: "R7_6.55M",      taxYear: 2025, totalIncome: 6_550_000,   want: 630_000},
		// 655万超2,350万以下 → 58万
		{name: "R7_6.55M+1",    taxYear: 2025, totalIncome: 6_550_001,   want: 580_000},
		{name: "R7_23.5M",      taxYear: 2025, totalIncome: 23_500_000,  want: 580_000},
		// 令和8年分(taxYear=2026)も同じ段階
		{name: "R8_low",        taxYear: 2026, totalIncome: 500_000,     want: 950_000},
		{name: "R8_mid",        taxYear: 2026, totalIncome: 5_000_000,   want: 630_000}, // 489万超655万以下 → 63万
		{name: "R8_655plus",    taxYear: 2026, totalIncome: 10_000_000,  want: 580_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BasicDeductionForYear(tc.taxYear, tc.totalIncome)
			assert.Equal(t, tc.want, got,
				"taxYear=%d totalIncome=%d", tc.taxYear, tc.totalIncome)
		})
	}
}

// TestBasicDeductionForYear_R9_Brackets verifies the 令和9年分以後の基礎控除。
// 中間層上乗せが解消し、132万以下=95万 / 132万超23,500,000以下=58万。
// TODO(legal/reiwa9): 詳細は一次情報確認後に更新すること。
func TestBasicDeductionForYear_R9_Brackets(t *testing.T) {
	tests := []struct {
		name        string
		taxYear     int
		totalIncome int64
		want        int64
	}{
		// 132万以下 → 95万 (令和7との共通部分)
		{name: "R9_0",        taxYear: 2027, totalIncome: 0,           want: 950_000},
		{name: "R9_1.32M",    taxYear: 2027, totalIncome: 1_320_000,   want: 950_000},
		// 132万超23,500,000以下 → 58万(中間層上乗せ解消で一律58万)
		{name: "R9_1.32M+1",  taxYear: 2027, totalIncome: 1_320_001,   want: 580_000},
		{name: "R9_3M",       taxYear: 2027, totalIncome: 3_000_000,   want: 580_000},
		{name: "R9_5M",       taxYear: 2027, totalIncome: 5_000_000,   want: 580_000},
		{name: "R9_23.5M",    taxYear: 2027, totalIncome: 23_500_000,  want: 580_000},
		// 令和10年分も同様
		{name: "R10_low",     taxYear: 2028, totalIncome: 500_000,     want: 950_000},
		{name: "R10_mid",     taxYear: 2028, totalIncome: 5_000_000,   want: 580_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BasicDeductionForYear(tc.taxYear, tc.totalIncome)
			assert.Equal(t, tc.want, got,
				"taxYear=%d totalIncome=%d", tc.taxYear, tc.totalIncome)
		})
	}
}

// TestBasicDeductionForYear_HighIncome_Compat verifies that the provisional
// high-income taper (R6-compat) is applied for incomes above 23,500,000 yen
// in R7/R8/R9 years. This is a temporary fallback pending authoritative R7
// taper confirmation (see TODO(legal/reiwa7-highearner)).
func TestBasicDeductionForYear_HighIncome_Compat(t *testing.T) {
	// For R7 (2025) with income > 23,500,000: R6-compat taper is applied.
	// 24M → 320k (暫定), 24.5M → 160k (暫定), >25M → 0 (暫定)
	assert.Equal(t, int64(320_000), BasicDeductionForYear(2025, 24_000_000),
		"R7 income=24M: R6-compat taper should return 320,000 (provisional)")
	assert.Equal(t, int64(160_000), BasicDeductionForYear(2025, 24_500_000),
		"R7 income=24.5M: R6-compat taper should return 160,000 (provisional)")
	assert.Equal(t, int64(0), BasicDeductionForYear(2025, 25_000_001),
		"R7 income>25M: R6-compat taper should return 0 (provisional)")
}

// ---------------------------------------------------------------------------
// EmploymentDeductionMinimum — 給与所得控除の最低保障額 (年度依存)
// ---------------------------------------------------------------------------

// TestEmploymentDeductionMinimum verifies the floor switch at taxYear=2025.
// 出典: 国税庁「令和7年分の基礎控除等の改正について」
// https://www.nta.go.jp/users/gensen/2025kiso/index.htm
func TestEmploymentDeductionMinimum(t *testing.T) {
	tests := []struct {
		name    string
		taxYear int
		want    int64
	}{
		// 令和6年分以前 → 55万
		{name: "R5_2023",  taxYear: 2023, want: 550_000},
		{name: "R6_2024",  taxYear: 2024, want: 550_000},
		// 令和7年分以後 → 65万
		{name: "R7_2025",  taxYear: 2025, want: 650_000},
		{name: "R8_2026",  taxYear: 2026, want: 650_000},
		{name: "R9_2027",  taxYear: 2027, want: 650_000},
		{name: "R10_2028", taxYear: 2028, want: 650_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EmploymentDeductionMinimum(tc.taxYear)
			assert.Equal(t, tc.want, got, "taxYear=%d", tc.taxYear)
		})
	}
}

// ---------------------------------------------------------------------------
// DependentIncomeLimit — 扶養親族・同一生計配偶者の合計所得要件 (年度依存)
// ---------------------------------------------------------------------------

// TestDependentIncomeLimit verifies the income threshold for dependents and
// qualifying spouse across tax year ranges.
// 出典: 国税庁 No.1191 配偶者控除
// https://www.nta.go.jp/taxes/shiraberu/taxanswer/shotoku/1191.htm
func TestDependentIncomeLimit(t *testing.T) {
	tests := []struct {
		name    string
		taxYear int
		want    int64
	}{
		// 令和元年分以前 → 38万
		{name: "pre2020_2018", taxYear: 2018, want: 380_000},
		{name: "pre2020_2019", taxYear: 2019, want: 380_000},
		// 令和2〜6年分 → 48万
		{name: "R2_2020",  taxYear: 2020, want: 480_000},
		{name: "R3_2021",  taxYear: 2021, want: 480_000},
		{name: "R6_2024",  taxYear: 2024, want: 480_000},
		// 令和7年分以後 → 58万
		{name: "R7_2025",  taxYear: 2025, want: 580_000},
		{name: "R8_2026",  taxYear: 2026, want: 580_000},
		{name: "R9_2027",  taxYear: 2027, want: 580_000},
		{name: "R10_2028", taxYear: 2028, want: 580_000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DependentIncomeLimit(tc.taxYear)
			assert.Equal(t, tc.want, got, "taxYear=%d", tc.taxYear)
		})
	}
}

// ---------------------------------------------------------------------------
// StubPayrollPusher
// ---------------------------------------------------------------------------

func TestStubPayrollPusher_DefaultProvider(t *testing.T) {
	p := NewStubPayrollPusher("")
	assert.Equal(t, ProviderMock, p.Provider())
}

func TestStubPayrollPusher_Push(t *testing.T) {
	p := NewStubPayrollPusher(ProviderFreee)
	assert.Equal(t, ProviderFreee, p.Provider())

	result, err := p.Push(nil, PushRequest{TaxYear: 2026}) //nolint:staticcheck // nil context is safe in stub
	require.NoError(t, err)
	assert.NotEmpty(t, result.ProviderRef)
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

func TestModelTableNames(t *testing.T) {
	assert.Equal(t, "yearend_settings", Settings{}.TableName())
	assert.Equal(t, "yearend_submissions", Submission{}.TableName())
	assert.Equal(t, "yearend_calculations", Calculation{}.TableName())
	assert.Equal(t, "yearend_reports", Report{}.TableName())
	assert.Equal(t, "yearend_payroll_pushes", PayrollPush{}.TableName())
}

// ---------------------------------------------------------------------------
// PDF rendering (no DB required)
// ---------------------------------------------------------------------------

// TestRenderWithholdingSlipPDF verifies that a non-empty valid PDF is produced.
func TestRenderWithholdingSlipPDF(t *testing.T) {
	r := CalculateTax(TaxInput{
		GrossIncome:              5_000_000,
		EmploymentDeduction:      1_440_000,
		BasicDeduction:           480_000,
		SocialInsuranceDeduction: 714_000,
		WithheldTax:              150_000,
	})
	// Synthetic employee ID — no real PII.
	empID, err := uuid.Parse("00000000-0000-0000-0000-000000000001")
	require.NoError(t, err)

	pdfBytes, err := renderWithholdingSlipPDF(empID, 2026, r)
	require.NoError(t, err)
	assert.Greater(t, len(pdfBytes), 0, "PDF output must be non-empty")
	// PDF files start with the %PDF magic bytes.
	assert.True(t, len(pdfBytes) >= 4 && string(pdfBytes[:4]) == "%PDF",
		"output must start with %%PDF magic bytes")
}

// TestRenderSummaryReturnPDF verifies that a non-empty valid PDF is produced.
func TestRenderSummaryReturnPDF(t *testing.T) {
	sr := SummaryReturn{
		TenantID:      "00000000-0000-0000-0000-000000000002",
		TaxYear:       2026,
		EmployeeCount: 3,
		TotalGross:    15_000_000,
		TotalTax:      900_000,
		TotalWithheld: 750_000,
		TotalDiff:     150_000,
	}

	pdfBytes, err := renderSummaryReturnPDF(sr)
	require.NoError(t, err)
	assert.Greater(t, len(pdfBytes), 0, "PDF output must be non-empty")
	assert.True(t, len(pdfBytes) >= 4 && string(pdfBytes[:4]) == "%PDF",
		"output must start with %%PDF magic bytes")
}

// TestRenderSummaryReturnCSV verifies CSV structure for a synthetic tenant.
func TestRenderSummaryReturnCSV(t *testing.T) {
	sr := SummaryReturn{
		TenantID:      "00000000-0000-0000-0000-000000000003",
		TaxYear:       2026,
		EmployeeCount: 2,
		TotalGross:    10_000_000,
		TotalTax:      600_000,
		TotalWithheld: 500_000,
		TotalDiff:     100_000,
	}

	csvBytes, err := renderSummaryReturnCSV(sr)
	require.NoError(t, err)
	require.True(t, json.Valid([]byte(`"test"`))) // import sanity only

	csvStr := string(csvBytes)
	assert.Contains(t, csvStr, "tenant_id")
	assert.Contains(t, csvStr, "employee_count")
	assert.Contains(t, csvStr, "2")  // EmployeeCount
	assert.Contains(t, csvStr, "2026")
	assert.Contains(t, csvStr, "10000000")
}
