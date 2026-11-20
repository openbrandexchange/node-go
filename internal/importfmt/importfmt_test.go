package importfmt

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParseImageName(t *testing.T) {
	cases := []struct {
		name                     string
		style, color, role, view string
		pos                      int
		err                      bool
	}{
		{"MN-1001_NVY_1.jpg", "MN-1001", "NVY", "", "", 1, false},
		{"MN-1001_NVY_2_back.jpg", "MN-1001", "NVY", "", "back", 2, false},
		{"MN-1003_RST_3_model.png", "MN-1003", "RST", "model", "", 3, false},
		{"AB_12_34_BLK_2_packshot_back.JPG", "AB_12_34", "BLK", "packshot", "back", 2, false},
		{"MN-1004_IND_3_detail.jpg", "MN-1004", "IND", "detail", "detail", 3, false},
		{"MN-1005_COG_2_tilted_left.webp", "MN-1005", "COG", "", "tilted_left", 2, false},
		{"MN-1001_NVY_front.jpg", "", "", "", "", 0, true},
		{"MN-1001_NVY_01.jpg", "", "", "", "", 0, true},
		{"MN-1001_NVY_1.gif", "", "", "", "", 0, true},
	}
	for _, c := range cases {
		n, err := ParseImageName(c.name)
		if c.err {
			if err == nil {
				t.Errorf("%s: expected an error", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if n.Style != c.style || n.Color != c.color || n.Position != c.pos || n.Role != c.role || n.View != c.view {
			t.Errorf("%s: got %+v", c.name, n)
		}
	}
}

func TestParseCSV(t *testing.T) {
	data := []byte("\xef\xbb\xbfStyle Number;Name:EN;Colour-Code;attr:Fit Type\r\n\"MN-1\";\"Sweater; \"\"soft\"\"\";NVY;slim\r\n;;;\r\n")
	tb, err := ParseCSV(data)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Header[0] != "style_number" || tb.Header[1] != "name:en" || tb.Header[2] != "color_code" || tb.Header[3] != "attr:fit_type" {
		t.Fatalf("header %v", tb.Header)
	}
	if len(tb.Rows) != 1 || tb.Rows[0].Get("name:en") != `Sweater; "soft"` || tb.Rows[0].Num != 1 {
		t.Fatalf("rows %+v", tb.Rows)
	}
}

func TestMaterialsAndCare(t *testing.T) {
	parts, warns, err := ParseMaterials("outer: 65% Cotton, 35% polyamide; lining: 100% polyester")
	if err != nil || len(parts) != 2 || parts[0].Name != "outer" || parts[0].Fibers[0].Material != "cotton" || len(warns) != 0 {
		t.Fatalf("%v %v %v", parts, warns, err)
	}
	fib := func(m string, pct float64, qs ...string) string {
		var p *float64
		if pct > 0 {
			p = &pct
		}
		return fmt.Sprintf("%s %v %v", m, ptrStr(p), qs)
	}
	str := func(f Fiber) string { return fmt.Sprintf("%s %v %v", f.Material, ptrStr(f.Percentage), f.Qualifiers) }
	// 0.1 keys and qualifiers give the same normalised fibre; sums are not a parse warning.
	for _, in := range []string{"90% organic cotton", "90% cotton (organic)", "90% Cotton (Organic)", "90% organic_cotton (organic)"} {
		p, w, err := ParseMaterials(in)
		if err != nil || len(w) != 0 || str(p[0].Fibers[0]) != fib("cotton", 90, "organic") {
			t.Fatalf("%s: %+v %v %v", in, p, w, err)
		}
	}
	p, _, err := ParseMaterials("outer: 100% polyester (recycled) (Post-Consumer), 0.5% cotton (organic)(regenerative) (organic); lining: 100% recycled_polyamide (pre_consumer); trim: 100% Merino Wool (Regenerative)")
	if err != nil || str(p[0].Fibers[0]) != fib("polyester", 100, "recycled", "post_consumer") ||
		str(p[0].Fibers[1]) != fib("cotton", 0.5, "organic", "regenerative") ||
		str(p[1].Fibers[0]) != fib("polyamide", 100, "recycled", "pre_consumer") ||
		str(p[2].Fibers[0]) != fib("merino_wool", 100, "regenerative") {
		t.Fatalf("%+v %v", p, err)
	}
	// The 0.1 key's qualifier comes first; combined prefixes in the order written.
	for in, want := range map[string]string{
		"100% recycled_polyester (post_consumer)": fib("polyester", 100, "recycled", "post_consumer"),
		"100% organic_cotton (recycled)":          fib("cotton", 100, "organic", "recycled"),
		"100% recycled_organic_cotton":            fib("cotton", 100, "recycled", "organic"),
	} {
		if p, _, err := ParseMaterials(in); err != nil || str(p[0].Fibers[0]) != want {
			t.Fatalf("%s: %+v %v", in, p, err)
		}
	}
	if p, _, err := ParseMaterials("100% cotton"); err != nil || p[0].Fibers[0].Qualifiers != nil {
		t.Fatalf("%+v %v", p, err)
	}
	// One code per bracket: the comma separates fibres.
	for _, in := range []string{"100% cotton (organic, recycled)", "50% cotton (organic,recycled), 50% wool", "100% cotton ()", "100% cotton (?!)", "100% cotton (organic"} {
		if _, _, err := ParseMaterials(in); err == nil {
			t.Fatalf("%s: expected an error", in)
		}
	}
	// A fibre without a percentage is null; decimal commas are refused.
	p, _, err = ParseMaterials("outer: leather; lining: 60% cotton, polyester (recycled)")
	if err != nil || str(p[0].Fibers[0]) != fib("leather", 0) || str(p[1].Fibers[0]) != fib("cotton", 60) || str(p[1].Fibers[1]) != fib("polyester", 0, "recycled") {
		t.Fatalf("%+v %v", p, err)
	}
	if p, _, err := ParseMaterials("95.5% cotton, 4.5% elastane"); err != nil || str(p[0].Fibers[0]) != fib("cotton", 95.5) {
		t.Fatalf("%+v %v", p, err)
	}
	for _, in := range []string{"95,5% cotton, 4,5% elastane", "main: 100", "lots % cotton", "0% cotton", "101% cotton"} {
		_, _, err := ParseMaterials(in)
		if err == nil {
			t.Fatalf("%s: expected an error", in)
		}
		if strings.Contains(in, ",5") && !strings.HasPrefix(err.Error(), "decimal comma") {
			t.Fatalf("%s: %v", in, err)
		}
	}
	codes, warns, err := ParseCare("wash_30|Do Not Bleach|special_treatment")
	if err != nil || len(codes) != 3 || codes[1] != "do_not_bleach" || len(warns) != 1 {
		t.Fatalf("%v %v %v", codes, warns, err)
	}
}

func TestGTINCellAndSlug(t *testing.T) {
	if _, err := GTINCell("2.00010010102E+12"); err == nil {
		t.Fatal("scientific notation must be refused")
	}
	if g, err := GTINCell("2000 1001 01025"); err != nil || g != "2000100101025" {
		t.Fatalf("%v %v", g, err)
	}
	if s := Slug("NOOR ATELIER"); s != "noor-atelier" {
		t.Fatal(s)
	}
	if s := Slug("  Éclair & Co. "); s != "eclair-co" {
		t.Fatal(s)
	}
}

func TestSafeEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"ok/MN-1_A_1.jpg", "../evil.jpg", "/abs.jpg", "C:/x.jpg", "a/CON.jpg"} {
		w, _ := zw.Create(n)
		_, _ = w.Write([]byte("x"))
	}
	_ = zw.Close()
	z, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	for i, f := range z.File {
		err := SafeEntry(f)
		if (i == 0) != (err == nil) {
			t.Errorf("%s: %v", f.Name, err)
		}
	}
	if !Ignored("__MACOSX/._x.jpg") || !Ignored("a/.DS_Store") || Ignored("a/b.jpg") {
		t.Fatal("ignored files")
	}
}

// TestImageNameVectors runs the spec's shared filename test vectors
// (fixtures/expected/image-filenames.json, synced into testdata/spec).
func TestImageNameVectors(t *testing.T) {
	data, err := os.ReadFile("../../testdata/spec/expected/image-filenames.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []struct {
			Name       string  `json:"name"`
			Style      string  `json:"style"`
			Color      *string `json:"color"`
			Position   int     `json:"position"`
			Role       string  `json:"role"`
			View       *string `json:"view"`
			ExternalID string  `json:"external_id"`
			Error      string  `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 20 {
		t.Fatalf("only %d vectors", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		n, err := ParseImageName(c.Name)
		if c.Error != "" {
			if err == nil {
				t.Errorf("%s: want an error, got %+v", c.Name, n)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		// What the importer makes of it: ALL means every colour, no role means packshot.
		var color *string
		if !strings.EqualFold(n.Color, "ALL") {
			color = &n.Color
		}
		role := n.Role
		if role == "" {
			role = "packshot"
		}
		var view *string
		if n.View != "" {
			view = &n.View
		}
		colorPart := "ALL"
		if color != nil {
			colorPart = *color
		}
		ext := n.Style + "_" + colorPart + "_" + strconv.Itoa(n.Position)
		if n.Style != c.Style || !sameStr(color, c.Color) || n.Position != c.Position || role != c.Role || !sameStr(view, c.View) || ext != c.ExternalID {
			t.Errorf("%s: got style %s colour %v position %d role %s view %v id %s", c.Name, n.Style, deref(color), n.Position, role, deref(view), ext)
		}
	}
}

func sameStr(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

func deref(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

func TestArticleDataCells(t *testing.T) {
	if NormalizeHeader("Feature:Detachable Parts") != "feature:detachable_parts" || NormalizeHeader("Colour Family") != "color_family" {
		t.Fatal(NormalizeHeader("Feature:Detachable Parts"), NormalizeHeader("Colour Family"))
	}
	f, err := ParseFootwearMaterials("upper: Leather; lining_and_sock: coated leather")
	if err != nil || *f != (Footwear{Upper: "leather", LiningAndSock: "coated_leather"}) {
		t.Fatal(f, err)
	}
	for _, bad := range []string{"upper: plastic", "heel: leather", "upper leather", "upper: leather; upper: other"} {
		if _, err := ParseFootwearMaterials(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	cs, err := ParseCertifications("GOTS:CU 123456; rws:CU 200101 (material); fsc")
	if err != nil || len(cs) != 3 || cs[0] != (Certification{"gots", "CU 123456", "product"}) ||
		cs[1] != (Certification{"rws", "CU 200101", "material"}) || cs[2] != (Certification{"fsc", "", "product"}) {
		t.Fatal(cs, err)
	}
	for in, want := range map[string]float64{"2.6": 2.6, "2,7": 2.7, "55": 55, ".5": 0.5} {
		if v, err := ParseMeasurement(in); err != nil || v != want {
			t.Errorf("%s: %v %v", in, v, err)
		}
	}
	for _, bad := range []string{"30 cm", "0", "-2", "1.000,5", "2.6kg", ""} {
		if _, err := ParseMeasurement(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if lo, hi, err := ParseStrapLength("80-130"); err != nil || lo != 80 || hi != 130 {
		t.Fatal(lo, hi, err)
	}
	if lo, hi, err := ParseStrapLength("120"); err != nil || lo != 120 || hi != 120 {
		t.Fatal(lo, hi, err)
	}
	if _, _, err := ParseStrapLength("130-80"); err == nil {
		t.Fatal("min > max accepted")
	}
	if v, err := ParseColorFamily("Gray"); err != nil || v != "grey" {
		t.Fatal(v, err)
	}
	if _, err := ParseColorFamily("navy"); err == nil {
		t.Fatal("navy accepted")
	}
	cases := []struct {
		key, cell string
		want      string // FormatFeature of the value, "" = error
	}{
		{"wheels", "4", "4"}, {"wheels", "3", ""}, {"shell", "Hard", "hard"}, {"lock", "TSA combination", "tsa_combination"},
		{"expandable", "yes", "yes"}, {"expandable", "maybe", ""}, {"detachable_parts", "shoulder_strap|Lining|lining", "shoulder_strap|lining"},
		{"detachable_parts", "strap", ""}, {"handle_type", "Padded", "Padded"},
	}
	for _, c := range cases {
		v, err := ParseFeature(c.key, c.cell)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s=%q accepted", c.key, c.cell)
			}
			continue
		}
		if err != nil || FormatFeature(v) != c.want {
			t.Errorf("%s=%q: %v %v", c.key, c.cell, v, err)
		}
	}
}

func ptrStr(p *float64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

func TestLanguageTags(t *testing.T) {
	for in, want := range map[string]string{"NL": "nl", "de_ch": "de-CH", "ZH_hant": "zh-Hant", "sr-latn-rs": "sr-Latn-RS", "es-419": "es-419"} {
		if got := NormalizeLang(in); got != want || !ValidLang(got) {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if got := NormalizeHeader("Name:ZH_hant"); got != "name:zh-Hant" {
		t.Errorf("header %s", got)
	}
	for _, bad := range []string{"nl-x-private", "english", "de-CH-1996"} {
		if ValidLang(NormalizeLang(bad)) {
			t.Errorf("%s accepted", bad)
		}
	}
	if u := UnknownColumns([]string{"name:zh-Hant", "name:english"}, KnownColumns); len(u) != 1 || u[0] != "name:english" {
		t.Errorf("unknown columns %v", u)
	}
}
