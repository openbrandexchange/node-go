package importfmt

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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
		{"KO-1001_NVY_1.jpg", "KO-1001", "NVY", "", "", 1, false},
		{"KO-1001_NVY_2_back.jpg", "KO-1001", "NVY", "", "back", 2, false},
		{"KO-1003_RST_3_model.png", "KO-1003", "RST", "model", "", 3, false},
		{"AB_12_34_BLK_2_packshot_back.JPG", "AB_12_34", "BLK", "packshot", "back", 2, false},
		{"KO-1004_IND_3_detail.jpg", "KO-1004", "IND", "detail", "detail", 3, false},
		{"KO-1005_COG_2_tilted_left.webp", "KO-1005", "COG", "", "tilted_left", 2, false},
		{"KO-1001_NVY_front.jpg", "", "", "", "", 0, true},
		{"KO-1001_NVY_01.jpg", "", "", "", "", 0, true},
		{"KO-1001_NVY_1.gif", "", "", "", "", 0, true},
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
	data := []byte("\xef\xbb\xbfStyle Number;Name:EN;Colour-Code;attr:Fit Type\r\n\"KO-1\";\"Sweater; \"\"soft\"\"\";NVY;slim\r\n;;;\r\n")
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
	if err != nil || len(parts) != 2 || parts[0].Name != "outer" || parts[0].Fibers[0].Key != "cotton" || len(warns) != 0 {
		t.Fatalf("%v %v %v", parts, warns, err)
	}
	if _, w, _ := ParseMaterials("90% organic cotton"); len(w) != 1 {
		t.Fatalf("expected a sum warning")
	}
	if _, _, err := ParseMaterials("lots of cotton"); err == nil {
		t.Fatal("expected an error")
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
	if s := Slug("Kestrel Trail"); s != "kestrel-trail" {
		t.Fatal(s)
	}
	if s := Slug("  Éclair & Co. "); s != "eclair-co" {
		t.Fatal(s)
	}
}

func TestSafeEntry(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"ok/KO-1_A_1.jpg", "../evil.jpg", "/abs.jpg", "C:/x.jpg", "a/CON.jpg"} {
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
