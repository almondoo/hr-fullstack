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
//
// CJK font support:
//   When ipaexg.ttf is present in internal/yearend/fonts/ the PDF renders
//   Japanese labels using AddUTF8FontFromBytes / SetFont(cjkFontFamily).
//   When absent the renderer falls back to Helvetica with romanised labels.
//   See fonts/README.md for font placement instructions.
// ---------------------------------------------------------------------------

// newPDF constructs a new *fpdf.Fpdf and, when the CJK font is available,
// registers it so Japanese text can be rendered in subsequent SetFont calls.
// Returns the Fpdf instance and the font family to use throughout the document.
func newPDF() (pdf *fpdf.Fpdf, fontFamily string) {
	pdf = fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(15, 15, 15)

	if hasCJKFont() {
		pdf.AddUTF8FontFromBytes(cjkFontFamily, "", cjkFontBytes)
		pdf.AddUTF8FontFromBytes(cjkFontFamily, "B", cjkFontBytes)
		fontFamily = cjkFontFamily
	} else {
		fontFamily = "Helvetica"
	}
	return pdf, fontFamily
}

// ---------------------------------------------------------------------------
// 源泉徴収票 label sets
// ---------------------------------------------------------------------------

type withholdingRow struct {
	labelJA string // Japanese label (used when CJK font is available)
	labelEN string // Romanised fallback
	amount  int64
}

func withholdingRows(r TaxResult) []withholdingRow {
	return []withholdingRow{
		{"給与収入金額", "Gross Income / Nenkatsu", r.GrossIncome},
		{"給与所得控除", "Employment Deduction / Kyuyo-shotoku Kojo", r.EmploymentDeduction},
		{"所得控除合計", "Total Deductions / Shotoku-kojo Gokei", r.TotalDeductions},
		{"課税所得金額", "Taxable Income / Kazei Shotoku", r.TaxableIncome},
		{"年税額", "Annual Tax / Nenzei-gaku", r.AnnualTax},
		{"源泉徴収済み税額", "Withheld Tax / Gensen Choshu-zumi", r.WithheldTax},
		{"過不足税額", "Difference / Kazei Fusoku", r.Difference},
	}
}

// ---------------------------------------------------------------------------
// 法定調書合計表 label sets
// ---------------------------------------------------------------------------

type summaryRow struct {
	labelJA string
	labelEN string
	value   string
}

func summaryRows(sr SummaryReturn) []summaryRow {
	return []summaryRow{
		{"従業員数", "Employee Count / Taishoku Jinin", fmt.Sprintf("%d", sr.EmployeeCount)},
		{"給与支払金額合計", "Total Gross Income / Kyuyo Sougaku", fmt.Sprintf("%d", sr.TotalGross)},
		{"年税額合計", "Total Annual Tax / Nenzei-gaku Gokei", fmt.Sprintf("%d", sr.TotalTax)},
		{"源泉徴収税額合計", "Total Withheld Tax / Gensen Choshu Gokei", fmt.Sprintf("%d", sr.TotalWithheld)},
		{"過不足税額合計", "Total Difference / Kazei Fusoku Gokei", fmt.Sprintf("%d", sr.TotalDiff)},
	}
}

// ---------------------------------------------------------------------------
// label helper: picks Japanese or romanised text based on font availability
// ---------------------------------------------------------------------------

func label(cjk bool, ja, en string) string {
	if cjk {
		return ja
	}
	return en
}

// ---------------------------------------------------------------------------
// renderWithholdingSlipPDF
// ---------------------------------------------------------------------------

// renderWithholdingSlipPDF generates a 源泉徴収票 as a PDF byte slice.
// Content contains computed amounts only — no decrypted PII.
func renderWithholdingSlipPDF(employeeID uuid.UUID, taxYear int, r TaxResult) ([]byte, error) {
	pdf, fontFamily := newPDF()
	useCJK := hasCJKFont()
	pdf.AddPage()

	// Title
	pdf.SetFont(fontFamily, "B", 16)
	pdf.Cell(0, 10, label(useCJK,
		fmt.Sprintf("源泉徴収票  %d年分", taxYear),
		fmt.Sprintf("Withholding Tax Slip / Gensen Choshu-hyo  %d", taxYear),
	))
	pdf.Ln(12)

	// Sub-header
	pdf.SetFont(fontFamily, "", 10)
	pdf.Cell(0, 6, fmt.Sprintf("Employee ID: %s", employeeID.String()))
	pdf.Ln(10)

	// Table header
	pdf.SetFont(fontFamily, "B", 10)
	col1W := 90.0
	col2W := 40.0
	pdf.CellFormat(col1W, 8, label(useCJK, "項目", "Item"), "1", 0, "L", false, 0, "")
	pdf.CellFormat(col2W, 8, label(useCJK, "金額（円）", "Amount (JPY)"), "1", 0, "R", false, 0, "")
	pdf.Ln(8)

	// Rows
	pdf.SetFont(fontFamily, "", 10)
	for _, row := range withholdingRows(r) {
		pdf.CellFormat(col1W, 7, label(useCJK, row.labelJA, row.labelEN), "1", 0, "L", false, 0, "")
		pdf.CellFormat(col2W, 7, fmt.Sprintf("%d", row.amount), "1", 0, "R", false, 0, "")
		pdf.Ln(7)
	}

	pdf.Ln(6)
	pdf.SetFont(fontFamily, "", 8)
	pdf.MultiCell(0, 5,
		label(useCJK,
			"【注意】本書類に記載された税額は年末調整計算結果に基づく試算値です。"+
				"国税庁の最新通達に照らして社労士・税理士による確認が必要です。法的助言ではありません。",
			"LEGAL NOTE: The tax amounts in this document are computed from the "+
				"year-end adjustment calculation result and must be confirmed against "+
				"the current National Tax Agency (Kokuzeicho) guidance before use. "+
				"This document is NOT legal advice.",
		),
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

// ---------------------------------------------------------------------------
// renderSummaryReturnPDF
// ---------------------------------------------------------------------------

// renderSummaryReturnPDF generates a 法定調書合計表 as a PDF byte slice.
// Content contains aggregate amounts only — no per-employee PII.
func renderSummaryReturnPDF(sr SummaryReturn) ([]byte, error) {
	pdf, fontFamily := newPDF()
	useCJK := hasCJKFont()
	pdf.AddPage()

	// Title
	pdf.SetFont(fontFamily, "B", 16)
	pdf.Cell(0, 10, label(useCJK,
		fmt.Sprintf("法定調書合計表  %d年分", sr.TaxYear),
		fmt.Sprintf("Summary Return / Horitsu Chosho Gokeihyo  %d", sr.TaxYear),
	))
	pdf.Ln(12)

	// Tenant / tax-year info
	pdf.SetFont(fontFamily, "", 10)
	pdf.Cell(0, 6, fmt.Sprintf("Tenant ID: %s", sr.TenantID))
	pdf.Ln(8)

	// Table header
	pdf.SetFont(fontFamily, "B", 10)
	col1W := 110.0
	col2W := 60.0
	pdf.CellFormat(col1W, 8, label(useCJK, "項目", "Item"), "1", 0, "L", false, 0, "")
	pdf.CellFormat(col2W, 8, label(useCJK, "値", "Value"), "1", 0, "R", false, 0, "")
	pdf.Ln(8)

	// Rows
	pdf.SetFont(fontFamily, "", 10)
	for _, row := range summaryRows(sr) {
		pdf.CellFormat(col1W, 7, label(useCJK, row.labelJA, row.labelEN), "1", 0, "L", false, 0, "")
		pdf.CellFormat(col2W, 7, row.value, "1", 0, "R", false, 0, "")
		pdf.Ln(7)
	}

	pdf.Ln(6)
	pdf.SetFont(fontFamily, "", 8)
	pdf.MultiCell(0, 5,
		label(useCJK,
			"【注意】本書類に記載された集計額は確定済み年末調整計算に基づく試算値です。"+
				"国税庁の最新通達に照らして社労士・税理士による確認の上、提出してください。法的助言ではありません。",
			"LEGAL NOTE: The aggregate amounts in this document are computed from "+
				"finalised year-end adjustment calculations and must be confirmed against "+
				"the current National Tax Agency (Kokuzeicho) guidance before submission. "+
				"This document is NOT legal advice.",
		),
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
