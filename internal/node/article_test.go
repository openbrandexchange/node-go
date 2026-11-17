package node

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openbrandexchange/node-go/internal/db"
)

// The article data (protocol.md §5.9, materials and care as terms since 0.3):
// imports (fixtures/imports, the cases of conformance I-IMP-7 and I-IMP-8),
// the Ingest API, the CSV export and the upgrade of 0.1 and 0.2 databases.

func importsFixture(t *testing.T, name string) string {
	return filepath.Join(specDir(t), "imports", name)
}

func TestArticleDataImports(t *testing.T) {
	k := newKestrelNode(t)
	var exp struct {
		Cases []struct {
			File     string                    `json:"file"`
			Counts   map[string]float64        `json:"counts"`
			Errors   []map[string]any          `json:"errors"`
			Changes  []map[string]any          `json:"changes"`
			Products map[string]map[string]any `json:"products"`
			Variants map[string]map[string]any `json:"variants"`
		} `json:"cases"`
	}
	raw, _ := json.Marshal(readFixture(t, "imports/expected.json"))
	if err := json.Unmarshal(raw, &exp); err != nil || len(exp.Cases) != 6 {
		t.Fatalf("expected.json: %v", err)
	}
	_, cursor, _ := drainChanges(t, k.all, "", 500)
	for _, c := range exp.Cases {
		run := k.runImport(k.admin, map[string][]string{"csv": {importsFixture(t, c.File)}}, nil)
		if run["status"] != "succeeded" {
			t.Fatalf("%s: %v", c.File, run)
		}
		got := run["counts"].(map[string]any)
		for key, want := range c.Counts {
			if got[key].(float64) != want {
				t.Errorf("%s: counts %v, want %v", c.File, got, c.Counts)
				break
			}
		}
		items := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)["items"].([]any)
		var errs []map[string]any
		for _, e := range items {
			validate(t, "ImportRowError", e)
			if em := e.(map[string]any); em["severity"] == "error" {
				errs = append(errs, em)
			}
		}
		if len(errs) != len(c.Errors) {
			t.Fatalf("%s: errors %v, want %v", c.File, errs, c.Errors)
		}
		for i, want := range c.Errors {
			for f, v := range want {
				if errs[i][f] != v {
					t.Errorf("%s: error %d %s = %v, want %v (%v)", c.File, i, f, errs[i][f], v, errs[i])
				}
			}
		}
		var ch []map[string]any
		ch, cursor, _ = drainChanges(t, k.all, cursor, 100)
		if len(ch) != len(c.Changes) {
			t.Fatalf("%s: changes %v, want %v", c.File, ch, c.Changes)
		}
		for i, want := range c.Changes {
			data, _ := ch[i]["data"].(map[string]any)
			if ch[i]["type"] != want["type"] || ch[i]["op"] != want["op"] || data["external_id"] != want["external_id"] {
				t.Errorf("%s: change %d %v, want %v", c.File, i, ch[i], want)
			}
		}
		for ext, fields := range c.Products {
			p := k.all.must(200, "GET", "/obx/v1/products/"+k.products[ext]["id"].(string), nil)
			for f, v := range fields {
				match(t, c.File+" "+ext+"."+f, v, p[f], nil)
			}
		}
		for ext, fields := range c.Variants {
			v := variantByExternalID(t, k, ext)
			for f, want := range fields {
				match(t, c.File+" "+ext+"."+f, want, v[f], nil)
			}
		}
	}

	t.Run("label completeness warnings (dry run)", func(t *testing.T) {
		run := k.runImport(k.admin, map[string][]string{"csv": {importsFixture(t, "label-warnings.csv")}}, map[string]any{"dry_run": true})
		if run["status"] != "succeeded" || run["counts"].(map[string]any)["errors"].(float64) != 0 {
			t.Fatalf("%v", run)
		}
		items := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)["items"].([]any)
		var got []map[string]any
		for _, e := range items {
			em := e.(map[string]any)
			switch em["obx_error"] {
			case warnFibreCompositionMissing, warnAnimalOriginUnknown, warnFootwearMaterialsMissing, warnFibrePercentagesNot100:
				got = append(got, em)
			}
		}
		want := readFixture(t, "imports/label-warnings.expected.json").(map[string]any)["warnings"].([]any)
		if len(got) != len(want) {
			t.Fatalf("label warnings %v, want %v", got, want)
		}
		for i, w := range want {
			for f, v := range w.(map[string]any) {
				if got[i][f] != v {
					t.Errorf("warning %d %s = %v, want %v", i, f, got[i][f], v)
				}
			}
		}
		if r := k.admin.do("GET", "/admin/api/catalog/products/KL-8001", nil); r.Status != 404 {
			t.Fatalf("a dry run wrote KL-8001: %d", r.Status)
		}
	})

	t.Run("the Kestrel import has no label warnings", func(t *testing.T) {
		run := k.runImport(k.admin, k.kestrelFiles(), nil)
		items := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)["items"].([]any)
		for _, e := range items {
			if code := e.(map[string]any)["obx_error"]; strings.Contains(code.(string), "missing") || strings.Contains(code.(string), "unknown") || strings.Contains(code.(string), "not_100") {
				t.Errorf("label warning in the Kestrel import: %v", e)
			}
		}
	})

	t.Run("exported CSV imports again unchanged", func(t *testing.T) {
		r := k.admin.do("GET", "/admin/api/portal/products/data.csv", nil)
		if r.Status != 200 {
			t.Fatalf("csv %d %s", r.Status, r.Body)
		}
		rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(r.Body, []byte("\xef\xbb\xbf")))).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		header := strings.Join(rows[0], ",")
		for _, col := range []string{
			"item_type,item_subtype,target_group,season,collection,gpc_brick,materials,contains_animal_origin,footwear_materials,care,country_of_origin,certifications,dpp_url,status," +
				"feature:shell,feature:wheels,feature:lock,feature:expandable,feature:closure,feature:laptop_compartment,feature:water_resistance," +
				"feature:detachable_parts,feature:adjustable_parts,feature:fit,feature:sleeve_length,feature:neckline,",
			"color_family,supplier_color,size,size_length,size_system,height_cm,width_cm,depth_cm,weight_kg,volume_l,volume_expanded_l," +
				"handle_drop_cm,strap_length_cm,laptop_max_inch,heel_height_cm,shaft_height_cm,inseam_cm,variant_status,image_files",
		} {
			if !strings.Contains(header, col) {
				t.Fatalf("header %s\nlacks %s", header, col)
			}
		}
		col := map[string]int{}
		for i, h := range rows[0] {
			col[h] = i
		}
		found := false
		for _, row := range rows[1:] {
			if row[col["style_number"]] == "KO-1009" {
				found = true
				if row[col["strap_length_cm"]] != "85-140" || row[col["laptop_max_inch"]] != "15.6" || row[col["feature:detachable_parts"]] != "shoulder_strap" ||
					row[col["certifications"]] != "grs:CU 200901 (material)" || row[col["color_family"]] != "grey" || row[col["feature:laptop_compartment"]] != "yes" {
					t.Errorf("KO-1009 row %v", row)
				}
			}
			if row[col["style_number"]] == "KT-2001" && !strings.Contains(row[col["materials"]], "100% polyester (recycled)") {
				t.Errorf("KT-2001 materials %q", row[col["materials"]])
			}
		}
		if !found {
			t.Fatal("KO-1009 not exported")
		}
		_, cur, _ := drainChanges(t, k.all, "", 500)
		run := k.runImport(k.admin, map[string][]string{"csv": {writeTemp(t, "products.csv", r.Body)}}, nil)
		c := run["counts"].(map[string]any)
		if c["errors"].(float64) != 0 || c["updated"].(float64) != 0 || c["created"].(float64) != 0 || c["unchanged"].(float64) != 11+46 {
			errs := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)
			t.Fatalf("re-import of the export: %v %v", c, errs)
		}
		if ch, _, _ := drainChanges(t, k.all, cur, 100); len(ch) != 0 {
			t.Fatalf("re-import of the export wrote %v", ch)
		}
	})
}

func variantByExternalID(t *testing.T, k *kestrelNode, ext string) map[string]any {
	t.Helper()
	for _, p := range k.products {
		for _, v := range p["variants"].([]any) {
			if vm := v.(map[string]any); vm["external_id"] == ext {
				prod := k.all.must(200, "GET", "/obx/v1/products/"+p["id"].(string), nil)
				for _, x := range prod["variants"].([]any) {
					if xm := x.(map[string]any); xm["external_id"] == ext {
						return xm
					}
				}
			}
		}
	}
	t.Fatalf("variant %s not found", ext)
	return nil
}

func TestArticleDataIngest(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	ing := n.ingestToken(admin)
	retail, _ := n.apiClient(admin, "Store", map[string]any{})
	brand := admin.must(200, "GET", "/admin/api/catalog/brands", nil)["items"].([]any)[0].(map[string]any)["external_id"].(string)

	product := map[string]any{
		"brand_external_id": brand, "style_number": "LG-1", "name": map[string]any{"en": "Cabin case"},
		"item_type": "luggage", "item_subtype": "cabin",
		"materials": []any{map[string]any{"part": map[string]any{"code": "shell"}, "fibers": []any{
			map[string]any{"material": map[string]any{"code": "recycled_polyester"}, "percentage": 60, "qualifiers": []any{
				map[string]any{"code": "post_consumer"}, map[string]any{"code": "recycled"}, map[string]any{"code": "post_consumer"}}},
			map[string]any{"material": map[string]any{"code": "polyamide", "name": map[string]any{"en": "Nylon"}}, "percentage": 30},
			map[string]any{"material": map[string]any{"code": "x_fibre", "name": map[string]any{"en": "X fibre"}}, "percentage": 5,
				"qualifiers": []any{map[string]any{"code": "upcycled", "name": map[string]any{"en": "Upcycled", "nl": " "}}}}}}},
		"care": []any{map[string]any{"code": "wash_30", "category": "drying", "text": map[string]any{"en": "Wash inside out"}},
			map[string]any{"code": "steam_only", "category": "ironing", "symbol": "4_Z", "name": map[string]any{"en": "Steam only"}},
			map[string]any{"code": "air_out"}},
		"features":       map[string]any{"wheels": 4, "shell": "hard", "handle_type": "telescopic", "lock": nil},
		"certifications": []any{map[string]any{"scheme": "grs", "scope": "material"}},
	}
	res := ing.must(201, "PUT", "/obx/v1/ingest/products/LG-1", product)
	validate(t, "IngestResult", res)
	details, _ := res["warning_details"].([]any)
	if len(details) != 1 || details[0].(map[string]any)["code"] != warnFibrePercentagesNot100 || details[0].(map[string]any)["pointer"] != "/materials/0" {
		t.Fatalf("warning_details %v", res)
	}
	if ws, _ := res["warnings"].([]any); len(ws) != 1 || !strings.HasPrefix(ws[0].(string), warnFibrePercentagesNot100) {
		t.Fatalf("warnings %v", res["warnings"])
	}

	rec := ing.must(200, "GET", "/obx/v1/ingest/products/LG-1", nil)
	data := rec["data"].(map[string]any)
	// Terms: known codes get the vocabulary's names whatever was sent, the
	// 0.1 key's qualifier comes first, repeats are dropped, unknown codes keep
	// what was sent (without empty languages), a missing name is null and
	// no qualifiers are [].
	asJSON := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	ms := data["materials"].([]any)[0].(map[string]any)
	if got := asJSON(ms["part"]); got != `{"code":"shell","name":{"de":"Schale","en":"Shell","fr":"Coque","nl":"Schaal"}}` {
		t.Errorf("part %s", got)
	}
	fib := ms["fibers"].([]any)
	for i, want := range []string{
		`{"material":{"code":"polyester","name":{"de":"Polyester","en":"Polyester","fr":"Polyester","nl":"Polyester"}},"percentage":60,"qualifiers":[{"code":"recycled","name":{"de":"Recycelt","en":"Recycled","fr":"Recyclé","nl":"Gerecycled"}},{"code":"post_consumer","name":{"de":"Post-Consumer","en":"Post-consumer","fr":"Post-consommation","nl":"Post-consumer"}}]}`,
		`{"material":{"code":"polyamide","name":{"de":"Polyamid","en":"Polyamide","fr":"Polyamide","nl":"Polyamide"}},"percentage":30,"qualifiers":[]}`,
		`{"material":{"code":"x_fibre","name":{"en":"X fibre"}},"percentage":5,"qualifiers":[{"code":"upcycled","name":{"en":"Upcycled"}}]}`,
	} {
		if got := asJSON(fib[i]); got != want {
			t.Errorf("fibre %d\n%s\nwant\n%s", i, got, want)
		}
	}
	for i, want := range []string{
		`{"category":"washing","code":"wash_30","name":{"de":"Waschen bei 30 °C","en":"Wash at 30 °C","fr":"Lavage à 30 °C","nl":"Wassen op 30 °C"},"symbol":"1_D_30","text":{"en":"Wash inside out"}}`,
		`{"category":"ironing","code":"steam_only","name":{"en":"Steam only"},"symbol":"4_Z","text":null}`,
		`{"category":null,"code":"air_out","name":null,"symbol":null,"text":null}`,
	} {
		if got := asJSON(data["care"].([]any)[i]); got != want {
			t.Errorf("care %d\n%s\nwant\n%s", i, got, want)
		}
	}
	if f := data["features"].(map[string]any); f["wheels"] != float64(4) || f["handle_type"] != "telescopic" || len(f) != 3 {
		t.Fatalf("features %v", f)
	}
	if c := data["certifications"].([]any)[0].(map[string]any); c["licence_number"] != nil || c["scope"] != "material" {
		t.Fatalf("certifications %v", c)
	}

	// The record written back is unchanged, and still reports its warnings.
	back := map[string]any{}
	for k, v := range data {
		back[k] = v
	}
	res = ing.must(200, "PUT", "/obx/v1/ingest/products/LG-1", back)
	if res["result"] != "unchanged" || len(res["warning_details"].([]any)) != 1 {
		t.Fatalf("round trip %v", res)
	}

	// "qualifiers" may be left out (none); the single "qualifier" of the
	// first 0.3 draft is read as a list of one.
	for _, c := range []struct {
		fibre map[string]any
		want  string
	}{
		{map[string]any{"material": map[string]any{"code": "cotton"}, "percentage": 100}, `[]`},
		{map[string]any{"material": map[string]any{"code": "cotton"}, "percentage": 100, "qualifier": nil}, `[]`},
		{map[string]any{"material": map[string]any{"code": "cotton"}, "percentage": 100, "qualifier": map[string]any{"code": "organic"}},
			`[{"code":"organic","name":{"de":"Bio","en":"Organic","fr":"Biologique","nl":"Biologisch"}}]`},
		{map[string]any{"material": map[string]any{"code": "cotton"}, "percentage": 100, "qualifier": map[string]any{"code": "organic"},
			"qualifiers": []any{map[string]any{"code": "in_conversion"}}},
			`[{"code":"in_conversion","name":{"de":"In Umstellung","en":"In conversion","fr":"En conversion","nl":"In omschakeling"}}]`},
	} {
		doc := map[string]any{"brand_external_id": brand, "style_number": "Q-1", "name": map[string]any{"en": "Q"},
			"materials": []any{map[string]any{"part": map[string]any{"code": "main"}, "fibers": []any{c.fibre}}}}
		ing.do("PUT", "/obx/v1/ingest/products/Q-1", doc)
		rec := ing.must(200, "GET", "/obx/v1/ingest/products/Q-1", nil)["data"].(map[string]any)
		f := rec["materials"].([]any)[0].(map[string]any)["fibers"].([]any)[0].(map[string]any)
		if got := asJSON(f["qualifiers"]); got != c.want {
			t.Errorf("%v: qualifiers %s, want %s", c.fibre, got, c.want)
		}
		if _, ok := f["qualifier"]; ok {
			t.Errorf("%v: stored a qualifier property", c.fibre)
		}
	}
	ing.must(200, "DELETE", "/obx/v1/ingest/products/Q-1", nil)

	// Registry values outside their type are refused; empty objects are null.
	for name, bad := range map[string]map[string]any{
		"qualifiers null": {"materials": []any{map[string]any{"part": map[string]any{"code": "main"}, "fibers": []any{
			map[string]any{"material": map[string]any{"code": "cotton"}, "percentage": 100, "qualifiers": nil}}}}},
		"wheels 3":   {"features": map[string]any{"wheels": 3}},
		"closure":    {"features": map[string]any{"closure": "velcro"}},
		"duplicates": {"features": map[string]any{"detachable_parts": []any{"handle", "handle"}}},
		"footwear":   {"footwear_materials": map[string]any{"upper": "plastic"}},
	} {
		doc := map[string]any{}
		for k, v := range product {
			doc[k] = v
		}
		for k, v := range bad {
			doc[k] = v
		}
		r := ing.do("PUT", "/obx/v1/ingest/products/LG-1", doc)
		if r.Status != 422 || r.JSON(t)["obx_error"] != "validation_failed" {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	ing.must(200, "PATCH", "/obx/v1/ingest/products/LG-1", map[string]any{"features": map[string]any{"wheels": nil, "shell": nil, "handle_type": nil}}, "Content-Type", "application/merge-patch+json")
	if f := ing.must(200, "GET", "/obx/v1/ingest/products/LG-1", nil)["data"].(map[string]any)["features"]; f != nil {
		t.Fatalf("features after removing every key: %v", f)
	}

	variant := map[string]any{"product_external_id": "LG-1", "gtin": "2000100801017", "size": "CABIN",
		"color": map[string]any{"code": "BLK", "family": "black"}, "measurements": map[string]any{"height_cm": 55, "weight_kg": 2.6, "strap_length_cm": map[string]any{"min": 80, "max": 80}}}
	ing.must(201, "PUT", "/obx/v1/ingest/variants/2000100801017", variant)
	variant["measurements"] = map[string]any{"height_cm": -1}
	if r := ing.do("PUT", "/obx/v1/ingest/variants/2000100801017", variant); r.Status != 422 {
		t.Fatalf("negative measurement: %d %s", r.Status, r.Body)
	}
	variant["measurements"] = map[string]any{"strap_length_cm": map[string]any{"min": 130, "max": 80}}
	if r := ing.do("PUT", "/obx/v1/ingest/variants/2000100801017", variant); r.Status != 422 || r.JSON(t)["obx_error"] != "validation_failed" {
		t.Fatalf("strap min > max: %d %s", r.Status, r.Body)
	}
	variant["color"] = map[string]any{"code": "BLK", "family": "navy"}
	variant["measurements"] = map[string]any{}
	if r := ing.do("PUT", "/obx/v1/ingest/variants/2000100801017", variant); r.Status != 422 {
		t.Fatalf("colour family navy: %d %s", r.Status, r.Body)
	}
	variant["color"] = map[string]any{"code": "BLK"}
	ing.must(200, "PUT", "/obx/v1/ingest/variants/2000100801017", variant)

	p := retail.must(200, "GET", "/obx/v1/products?limit=10", nil)["items"].([]any)[0].(map[string]any)
	validate(t, "ProductDetail", p)
	if asJSON(p["materials"]) != asJSON(data["materials"]) || asJSON(p["care"]) != asJSON(data["care"]) {
		t.Fatalf("protocol materials/care differ from the record:\n%s\n%s", asJSON(p["materials"]), asJSON(p["care"]))
	}
	for _, f := range []string{"item_subtype", "contains_animal_origin", "footwear_materials", "certifications", "features"} {
		if _, ok := p[f]; !ok {
			t.Errorf("product lacks %s", f)
		}
	}
	if p["features"] != nil || p["contains_animal_origin"] != nil || p["item_subtype"] != "cabin" {
		t.Fatalf("product %v", p)
	}
	v := p["variants"].([]any)[0].(map[string]any)
	if v["measurements"] != nil || v["size_length"] != nil || v["color"].(map[string]any)["family"] != nil {
		t.Fatalf("variant %v", v)
	}
	if _, ok := v["color"].(map[string]any)["family"]; !ok {
		t.Fatal("color.family is absent instead of null")
	}
}

// downgradeProduct turns a stored 0.3 product (data or rep) into what 0.1
// stored: materials as {part, fibers: [{fiber, percentage}]} with the 0.1
// key recycled_<fibre>, care as {code, text}, and without the 0.2 article
// data properties that are null. With v02 it is the 0.2 shape instead (flags, the 0.2
// properties kept).
func downgradeProduct(t *testing.T, s string, v02 bool) string {
	t.Helper()
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if ms, ok := m["materials"].([]any); ok {
		for _, p := range ms {
			pm := p.(map[string]any)
			pm["part"] = pm["part"].(map[string]any)["code"]
			for _, f := range pm["fibers"].([]any) {
				fm := f.(map[string]any)
				code := fm["material"].(map[string]any)["code"].(string)
				var q map[string]any
				if qs, _ := fm["qualifiers"].([]any); len(qs) > 0 {
					q = qs[0].(map[string]any)
				}
				delete(fm, "material")
				delete(fm, "qualifiers")
				switch {
				case q != nil && !v02:
					fm["fiber"] = q["code"].(string) + "_" + code
				case q != nil:
					fm["fiber"], fm[q["code"].(string)] = code, true
				default:
					fm["fiber"] = code
				}
			}
		}
	}
	if cs, ok := m["care"].([]any); ok {
		for _, c := range cs {
			cm := c.(map[string]any)
			delete(cm, "category")
			delete(cm, "symbol")
			delete(cm, "name")
		}
	}
	if !v02 {
		for _, f := range []string{"item_subtype", "contains_animal_origin", "footwear_materials", "certifications", "features"} {
			if m[f] == nil {
				delete(m, f)
			}
		}
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// TestUpgradeStoredProducts starts from products stored by 0.1 (KT-2001 with
// the fibre key recycled_polyester, no article data properties) and 0.2
// (KO-1008 with the flag recycled) and checks the one-time rewrites of
// migrations 3 and 4: the stored objects get the 0.3 shape, exactly as a
// fresh import gives it; the rewritten products get a change entry, the
// untouched ones none.
func TestUpgradeStoredProducts(t *testing.T) {
	k := newKestrelNode(t)
	ctx := context.Background()
	_, cursor, _ := drainChanges(t, k.all, "", 500)
	old := map[string]string{}
	for ext, v02 := range map[string]bool{"KT-2001": false, "KO-1008": true} {
		var data, rep string
		if err := k.app.DB.QueryRow(ctx, `SELECT data, rep FROM products WHERE external_id = ?`, ext).Scan(&data, &rep); err != nil {
			t.Fatal(err)
		}
		old[ext] = rep
		data, rep = downgradeProduct(t, data, v02), downgradeProduct(t, rep, v02)
		if strings.Contains(data, `"material"`) || strings.Contains(rep, `"category"`) {
			t.Fatalf("%s is not in the old shape: %s", ext, data)
		}
		if err := k.app.DB.WriteTx(ctx, func(tx *db.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE products SET data = ?, rep = ?, rep_hash = ? WHERE external_id = ?`, data, rep, hashHex([]byte(rep)), ext)
			if err == nil {
				err = setMeta(ctx, tx, "upgrade_article_data", "0.2")
			}
			if err == nil {
				err = setMeta(ctx, tx, "upgrade_terms", "0.3")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(downgradeProduct(t, old["KT-2001"], false), `"fiber":"recycled_polyester"`) ||
		!strings.Contains(downgradeProduct(t, old["KO-1008"], true), `"fiber":"polyester","percentage":100,"recycled":true`) {
		t.Fatal("the fixture products lack the recycled/organic fibres this test needs")
	}
	if err := k.app.upgradeTerms(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.app.upgradeArticleData(ctx); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := drainChanges(t, k.all, cursor, 100)
	got := map[string]bool{}
	for _, c := range ch {
		got[c["data"].(map[string]any)["external_id"].(string)] = true
	}
	if len(ch) != 2 || !got["KT-2001"] || !got["KO-1008"] {
		t.Fatalf("changes after the upgrade: %v", ch)
	}
	for ext, rep := range old {
		var now string
		_ = k.app.DB.QueryRow(ctx, `SELECT rep FROM products WHERE external_id = ?`, ext).Scan(&now)
		if now != rep {
			t.Errorf("%s rep after the upgrade\n%s\nwant\n%s", ext, now, rep)
		}
	}
	for _, key := range []string{"upgrade_article_data", "upgrade_terms"} {
		if v, _ := getMeta(ctx, k.app.DB, key); v != "" {
			t.Fatalf("the upgrade marker %s is still there", key)
		}
	}
	// Writing the same data again is unchanged.
	run := k.runImport(k.admin, k.kestrelFiles(), nil)
	if c := run["counts"].(map[string]any); c["updated"].(float64) != 0 {
		t.Fatalf("import after the upgrade: %v", c)
	}
}

// TestUpgradeFromSingleQualifier: products stored by the first 0.3 draft
// (one "qualifier" per fibre, null when none) get "qualifiers" from
// migration 5 and the one-time rewrite: the same shape as a fresh import, an
// upsert in /changes, and their codes in the portal's qualifier facet.
func TestUpgradeFromSingleQualifier(t *testing.T) {
	k := newKestrelNode(t)
	ctx := context.Background()
	_, cursor, _ := drainChanges(t, k.all, "", 500)
	draft := func(s string) string {
		var m map[string]any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		for _, p := range m["materials"].([]any) {
			for _, f := range p.(map[string]any)["fibers"].([]any) {
				fm := f.(map[string]any)
				var q any
				if qs := fm["qualifiers"].([]any); len(qs) > 0 {
					q = qs[0]
				}
				fm["qualifier"] = q
				delete(fm, "qualifiers")
			}
		}
		b, _ := json.Marshal(m)
		return string(b)
	}
	old := map[string]string{}
	if err := k.app.DB.WriteTx(ctx, func(tx *db.Tx) error {
		for _, ext := range []string{"KO-1008", "KO-1001"} { // one qualifier; none
			var data, rep string
			if err := tx.QueryRow(ctx, `SELECT data, rep FROM products WHERE external_id = ?`, ext).Scan(&data, &rep); err != nil {
				return err
			}
			old[ext] = rep
			data, rep = draft(data), draft(rep)
			if !strings.Contains(data, `"qualifier":`) {
				return fmt.Errorf("%s not in the draft shape: %s", ext, data)
			}
			if _, err := tx.Exec(ctx, `UPDATE products SET data = ?, rep = ?, rep_hash = ? WHERE external_id = ?`, data, rep, hashHex([]byte(rep)), ext); err != nil {
				return err
			}
		}
		// The schema of the first 0.3 draft: migration 5 not applied.
		for _, q := range []string{`DELETE FROM schema_migrations WHERE version = 5`, `DROP TABLE product_qualifiers`} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := k.app.DB.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := k.app.upgradeTerms(ctx); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := drainChanges(t, k.all, cursor, 100)
	got := map[string]bool{}
	for _, c := range ch {
		got[c["data"].(map[string]any)["external_id"].(string)] = true
	}
	if len(ch) != 2 || !got["KO-1008"] || !got["KO-1001"] {
		t.Fatalf("changes after the upgrade: %v", ch)
	}
	for ext, rep := range old {
		var now string
		_ = k.app.DB.QueryRow(ctx, `SELECT rep FROM products WHERE external_id = ?`, ext).Scan(&now)
		if now != rep {
			t.Errorf("%s rep after the upgrade\n%s\nwant\n%s", ext, now, rep)
		}
	}
	facets := k.admin.must(200, "GET", "/admin/api/portal/facets", nil)["qualifiers"].([]any)
	counts := map[string]float64{}
	for _, f := range facets {
		fm := f.(map[string]any)
		counts[fm["value"].(string)] = fm["count"].(float64)
	}
	// recycled: KO-1008, KO-1009, KT-2001, KT-2002 (admins see both brands).
	if counts["recycled"] != 4 || counts["post_consumer"] != 1 || counts["organic"] != 1 {
		t.Fatalf("qualifier facet after the upgrade: %v", facets)
	}
	run := k.runImport(k.admin, k.kestrelFiles(), nil)
	if c := run["counts"].(map[string]any); c["updated"].(float64) != 0 {
		t.Fatalf("import after the upgrade: %v", c)
	}
}
