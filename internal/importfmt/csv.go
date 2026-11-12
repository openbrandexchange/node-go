// Package importfmt parses the OBX canonical import format
// (docs/import-format.md): products.csv, assets.csv, the image file name
// pattern, the materials grammar and care codes.
package importfmt

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Table is a parsed CSV file.
type Table struct {
	Header []string // normalised column names
	Raw    []string // header as written
	Rows   []Row
}

// Row is one data row (Num is 1-based; header = row 0).
type Row struct {
	Num    int
	Values map[string]string
}

// Get returns the trimmed cell of a column ("" when absent).
func (r Row) Get(col string) string { return r.Values[col] }

var aliases = map[string]string{
	"style":         "style_number",
	"ean":           "gtin",
	"barcode":       "gtin",
	"colour_code":   "color_code",
	"colour_name":   "color_name",
	"colour":        "color_code",
	"colour_family": "color_family",
}

// NormalizeHeader normalises a column name: case-insensitive, spaces and
// '-' become '_', language suffixes (name:de-CH) keep their tag form.
func NormalizeHeader(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
	base, lang, hasLang := strings.Cut(h, ":")
	base = strings.ToLower(strings.TrimSpace(base))
	base = strings.NewReplacer(" ", "_", "-", "_").Replace(base)
	if a, ok := aliases[base]; ok {
		base = a
	}
	if !hasLang {
		return base
	}
	lang = strings.TrimSpace(lang)
	if base == "attr" || base == "feature" {
		return base + ":" + strings.ToLower(strings.NewReplacer(" ", "_", "-", "_").Replace(lang))
	}
	return base + ":" + NormalizeLang(lang)
}

// NormalizeLang lowercases the language and uppercases the region.
func NormalizeLang(l string) string {
	l = strings.ReplaceAll(strings.TrimSpace(l), "_", "-")
	lang, region, ok := strings.Cut(l, "-")
	if !ok {
		return strings.ToLower(lang)
	}
	return strings.ToLower(lang) + "-" + strings.ToUpper(region)
}

// DetectDelimiter picks ',' or ';' (or tab) from the header line: whichever
// occurs more often outside quotes; ties give ','.
func DetectDelimiter(headerLine string) rune {
	counts := map[rune]int{}
	inQ := false
	for _, r := range headerLine {
		if r == '"' {
			inQ = !inQ
			continue
		}
		if !inQ && (r == ',' || r == ';' || r == '\t') {
			counts[r]++
		}
	}
	best := ','
	if counts[';'] > counts[best] {
		best = ';'
	}
	if counts['\t'] > counts[best] {
		best = '\t'
	}
	return best
}

// ParseCSV reads a canonical CSV (UTF-8 with or without BOM, RFC 4180,
// ',' or ';' delimited).
func ParseCSV(data []byte) (*Table, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("the file is not UTF-8 text")
	}
	first := data
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		first = data[:i]
	}
	if len(bytes.TrimSpace(first)) == 0 {
		return nil, fmt.Errorf("the file has no header row")
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = DetectDelimiter(string(first))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("the header row cannot be read: %v", err)
	}
	records := [][]string{header}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("row %d cannot be read: %v", len(records), err)
		}
		records = append(records, rec)
	}
	return newTable(records), nil
}

// TableFromRecords builds a table from rows that are already split into
// fields, header first (an Excel workbook, import-format.md §5.3). The rows
// are treated exactly as the same rows in a CSV file.
func TableFromRecords(records [][]string) (*Table, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("the file has no header row")
	}
	first := string(CanonicalCSV(records[:1]))
	if i := strings.IndexAny(first, "\r\n"); i >= 0 {
		first = first[:i]
	}
	if strings.TrimSpace(first) == "" {
		return nil, fmt.Errorf("the file has no header row")
	}
	return newTable(records), nil
}

func newTable(records [][]string) *Table {
	header := records[0]
	t := &Table{Raw: header}
	for _, h := range header {
		t.Header = append(t.Header, NormalizeHeader(h))
	}
	for num, rec := range records[1:] {
		empty := true
		vals := map[string]string{}
		for i, v := range rec {
			v = strings.TrimSpace(v)
			if v != "" {
				empty = false
			}
			if i < len(t.Header) && t.Header[i] != "" {
				if _, dup := vals[t.Header[i]]; !dup || v != "" {
					vals[t.Header[i]] = v
				}
			}
		}
		if empty {
			continue
		}
		t.Rows = append(t.Rows, Row{Num: num + 1, Values: vals})
	}
	return t
}

// Has reports whether the table has a column.
func (t *Table) Has(col string) bool {
	for _, h := range t.Header {
		if h == col {
			return true
		}
	}
	return false
}

// Localized collects "<base>" and "<base>:<lang>" columns of a row into a
// map; the plain column is defaultLang (a tagged column wins).
func Localized(r Row, header []string, base, defaultLang string) map[string]string {
	out := map[string]string{}
	if v := r.Get(base); v != "" {
		out[defaultLang] = v
	}
	for _, h := range header {
		if strings.HasPrefix(h, base+":") {
			if v := r.Get(h); v != "" {
				out[strings.TrimPrefix(h, base+":")] = v
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// KnownColumns are the products.csv columns (without language suffixes).
var KnownColumns = map[string]bool{
	"brand": true, "product_id": true, "style_number": true, "name": true, "description": true, "item_type": true,
	"target_group": true, "season": true, "collection": true, "gpc_brick": true, "materials": true, "care": true,
	"country_of_origin": true, "dpp_url": true, "status": true, "variant_id": true, "gtin": true, "sku": true,
	"color_code": true, "color_name": true, "supplier_color": true, "size": true, "size_system": true,
	"variant_status": true, "image_files": true,
	// 0.2 article data (import-format.md §1.6)
	"item_subtype": true, "contains_animal_origin": true, "footwear_materials": true, "certifications": true,
	"color_family": true, "size_length": true, "height_cm": true, "width_cm": true, "depth_cm": true, "weight_kg": true,
	"volume_l": true, "volume_expanded_l": true, "handle_drop_cm": true, "strap_length_cm": true, "laptop_max_inch": true,
	"heel_height_cm": true, "shaft_height_cm": true, "inseam_cm": true,
}

// KnownAssetColumns are the assets.csv columns.
var KnownAssetColumns = map[string]bool{
	"file": true, "asset_id": true, "product_id": true, "style_number": true, "color_code": true, "position": true,
	"role": true, "view": true, "alt_text": true, "license": true, "license_text": true, "channels": true,
	"territories": true, "embargo_until": true, "expires_at": true, "credit": true, "model_release": true,
	"ai_generated": true, "ai_edited": true, "source": true, "c2pa": true, "c2pa_manifest_url": true,
	"generator": true, "status": true,
}

// UnknownColumns lists header columns that are not part of the format.
func UnknownColumns(header []string, known map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range header {
		base, _, _ := strings.Cut(h, ":")
		if h == "" || known[base] || strings.HasPrefix(h, "attr:") || strings.HasPrefix(h, "feature:") || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// ParseBool accepts true/false, yes/no, ja/nee, 1/0.
func ParseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "ja", "1", "y", "j":
		return true, true
	case "false", "no", "nee", "0", "n":
		return false, true
	}
	return false, false
}

// ParseDate accepts RFC 3339 or YYYY-MM-DD (midnight UTC).
func ParseDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339Nano), true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format(time.RFC3339Nano), true
	}
	return "", false
}

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Key normalises an enum-like key ("Organic Cotton" → "organic_cotton").
func Key(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), "_")
	return strings.ReplaceAll(s, "-", "_")
}

// ValidKey reports whether s matches ^[a-z][a-z0-9_]{0,63}$.
func ValidKey(s string) bool { return keyRe.MatchString(s) }

// Slug implements the OBX slug rule: lowercase, diacritics removed, every
// run outside a-z0-9 becomes '-', trimmed.
func Slug(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	out = strings.ToLower(out)
	var b strings.Builder
	dash := false
	for _, r := range out {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// Fiber is one fibre of a part, normalised (0.1 keys recycled_<fibre> and
// organic_<fibre> become the fibre with the flag).
type Fiber struct {
	Key        string
	Percentage float64
	Recycled   bool
	Organic    bool
}

// Part is one material part.
type Part struct {
	Name   string
	Fibers []Fiber
}

var (
	fiberRe = regexp.MustCompile(`^([0-9]+(?:[.,][0-9]+)?)\s*%\s*(.+)$`)
	flagRe  = regexp.MustCompile(`(?i)\s*\((recycled|organic)\)\s*$`)
)

// ParseMaterials parses "outer: 65% cotton, 35% polyamide; lining: 100%
// polyester (recycled)". Whether the percentages of a part add up to 100 is
// a label completeness check of the stored product (import-format.md rule
// 10), not a parse warning.
func ParseMaterials(s string) ([]Part, []string, error) {
	var parts []Part
	var warnings []string
	for _, p := range strings.Split(s, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name := "main"
		body := p
		if i := strings.Index(p, ":"); i >= 0 {
			name = Key(p[:i])
			body = p[i+1:]
		}
		if !ValidKey(name) {
			return nil, nil, fmt.Errorf("invalid part name %q", name)
		}
		part := Part{Name: name}
		for _, f := range strings.Split(body, ",") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			m := fiberRe.FindStringSubmatch(f)
			if m == nil {
				return nil, nil, fmt.Errorf("%q is not like \"95%% cotton\"", f)
			}
			pct, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
			if err != nil || pct <= 0 || pct > 100 {
				return nil, nil, fmt.Errorf("invalid percentage in %q", f)
			}
			fib := Fiber{Percentage: pct}
			rest := m[2]
			for {
				fm := flagRe.FindStringSubmatch(rest)
				if fm == nil {
					break
				}
				if strings.EqualFold(fm[1], "recycled") {
					fib.Recycled = true
				} else {
					fib.Organic = true
				}
				rest = rest[:len(rest)-len(fm[0])]
			}
			key := Key(rest)
			if !ValidKey(key) {
				return nil, nil, fmt.Errorf("invalid fibre %q", m[2])
			}
			var rec, org bool
			fib.Key, rec, org = NormalizeFiber(key)
			fib.Recycled, fib.Organic = fib.Recycled || rec, fib.Organic || org
			part.Fibers = append(part.Fibers, fib)
		}
		if len(part.Fibers) == 0 {
			return nil, nil, fmt.Errorf("part %q has no fibres", name)
		}
		parts = append(parts, part)
	}
	return parts, warnings, nil
}

// CareCodes is the recommended care code list (ISO 3758 aligned).
var CareCodes = map[string]bool{}

func init() {
	for _, c := range strings.Fields(`wash_95 wash_70 wash_60 wash_60_mild wash_50 wash_50_mild wash_40 wash_40_mild wash_40_very_mild
wash_30 wash_30_mild wash_30_very_mild hand_wash do_not_wash bleach_any bleach_oxygen_only do_not_bleach
tumble_dry_normal tumble_dry_low do_not_tumble_dry line_dry drip_line_dry dry_flat drip_dry_flat line_dry_shade
drip_line_dry_shade dry_flat_shade drip_dry_flat_shade iron_high iron_medium iron_low do_not_iron dry_clean_p
dry_clean_p_mild dry_clean_p_very_mild dry_clean_f dry_clean_f_mild dry_clean_f_very_mild do_not_dry_clean
wet_clean_w wet_clean_w_mild wet_clean_w_very_mild do_not_wet_clean`) {
		CareCodes[c] = true
	}
}

// ParseCare parses "wash_30|do_not_bleach". Unknown codes with a valid key
// are kept with a warning; others are errors.
func ParseCare(s string) ([]string, []string, error) {
	var codes, warnings []string
	for _, c := range strings.Split(s, "|") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		k := Key(c)
		if !ValidKey(k) {
			return nil, nil, fmt.Errorf("invalid care code %q", c)
		}
		if !CareCodes[k] {
			warnings = append(warnings, fmt.Sprintf("unknown care code %q kept", k))
		}
		codes = append(codes, k)
	}
	return codes, warnings, nil
}

// GTINCell validates a GTIN cell: digits after removing spaces; Excel
// scientific notation is refused.
func GTINCell(s string) (string, error) {
	s = strings.Join(strings.Fields(s), "")
	if strings.ContainsAny(s, "eE+.,") {
		return "", fmt.Errorf("%q looks like a number mangled by a spreadsheet; format the column as text", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%q is not a GTIN (digits only)", s)
		}
	}
	return s, nil
}
