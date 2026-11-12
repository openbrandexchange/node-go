package importfmt

// Excel workbooks (docs/import-format.md §5, a node feature): an .xlsx file is
// turned into the rows of products.csv or assets.csv, exactly as the spec
// says, so every node gives the same rows, errors and row numbers. Only the
// standard library is used: archive/zip for the package (with the §5.6
// limits checked from the central directory before anything is inflated and
// the inflated bytes counted) and encoding/xml for the parts (DOCTYPEs
// refused, only the five predefined entities). Nothing is calculated, run or
// fetched, and no part is written to disk.

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Workbook limits (§5.6). They are fixed so every node gives the same answer.
const (
	WorkbookMaxEntries    = 1000
	WorkbookMaxTotalBytes = 256 << 20
	WorkbookRatio         = 100
	WorkbookRatioFloor    = 10 << 20
	WorkbookMaxRow        = 100001
	WorkbookMaxCol        = 1000
)

// WorkbookContentType is the media type of an .xlsx file (§5.1).
const WorkbookContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

// Workbook error codes (§5.5).
const (
	ErrWorkbookInvalid      = "workbook_invalid"
	ErrWorkbookUnsafe       = "workbook_unsafe"
	ErrWorkbookSheetMissing = "workbook_sheet_missing"
	ErrWorkbookTooLarge     = "workbook_too_large"
)

// WorkbookError is a workbook that cannot be imported: one of the §5.5 codes.
type WorkbookError struct {
	Code   string
	Detail string
}

func (e *WorkbookError) Error() string { return e.Code + ": " + e.Detail }

func wbErr(code, format string, args ...any) *WorkbookError {
	return &WorkbookError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// IsWorkbookName reports whether a file name makes a file a workbook (§5.1):
// it ends in .xlsx, in any case.
func IsWorkbookName(name string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), ".xlsx")
}

// IsOtherSpreadsheetName reports spreadsheet files that are not workbooks in
// the sense of §5.1 (.xlsm, .xls, .xlsb, .ods, .numbers, .xltx, .xltm).
func IsOtherSpreadsheetName(name string) bool {
	switch strings.ToLower(path.Ext(strings.TrimSpace(name))) {
	case ".xlsm", ".xls", ".xlsb", ".ods", ".numbers", ".xltm", ".xltx", ".xlam":
		return true
	}
	return false
}

// WorkbookFile is the ImportRowError file name of a role: products.xlsx or
// assets.xlsx.
func WorkbookFile(role string) string { return role + ".xlsx" }

// CellIssue is a conversion error or warning, in ImportRowError form.
type CellIssue struct {
	File     string
	Row      int // 0 = null
	Column   string
	Severity string
	ObxError string
	Detail   string
	Value    *string
}

// Workbook is the result of a conversion.
type Workbook struct {
	Sheet    string
	Rows     [][]string // header first; Rows[i] is Excel row i+1
	Errors   []CellIssue
	Warnings []CellIssue
}

const (
	nsMain        = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	nsStrict      = "http://purl.oclc.org/ooxml/spreadsheetml/main"
	nsRel         = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	relOfficeDoc  = nsRel + "/officeDocument"
	relWorksheet  = nsRel + "/worksheet"
	relStyles     = nsRel + "/styles"
	relSharedStrs = nsRel + "/sharedStrings"
)

var mainTypes = map[string]bool{
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml":    true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.template.main+xml": true,
}

type wbRole struct {
	sheet string
	keys  []string
	label string
}

var wbRoles = map[string]wbRole{
	"products": {sheet: "products", keys: []string{"style_number", "style"}, label: "Products"},
	"assets":   {sheet: "assets", keys: []string{"file"}, label: "Assets"},
}

// ---------------------------------------------------------------- helpers

// ColumnLetter is the Excel column letter of a 1-based column number.
func ColumnLetter(n int) string {
	s := ""
	for ; n > 0; n = (n - 1) / 26 {
		s = string(rune('A'+(n-1)%26)) + s
	}
	return s
}

func colNumber(letters string) int {
	n := 0
	for _, c := range letters {
		n = n*26 + int(c-'A') + 1
		if n > 1<<40 {
			return 1 << 40
		}
	}
	return n
}

// wbHeaderName is the §1.1 header normalisation used to choose a sheet
// (exactly the CSV one).
func wbHeaderName(h string) string { return headerName(h) }

var xstringRe = regexp.MustCompile(`_x([0-9A-Fa-f]{4})_`)

// UnescapeXstring replaces the escapes _xHHHH_ by the UTF-16 code unit
// U+HHHH, in one pass from left to right (§5.4).
func UnescapeXstring(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	var units []uint16
	last := 0
	for _, m := range xstringRe.FindAllStringSubmatchIndex(s, -1) {
		units = append(units, utf16.Encode([]rune(s[last:m[0]]))...)
		v, _ := strconv.ParseUint(s[m[2]:m[3]], 16, 16)
		units = append(units, uint16(v))
		last = m[1]
	}
	units = append(units, utf16.Encode([]rune(s[last:]))...)
	return string(utf16.Decode(units))
}

// ---------------------------------------------------------------- numbers (§5.4)

var numberRe = regexp.MustCompile(`^([+-]?)(\d*)(?:\.(\d*))?(?:[eE]([+-]?\d+))?$`)

// NumberText is the text of a number cell's <v>: the exact decimal value,
// rounded to 15 significant digits (halves away from zero), without
// exponent, trailing zeros or a trailing point. ok is false when <v> is not a
// number or, other than 0, outside Excel's range 1E-308 <= |x| < 1E+308 after
// the rounding (§5.4, numbers, steps 1 to 3).
func NumberText(lex string) (string, bool) {
	m := numberRe.FindStringSubmatch(strings.TrimSpace(lex))
	if m == nil || (m[2] == "" && m[3] == "") {
		return "", false
	}
	neg := m[1] == "-"
	all := m[2] + m[3]
	digits := strings.TrimLeft(all, "0")
	z := len(all) - len(digits)
	if strings.TrimRight(digits, "0") == "" {
		return "0", true // zero, whatever its exponent
	}
	exp := 0
	if m[4] != "" {
		// An exponent of more than 9 digits puts every non-zero number out of range.
		if len(strings.TrimLeft(strings.TrimLeft(m[4], "+-"), "0")) > 9 {
			return "", false
		}
		e, err := strconv.Atoi(m[4])
		if err != nil {
			return "", false
		}
		exp = e
	}
	point := len(m[2]) + exp - z
	if len(digits) > 15 {
		kept := []byte(digits[:15])
		if digits[15] >= '5' {
			i := 14
			for ; i >= 0; i-- {
				if kept[i] == '9' {
					kept[i] = '0'
				} else {
					kept[i]++
					break
				}
			}
			if i < 0 {
				kept = append([]byte{'1'}, kept[:14]...)
				point++
			}
		}
		digits = string(kept)
	}
	digits = strings.TrimRight(digits, "0")
	// Excel's range: the leading digit's place 10^(point-1) is from 1E-308 to 1E+307.
	if point-1 >= 308 || point-1 < -308 {
		return "", false
	}
	var intPart, frac string
	switch {
	case point <= 0:
		intPart, frac = "0", strings.Repeat("0", -point)+digits
	case point >= len(digits):
		intPart = digits + strings.Repeat("0", point-len(digits))
	default:
		intPart, frac = digits[:point], digits[point:]
	}
	out := intPart
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out, true
}

var (
	builtinDate     = map[int]bool{14: true, 15: true, 16: true, 17: true, 27: true, 28: true, 29: true, 30: true, 31: true, 34: true, 35: true, 36: true, 50: true, 51: true, 52: true, 53: true, 54: true, 57: true, 58: true}
	builtinDateTime = map[int]bool{18: true, 19: true, 20: true, 21: true, 22: true, 32: true, 33: true, 45: true, 46: true, 47: true, 55: true, 56: true}
	elapsedRe       = regexp.MustCompile(`(?i)^(h+|m+|s+)$`)
)

// FormatSkeleton is the first section of a number format code without its
// literals: quoted text, the character after \ _ or *, and [...] except
// elapsed time ([h], [mm], [ss]).
func FormatSkeleton(code string) string {
	rs := []rune(code)
	var out []rune
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch c {
		case '"':
			j := indexRune(rs, '"', i+1)
			if j < 0 {
				i = len(rs)
			} else {
				i = j
			}
			continue
		case '\\', '_', '*':
			i++
			continue
		case '[':
			j := indexRune(rs, ']', i+1)
			if j < 0 {
				i = len(rs)
				continue
			}
			if inner := string(rs[i+1 : j]); elapsedRe.MatchString(inner) {
				out = append(out, rs[i+1:j]...)
			}
			i = j
			continue
		case ';':
			return string(out)
		}
		out = append(out, c)
	}
	return string(out)
}

func indexRune(rs []rune, r rune, from int) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// Date kinds of a number format.
const (
	KindDate     = "date"
	KindDateTime = "datetime"
)

// DateKind says whether a number format (its id and, for a custom format,
// its code; hasCode false for a built-in id without one) is a date
// ("date"), a date and time ("datetime") or neither ("").
func DateKind(id int, code string, hasCode bool) string {
	if builtinDate[id] {
		return KindDate
	}
	if builtinDateTime[id] {
		return KindDateTime
	}
	if !hasCode {
		return ""
	}
	sk := strings.ToLower(FormatSkeleton(code))
	if !strings.ContainsAny(sk, "dmyhs") {
		return ""
	}
	if strings.ContainsAny(sk, "hs") {
		return KindDateTime
	}
	return KindDate
}

var zeroPadRe = regexp.MustCompile(`^0{2,}$`)

// ZeroPad is the width a format code made only of zeros (two or more) pads
// non-negative integers to; 0 for every other code.
func ZeroPad(code string) int {
	if zeroPadRe.MatchString(code) {
		return len(code)
	}
	return 0
}

// maxSerialDays bounds the day count before the date is computed; dates after
// 9999-12-31 (Excel's last date) are refused from the computed year.
const maxSerialDays = 2958466

// SerialText turns a date serial number into YYYY-MM-DD (kind date) or
// YYYY-MM-DDTHH:MM:SSZ (kind datetime). ok is false for serials the rule
// does not accept (before 1900-03-01, below 0 in the 1904 system, or after
// 9999-12-31, also when the seconds carry over into 10000-01-01).
func SerialText(lex, kind string, date1904 bool) (string, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(lex), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return "", false
	}
	if (date1904 && v < 0) || (!date1904 && v < 61) {
		return "", false
	}
	day := math.Floor(v)
	secs := int(math.Round((v - day) * 86400))
	days := int(day)
	if day > maxSerialDays {
		return "", false
	}
	if secs >= 86400 {
		days++
		secs -= 86400
	}
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		epoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	d := epoch.AddDate(0, 0, days)
	if d.Year() > 9999 {
		return "", false
	}
	ymd := fmt.Sprintf("%04d-%02d-%02d", d.Year(), int(d.Month()), d.Day())
	if kind == KindDate {
		return ymd, true
	}
	return fmt.Sprintf("%sT%02d:%02d:%02dZ", ymd, secs/3600, secs/60%60, secs%60), true
}

var isoCellRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})(?:T(\d{2}):(\d{2})(?::(\d{2}))?(?:\.\d+)?(?:Z|[+-]00:00)?)?$`)

// IsoCellText converts a t="d" cell (ISO 8601).
func IsoCellText(lex string) (string, bool) {
	m := isoCellRe.FindStringSubmatch(strings.TrimSpace(lex))
	if m == nil {
		return "", false
	}
	sec := m[4]
	if sec == "" {
		sec = "00"
	}
	if m[2] == "" || (m[2] == "00" && m[3] == "00" && sec == "00") {
		return m[1], true
	}
	return fmt.Sprintf("%sT%s:%s:%sZ", m[1], m[2], m[3], sec), true
}

// ---------------------------------------------------------------- XML

var errStop = errors.New("stop")

// walkXML streams one part. DOCTYPEs are refused, only the five predefined
// entities exist, and anything not well-formed is workbook_invalid.
func walkXML(data []byte, part string, fn func(tok xml.Token) error) error {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = true
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return wbErr(ErrWorkbookInvalid, "%s is not well-formed XML (%v)", part, err)
		}
		if dir, ok := tok.(xml.Directive); ok {
			if bytes.HasPrefix(bytes.ToUpper(bytes.TrimSpace(dir)), []byte("DOCTYPE")) {
				return wbErr(ErrWorkbookInvalid, "%s has a DOCTYPE", part)
			}
			return wbErr(ErrWorkbookInvalid, "%s is not well-formed XML (a <!%s> declaration)", part, firstWord(dir))
		}
		if err := fn(tok); err != nil {
			return err
		}
	}
}

func firstWord(b []byte) string {
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func attr(e xml.StartElement, local string) (string, bool) {
	for _, a := range e.Attr {
		if a.Name.Local == local && a.Name.Space == "" {
			return a.Value, true
		}
	}
	return "", false
}

func attrNS(e xml.StartElement, ns, local string) (string, bool) {
	for _, a := range e.Attr {
		if a.Name.Local == local && a.Name.Space == ns {
			return a.Value, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- the package

type wbPackage struct {
	entries  []*zip.File
	byName   map[string]*zip.File
	inflated int64
	done     map[*zip.File][]byte // a part read twice counts once (§5.6 step 4)
}

func openWorkbookPackage(r io.ReaderAt, size int64) (*wbPackage, error) {
	// §5.6 step 2: signature.
	head := make([]byte, 8)
	n, _ := r.ReadAt(head, 0)
	head = head[:n]
	if bytes.Equal(head, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
		return nil, wbErr(ErrWorkbookInvalid, "the file is an encrypted workbook or a legacy .xls file (OLE compound file)")
	}
	if !bytes.HasPrefix(head, []byte("PK\x03\x04")) {
		return nil, wbErr(ErrWorkbookInvalid, "the file is not an .xlsx workbook (not a zip file)")
	}
	// Step 3: a readable zip without encrypted entries.
	z, err := zip.NewReader(r, size)
	if err != nil && !(errors.Is(err, zip.ErrInsecurePath) && z != nil) {
		return nil, wbErr(ErrWorkbookInvalid, "the zip cannot be read (%v)", err)
	}
	p := &wbPackage{byName: map[string]*zip.File{}, done: map[*zip.File][]byte{}}
	for _, f := range z.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if f.Flags&0x1 != 0 {
			return nil, wbErr(ErrWorkbookInvalid, "the zip entry %s is encrypted", f.Name)
		}
		p.entries = append(p.entries, f)
	}
	// Step 4: zip bomb, from the central directory.
	if len(p.entries) > WorkbookMaxEntries {
		return nil, wbErr(ErrWorkbookTooLarge, "%d zip entries (limit %d)", len(p.entries), WorkbookMaxEntries)
	}
	var total uint64
	for _, f := range p.entries {
		u, c := f.UncompressedSize64, f.CompressedSize64
		total += u
		if u > WorkbookRatioFloor && u > WorkbookRatio*max(c, 1) {
			return nil, wbErr(ErrWorkbookTooLarge, "%s expands from %d to %d bytes (more than %d times)", f.Name, c, u, WorkbookRatio)
		}
	}
	if total > WorkbookMaxTotalBytes {
		return nil, wbErr(ErrWorkbookTooLarge, "the parts expand to %d bytes (limit %d)", total, WorkbookMaxTotalBytes)
	}
	for _, f := range p.entries {
		p.byName[strings.ToLower(f.Name)] = f
	}
	return p, nil
}

// read inflates one part, counting the bytes; nil when it does not exist.
func (p *wbPackage) read(name string) ([]byte, error) {
	f := p.byName[strings.ToLower(strings.TrimPrefix(name, "/"))]
	if f == nil {
		return nil, nil
	}
	if data, ok := p.done[f]; ok {
		return data, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, wbErr(ErrWorkbookInvalid, "%s cannot be read (%v)", f.Name, err)
	}
	defer rc.Close()
	declared := int64(f.UncompressedSize64)
	room := WorkbookMaxTotalBytes - p.inflated
	limit := min(declared, room) + 1
	data, err := io.ReadAll(io.LimitReader(rc, limit))
	p.inflated += int64(len(data))
	// archive/zip answers ErrFormat when an entry inflates beyond its declared size.
	if int64(len(data)) > declared || p.inflated > WorkbookMaxTotalBytes || errors.Is(err, zip.ErrFormat) {
		return nil, wbErr(ErrWorkbookTooLarge, "%s expands beyond its declared size", f.Name)
	}
	if err != nil {
		return nil, wbErr(ErrWorkbookInvalid, "%s cannot be read (%v)", f.Name, err)
	}
	if data == nil {
		data = []byte{}
	}
	p.done[f] = data
	return data, nil
}

func (p *wbPackage) checkUnsafeNames() error {
	for _, f := range p.entries {
		n := strings.ToLower(f.Name)
		if path.Base(n) == "vbaproject.bin" {
			return wbErr(ErrWorkbookUnsafe, "the workbook contains macros (%s)", f.Name)
		}
		for _, pre := range []string{"xl/macrosheets/", "xl/activex/", "xl/externallinks/"} {
			if strings.HasPrefix(n, pre) {
				return wbErr(ErrWorkbookUnsafe, "the workbook contains %s (%s)", pre[3:len(pre)-1], f.Name)
			}
		}
		if n == "xl/connections.xml" {
			return wbErr(ErrWorkbookUnsafe, "the workbook contains data connections (xl/connections.xml)")
		}
	}
	return nil
}

type contentTypes struct {
	defaults, overrides map[string]string
	all                 []string
}

func (ct *contentTypes) typeOf(part string) string {
	if t, ok := ct.overrides[strings.ToLower(part)]; ok {
		return t
	}
	ext := part
	if i := strings.LastIndexByte(part, '.'); i >= 0 {
		ext = part[i+1:]
	}
	return ct.defaults[strings.ToLower(ext)]
}

func parseContentTypes(data []byte) (*contentTypes, error) {
	ct := &contentTypes{defaults: map[string]string{}, overrides: map[string]string{}}
	err := walkXML(data, "[Content_Types].xml", func(tok xml.Token) error {
		if e, ok := tok.(xml.StartElement); ok {
			switch e.Name.Local {
			case "Default":
				ext, _ := attr(e, "Extension")
				t, _ := attr(e, "ContentType")
				ct.defaults[strings.ToLower(ext)] = t
				ct.all = append(ct.all, t)
			case "Override":
				pn, _ := attr(e, "PartName")
				t, _ := attr(e, "ContentType")
				ct.overrides[strings.ToLower(pn)] = t
				ct.all = append(ct.all, t)
			}
		}
		return nil
	})
	return ct, err
}

type relationship struct {
	id, typ, target string
	external        bool
}

func parseRels(data []byte, part string) ([]relationship, error) {
	var rels []relationship
	err := walkXML(data, part, func(tok xml.Token) error {
		if e, ok := tok.(xml.StartElement); ok && e.Name.Local == "Relationship" {
			id, _ := attr(e, "Id")
			typ, _ := attr(e, "Type")
			target, _ := attr(e, "Target")
			mode, _ := attr(e, "TargetMode")
			rels = append(rels, relationship{id: id, typ: typ, target: target, external: mode == "External"})
		}
		return nil
	})
	return rels, err
}

// resolvePart resolves a relationship target against its source part.
func resolvePart(source, target string) string {
	var base []string
	if !strings.HasPrefix(target, "/") {
		segs := strings.Split(source, "/")
		for _, s := range segs[:len(segs)-1] {
			if s != "" {
				base = append(base, s)
			}
		}
	}
	for _, seg := range strings.Split(strings.TrimPrefix(target, "/"), "/") {
		switch seg {
		case "..":
			if len(base) > 0 {
				base = base[:len(base)-1]
			}
		case ".", "":
		default:
			base = append(base, seg)
		}
	}
	return "/" + strings.Join(base, "/")
}

func relsOf(part string) string {
	dir, file := path.Split(part)
	return dir + "_rels/" + file + ".rels"
}

type wbSheet struct {
	name, state, rid, part string
}

func parseWorkbookPart(data []byte, part string) ([]wbSheet, bool, error) {
	var sheets []wbSheet
	date1904, root := false, false
	err := walkXML(data, part, func(tok xml.Token) error {
		e, ok := tok.(xml.StartElement)
		if !ok {
			return nil
		}
		if !root {
			root = true
			if e.Name.Local != "workbook" || e.Name.Space != nsMain {
				if e.Name.Space == nsStrict {
					return wbErr(ErrWorkbookInvalid, "the workbook is saved as Strict Open XML; save it as an Excel Workbook (.xlsx)")
				}
				return wbErr(ErrWorkbookInvalid, "%s is not a SpreadsheetML workbook", part)
			}
		}
		switch e.Name.Local {
		case "workbookPr":
			v, _ := attr(e, "date1904")
			date1904 = v == "1" || v == "true"
		case "sheet":
			name, _ := attr(e, "name")
			state, ok := attr(e, "state")
			if !ok {
				state = "visible"
			}
			rid, _ := attrNS(e, nsRel, "id")
			sheets = append(sheets, wbSheet{name: name, state: state, rid: rid})
		}
		return nil
	})
	return sheets, date1904, err
}

type numFmt struct {
	id      int
	code    string
	hasCode bool
}

type styles struct {
	codes map[int]string
	xfs   []int
}

func (s *styles) format(idx int) numFmt {
	id := 0
	if idx >= 0 && idx < len(s.xfs) {
		id = s.xfs[idx]
	}
	code, ok := s.codes[id]
	return numFmt{id: id, code: code, hasCode: ok}
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

func parseStyles(data []byte) (*styles, error) {
	st := &styles{codes: map[int]string{}}
	if data == nil {
		return st, nil
	}
	inXfs := false
	err := walkXML(data, "styles", func(tok xml.Token) error {
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "numFmt":
				id, _ := attr(e, "numFmtId")
				code, _ := attr(e, "formatCode")
				st.codes[atoiOr(id, -1)] = code
			case "cellXfs":
				inXfs = true
			case "xf":
				if inXfs {
					id, ok := attr(e, "numFmtId")
					if !ok {
						id = "0"
					}
					st.xfs = append(st.xfs, atoiOr(id, -1))
				}
			}
		case xml.EndElement:
			if e.Name.Local == "cellXfs" {
				inXfs = false
			}
		}
		return nil
	})
	return st, err
}

// richText collects the text of <si> / <is>: every <t> outside phonetic
// runs <rPh>.
type richText struct {
	inPh int
	inT  bool
	buf  strings.Builder
}

func (r *richText) open(e xml.StartElement) {
	if e.Name.Local == "rPh" {
		r.inPh++
	}
	if e.Name.Local == "t" && r.inPh == 0 {
		r.inT = true
	}
}

func (r *richText) close(e xml.EndElement) {
	if e.Name.Local == "rPh" {
		r.inPh--
	}
	if e.Name.Local == "t" {
		r.inT = false
	}
}

func (r *richText) text(b []byte) {
	if r.inT {
		r.buf.Write(b)
	}
}

func (r *richText) take() string {
	s := r.buf.String()
	r.buf.Reset()
	return UnescapeXstring(s)
}

func parseSharedStrings(data []byte) ([]string, error) {
	var out []string
	if data == nil {
		return out, nil
	}
	rt := &richText{}
	inSi := false
	err := walkXML(data, "sharedStrings", func(tok xml.Token) error {
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local == "si" {
				inSi = true
			} else if inSi {
				rt.open(e)
			}
		case xml.EndElement:
			if e.Name.Local == "si" {
				inSi = false
				out = append(out, rt.take())
			} else if inSi {
				rt.close(e)
			}
		case xml.CharData:
			if inSi {
				rt.text(e)
			}
		}
		return nil
	})
	return out, err
}

type rawCell struct {
	t    string
	s    int
	v, f *string
	is   *string
}

type sheetCells struct {
	rows   map[int]map[int]*rawCell
	merges []string
}

var cellRefRe = regexp.MustCompile(`^([A-Z]+)(\d+)$`)

func parseSheet(data []byte, part string, onlyRow1 bool) (*sheetCells, error) {
	sc := &sheetCells{rows: map[int]map[int]*rawCell{}}
	row, col := 0, 0
	var cell *rawCell
	var field *strings.Builder
	var fieldName string
	var rt *richText
	err := walkXML(data, part, func(tok xml.Token) error {
		switch e := tok.(type) {
		case xml.StartElement:
			switch {
			case e.Name.Local == "row":
				if r, ok := attr(e, "r"); ok {
					row = atoiOr(r, row+1)
				} else {
					row++
				}
				col = 0
				if onlyRow1 && row > 1 {
					return errStop
				}
			case e.Name.Local == "c":
				ref, _ := attr(e, "r")
				if m := cellRefRe.FindStringSubmatch(ref); m != nil {
					col = colNumber(m[1])
					row = atoiOr(m[2], 1<<40)
				} else {
					col++
				}
				t, ok := attr(e, "t")
				if !ok {
					t = "n"
				}
				s, _ := attr(e, "s")
				cell = &rawCell{t: t}
				if s != "" {
					cell.s = atoiOr(s, -1)
				}
			case cell != nil && e.Name.Local == "v":
				field, fieldName = &strings.Builder{}, "v"
			case cell != nil && e.Name.Local == "f":
				field, fieldName = &strings.Builder{}, "f"
			case cell != nil && e.Name.Local == "is":
				rt = &richText{}
			case rt != nil:
				rt.open(e)
			case e.Name.Local == "mergeCell":
				ref, _ := attr(e, "ref")
				sc.merges = append(sc.merges, ref)
			}
		case xml.EndElement:
			switch {
			case e.Name.Local == "c" && cell != nil:
				if sc.rows[row] == nil {
					sc.rows[row] = map[int]*rawCell{}
				}
				sc.rows[row][col] = cell
				cell = nil
			case e.Name.Local == "is" && rt != nil:
				s := rt.take()
				cell.is = &s
				rt = nil
			case rt != nil:
				rt.close(e)
			case (e.Name.Local == "v" || e.Name.Local == "f") && field != nil:
				s := field.String()
				if fieldName == "v" {
					cell.v = &s
				} else {
					cell.f = &s
				}
				field = nil
			}
		case xml.CharData:
			if rt != nil {
				rt.text(e)
			} else if cell != nil && field != nil {
				field.Write(e)
			}
		}
		return nil
	})
	if err != nil && err != errStop {
		return nil, err
	}
	return sc, nil
}

// cellResult is a converted cell: text, or a conversion error with its value.
type cellResult struct {
	text    string
	isError bool
	value   string
	reason  string
}

type converter struct {
	styles   *styles
	date1904 bool
	shared   []string
}

func (cv *converter) value(c *rawCell) (cellResult, error) {
	if c.f != nil && c.v == nil && c.t != "inlineStr" {
		return cellResult{isError: true, value: "=" + *c.f, reason: "a formula without a saved result"}, nil
	}
	blank := c.v == nil || strings.TrimSpace(*c.v) == ""
	switch c.t {
	case "s":
		if blank {
			return cellResult{}, nil
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(*c.v), 64)
		if err != nil || f != math.Trunc(f) || f < 0 || f >= float64(len(cv.shared)) {
			return cellResult{}, wbErr(ErrWorkbookInvalid, "shared string %s does not exist", *c.v)
		}
		return cellResult{text: cv.shared[int(f)]}, nil
	case "inlineStr":
		if c.is == nil {
			return cellResult{}, nil
		}
		return cellResult{text: *c.is}, nil
	case "str":
		if c.v == nil {
			return cellResult{}, nil
		}
		return cellResult{text: UnescapeXstring(*c.v)}, nil
	case "b":
		if c.v != nil && *c.v == "1" {
			return cellResult{text: "true"}, nil
		}
		if c.v != nil && *c.v == "0" {
			return cellResult{text: "false"}, nil
		}
		return cellResult{}, nil
	case "e":
		v := ""
		if c.v != nil {
			v = *c.v
		}
		return cellResult{isError: true, value: v, reason: "an error value"}, nil
	case "d":
		if c.v == nil || *c.v == "" {
			return cellResult{}, nil
		}
		if t, ok := IsoCellText(*c.v); ok {
			return cellResult{text: t}, nil
		}
		return cellResult{isError: true, value: *c.v, reason: "a date that cannot be read"}, nil
	}
	if blank {
		return cellResult{}, nil
	}
	// A date cell holds a number too: one that is not a number, or is outside
	// Excel's range, is workbook_invalid before it is read as a date.
	t, ok := NumberText(*c.v)
	if !ok {
		shown := *c.v
		if len(shown) > 40 {
			shown = shown[:40] + "…"
		}
		return cellResult{}, wbErr(ErrWorkbookInvalid, "a number cell holds %q, which is not a number Excel can store", shown)
	}
	f := cv.styles.format(c.s)
	if kind := DateKind(f.id, f.code, f.hasCode); kind != "" {
		if d, ok := SerialText(*c.v, kind, cv.date1904); ok {
			return cellResult{text: d}, nil
		}
		first := "1900-03-01"
		if cv.date1904 {
			first = "1904-01-01"
		}
		return cellResult{isError: true, value: *c.v, reason: "a date outside " + first + " to 9999-12-31"}, nil
	}
	if w := ZeroPad(f.code); w > 0 && f.hasCode && isDigits(t) && len(t) < w {
		t = strings.Repeat("0", w-len(t)) + t
	}
	return cellResult{text: t}, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- the conversion

// ConvertWorkbook turns a workbook into the rows of products.csv (role
// "products") or assets.csv ("assets"), as §5 says. A workbook that cannot be
// imported gives a *WorkbookError. The caller checks the upload limit (§5.6
// step 1) before.
func ConvertWorkbook(r io.ReaderAt, size int64, role string) (*Workbook, error) {
	ro, ok := wbRoles[role]
	if !ok {
		return nil, fmt.Errorf("unknown workbook role %q", role)
	}
	file := WorkbookFile(role)
	pkg, err := openWorkbookPackage(r, size)
	if err != nil {
		return nil, err
	}
	// Step 5: unsafe parts, by entry names and content types, before they are read.
	if err := pkg.checkUnsafeNames(); err != nil {
		return nil, err
	}
	ctData, err := pkg.read("[Content_Types].xml")
	if err != nil {
		return nil, err
	}
	if ctData == nil {
		return nil, wbErr(ErrWorkbookInvalid, "[Content_Types].xml is missing")
	}
	ct, err := parseContentTypes(ctData)
	if err != nil {
		return nil, err
	}
	for _, t := range ct.all {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "macroenabled") || strings.Contains(lt, "vbaproject") {
			return nil, wbErr(ErrWorkbookUnsafe, "the workbook is macro-enabled (%s)", t)
		}
	}
	// Step 6: structure.
	rootRels, err := pkg.read("_rels/.rels")
	if err != nil {
		return nil, err
	}
	if rootRels == nil {
		return nil, wbErr(ErrWorkbookInvalid, "_rels/.rels is missing")
	}
	rels, err := parseRels(rootRels, "_rels/.rels")
	if err != nil {
		return nil, err
	}
	var main *relationship
	for i := range rels {
		if rels[i].typ == relOfficeDoc && !rels[i].external {
			main = &rels[i]
			break
		}
	}
	if main == nil {
		return nil, wbErr(ErrWorkbookInvalid, "the package has no main document")
	}
	wbPart := resolvePart("/", main.target)
	if t := ct.typeOf(wbPart); !mainTypes[t] {
		if t == "" {
			t = "missing"
		}
		return nil, wbErr(ErrWorkbookInvalid, "the main document is %s, not an Excel workbook", t)
	}
	wbData, err := pkg.read(wbPart)
	if err != nil {
		return nil, err
	}
	if wbData == nil {
		return nil, wbErr(ErrWorkbookInvalid, "%s is missing", wbPart)
	}
	sheets, date1904, err := parseWorkbookPart(wbData, wbPart)
	if err != nil {
		return nil, err
	}
	var wbRels []relationship
	relsData, err := pkg.read(relsOf(wbPart))
	if err != nil {
		return nil, err
	}
	if relsData != nil {
		if wbRels, err = parseRels(relsData, relsOf(wbPart)); err != nil {
			return nil, err
		}
	}
	// Step 7: external relationships of the workbook part.
	for _, rel := range wbRels {
		if rel.external {
			return nil, wbErr(ErrWorkbookUnsafe, "the workbook refers to an external resource (%s)", path.Base(rel.typ))
		}
	}
	relByID := map[string]relationship{}
	for _, rel := range wbRels {
		if _, dup := relByID[rel.id]; !dup {
			relByID[rel.id] = rel
		}
	}
	readRel := func(typ string) ([]byte, error) {
		for _, rel := range wbRels {
			if rel.typ == typ {
				return pkg.read(resolvePart(wbPart, rel.target))
			}
		}
		return nil, nil
	}
	stylesData, err := readRel(relStyles)
	if err != nil {
		return nil, err
	}
	st, err := parseStyles(stylesData)
	if err != nil {
		return nil, err
	}
	ssData, err := readRel(relSharedStrs)
	if err != nil {
		return nil, err
	}
	shared, err := parseSharedStrings(ssData)
	if err != nil {
		return nil, err
	}
	cv := &converter{styles: st, date1904: date1904, shared: shared}

	// Step 8: the sheet (§5.2): visible worksheets only.
	var visible []wbSheet
	for _, s := range sheets {
		rel, ok := relByID[s.rid]
		if s.state != "visible" || !ok || rel.typ != relWorksheet {
			continue
		}
		s.part = resolvePart(wbPart, rel.target)
		visible = append(visible, s)
	}
	readSheet := func(s wbSheet, onlyRow1 bool) (*sheetCells, error) {
		data, err := pkg.read(s.part)
		if err != nil {
			return nil, err
		}
		if data == nil {
			return nil, wbErr(ErrWorkbookInvalid, "%s is missing", s.part)
		}
		return parseSheet(data, s.part, onlyRow1)
	}
	var chosen *wbSheet
	for i := range visible {
		if asciiLower(strings.TrimSpace(visible[i].name)) == ro.sheet {
			chosen = &visible[i]
			break
		}
	}
	if chosen == nil {
	search:
		for i := range visible {
			sc, err := readSheet(visible[i], true)
			if err != nil {
				return nil, err
			}
			for _, c := range sc.rows[1] {
				v, err := cv.value(c)
				if err != nil {
					return nil, err
				}
				name := wbHeaderName(v.text)
				for _, k := range ro.keys {
					if name == k {
						chosen = &visible[i]
						break search
					}
				}
			}
		}
	}
	if chosen == nil {
		return nil, wbErr(ErrWorkbookSheetMissing, "no visible sheet is named %s or has a %s column in row 1", ro.label, ro.keys[0])
	}

	// Steps 9 and 10: the cells.
	sc, err := readSheet(*chosen, false)
	if err != nil {
		return nil, err
	}
	for _, ref := range sc.merges {
		r1, c1, r2, c2, ok := parseRange(ref)
		if !ok {
			continue
		}
		for rn, cells := range sc.rows {
			if rn < r1 || rn > r2 {
				continue
			}
			for cn := range cells {
				if cn >= c1 && cn <= c2 && (rn != r1 || cn != c1) {
					delete(cells, cn)
				}
			}
		}
	}
	values := map[int]map[int]cellResult{}
	for _, rn := range sortedKeys(sc.rows) {
		cells := sc.rows[rn]
		for _, cn := range sortedKeys(cells) {
			v, err := cv.value(cells[cn])
			if err != nil {
				return nil, err
			}
			if !v.isError && v.text == "" {
				continue
			}
			if rn > WorkbookMaxRow {
				return nil, wbErr(ErrWorkbookTooLarge, "the sheet has values below row %d (cell %s%d)", WorkbookMaxRow, ColumnLetter(cn), rn)
			}
			if cn > WorkbookMaxCol {
				return nil, wbErr(ErrWorkbookTooLarge, "the sheet has values right of column %s (cell %s%d)", ColumnLetter(WorkbookMaxCol), ColumnLetter(cn), rn)
			}
			if values[rn] == nil {
				values[rn] = map[int]cellResult{}
			}
			values[rn][cn] = v
		}
	}
	out := &Workbook{Sheet: chosen.name}
	row1 := values[1]
	width := 0
	for cn, v := range row1 {
		if !v.isError && v.text != "" && cn > width {
			width = cn
		}
	}
	header := make([]string, width)
	for i := range header {
		if v, ok := row1[i+1]; ok && !v.isError {
			header[i] = v.text
		}
	}
	issue := func(rn, cn int, v cellResult) CellIssue {
		column := ""
		if rn > 1 {
			column = TrimHeader(header[cn-1])
		}
		val := v.value
		var vp *string
		if val != "" {
			vp = &val
		}
		return CellIssue{File: file, Row: rn, Column: column, Severity: "error", ObxError: "validation_failed",
			Detail: CellLocator(rn, column, cn) + "the cell holds " + v.reason, Value: vp}
	}
	for _, cn := range sortedKeys(row1) {
		if v := row1[cn]; v.isError && cn <= width {
			out.Errors = append(out.Errors, issue(1, cn, v))
		}
	}
	last := 1
	for rn, cells := range values {
		if rn <= 1 {
			continue
		}
		for cn := range cells {
			if cn <= width && rn > last {
				last = rn
			}
		}
	}
	out.Rows = append(out.Rows, header)
	for rn := 2; rn <= last; rn++ {
		cells := values[rn]
		rec := make([]string, width)
		bad := false
		for _, cn := range sortedKeys(cells) {
			if v := cells[cn]; cn <= width && v.isError {
				out.Errors = append(out.Errors, issue(rn, cn, v))
				bad = true
			}
		}
		if !bad {
			for i := range rec {
				rec[i] = cells[i+1].text
			}
		}
		out.Rows = append(out.Rows, rec)
	}
	// §5.3: values without a header: right of W, or (rows 2 to R) under an
	// empty header name.
	stray := map[int]bool{}
	for rn, cells := range values {
		for cn := range cells {
			if cn > width || (rn > 1 && TrimHeader(header[cn-1]) == "") {
				stray[cn] = true
			}
		}
	}
	for _, cn := range sortedKeys(stray) {
		out.Warnings = append(out.Warnings, CellIssue{File: file, Severity: "warning", ObxError: "validation_failed",
			Detail: fmt.Sprintf("column %s: has values but no header in row 1; ignored", ColumnLetter(cn))})
	}
	return out, nil
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

var rangeRe = regexp.MustCompile(`^([A-Z]+)(\d+)(?::([A-Z]+)(\d+))?$`)

func parseRange(ref string) (r1, c1, r2, c2 int, ok bool) {
	m := rangeRe.FindStringSubmatch(ref)
	if m == nil || m[3] == "" {
		return 0, 0, 0, 0, false
	}
	return atoiOr(m[2], 0), colNumber(m[1]), atoiOr(m[4], 0), colNumber(m[3]), true
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// CellLocator is the §5.5 locator at the start of a detail about a cell:
// "row 12, column ean (C): " (column is the header text, trimmed), or
// "row 12, column C: " when the column has no header.
func CellLocator(row int, column string, col int) string {
	if column == "" {
		return fmt.Sprintf("row %d, column %s: ", row, ColumnLetter(col))
	}
	return fmt.Sprintf("row %d, column %s (%s): ", row, column, ColumnLetter(col))
}

// WorkbookLocator is the §5.5 locator for a problem the CSV import of a
// workbook's rows reports: row is the Excel row (0 = none), column the column
// as the CSV import names it ("" = none), header the workbook's row 1.
func WorkbookLocator(header []string, row int, column string) string {
	idx := -1
	if column != "" {
		norm := NormalizeHeader(column)
		for i, h := range header {
			if TrimHeader(h) == column || NormalizeHeader(h) == norm {
				idx = i
				break
			}
		}
	}
	switch {
	case idx >= 0 && row > 0:
		return CellLocator(row, TrimHeader(header[idx]), idx+1)
	case idx >= 0:
		if h := TrimHeader(header[idx]); h != "" {
			return fmt.Sprintf("column %s (%s): ", h, ColumnLetter(idx+1))
		}
		return fmt.Sprintf("column %s: ", ColumnLetter(idx+1))
	case row > 0:
		return fmt.Sprintf("row %d: ", row)
	}
	return ""
}

// CanonicalCSV writes rows as canonical CSV text (§5.3): UTF-8 without BOM,
// ",", CRLF after every row, quotes only around fields with , " CR or LF.
func CanonicalCSV(rows [][]string) []byte {
	var b bytes.Buffer
	for _, r := range rows {
		for i, f := range r {
			if i > 0 {
				b.WriteByte(',')
			}
			if strings.ContainsAny(f, ",\"\r\n") {
				b.WriteByte('"')
				b.WriteString(strings.ReplaceAll(f, `"`, `""`))
				b.WriteByte('"')
			} else {
				b.WriteString(f)
			}
		}
		b.WriteString("\r\n")
	}
	return b.Bytes()
}
