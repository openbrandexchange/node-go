package importfmt

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const xlsxFixtures = "../../testdata/spec/imports/xlsx"

type xlsxIssue struct {
	File         string  `json:"file"`
	Row          *int    `json:"row"`
	Column       *string `json:"column"`
	Severity     string  `json:"severity"`
	ObxError     string  `json:"obx_error"`
	DetailPrefix string  `json:"detail_prefix"`
	Value        *string `json:"value"`
}

type xlsxExpected struct {
	Limits map[string]int64 `json:"limits"`
	Cases  []struct {
		File     string      `json:"file"`
		Role     string      `json:"role"`
		Sheet    string      `json:"sheet"`
		CSVFile  string      `json:"csv_file"`
		Rows     [][]string  `json:"rows"`
		CSV      *string     `json:"csv"`
		Errors   []xlsxIssue `json:"errors"`
		Warnings []xlsxIssue `json:"warnings"`
		Error    *struct {
			File           string `json:"file"`
			ObxError       string `json:"obx_error"`
			DetailContains string `json:"detail_contains"`
		} `json:"error"`
	} `json:"cases"`
	NumberVectors []struct {
		V      string  `json:"v"`
		Format string  `json:"format"`
		Text   *string `json:"text"` // null: workbook_invalid
	} `json:"number_vectors"`
	DateVectors []struct {
		V        string  `json:"v"`
		Kind     string  `json:"kind"`
		Date1904 bool    `json:"date1904"`
		Text     *string `json:"text"`
	} `json:"date_vectors"`
	FormatVectors []struct {
		ID   int     `json:"id"`
		Code string  `json:"code"`
		Kind *string `json:"kind"`
	} `json:"format_vectors"`
}

func loadXLSXExpected(t *testing.T) *xlsxExpected {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(xlsxFixtures, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e xlsxExpected
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

func convertFile(t *testing.T, name, role string) (*Workbook, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(xlsxFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return ConvertWorkbook(bytes.NewReader(b), int64(len(b)), role)
}

func issueMatches(got CellIssue, want xlsxIssue) bool {
	if got.File != want.File || got.Severity != want.Severity || got.ObxError != want.ObxError || !strings.HasPrefix(got.Detail, want.DetailPrefix) {
		return false
	}
	if (want.Row == nil) != (got.Row == 0) || (want.Row != nil && *want.Row != got.Row) {
		return false
	}
	if (want.Column == nil) != (got.Column == "") || (want.Column != nil && *want.Column != got.Column) {
		return false
	}
	if (want.Value == nil) != (got.Value == nil) || (want.Value != nil && *want.Value != *got.Value) {
		return false
	}
	return true
}

func checkIssues(t *testing.T, what string, got []CellIssue, want []xlsxIssue) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d, want %d: %+v", what, len(got), len(want), got)
		return
	}
	for i := range want {
		if !issueMatches(got[i], want[i]) {
			t.Errorf("%s[%d]: got %+v, want %+v", what, i, got[i], want[i])
		}
	}
}

// TestWorkbookFixtures runs every case of the spec's
// fixtures/imports/xlsx/expected.json (import-format.md §5).
func TestWorkbookFixtures(t *testing.T) {
	e := loadXLSXExpected(t)
	want := map[string]int64{"entries": WorkbookMaxEntries, "totalBytes": WorkbookMaxTotalBytes, "ratio": WorkbookRatio,
		"ratioFloor": WorkbookRatioFloor, "maxRow": WorkbookMaxRow, "maxCol": WorkbookMaxCol}
	if !reflect.DeepEqual(e.Limits, want) {
		t.Errorf("limits: spec %v, node %v", e.Limits, want)
	}
	if len(e.Cases) < 15 {
		t.Fatalf("only %d cases", len(e.Cases))
	}
	for _, c := range e.Cases {
		t.Run(c.File, func(t *testing.T) {
			wb, err := convertFile(t, c.File, c.Role)
			if c.Error != nil {
				var we *WorkbookError
				if !errors.As(err, &we) {
					t.Fatalf("want %s, got %v (%v)", c.Error.ObxError, err, wb)
				}
				if we.Code != c.Error.ObxError || c.Error.File != WorkbookFile(c.Role) {
					t.Fatalf("want %s, got %s: %s", c.Error.ObxError, we.Code, we.Detail)
				}
				if !strings.Contains(strings.ToLower(we.Detail), strings.ToLower(c.Error.DetailContains)) {
					t.Errorf("detail %q does not mention %q", we.Detail, c.Error.DetailContains)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if wb.Sheet != c.Sheet {
				t.Errorf("sheet %q, want %q", wb.Sheet, c.Sheet)
			}
			csv := CanonicalCSV(wb.Rows)
			switch {
			case c.CSVFile != "":
				b, err := os.ReadFile(filepath.Join("../../testdata/spec", strings.TrimPrefix(c.CSVFile, "fixtures/")))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(csv, b) {
					t.Errorf("csv differs from %s:\n%q\nwant\n%q", c.CSVFile, csv, b)
				}
			default:
				if !reflect.DeepEqual(wb.Rows, c.Rows) {
					t.Errorf("rows:\n%q\nwant\n%q", wb.Rows, c.Rows)
				}
				if c.CSV == nil || string(csv) != *c.CSV {
					t.Errorf("csv:\n%q\nwant\n%v", csv, c.CSV)
				}
			}
			checkIssues(t, "errors", wb.Errors, c.Errors)
			checkIssues(t, "warnings", wb.Warnings, c.Warnings)
		})
	}
}

func TestWorkbookVectors(t *testing.T) {
	e := loadXLSXExpected(t)
	for _, v := range e.NumberVectors {
		got, ok := NumberText(v.V)
		if v.Text == nil {
			if ok {
				t.Errorf("number %s: %s, want workbook_invalid", v.V, got)
			}
			continue
		}
		if !ok {
			t.Errorf("number %s: not a number", v.V)
			continue
		}
		if code := v.Format; code != "General" {
			if w := ZeroPad(code); w > 0 && isDigits(got) && len(got) < w {
				got = strings.Repeat("0", w-len(got)) + got
			}
		}
		if got != *v.Text {
			t.Errorf("number %s (%s): %s, want %s", v.V, v.Format, got, *v.Text)
		}
	}
	for _, v := range e.DateVectors {
		got, ok := SerialText(v.V, v.Kind, v.Date1904)
		if v.Text == nil {
			if ok {
				t.Errorf("date %s: %s, want a conversion error", v.V, got)
			}
			continue
		}
		if !ok || got != *v.Text {
			t.Errorf("date %s %s 1904=%v: %q %v, want %s", v.V, v.Kind, v.Date1904, got, ok, *v.Text)
		}
	}
	for _, v := range e.FormatVectors {
		got := DateKind(v.ID, v.Code, true)
		want := ""
		if v.Kind != nil {
			want = *v.Kind
		}
		if got != want {
			t.Errorf("format %d %q: %q, want %q", v.ID, v.Code, got, want)
		}
	}
	for _, s := range []string{"abc", "", "1.2.3", "1e", "--1", "1e999"} {
		if got, ok := NumberText(s); ok {
			t.Errorf("NumberText(%q) = %q, want not a number", s, got)
		}
	}
	for in, want := range map[string]string{"_x000D_": "\r", "_x005F_x0031_": "_x0031_", "a_x0041_b": "aAb", "_xD83D__xDE00_": "\U0001F600", "_x00_": "_x00_"} {
		if got := UnescapeXstring(in); got != want {
			t.Errorf("UnescapeXstring(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"2026-03-01": "2026-03-01", "2026-03-01T00:00:00Z": "2026-03-01", "2026-03-01T12:30:15.5Z": "2026-03-01T12:30:15Z", "2026-03-01T12:30": "2026-03-01T12:30:00Z"} {
		if got, ok := IsoCellText(in); !ok || got != want {
			t.Errorf("IsoCellText(%q) = %q, want %q", in, got, want)
		}
	}
	if _, ok := IsoCellText("1 March 2026"); ok {
		t.Error("IsoCellText accepted a text date")
	}
}

// buildWorkbook writes a minimal workbook with the given parts overriding
// the defaults (nil removes a part).
func buildWorkbook(t *testing.T, sheet string, over map[string]*string) []byte {
	t.Helper()
	parts := map[string]string{
		"[Content_Types].xml":        `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`,
		"_rels/.rels":                `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml":            `<?xml version="1.0" encoding="UTF-8"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Products" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   `<?xml version="1.0" encoding="UTF-8"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` + sheet + `</sheetData></worksheet>`,
	}
	for k, v := range over {
		if v == nil {
			delete(parts, k)
		} else {
			parts[k] = *v
		}
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml"} {
		if v, ok := parts[name]; ok {
			w, _ := zw.Create(name)
			w.Write([]byte(v))
			delete(parts, name)
		}
	}
	for name, v := range parts {
		w, _ := zw.Create(name)
		w.Write([]byte(v))
	}
	zw.Close()
	return buf.Bytes()
}

func strp(s string) *string { return &s }

func TestWorkbookRefusals(t *testing.T) {
	inl := func(ref, s string) string {
		return fmt.Sprintf(`<c r="%s" t="inlineStr"><is><t>%s</t></is></c>`, ref, s)
	}
	ok := `<row r="1">` + inl("A1", "style_number") + `</row><row r="2">` + inl("A2", "X-1") + `</row>`
	cases := []struct {
		name string
		data []byte
		code string
	}{
		{"plain", buildWorkbook(t, ok, nil), ""},
		{"no content types", buildWorkbook(t, ok, map[string]*string{"[Content_Types].xml": nil}), ErrWorkbookInvalid},
		{"no main document", buildWorkbook(t, ok, map[string]*string{"_rels/.rels": strp(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"/>`)}), ErrWorkbookInvalid},
		{"strict", buildWorkbook(t, ok, map[string]*string{"xl/workbook.xml": strp(`<workbook xmlns="http://purl.oclc.org/ooxml/spreadsheetml/main"/>`)}), ErrWorkbookInvalid},
		{"not well-formed", buildWorkbook(t, `<row r="1"><c r="A1"><v>1</v></row>`, nil), ErrWorkbookInvalid},
		{"doctype in sheet", buildWorkbook(t, ok, map[string]*string{"xl/worksheets/sheet1.xml": strp(`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY a "aaaa">]><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="inlineStr"><is><t>&a;</t></is></c></row></sheetData></worksheet>`)}), ErrWorkbookInvalid},
		{"unknown entity", buildWorkbook(t, `<row r="1"><c r="A1" t="inlineStr"><is><t>&nbsp;style_number</t></is></c></row>`, nil), ErrWorkbookInvalid},
		{"number not a number", buildWorkbook(t, ok+`<row r="3"><c r="A3"><v>12abc</v></c></row>`, nil), ErrWorkbookInvalid},
		{"missing shared string", buildWorkbook(t, ok+`<row r="3"><c r="A3" t="s"><v>7</v></c></row>`, nil), ErrWorkbookInvalid},
		{"activeX", buildWorkbook(t, ok, map[string]*string{"xl/activeX/activeX1.xml": strp("<x/>")}), ErrWorkbookUnsafe},
		{"connections", buildWorkbook(t, ok, map[string]*string{"xl/connections.xml": strp("<x/>")}), ErrWorkbookUnsafe},
		{"macrosheet", buildWorkbook(t, ok, map[string]*string{"xl/macrosheets/sheet1.xml": strp("<x/>")}), ErrWorkbookUnsafe},
		{"external relationship", buildWorkbook(t, ok, map[string]*string{"xl/_rels/workbook.xml.rels": strp(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/><Relationship Id="rId9" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink" Target="https://example.com/" TargetMode="External"/></Relationships>`)}), ErrWorkbookUnsafe},
		{"column 1001", buildWorkbook(t, ok+`<row r="3">`+inl("ALM3", "x")+`</row>`, nil), ErrWorkbookTooLarge},
		{"column 1000 is fine", buildWorkbook(t, ok+`<row r="3">`+inl("ALL3", "x")+`</row>`, nil), ""},
		{"empty cells far away are fine", buildWorkbook(t, ok+`<row r="200000"><c r="A200000" s="0"/></row>`, nil), ""},
		{"no visible sheet", buildWorkbook(t, ok, map[string]*string{"xl/workbook.xml": strp(`<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Products" sheetId="1" state="veryHidden" r:id="rId1"/></sheets></workbook>`)}), ErrWorkbookSheetMissing},
	}
	// too many entries
	var many bytes.Buffer
	zw := zip.NewWriter(&many)
	for i := 0; i < 1001; i++ {
		w, _ := zw.Create(fmt.Sprintf("x/%d.txt", i))
		w.Write([]byte("x"))
	}
	zw.Close()
	cases = append(cases, struct {
		name string
		data []byte
		code string
	}{"1001 entries", many.Bytes(), ErrWorkbookTooLarge})
	for _, c := range cases {
		_, err := ConvertWorkbook(bytes.NewReader(c.data), int64(len(c.data)), "products")
		var we *WorkbookError
		switch {
		case c.code == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.code != "" && (!errors.As(err, &we) || we.Code != c.code):
			t.Errorf("%s: got %v, want %s", c.name, err, c.code)
		}
	}
}

// An entry whose declared size is smaller than what it inflates to stops
// with workbook_too_large.
func TestWorkbookLyingSize(t *testing.T) {
	big := `<row r="1"><c r="A1" t="inlineStr"><is><t>style_number</t></is></c></row>` + strings.Repeat(`<row><c t="inlineStr"><is><t>x</t></is></c></row>`, 2000)
	data := buildWorkbook(t, big, nil)
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt bytes.Buffer
	zw := zip.NewWriter(&rebuilt)
	for _, f := range z.File {
		raw, _ := f.OpenRaw()
		h := f.FileHeader
		if f.Name == "xl/worksheets/sheet1.xml" {
			h.UncompressedSize64 = 1000
		}
		w, err := zw.CreateRaw(&h)
		if err != nil {
			t.Fatal(err)
		}
		b := new(bytes.Buffer)
		b.ReadFrom(raw)
		w.Write(b.Bytes())
	}
	zw.Close()
	_, err = ConvertWorkbook(bytes.NewReader(rebuilt.Bytes()), int64(rebuilt.Len()), "products")
	var we *WorkbookError
	if !errors.As(err, &we) || we.Code != ErrWorkbookTooLarge {
		t.Fatalf("got %v, want workbook_too_large", err)
	}
}

func TestWorkbookLocator(t *testing.T) {
	header := []string{"Style Number", "name:en", " ean ", "", "weight_kg"}
	cases := []struct {
		row  int
		col  string
		want string
	}{
		{12, "gtin", "row 12, column ean (C): "},
		{12, "style_number", "row 12, column Style Number (A): "},
		{0, "weight_kg", "column weight_kg (E): "},
		{4, "brand", "row 4: "},
		{0, "brand", ""},
		{4, "", "row 4: "},
	}
	for _, c := range cases {
		if got := WorkbookLocator(header, c.row, c.col); got != c.want {
			t.Errorf("(%d, %q): %q, want %q", c.row, c.col, got, c.want)
		}
	}
	if ColumnLetter(1000) != "ALL" || ColumnLetter(26) != "Z" || ColumnLetter(27) != "AA" || colNumber("ALL") != 1000 {
		t.Error("column letters")
	}
}

// §5.4: a CDATA section is text in <t>, <v> and <f>.
func TestWorkbookCDATA(t *testing.T) {
	sheet := `<row r="1"><c r="A1" t="inlineStr"><is><t><![CDATA[style_number]]></t></is></c><c r="B1" t="inlineStr"><is><t>weight_kg</t></is></c><c r="C1" t="inlineStr"><is><t>name:en</t></is></c></row>` +
		`<row r="2"><c r="A2" t="inlineStr"><is><t>X-<![CDATA[1 & <2>]]></t></is></c><c r="B2"><v><![CDATA[1.50]]></v></c><c r="C2"><f><![CDATA[A1&"x"]]></f></c></row>`
	data := buildWorkbook(t, sheet, nil)
	wb, err := ConvertWorkbook(bytes.NewReader(data), int64(len(data)), "products")
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{"style_number", "weight_kg", "name:en"}, {"", "", ""}}; !reflect.DeepEqual(wb.Rows, want) {
		t.Errorf("rows %q, want %q", wb.Rows, want)
	}
	if len(wb.Errors) != 1 || wb.Errors[0].Value == nil || *wb.Errors[0].Value != `=A1&"x"` {
		t.Errorf("errors %+v, want the formula from CDATA", wb.Errors)
	}
	sheet = strings.Replace(sheet, `<c r="C2"><f><![CDATA[A1&"x"]]></f></c>`, "", 1)
	data = buildWorkbook(t, sheet, nil)
	if wb, err = ConvertWorkbook(bytes.NewReader(data), int64(len(data)), "products"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"X-1 & <2>", "1.5", ""}; !reflect.DeepEqual(wb.Rows[1], want) {
		t.Errorf("row 2 %q, want %q", wb.Rows[1], want)
	}
}

// §5.6 step 4: a part read twice (the sheet choice, then the conversion)
// counts once toward the inflated total.
func TestWorkbookPartCountedOnce(t *testing.T) {
	data := buildWorkbook(t, `<row r="1"><c r="A1" t="inlineStr"><is><t>style_number</t></is></c></row>`, nil)
	p, err := openWorkbookPackage(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := p.read("/xl/worksheets/sheet1.xml")
	n := p.inflated
	b, _ := p.read("xl/worksheets/sheet1.xml")
	if p.inflated != n || n != int64(len(a)) || !bytes.Equal(a, b) {
		t.Errorf("inflated %d after two reads of %d bytes", p.inflated, len(a))
	}
}

// §1.1: trimmed (space, tab, CR, LF), case ignored, each run of white space
// and '-' one '_'; the sheet choice uses the same.
func TestHeaderNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"Style Number": "style_number", "Style  -  Number": "style_number", " \tStyle\r\nNumber\n": "style_number",
		"STYLE--NUMBER": "style_number", "Colour Code": "color_code", "Name:NL": "name:nl", "attr:Launch  Date": "attr:launch_date",
		"  ": "", " style_number": " style_number",
	} {
		if got := NormalizeHeader(in); got != want {
			t.Errorf("NormalizeHeader(%q) = %q, want %q", in, got, want)
		}
	}
	if UnknownColumns([]string{NormalizeHeader(" "), "brand"}, map[string]bool{"brand": true}) != nil {
		t.Error("an empty header is an unknown column")
	}
	if CellLocator(1, "", 5) != "row 1, column E: " || CellLocator(3, "ean", 3) != "row 3, column ean (C): " {
		t.Error("cell locator")
	}
}
