package reporting

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// renderXLSX serialises a ReportResult to a Microsoft "SpreadsheetML 2003"
// (XML Spreadsheet) document.  This is an Excel-recognised .xls/.xlsx-compatible
// XML format that requires NO third-party xlsx library — only encoding/xml from
// the standard library.  The document is UTF-8 encoded with an explicit
// declaration so Japanese text renders without 文字化け.
//
// The format intentionally trades the binary OOXML container (which would need a
// zip + multiple XML parts) for a single self-describing XML payload that Excel
// opens directly.  This keeps the export self-contained and dependency-free per
// the slice's isolation constraints.
func renderXLSX(r ReportResult) ([]byte, error) {
	ss := xmlWorkbook{
		XMLNS:   "urn:schemas-microsoft-com:office:spreadsheet",
		XMLNSSS: "urn:schemas-microsoft-com:office:spreadsheet",
	}
	sheet := xmlWorksheet{Name: sheetName(r.ReportKey)}

	// Header row.
	headerCells := make([]xmlCell, 0, len(r.Columns))
	for _, col := range r.Columns {
		headerCells = append(headerCells, xmlCell{Data: xmlData{Type: "String", Value: col}})
	}
	sheet.Table.Rows = append(sheet.Table.Rows, xmlRow{Cells: headerCells})

	// Data rows.
	for _, row := range r.Rows {
		cells := make([]xmlCell, 0, len(row))
		for _, v := range row {
			cells = append(cells, xmlCell{Data: xmlData{Type: "String", Value: v}})
		}
		sheet.Table.Rows = append(sheet.Table.Rows, xmlRow{Cells: cells})
	}

	ss.Worksheets = append(ss.Worksheets, sheet)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(`<?mso-application progid="Excel.Sheet"?>` + "\n")
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(ss); err != nil {
		return nil, fmt.Errorf("reporting: xlsx encode: %w", err)
	}
	return buf.Bytes(), nil
}

// sheetName returns a safe Excel worksheet name (max 31 chars) for a report key.
func sheetName(reportKey string) string {
	if len(reportKey) > 31 {
		return reportKey[:31]
	}
	if reportKey == "" {
		return "Sheet1"
	}
	return reportKey
}

// --- SpreadsheetML 2003 XML element types ---

type xmlWorkbook struct {
	XMLName    xml.Name       `xml:"Workbook"`
	XMLNS      string         `xml:"xmlns,attr"`
	XMLNSSS    string         `xml:"xmlns:ss,attr"`
	Worksheets []xmlWorksheet `xml:"Worksheet"`
}

type xmlWorksheet struct {
	Name  string   `xml:"ss:Name,attr"`
	Table xmlTable `xml:"Table"`
}

type xmlTable struct {
	Rows []xmlRow `xml:"Row"`
}

type xmlRow struct {
	Cells []xmlCell `xml:"Cell"`
}

type xmlCell struct {
	Data xmlData `xml:"Data"`
}

type xmlData struct {
	Type  string `xml:"ss:Type,attr"`
	Value string `xml:",chardata"`
}
