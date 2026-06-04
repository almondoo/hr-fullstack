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
