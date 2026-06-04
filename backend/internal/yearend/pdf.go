package yearend

import (
	"bytes"
	"fmt"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// PDF rendering helpers (帳票 PDF 生成)
//
// Security: generated PDFs contain amounts from result_json only.
// No decrypted PII from submission declarations is written to the PDF.
// ---------------------------------------------------------------------------

// renderWithholdingSlipPDF generates a 源泉徴収票 as a PDF byte slice.
// Content contains computed amounts only — no decrypted PII.
func renderWithholdingSlipPDF(employeeID uuid.UUID, taxYear int, r TaxResult) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.AddPage()

	// Title
	pdf.SetFont("Helvetica", "B", 16)
	pdf.Cell(0, 10, fmt.Sprintf("Withholding Tax Slip / Gensen Choshu-hyo  %d", taxYear))
	pdf.Ln(12)

	// Sub-header
	pdf.SetFont("Helvetica", "", 10)
	pdf.Cell(0, 6, fmt.Sprintf("Employee ID: %s", employeeID.String()))
	pdf.Ln(10)

	// Table header
	pdf.SetFont("Helvetica", "B", 10)
	col1W := 90.0
	col2W := 40.0
	pdf.CellFormat(col1W, 8, "Item", "1", 0, "L", false, 0, "")
	pdf.CellFormat(col2W, 8, "Amount (JPY)", "1", 0, "R", false, 0, "")
	pdf.Ln(8)

	// Rows
	pdf.SetFont("Helvetica", "", 10)
	rows := []struct {
		label  string
		amount int64
	}{
		{"Gross Income / Nenkatsu", r.GrossIncome},
		{"Employment Deduction / Kyuyo-shotoku Kojo", r.EmploymentDeduction},
		{"Total Deductions / Shotoku-kojo Gokei", r.TotalDeductions},
		{"Taxable Income / Kazei Shotoku", r.TaxableIncome},
		{"Annual Tax / Nenzei-gaku", r.AnnualTax},
		{"Withheld Tax / Gensen Choshu-zumi", r.WithheldTax},
		{"Difference / Kazei Fusoku", r.Difference},
	}
	for _, row := range rows {
		pdf.CellFormat(col1W, 7, row.label, "1", 0, "L", false, 0, "")
		pdf.CellFormat(col2W, 7, fmt.Sprintf("%d", row.amount), "1", 0, "R", false, 0, "")
		pdf.Ln(7)
	}

	pdf.Ln(6)
	pdf.SetFont("Helvetica", "I", 8)
	pdf.MultiCell(0, 5,
		"LEGAL NOTE: The tax amounts in this document are computed from the "+
			"year-end adjustment calculation result and must be confirmed against "+
			"the current National Tax Agency (Kokuzeicho) guidance before use. "+
			"This document is NOT legal advice.",
		"", "L", false)

	if pdf.Err() {
		return nil, fmt.Errorf("yearend: render withholding slip PDF: %w", pdf.Error())
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("yearend: output withholding slip PDF: %w", err)
	}
	return buf.Bytes(), nil
}

// renderSummaryReturnPDF generates a 法定調書合計表 as a PDF byte slice.
// Content contains aggregate amounts only — no per-employee PII.
func renderSummaryReturnPDF(sr SummaryReturn) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)
	pdf.AddPage()

	// Title
	pdf.SetFont("Helvetica", "B", 16)
	pdf.Cell(0, 10, fmt.Sprintf("Summary Return / Horitsu Chosho Gokeihyo  %d", sr.TaxYear))
	pdf.Ln(12)

	// Tenant / tax-year info
	pdf.SetFont("Helvetica", "", 10)
	pdf.Cell(0, 6, fmt.Sprintf("Tenant ID: %s", sr.TenantID))
	pdf.Ln(8)

	// Table header
	pdf.SetFont("Helvetica", "B", 10)
	col1W := 110.0
	col2W := 60.0
	pdf.CellFormat(col1W, 8, "Item", "1", 0, "L", false, 0, "")
	pdf.CellFormat(col2W, 8, "Value", "1", 0, "R", false, 0, "")
	pdf.Ln(8)

	// Rows
	pdf.SetFont("Helvetica", "", 10)
	rows := []struct {
		label string
		value string
	}{
		{"Employee Count / Taishoku Jinin", fmt.Sprintf("%d", sr.EmployeeCount)},
		{"Total Gross Income / Kyuyo Sougaku", fmt.Sprintf("%d", sr.TotalGross)},
		{"Total Annual Tax / Nenzei-gaku Gokei", fmt.Sprintf("%d", sr.TotalTax)},
		{"Total Withheld Tax / Gensen Choshu Gokei", fmt.Sprintf("%d", sr.TotalWithheld)},
		{"Total Difference / Kazei Fusoku Gokei", fmt.Sprintf("%d", sr.TotalDiff)},
	}
	for _, row := range rows {
		pdf.CellFormat(col1W, 7, row.label, "1", 0, "L", false, 0, "")
		pdf.CellFormat(col2W, 7, row.value, "1", 0, "R", false, 0, "")
		pdf.Ln(7)
	}

	pdf.Ln(6)
	pdf.SetFont("Helvetica", "I", 8)
	pdf.MultiCell(0, 5,
		"LEGAL NOTE: The aggregate amounts in this document are computed from "+
			"finalised year-end adjustment calculations and must be confirmed against "+
			"the current National Tax Agency (Kokuzeicho) guidance before submission. "+
			"This document is NOT legal advice.",
		"", "L", false)

	if pdf.Err() {
		return nil, fmt.Errorf("yearend: render summary return PDF: %w", pdf.Error())
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("yearend: output summary return PDF: %w", err)
	}
	return buf.Bytes(), nil
}
