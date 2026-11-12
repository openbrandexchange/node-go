package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/openbrandexchange/node-go/internal/importfmt"
)

// Excel workbooks (import-format.md §5): the fixtures of the spec's
// fixtures/imports/xlsx/ imported through the node, as conformance I-IMP-9
// and I-IMP-10 do.

func xlsxFixture(t *testing.T, name string) string {
	return filepath.Join(specDir(t), "imports", "xlsx", name)
}

type xlsxCase struct {
	File         string           `json:"file"`
	Role         string           `json:"role"`
	Errors       []map[string]any `json:"errors"`
	Warnings     []map[string]any `json:"warnings"`
	ImportErrors []map[string]any `json:"import_errors"`
	Error        map[string]any   `json:"error"`
	Send         struct {
		ImagesManifest []string `json:"images_manifest"`
	} `json:"send"`
}

func xlsxCases(t *testing.T) []xlsxCase {
	var exp struct {
		Cases []xlsxCase `json:"cases"`
	}
	raw, _ := json.Marshal(readFixture(t, "imports/xlsx/expected.json"))
	if err := json.Unmarshal(raw, &exp); err != nil || len(exp.Cases) == 0 {
		t.Fatalf("expected.json: %v", err)
	}
	return exp.Cases
}

func (n *testNode) importItems(c *client, runID string) []map[string]any {
	var out []map[string]any
	for _, e := range c.must(200, "GET", "/admin/api/imports/"+runID+"/errors?limit=200", nil)["items"].([]any) {
		validate(n.t, "ImportRowError", e)
		out = append(out, e.(map[string]any))
	}
	return out
}

// rowErrorMatches compares an ImportRowError with an expected.json entry
// (detail_prefix: the detail starts with it).
func rowErrorMatches(got, want map[string]any) bool {
	for k, v := range want {
		switch k {
		case "detail_prefix":
			d, _ := got["detail"].(string)
			if !strings.HasPrefix(d, v.(string)) {
				return false
			}
		case "detail_contains":
		default:
			if fmt.Sprint(got[k]) != fmt.Sprint(v) {
				return false
			}
		}
	}
	return true
}

func sortRowErrors(errs []map[string]any) {
	key := func(e map[string]any) (float64, string) {
		r, _ := e["row"].(float64)
		c, _ := e["column"].(string)
		return r, c
	}
	sort.SliceStable(errs, func(i, j int) bool {
		ri, ci := key(errs[i])
		rj, cj := key(errs[j])
		if ri != rj {
			return ri < rj
		}
		return ci < cj
	})
}

func TestWorkbookImports(t *testing.T) {
	k := newKestrelNode(t)
	counts := func(run map[string]any) map[string]any { return run["counts"].(map[string]any) }
	_, cursor, _ := drainChanges(t, k.all, "", 500)
	noChanges := func(t *testing.T, what string) {
		t.Helper()
		var ch []map[string]any
		ch, cursor, _ = drainChanges(t, k.all, cursor, 100)
		if len(ch) != 0 {
			t.Errorf("%s wrote %d change entries: %v", what, len(ch), ch)
		}
	}
	kestrel := filepath.Join(specDir(t), "kestrel")
	images, _ := filepath.Glob(filepath.Join(kestrel, "images", "*.png"))

	for _, c := range xlsxCases(t) {
		t.Run(c.File, func(t *testing.T) {
			// As expected.json says: the workbook alone, as products.xlsx or
			// assets.xlsx, plus the images_manifest of send.
			part := "csv"
			if c.Role == "assets" {
				part = "assets_csv"
			}
			fields := map[string]string{"options": `{"dry_run":true}`}
			if m := c.Send.ImagesManifest; len(m) > 0 {
				var manifest []map[string]any
				for _, f := range m {
					data, err := os.ReadFile(filepath.Join(kestrel, "images", f))
					if err != nil {
						t.Fatal(err)
					}
					sum := sha256.Sum256(data)
					manifest = append(manifest, map[string]any{"file": f, "sha256": hex.EncodeToString(sum[:])})
				}
				fields["images_manifest"] = string(mustJSON(manifest))
			}
			body, ct := multipartBody(t, map[string][]string{part: {xlsxFixture(t, c.File)}}, fields)
			started := k.admin.must(202, "POST", "/admin/api/imports", body, "Content-Type", ct)
			k.app.RunJobsNow(t.Context())
			run := k.admin.must(200, "GET", "/admin/api/imports/"+started["id"].(string), nil)
			validate(t, "ImportRun", run)
			items := k.importItems(k.admin, run["id"].(string))
			var errs, warns []map[string]any
			for _, e := range items {
				if e["severity"] == "error" {
					errs = append(errs, e)
				} else {
					warns = append(warns, e)
				}
			}
			if c.Error != nil {
				// I-IMP-10: one ImportRowError with row null and the code; nothing written.
				if len(errs) != 1 || !rowErrorMatches(errs[0], c.Error) || errs[0]["row"] != nil || errs[0]["column"] != nil {
					t.Fatalf("want one %s, got %v", c.Error["obx_error"], errs)
				}
				if run["status"] != "failed" || run["error"].(map[string]any)["obx_error"] != c.Error["obx_error"] || run["error"].(map[string]any)["status"].(float64) != 422 {
					t.Errorf("run %v", run)
				}
				if cc := counts(run); cc["created"].(float64)+cc["updated"].(float64)+cc["unchanged"].(float64) != 0 || cc["errors"].(float64) != 1 {
					t.Errorf("a refused workbook was imported: %v", cc)
				}
				if prev, _ := run["errors_preview"].([]any); len(prev) != 1 || !rowErrorMatches(prev[0].(map[string]any), c.Error) {
					t.Errorf("errors_preview %v", run["errors_preview"])
				}
				return
			}
			want := c.ImportErrors
			if want == nil {
				t.Fatal("every converting case has import_errors")
			}
			sortRowErrors(errs)
			if run["status"] != "succeeded" || len(errs) != len(want) {
				t.Fatalf("errors %v, want %v (%v)", errs, want, run)
			}
			for i := range want {
				if !rowErrorMatches(errs[i], want[i]) {
					t.Errorf("error %d: %v, want %v", i, errs[i], want[i])
				}
			}
			for _, w := range c.Warnings {
				found := false
				for _, g := range warns {
					found = found || rowErrorMatches(g, w)
				}
				if !found {
					t.Errorf("warning %v missing in %v", w, warns)
				}
			}
			if c.File == "products-template.xlsx" || c.File == "assets-template.xlsx" {
				if cc := counts(run); cc["created"].(float64) != 0 || cc["updated"].(float64) != 0 || cc["unchanged"].(float64) == 0 {
					t.Errorf("the template should be all unchanged on a node with the fixtures: %v", cc)
				}
			}
		})
	}
	noChanges(t, "dry runs")

	t.Run("the template without dry_run is all unchanged, as products.csv", func(t *testing.T) {
		x := counts(k.runImport(k.admin, map[string][]string{"csv": {xlsxFixture(t, "products-template.xlsx")}}, nil))
		c := counts(k.runImport(k.admin, map[string][]string{"csv": {filepath.Join(kestrel, "products.csv")}}, nil))
		for _, f := range []string{"rows", "created", "updated", "unchanged", "errors", "warnings"} {
			if x[f] != c[f] {
				t.Errorf("counts %s: workbook %v, csv %v", f, x[f], c[f])
			}
		}
		if x["created"].(float64) != 0 || x["updated"].(float64) != 0 || x["unchanged"].(float64) != 11+46 {
			t.Errorf("counts %v", x)
		}
		noChanges(t, "an unchanged workbook import")
	})

	t.Run("a package with products.xlsx and the images", func(t *testing.T) {
		tpl, _ := os.ReadFile(xlsxFixture(t, "products-template.xlsx"))
		zf := map[string][]byte{"export/Products.XLSX": tpl}
		for _, p := range images {
			b, _ := os.ReadFile(p)
			zf["photos/"+filepath.Base(p)] = b
		}
		run := k.runImport(k.admin, map[string][]string{"package": {writeTemp(t, "package.zip", zipOf(t, zf))}}, nil)
		if c := counts(run); run["status"] != "succeeded" || c["errors"].(float64) != 0 || c["created"].(float64) != 0 || c["updated"].(float64) != 0 || c["unchanged"].(float64) != 11+46+35 {
			t.Fatalf("%v", run)
		}
		noChanges(t, "an unchanged package")
		csv, _ := os.ReadFile(filepath.Join(kestrel, "products.csv"))
		run = k.runImport(k.admin, map[string][]string{"package": {writeTemp(t, "package.zip", zipOf(t, map[string][]byte{"products.csv": csv, "products.xlsx": tpl}))}}, nil)
		if run["status"] != "failed" || run["error"].(map[string]any)["obx_error"] != "import_invalid" {
			t.Fatalf("products.csv and products.xlsx in one package: %v", run)
		}
	})

	t.Run("products.xlsx with one other CSV: the workbook is products, the CSV is not read", func(t *testing.T) {
		tpl, _ := os.ReadFile(xlsxFixture(t, "products-template.xlsx"))
		run := k.runImport(k.admin, map[string][]string{"package": {writeTemp(t, "package.zip", zipOf(t, map[string][]byte{
			"products.xlsx": tpl, "export.csv": []byte("style_number,name:en\nKX-OTHER,Not read\n")}))}}, map[string]any{"dry_run": true})
		if c := counts(run); run["status"] != "succeeded" || c["errors"].(float64) != 0 || c["unchanged"].(float64) != 11+46 || c["created"].(float64) != 0 {
			t.Fatalf("%v", run)
		}
	})

	t.Run("a refused workbook fails the whole import: nothing of it is written", func(t *testing.T) {
		bad, _ := os.ReadFile(xlsxFixture(t, "macro-enabled.xlsx"))
		zf := map[string][]byte{"products.xlsx": bad, "assets.csv": mustRead(t, filepath.Join(kestrel, "assets.csv"))}
		for _, p := range images {
			zf["photos/"+filepath.Base(p)] = mustRead(t, p)
		}
		ing := k.ingestToken(k.admin)
		body, ct := multipartBody(t, map[string][]string{"package": {writeTemp(t, "package.zip", zipOf(t, zf))}}, nil)
		job := ing.must(202, "POST", "/obx/v1/ingest/imports", body, "Content-Type", ct)
		k.app.RunJobsNow(t.Context())
		job = ing.must(200, "GET", "/obx/v1/ingest/jobs/"+job["id"].(string), nil)
		run := job["import_run"].(map[string]any)
		if job["status"] != "failed" || run["status"] != "failed" || run["error"].(map[string]any)["obx_error"] != "workbook_unsafe" {
			t.Fatalf("%v", job)
		}
		if c := counts(run); c["created"].(float64)+c["updated"].(float64)+c["unchanged"].(float64) != 0 || c["errors"].(float64) != 1 {
			t.Errorf("counts %v", c)
		}
		res := ing.must(200, "GET", "/obx/v1/ingest/jobs/"+job["id"].(string)+"/results?errors_only=true", nil)
		items, _ := res["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("results %v", res)
		}
		e := items[0].(map[string]any)
		if e["file"] != "products.xlsx" || e["row"] != nil || e["column"] != nil || e["obx_error"] != "workbook_unsafe" || e["severity"] != "error" {
			t.Errorf("result %v", e)
		}
		noChanges(t, "a refused workbook with images")
	})

	t.Run("an Ingest API part without a file name is a workbook by its content type", func(t *testing.T) {
		ing := k.ingestToken(k.admin)
		tpl, _ := os.ReadFile(xlsxFixture(t, "products-template.xlsx"))
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("options", `{"dry_run":true}`)
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="csv"`)
		h.Set("Content-Type", importfmt.WorkbookContentType)
		w, _ := mw.CreatePart(h)
		_, _ = w.Write(tpl)
		_ = mw.Close()
		job := ing.must(202, "POST", "/obx/v1/ingest/imports", &buf, "Content-Type", mw.FormDataContentType())
		k.app.RunJobsNow(t.Context())
		job = ing.must(200, "GET", "/obx/v1/ingest/jobs/"+job["id"].(string), nil)
		run := job["import_run"].(map[string]any)
		if job["status"] != "succeeded" || run["counts"].(map[string]any)["unchanged"].(float64) != 11+46 || run["counts"].(map[string]any)["errors"].(float64) != 0 {
			t.Fatalf("%v", job)
		}
	})

	t.Run("other spreadsheet files are unsupported_media_type", func(t *testing.T) {
		body, ct := multipartBody(t, map[string][]string{"csv": {writeTemp(t, "products.xls", []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"))}}, nil)
		if r := k.admin.do("POST", "/admin/api/imports", body, "Content-Type", ct); r.Status != 415 {
			t.Fatalf("an .xls upload: %d", r.Status)
		}
	})

	t.Run("a scheduled https import of a workbook", func(t *testing.T) {
		tpl, _ := os.ReadFile(xlsxFixture(t, "products-template.xlsx"))
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tpl) }))
		defer remote.Close()
		src := k.admin.must(201, "POST", "/admin/api/import-sources", map[string]any{"name": "Workbook", "kind": "https_url",
			"schedule": map[string]any{"frequency": "manual"}, "config": map[string]any{"url": remote.URL + "/exports/catalogue.xlsx"}})
		run := k.admin.must(202, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/run", nil)
		k.app.RunJobsNow(t.Context())
		done := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil)
		if c := counts(done); done["status"] != "succeeded" || c["unchanged"].(float64) != 11+46 || c["errors"].(float64) != 0 {
			t.Fatalf("%v", done)
		}
		noChanges(t, "an unchanged scheduled workbook")
	})
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A workbook imported into an empty node writes the same catalog as
// products.csv, and its products appear in /changes.
func TestWorkbookImportEndToEnd(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	all, _ := n.apiClient(admin, "All Brands Store", map[string]any{})
	run := n.runImport(admin, map[string][]string{"csv": {xlsxFixture(t, "products-template.xlsx")}}, nil)
	c := run["counts"].(map[string]any)
	// 11 products, 46 variants and the brand Kestrel Trail.
	if run["status"] != "succeeded" || c["errors"].(float64) != 0 || c["created"].(float64) != 11+46+1 || c["rows"].(float64) != 46 {
		t.Fatalf("%v", run)
	}
	ch, cursor, _ := drainChanges(t, all, "", 500)
	seen := map[string]bool{}
	for _, e := range ch {
		if e["type"] == "product" && e["op"] == "upsert" {
			seen[e["data"].(map[string]any)["external_id"].(string)] = true
		}
	}
	for _, ext := range []string{"KO-1001", "KO-1005", "KT-2001"} {
		if !seen[ext] {
			t.Errorf("%s from the workbook is not in /changes", ext)
		}
	}
	csvRun := n.runImport(admin, map[string][]string{"csv": {filepath.Join(specDir(t), "kestrel", "products.csv")}}, nil)
	if c := csvRun["counts"].(map[string]any); c["created"].(float64) != 0 || c["updated"].(float64) != 0 || c["unchanged"].(float64) != 11+46 {
		t.Fatalf("products.csv after the workbook is not all unchanged: %v", c)
	}
	if more, _, _ := drainChanges(t, all, cursor, 100); len(more) != 0 {
		t.Fatalf("products.csv after the workbook wrote %d change entries", len(more))
	}
}
