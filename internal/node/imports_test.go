package node

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbrandexchange/node-go/internal/secure"
)

func zipOf(t *testing.T, files map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (n *testNode) runImport(c *client, files map[string][]string, options map[string]any) map[string]any {
	fields := map[string]string{}
	if options != nil {
		fields["options"] = string(mustJSON(options))
	}
	body, ct := multipartBody(n.t, files, fields)
	run := c.must(202, "POST", "/admin/api/imports", body, "Content-Type", ct)
	n.app.RunJobsNow(n.t.Context())
	out := c.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil)
	validate(n.t, "ImportRun", out)
	return out
}

func TestImports(t *testing.T) {
	k := newKestrelNode(t)
	counts := func(run map[string]any) map[string]any { return run["counts"].(map[string]any) }

	t.Run("second import is unchanged and writes no entries", func(t *testing.T) {
		_, cursor, _ := drainChanges(t, k.all, "", 500)
		run := k.runImport(k.admin, k.kestrelFiles(), nil)
		c := counts(run)
		if c["created"].(float64) != 0 || c["updated"].(float64) != 0 || c["unchanged"].(float64) != 11+46+35 {
			t.Fatalf("second import counts %v", c)
		}
		ch, _, _ := drainChanges(t, k.all, cursor, 100)
		if len(ch) != 0 {
			t.Fatalf("unchanged import wrote %d entries", len(ch))
		}
	})

	t.Run("an import without assets.csv keeps what the images have (import rule 8)", func(t *testing.T) {
		k.admin.must(200, "PATCH", "/admin/api/catalog/assets/KO-1001_NVY_1", map[string]any{"rights": map[string]any{"credit": "Photo: Test Studio"}})
		defer k.admin.must(200, "PATCH", "/admin/api/catalog/assets/KO-1001_NVY_1", map[string]any{"rights": map[string]any{"credit": nil}})
		files := k.kestrelFiles()
		delete(files, "assets_csv")
		c := counts(k.runImport(k.admin, files, nil))
		if c["created"].(float64) != 0 || c["updated"].(float64) != 0 || c["errors"].(float64) != 0 {
			t.Fatalf("import without assets.csv changed images: %v", c)
		}
		data := func(product, asset string) map[string]any {
			rec := k.admin.must(200, "GET", "/admin/api/catalog/products/"+product, nil)
			for _, a := range rec["assets"].([]any) {
				rec := a.(map[string]any)["record"].(map[string]any)
				if rec["external_id"] == asset {
					return rec["data"].(map[string]any)
				}
			}
			t.Fatalf("%s has no image %s", product, asset)
			return nil
		}
		d := data("KO-1002", "KO-1002_BLK_2")
		rights := d["rights"].(map[string]any)
		if e, _ := rights["embargo_until"].(string); !strings.HasPrefix(e, "2030-01-01") || rights["credit"] != "Photo: Kestrel Studio" {
			t.Errorf("KO-1002_BLK_2 lost its embargo or credit: %v", rights)
		}
		d = data("KO-1003", "KO-1003_RST_3")
		if d["provenance"].(map[string]any)["ai_generated"] != true || d["alt_text"].(map[string]any)["nl"] == nil {
			t.Errorf("KO-1003_RST_3 lost its AI label or alt text: %v %v", d["provenance"], d["alt_text"])
		}
		if credit := data("KO-1001", "KO-1001_NVY_1")["rights"].(map[string]any)["credit"]; credit != "Photo: Test Studio" {
			t.Errorf("KO-1001_NVY_1 lost the credit set in the admin: %v", credit)
		}
	})

	t.Run("a column the file does not have keeps the field; an empty cell removes it (import rule 9)", func(t *testing.T) {
		csv := "brand,style_number,name:en\nKestrel,KO-1001,Merino Crew Sweater\n"
		c := counts(k.runImport(k.admin, map[string][]string{"csv": {writeTemp(t, "products.csv", []byte(csv))}}, nil))
		if c["updated"].(float64) != 0 || c["unchanged"].(float64) != 1 {
			t.Fatalf("a file with only the name changed the product: %v", c)
		}
		rec := k.admin.must(200, "GET", "/admin/api/catalog/products/KO-1001", nil)["record"].(map[string]any)["data"].(map[string]any)
		if rec["name"].(map[string]any)["nl"] != "Merino trui met ronde hals" || rec["description"] == nil || rec["materials"] == nil || rec["care"] == nil {
			t.Fatalf("KO-1001 lost fields whose columns the file did not have: %v", rec)
		}
		csv = "brand,style_number,name:en,season\nKestrel,KO-1001,Merino Crew Sweater,\n"
		c = counts(k.runImport(k.admin, map[string][]string{"csv": {writeTemp(t, "products.csv", []byte(csv))}}, nil))
		if c["updated"].(float64) != 1 {
			t.Fatalf("an empty season cell: %v", c)
		}
		rec = k.admin.must(200, "GET", "/admin/api/catalog/products/KO-1001", nil)["record"].(map[string]any)["data"].(map[string]any)
		if rec["season"] != nil || rec["collection"] == nil {
			t.Fatalf("the empty cell must remove the season and keep the rest: %v", rec)
		}
		// Back to the fixture for the other subtests.
		k.runImport(k.admin, k.kestrelFiles(), nil)
	})

	t.Run("package zip and images manifest give the same result", func(t *testing.T) {
		dir := filepath.Join(specDir(t), "kestrel")
		files := map[string][]byte{}
		files["products.csv"], _ = os.ReadFile(filepath.Join(dir, "products.csv"))
		files["assets.csv"], _ = os.ReadFile(filepath.Join(dir, "assets.csv"))
		imgs, _ := filepath.Glob(filepath.Join(dir, "images", "*.png"))
		var manifest []map[string]any
		for _, p := range imgs {
			data, _ := os.ReadFile(p)
			files["images/"+filepath.Base(p)] = data
			sum := sha256.Sum256(data)
			manifest = append(manifest, map[string]any{"file": filepath.Base(p), "sha256": hex.EncodeToString(sum[:])})
		}
		pkg := writeTemp(t, "package.zip", zipOf(t, files))
		c := counts(k.runImport(k.admin, map[string][]string{"package": {pkg}}, nil))
		if c["unchanged"].(float64) != 92 || c["errors"].(float64) != 0 {
			t.Fatalf("package import %v", c)
		}
		fields := map[string]string{"images_manifest": string(mustJSON(manifest))}
		body, ct := multipartBody(t, map[string][]string{"csv": {filepath.Join(dir, "products.csv")}, "assets_csv": {filepath.Join(dir, "assets.csv")}}, fields)
		run := k.admin.must(202, "POST", "/admin/api/imports", body, "Content-Type", ct)
		k.app.RunJobsNow(t.Context())
		c = counts(k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil))
		if c["unchanged"].(float64) != 92 || c["errors"].(float64) != 0 {
			t.Fatalf("manifest import %v", c)
		}
	})

	t.Run("zip slip is refused and nothing is written", func(t *testing.T) {
		pkg := writeTemp(t, "evil.zip", zipOf(t, map[string][]byte{"products.csv": []byte("style_number,name\nEVIL-1,Evil\n"), "../evil.jpg": []byte("x")}))
		run := k.runImport(k.admin, map[string][]string{"package": {pkg}}, nil)
		if run["status"] != "failed" || run["error"].(map[string]any)["obx_error"] != "import_invalid" {
			t.Fatalf("%v", run)
		}
		if r := k.admin.do("GET", "/admin/api/catalog/products/EVIL-1", nil); r.Status != 404 {
			t.Fatal("zip-slip import wrote a product")
		}
	})

	t.Run("mirror import", func(t *testing.T) {
		all, _ := os.ReadFile(filepath.Join(specDir(t), "kestrel", "products.csv"))
		lines := strings.Split(strings.TrimRight(string(all), "\n"), "\n")
		var missingOne, onlyOne []string
		for i, l := range lines {
			if i == 0 {
				missingOne, onlyOne = append(missingOne, l), append(onlyOne, l)
				continue
			}
			if !strings.Contains(l, ",KO-1006,") {
				missingOne = append(missingOne, l)
			}
			if strings.Contains(l, ",KO-1001,") && !strings.HasPrefix(l, "Kestrel Trail") {
				onlyOne = append(onlyOne, l)
			}
		}
		refused := k.runImport(k.admin, map[string][]string{"csv": {writeTemp(t, "products.csv", []byte(strings.Join(onlyOne, "\n")))}}, map[string]any{"mode": "mirror"})
		if refused["status"] != "failed" || refused["error"].(map[string]any)["obx_error"] != "mass_unpublish_refused" {
			t.Fatalf("%v", refused)
		}
		ok := k.runImport(k.admin, map[string][]string{"csv": {writeTemp(t, "products.csv", []byte(strings.Join(missingOne, "\n")))}}, map[string]any{"mode": "mirror"})
		if counts(ok)["unpublished"].(float64) != 1 {
			t.Fatalf("mirror %v", ok)
		}
		rec := k.admin.must(200, "GET", "/admin/api/catalog/products/KO-1006", nil)
		if rec["record"].(map[string]any)["status"] != "unpublished" {
			t.Fatalf("KO-1006 %v", rec["record"])
		}
	})

	t.Run("row errors carry file, row, column and code", func(t *testing.T) {
		csv := "brand,style_number,name:en,gtin,color_code,materials\n" +
			"Kestrel,ERR-1,Row error test,2.00010010102E+12,BLK,\n" +
			"Kestrel,ERR-2,Bad check digit,2000100101019,BLK,\n" +
			"Kestrel,ERR-3,Bad materials,,,lots of cotton\n" +
			"Kestrel,,No style,,,\n"
		run := k.runImport(k.admin, map[string][]string{"csv": {writeTemp(t, "products.csv", []byte(csv))}}, nil)
		if run["status"] != "succeeded" {
			t.Fatalf("%v", run)
		}
		errs := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)
		codes := map[string]bool{}
		for _, e := range errs["items"].([]any) {
			em := e.(map[string]any)
			validate(t, "ImportRowError", em)
			if em["file"] != "products.csv" || em["row"] == nil {
				t.Errorf("%v", em)
			}
			codes[em["obx_error"].(string)] = true
		}
		if !codes["invalid_gtin"] || !codes["validation_failed"] {
			t.Fatalf("codes %v", codes)
		}
	})

	t.Run("ingest import with idempotency key", func(t *testing.T) {
		ing := k.ingestToken(k.admin)
		body, ct := multipartBody(t, map[string][]string{"csv": {filepath.Join(specDir(t), "kestrel", "products.csv")}}, map[string]string{"options": `{"dry_run":true}`})
		raw, _ := io.ReadAll(body)
		j1 := ing.must(202, "POST", "/obx/v1/ingest/imports", bytes.NewReader(raw), "Content-Type", ct, "Idempotency-Key", "imp-1")
		j2 := ing.must(202, "POST", "/obx/v1/ingest/imports", bytes.NewReader(raw), "Content-Type", ct, "Idempotency-Key", "imp-1")
		if j1["id"] != j2["id"] {
			t.Fatalf("different jobs %v %v", j1["id"], j2["id"])
		}
		validate(t, "IngestJob", j1)
		k.app.RunJobsNow(t.Context())
		j := ing.must(200, "GET", "/obx/v1/ingest/jobs/"+j1["id"].(string), nil)
		validate(t, "IngestJob", j)
		if j["import_run"].(map[string]any)["dry_run"] != true {
			t.Fatalf("%v", j)
		}
	})

	t.Run("scheduled https import", func(t *testing.T) {
		var hits atomic.Int32
		csvBody := "brand,style_number,name:en,color_code,gtin,size\nKestrel,SCH-1,Scheduled Shirt,WHT,2000999900013,M\n"
		remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.Header.Get("Authorization") != "Bearer remote-secret" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(csvBody))
		}))
		defer remote.Close()
		src := k.admin.must(201, "POST", "/admin/api/import-sources", map[string]any{"name": "Nightly CSV", "kind": "https_url",
			"schedule":    map[string]any{"frequency": "hourly"},
			"config":      map[string]any{"url": remote.URL + "/products.csv"},
			"credentials": map[string]any{"headers": map[string]any{"Authorization": "Bearer remote-secret"}}})
		test := k.admin.must(200, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/test", nil)
		// A successful test carries no English message next to the UI's own translated line.
		if test["ok"] != true || test["message"] != nil || len(test["files"].([]any)) != 1 {
			t.Fatalf("test %v", test)
		}
		run := k.admin.must(202, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/run", nil)
		if run["trigger"] != "manual_run" {
			t.Fatalf("%v", run)
		}
		k.app.RunJobsNow(t.Context())
		done := k.admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil)
		if done["status"] != "succeeded" || done["counts"].(map[string]any)["created"].(float64) != 2 {
			t.Fatalf("scheduled run %v", done)
		}
		// Unchanged remote file: the next run skips.
		run2 := k.admin.must(202, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/run", nil)
		k.app.RunJobsNow(t.Context())
		done2 := k.admin.must(200, "GET", "/admin/api/imports/"+run2["id"].(string), nil)
		if done2["x-node-go-unchanged"] != true {
			t.Fatalf("second run should be skipped: %v", done2)
		}
		// The scheduler starts due runs from next_run_at.
		_, _ = k.app.DB.Exec(t.Context(), `UPDATE import_sources SET next_run_at = ? WHERE id = ?`, "2000-01-01T00:00:00.000000Z", src["id"])
		k.app.scheduleDue(t.Context())
		k.app.RunJobsNow(t.Context())
		got := k.admin.must(200, "GET", "/admin/api/import-sources/"+src["id"].(string), nil)
		if got["last_run"].(map[string]any)["trigger"] != "schedule" {
			t.Fatalf("scheduler did not run: %v", got["last_run"])
		}
		if na, _ := time.Parse(time.RFC3339, got["next_run_at"].(string)); !na.After(time.Now()) {
			t.Fatalf("next_run_at %v", got["next_run_at"])
		}
	})
}

func TestExportAndWebhooks(t *testing.T) {
	k := newKestrelNode(t)

	t.Run("export", func(t *testing.T) {
		ex := k.all.must(202, "POST", "/obx/v1/exports", map[string]any{"filter": map[string]any{"brand_ids": []string{k.brands["kestrel"]}}, "renditions": []string{"web"}})
		validate(t, "Export", ex)
		if r := k.all.do("POST", "/obx/v1/exports", map[string]any{"renditions": []string{"web"}}); r.Status != 409 {
			t.Fatalf("concurrent export: %d", r.Status)
		}
		k.app.RunJobsNow(t.Context())
		done := k.all.must(200, "GET", "/obx/v1/exports/"+ex["id"].(string), nil)
		validate(t, "Export", done)
		if done["status"] != "succeeded" || len(done["parts"].([]any)) != 1 {
			t.Fatalf("%v", done)
		}
		man := k.client().do("GET", done["manifest_url"].(string), nil)
		lines := strings.Split(strings.TrimSpace(string(man.Body)), "\n")
		var header map[string]any
		_ = json.Unmarshal([]byte(lines[0]), &header)
		if header["type"] != "header" {
			t.Fatalf("first line %s", lines[0])
		}
		part := done["parts"].([]any)[0].(map[string]any)
		pz := k.client().do("GET", part["url"].(string), nil)
		sum := sha256.Sum256(pz.Body)
		if hex.EncodeToString(sum[:]) != part["sha256"] {
			t.Fatal("part sha256 mismatch")
		}
		z, _ := zip.NewReader(bytes.NewReader(pz.Body), int64(len(pz.Body)))
		entries := map[string]string{}
		for _, f := range z.File {
			rc, _ := f.Open()
			data, _ := io.ReadAll(rc)
			rc.Close()
			s := sha256.Sum256(data)
			entries[f.Name] = hex.EncodeToString(s[:])
		}
		files := 0
		for _, l := range lines {
			var m map[string]any
			_ = json.Unmarshal([]byte(l), &m)
			validate(t, "ExportManifestLine", m)
			if m["type"] == "file" {
				files++
				if entries[m["path"].(string)] != m["sha256"] {
					t.Errorf("file line %v does not match the zip", m)
				}
			}
			if m["type"] == "product" && strings.HasPrefix(m["data"].(map[string]any)["style_number"].(string), "KT-") {
				t.Error("export contains another brand")
			}
		}
		if files != 31 || len(entries) != 31 {
			t.Fatalf("files %d, entries %d", files, len(entries))
		}
		// The changes cursor continues incrementally.
		cur := header["changes_cursor"].(string)
		page := k.all.must(200, "GET", "/obx/v1/changes?cursor="+cur, nil)
		if page["phase"] != "live" {
			t.Fatalf("%v", page)
		}
	})

	t.Run("webhooks are signed", func(t *testing.T) {
		type got struct {
			header http.Header
			body   []byte
		}
		ch := make(chan got, 10)
		recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			ch <- got{r.Header.Clone(), b}
		}))
		defer recv.Close()
		sub := k.all.must(201, "POST", "/obx/v1/webhooks", map[string]any{"url": recv.URL + "/hook", "events": []string{"changes.available"}})
		validate(t, "WebhookSubscription", sub)
		secret := sub["secret"].(string)
		res := k.all.must(200, "POST", "/obx/v1/webhooks/"+sub["id"].(string)+"/test", nil)
		if res["delivered"] != true {
			t.Fatalf("%v", res)
		}
		g := <-ch
		ts := g.header.Get("OBX-Webhook-Timestamp")
		if g.header.Get("OBX-Webhook-Signature") != secure.WebhookSignature(secret, ts, g.body) {
			t.Fatal("bad signature")
		}
		var evt map[string]any
		_ = json.Unmarshal(g.body, &evt)
		validate(t, "WebhookEvent", evt)
		if evt["type"] != "webhook.test" || g.header.Get("OBX-Webhook-Id") != evt["id"] {
			t.Fatalf("%v", evt)
		}
		// A catalog change wakes the subscriber.
		k.admin.must(200, "PATCH", "/admin/api/catalog/products/KO-1002", map[string]any{"season": "AW27"})
		k.app.notifyChanges(t.Context())
		k.app.deliverDue(t.Context())
		select {
		case g := <-ch:
			_ = json.Unmarshal(g.body, &evt)
			if evt["type"] != "changes.available" {
				t.Fatalf("%v", evt)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no changes.available delivery")
		}
		list := k.all.must(200, "GET", "/obx/v1/webhooks", nil)
		if strings.Contains(string(mustJSON(list)), secret) {
			t.Fatal("secret listed")
		}
		k.all.must(204, "DELETE", "/obx/v1/webhooks/"+sub["id"].(string), nil)
	})

	t.Run("webhook URL rules", func(t *testing.T) {
		k.app.Policy.AllowPrivate = false
		defer func() { k.app.Policy.AllowPrivate = true }()
		for _, u := range []string{"http://example.com/hook", "https://127.0.0.1/hook", "https://10.1.2.3/hook"} {
			r := k.all.do("POST", "/obx/v1/webhooks", map[string]any{"url": u, "events": []string{"changes.available"}})
			if r.Status != 422 || r.JSON(t)["obx_error"] != "unsafe_url" {
				t.Errorf("%s: %d %s", u, r.Status, r.Body)
			}
		}
	})
}

func TestWebhookSignatureVector(t *testing.T) {
	v := readFixture(t, "examples/webhook-signature-vector.json").(map[string]any)
	got := secure.WebhookSignature(v["secret"].(string), v["timestamp"].(string), []byte(v["raw_body"].(string)))
	if got != v["header"] {
		t.Fatalf("got %s, want %s", got, v["header"])
	}
}
