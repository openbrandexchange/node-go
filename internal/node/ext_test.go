package node

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtensions (ingest.md §3.1, conformance I-EXT-1): x- members are
// stored and returned by the Ingest GET, the protocol and /changes; other
// unknown members, at any depth, are ignored with a warning naming their
// pointer, never a 422; PUT replaces, PATCH null removes; a CSV import
// leaves them untouched.
func TestExtensions(t *testing.T) {
	k := newKestrelNode(t)
	ing := k.ingestToken(k.admin)
	_, cursor, _ := drainChanges(t, k.all, "", 500)
	doc := readFixture(t, "examples/ProductInput.extension.json").(map[string]any)
	doc["brand_external_id"] = "kestrel"
	doc["x-kestrel-big"] = strings.Repeat("a", 9000) // more than 8 KiB of extensions
	doc["unknown_top"] = 1
	fib := doc["materials"].([]any)[0].(map[string]any)["fibers"].([]any)[0].(map[string]any)
	fib["unknown_nested"] = true
	fib["x-kestrel-share"] = map[string]any{"a": []any{1, 2.5}}
	res := ing.must(201, "PUT", "/obx/v1/ingest/products/KO-1007", doc)
	if res["result"] != "created" {
		t.Fatalf("%v", res)
	}
	ws, _ := json.Marshal(res["warnings"])
	if !strings.Contains(string(ws), "/unknown_top") || !strings.Contains(string(ws), "/materials/0/fibers/0/unknown_nested") || strings.Contains(string(ws), "x-kestrel") {
		t.Fatalf("warnings %s", ws)
	}
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	check := func(where string, p map[string]any) {
		t.Helper()
		if p["x-kestrel-fabric-weight-gsm"] != float64(180) || asJSON(p["x-kestrel-care-note"]) != `{"en":"Wrinkles are part of the look"}` ||
			len(p["x-kestrel-big"].(string)) != 9000 || p["unknown_top"] != nil {
			t.Fatalf("%s: product members %v", where, p)
		}
		f := p["materials"].([]any)[0].(map[string]any)["fibers"].([]any)[0].(map[string]any)
		if asJSON(f["material"]) != `{"code":"linen","name":{"de":"Leinen","en":"Linen","fr":"Lin","nl":"Linnen"},"x-kestrel-mill":"Mill 7"}` ||
			asJSON(f["x-kestrel-share"]) != `{"a":[1,2.5]}` || f["unknown_nested"] != nil {
			t.Fatalf("%s: fibre %s", where, asJSON(f))
		}
	}
	rec := ing.must(200, "GET", "/obx/v1/ingest/products/KO-1007", nil)
	check("ingest record", rec["data"].(map[string]any))
	p := k.all.must(200, "GET", "/obx/v1/products/"+res["id"].(string), nil)
	validate(t, "ProductDetail", p)
	check("protocol", p)
	ch, cursor, _ := drainChanges(t, k.all, cursor, 100)
	if len(ch) != 1 {
		t.Fatalf("changes %v", ch)
	}
	check("changes", ch[0]["data"].(map[string]any))

	// The record written back is unchanged; a CSV import keeps the members.
	if r := ing.must(200, "PUT", "/obx/v1/ingest/products/KO-1007", rec["data"]); r["result"] != "unchanged" {
		t.Fatalf("round trip %v", r)
	}
	k.runImport(k.admin, k.kestrelFiles(), nil)
	check("after an import", ing.must(200, "GET", "/obx/v1/ingest/products/KO-1007", nil)["data"].(map[string]any))

	// PATCH null removes one; changing one is an update with a change entry.
	ing.must(200, "PATCH", "/obx/v1/ingest/products/KO-1007", map[string]any{"x-kestrel-big": nil, "x-kestrel-fabric-weight-gsm": 200}, "Content-Type", "application/merge-patch+json")
	p = k.all.must(200, "GET", "/obx/v1/products/"+res["id"].(string), nil)
	if _, ok := p["x-kestrel-big"]; ok || p["x-kestrel-fabric-weight-gsm"] != float64(200) {
		t.Fatalf("after PATCH %v", p)
	}
	if ch, _, _ := drainChanges(t, k.all, cursor, 100); len(ch) != 1 {
		t.Fatalf("changes after the patch %v", ch)
	}

	// Variants (color, measurements) and assets (rights, provenance).
	v := map[string]any{"product_external_id": "KO-1007", "color": map[string]any{"code": "WHT", "x-kestrel-dye": "natural"},
		"size": "M", "measurements": map[string]any{"x-kestrel-fit": "slim"}, "x-kestrel-plm": 7}
	ing.must(201, "PUT", "/obx/v1/ingest/variants/KO-1007-WHT-M", v)
	vr := ing.must(200, "GET", "/obx/v1/ingest/variants/KO-1007-WHT-M", nil)["data"].(map[string]any)
	if asJSON(vr["color"].(map[string]any)["x-kestrel-dye"]) != `"natural"` || vr["x-kestrel-plm"] != float64(7) ||
		asJSON(vr["measurements"]) != `{"x-kestrel-fit":"slim"}` {
		t.Fatalf("variant record %v", vr)
	}
	if r := ing.must(200, "PUT", "/obx/v1/ingest/variants/KO-1007-WHT-M", vr); r["result"] != "unchanged" {
		t.Fatalf("variant round trip %v", r)
	}
}

// TestDraftQualifier (ingest.md §3.1, conformance I-MAT-1): the draft
// single qualifier is read as a list of one (null: []), qualifiers wins
// when both are sent, one warning qualifier_deprecated per write, and the
// same data written with qualifiers only is unchanged.
func TestDraftQualifier(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	ing := n.ingestToken(admin)
	retail, _ := n.apiClient(admin, "Store", map[string]any{})
	brand := admin.must(200, "GET", "/admin/api/catalog/brands", nil)["items"].([]any)[0].(map[string]any)["external_id"].(string)
	fibre := func(code string, extra map[string]any) map[string]any {
		f := map[string]any{"material": map[string]any{"code": code}, "percentage": 50}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	doc := func(fibers ...any) map[string]any {
		return map[string]any{"brand_external_id": brand, "style_number": "Q-1", "name": map[string]any{"en": "Q"}, "item_type": "bag",
			"materials": []any{map[string]any{"part": map[string]any{"code": "main"}, "fibers": fibers}}}
	}
	res := ing.must(201, "PUT", "/obx/v1/ingest/products/Q-1", doc(
		fibre("polyester", map[string]any{"qualifier": map[string]any{"code": "recycled"}}),
		fibre("cotton", map[string]any{"qualifier": map[string]any{"code": "organic"}, "qualifiers": []any{map[string]any{"code": "regenerative"}}}),
		fibre("elastane", map[string]any{"qualifier": nil})))
	var deprecated []any
	for _, d := range res["warning_details"].([]any) {
		if d.(map[string]any)["code"] == warnQualifierDeprecated {
			deprecated = append(deprecated, d)
		}
	}
	if len(deprecated) != 1 || deprecated[0].(map[string]any)["pointer"] != "/materials/0/fibers/0/qualifier" {
		t.Fatalf("warning_details %v", res["warning_details"])
	}
	codes := func(p map[string]any) string {
		var out []string
		for _, f := range p["materials"].([]any)[0].(map[string]any)["fibers"].([]any) {
			fm := f.(map[string]any)
			if _, ok := fm["qualifier"]; ok {
				t.Fatalf("a qualifier member: %v", fm)
			}
			var cs []string
			for _, q := range fm["qualifiers"].([]any) {
				cs = append(cs, q.(map[string]any)["code"].(string))
			}
			out = append(out, "["+strings.Join(cs, ",")+"]")
		}
		return strings.Join(out, " ")
	}
	if got := codes(ing.must(200, "GET", "/obx/v1/ingest/products/Q-1", nil)["data"].(map[string]any)); got != "[recycled] [regenerative] []" {
		t.Fatalf("record %s", got)
	}
	ch, _, _ := drainChanges(t, retail, "", 100)
	if got := codes(ch[len(ch)-1]["data"].(map[string]any)); got != "[recycled] [regenerative] []" {
		t.Fatalf("changes %s", got)
	}
	again := ing.must(200, "PUT", "/obx/v1/ingest/products/Q-1", doc(
		fibre("polyester", map[string]any{"qualifiers": []any{map[string]any{"code": "recycled"}}}),
		fibre("cotton", map[string]any{"qualifiers": []any{map[string]any{"code": "regenerative"}}}),
		fibre("elastane", nil)))
	if again["result"] != "unchanged" || strings.Contains(asJSONString(again), warnQualifierDeprecated) {
		t.Fatalf("rewrite with qualifiers %v", again)
	}
}

func asJSONString(v any) string { b, _ := json.Marshal(v); return string(b) }
