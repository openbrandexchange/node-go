package node

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type scriptOp struct {
	Op         string         `json:"op"`
	Kind       string         `json:"kind"`
	ExternalID string         `json:"external_id"`
	Data       map[string]any `json:"data"`
}

func loadScript(t *testing.T) []scriptOp {
	f, err := os.Open(filepath.Join(specDir(t), "ingest", "script.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ops []scriptOp
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var op scriptOp
		if err := json.Unmarshal(sc.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op)
	}
	return ops
}

// suffixed returns the script with every external id (and reference)
// suffixed, as I-REPLAY-2 does.
func suffixed(ops []scriptOp, sfx string) []scriptOp {
	out := make([]scriptOp, len(ops))
	for i, op := range ops {
		var data map[string]any
		raw, _ := json.Marshal(op.Data)
		_ = json.Unmarshal(raw, &data)
		for _, k := range []string{"brand_external_id", "product_external_id"} {
			if s, ok := data[k].(string); ok {
				data[k] = s + sfx
			}
		}
		out[i] = scriptOp{Op: op.Op, Kind: op.Kind, ExternalID: op.ExternalID + sfx, Data: data}
		if op.Data == nil {
			out[i].Data = nil
		}
	}
	return out
}

func uploadIngestFiles(t *testing.T, ing *client) {
	files, _ := filepath.Glob(filepath.Join(specDir(t), "ingest", "files", "*.png"))
	for _, f := range files {
		data, _ := os.ReadFile(f)
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		r := ing.do("PUT", "/obx/v1/ingest/blobs/"+sha, data, "Content-Type", "image/png")
		if r.Status != 201 && r.Status != 200 {
			t.Fatalf("blob %s: %d %s", f, r.Status, r.Body)
		}
		validate(t, "Blob", r.JSON(t))
	}
}

type expectedChange struct {
	AfterOp    int    `json:"after_op"`
	Type       string `json:"type"`
	Op         string `json:"op"`
	ExternalID string `json:"external_id"`
}

func TestIngestReplay(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	ing := n.ingestToken(admin)
	reader, _ := n.apiClient(admin, "Replay Reader", map[string]any{})
	_, cursor, _ := drainChanges(t, reader, "", 500)
	uploadIngestFiles(t, ing)

	info := ing.must(200, "GET", "/obx/v1/ingest", nil)
	validate(t, "IngestInfo", info)

	var expResults []map[string]any
	raw, _ := os.ReadFile(filepath.Join(specDir(t), "ingest", "expected-results.json"))
	_ = json.Unmarshal(raw, &expResults)
	var expChanges struct {
		Changes []expectedChange `json:"changes"`
	}
	raw, _ = os.ReadFile(filepath.Join(specDir(t), "ingest", "expected-changes.json"))
	_ = json.Unmarshal(raw, &expChanges)
	ops := loadScript(t)

	check := func(label string, i int, res map[string]any, sfx string) {
		t.Helper()
		want := expResults[i]
		if res["result"] != want["result"] {
			t.Errorf("%s op %d (%s %s %s): want result %v, got %v (%v)", label, i, want["op"], want["kind"], want["external_id"], want["result"], res["result"], res["error"])
		}
		if code, ok := want["obx_error"]; ok {
			e, _ := res["error"].(map[string]any)
			got := res["obx_error"]
			if e != nil {
				got = e["obx_error"]
			}
			if got != code {
				t.Errorf("%s op %d: want %v, got %v", label, i, code, got)
			}
		}
		if res["external_id"] != nil && res["external_id"] != want["external_id"].(string)+sfx {
			t.Errorf("%s op %d: external_id %v", label, i, res["external_id"])
		}
	}
	changesFor := func(i int, sfx string) []string {
		var out []string
		for _, c := range expChanges.Changes {
			if c.AfterOp == i {
				out = append(out, c.Type+" "+c.Op+" "+c.ExternalID+sfx)
			}
		}
		return out
	}
	readNew := func() []string {
		ch, next, _ := drainChanges(t, reader, cursor, 100)
		cursor = next
		var out []string
		for _, c := range ch {
			ext, _ := c["external_id"].(string)
			out = append(out, fmt.Sprintf("%s %s %s", c["type"], c["op"], ext))
			if c["op"] == "upsert" {
				validate(t, "Change", c)
			}
		}
		return out
	}

	// I-REPLAY-1: one operation at a time on the dedicated endpoints.
	for i, op := range ops {
		path := "/obx/v1/ingest/" + op.Kind + "s/" + url.PathEscape(op.ExternalID)
		var r *resp
		switch op.Op {
		case "upsert":
			r = ing.do("PUT", path, op.Data)
		case "patch":
			r = ing.do("PATCH", path, op.Data, "Content-Type", "application/merge-patch+json")
		case "publish", "unpublish":
			status := "published"
			if op.Op == "unpublish" {
				status = "unpublished"
			}
			r = ing.do("PATCH", path, map[string]any{"status": status}, "Content-Type", "application/merge-patch+json")
		case "delete":
			r = ing.do("DELETE", path, nil)
		}
		res := r.JSON(t)
		if r.Status >= 400 {
			validate(t, "Problem", res)
			check("single", i, map[string]any{"result": "error", "obx_error": res["obx_error"]}, "")
		} else {
			validate(t, "IngestResult", res)
			check("single", i, res, "")
		}
		got := readNew()
		want := changesFor(i, "")
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("single op %d (%s %s %s): changes\n want %v\n got  %v", i, op.Op, op.Kind, op.ExternalID, want, got)
		}
	}

	// I-REPLAY-2: the same script as one JSON batch, and as NDJSON.
	for _, variant := range []struct{ sfx, ct string }{{"-b", "application/json"}, {"-c", "application/x-ndjson"}} {
		sops := suffixed(ops, variant.sfx)
		var body []byte
		if variant.ct == "application/json" {
			body, _ = json.Marshal(sops)
		} else {
			var buf bytes.Buffer
			for _, op := range sops {
				line, _ := json.Marshal(op)
				buf.Write(line)
				buf.WriteByte('\n')
			}
			body = buf.Bytes()
		}
		res := ing.must(200, "POST", "/obx/v1/ingest/batch", bytes.NewReader(body), "Content-Type", variant.ct)
		results := res["results"].([]any)
		if len(results) != len(ops) {
			t.Fatalf("batch %s: %d results", variant.sfx, len(results))
		}
		for i, x := range results {
			validate(t, "IngestResult", x)
			check("batch"+variant.sfx, i, x.(map[string]any), variant.sfx)
		}
		got := readNew()
		// Upsert entries of objects that are no longer visible when the feed
		// is read are omitted (docs/protocol.md §6.2); after the whole batch
		// only the brand is still visible.
		var want []string
		for i := range ops {
			for _, c := range changesFor(i, variant.sfx) {
				if strings.Contains(c, " upsert ") && !strings.HasPrefix(c, "brand ") {
					continue
				}
				want = append(want, c)
			}
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("batch %s changes\n want %v\n got  %v", variant.sfx, want, got)
		}
	}

	// I-IDEM-1: repeating an upsert is unchanged and writes nothing.
	ing.must(201, "PUT", "/obx/v1/ingest/products/IDEM-1", map[string]any{"brand_external_id": "kestrel", "style_number": "IDEM-1", "name": map[string]any{"en": "Idempotent"}})
	readNew()
	r := ing.must(200, "PUT", "/obx/v1/ingest/products/IDEM-1", map[string]any{"brand_external_id": "kestrel", "style_number": "IDEM-1", "name": map[string]any{"en": "Idempotent"}})
	if r["result"] != "unchanged" || len(readNew()) != 0 {
		t.Fatalf("repeat: %v", r)
	}
	key := "idem-key-1"
	b1 := ing.must(202, "POST", "/obx/v1/ingest/batch", `[{"op":"publish","kind":"product","external_id":"IDEM-1"}]`, "Idempotency-Key", key, "Prefer", "respond-async")
	b2 := ing.must(202, "POST", "/obx/v1/ingest/batch", `[{"op":"publish","kind":"product","external_id":"IDEM-1"}]`, "Idempotency-Key", key, "Prefer", "respond-async")
	if b1["id"] != b2["id"] {
		t.Fatalf("idempotency key returned different jobs: %v %v", b1["id"], b2["id"])
	}
	validate(t, "IngestJob", b1)
	n.app.RunJobsNow(t.Context())
	job := ing.must(200, "GET", "/obx/v1/ingest/jobs/"+b1["id"].(string), nil)
	validate(t, "IngestJob", job)
	if job["status"] != "succeeded" {
		t.Fatalf("job %v", job)
	}
	results := ing.must(200, "GET", "/obx/v1/ingest/jobs/"+b1["id"].(string)+"/results", nil)
	if len(results["items"].([]any)) != 1 {
		t.Fatalf("results %v", results)
	}
}

func TestIngestFilesAndValidation(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	ing := n.ingestToken(admin)
	img, _ := os.ReadFile(filepath.Join(specDir(t), "ingest", "files", "KL-9001_GRN_1.png"))
	sum := sha256.Sum256(img)
	sha := hex.EncodeToString(sum[:])

	t.Run("blob dedupe and checksum", func(t *testing.T) {
		if r := ing.do("HEAD", "/obx/v1/ingest/blobs/"+sha, nil); r.Status != 404 {
			t.Fatalf("HEAD before upload: %d", r.Status)
		}
		ing.must(201, "PUT", "/obx/v1/ingest/blobs/"+sha, img)
		if r := ing.do("HEAD", "/obx/v1/ingest/blobs/"+sha, nil); r.Status != 200 {
			t.Fatalf("HEAD after upload: %d", r.Status)
		}
		b := ing.must(200, "PUT", "/obx/v1/ingest/blobs/"+sha, img)
		if b["created"] != false {
			t.Fatalf("second upload should be a dedupe: %v", b)
		}
		wrong := strings.Repeat("a", 64)
		r := ing.do("PUT", "/obx/v1/ingest/blobs/"+wrong, img)
		if r.Status != 422 || r.JSON(t)["obx_error"] != "checksum_mismatch" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		r = ing.do("PUT", "/obx/v1/ingest/blobs/"+wrong[:63]+"b", []byte("<svg xmlns='http://www.w3.org/2000/svg'/>"))
		if r.Status != 422 {
			t.Fatalf("svg: %d %s", r.Status, r.Body)
		}
	})

	t.Run("validation codes", func(t *testing.T) {
		ing.must(201, "PUT", "/obx/v1/ingest/products/V-1", map[string]any{"style_number": "V-1", "name": map[string]any{"en": "Validation"}})
		cases := []struct {
			path string
			body any
			code string
		}{
			{"/obx/v1/ingest/variants/x1", map[string]any{"product_external_id": "V-1", "gtin": "2009001001015", "color": map[string]any{"code": "BLK"}}, "invalid_gtin"},
			{"/obx/v1/ingest/variants/x2", map[string]any{"product_external_id": "NOPE", "color": map[string]any{"code": "BLK"}}, "unknown_parent"},
			{"/obx/v1/ingest/assets/V-1_BLK_1", map[string]any{"product_external_id": "V-1", "color_code": "BLK", "role": "packshot", "position": 1, "files": map[string]any{"original": map[string]any{"sha256": strings.Repeat("c", 64)}}}, "blob_missing"},
			{"/obx/v1/ingest/products/V-2", map[string]any{"name": map[string]any{"en": "no style"}}, "validation_failed"},
		}
		for _, c := range cases {
			r := ing.do("PUT", c.path, c.body)
			if r.JSON(t)["obx_error"] != c.code {
				t.Errorf("%s: want %s, got %d %s", c.path, c.code, r.Status, r.Body)
			}
			validate(t, "Problem", r.JSON(t))
		}
		ing.must(201, "PUT", "/obx/v1/ingest/variants/v1", map[string]any{"product_external_id": "V-1", "gtin": "2009001001014", "color": map[string]any{"code": "BLK"}})
		r := ing.do("PUT", "/obx/v1/ingest/variants/v2", map[string]any{"product_external_id": "V-1", "gtin": "02009001001014", "color": map[string]any{"code": "BLK"}})
		if r.Status != 409 || r.JSON(t)["obx_error"] != "gtin_conflict" {
			t.Errorf("duplicate GTIN: %d %s", r.Status, r.Body)
		}
		ing.must(201, "PUT", "/obx/v1/ingest/assets/V-1_BLK_1", map[string]any{"product_external_id": "V-1", "color_code": "BLK", "role": "packshot", "position": 1, "files": map[string]any{"original": map[string]any{"sha256": sha}}})
		r = ing.do("PUT", "/obx/v1/ingest/assets/V-1_BLK_1b", map[string]any{"product_external_id": "V-1", "color_code": "BLK", "role": "packshot", "position": 1})
		if r.Status != 409 || r.JSON(t)["obx_error"] != "position_conflict" {
			t.Errorf("position: %d %s", r.Status, r.Body)
		}
		rec := ing.must(200, "GET", "/obx/v1/ingest/assets/V-1_BLK_1", nil)
		validate(t, "IngestRecord", rec)
		if rec["visibility"] != "visible" {
			t.Errorf("asset should be visible after server rendering: %v", rec)
		}
		// data.files lists every current blob with its dimensions, the
		// node's own renditions included; missing_files is [] once complete.
		files, _ := rec["data"].(map[string]any)["files"].(map[string]any)
		for _, n := range []string{"original", "web", "square", "thumb"} {
			f, _ := files[n].(map[string]any)
			if f == nil || f["sha256"] == nil || f["width"] == nil || f["height"] == nil {
				t.Errorf("data.files.%s should have sha256, width and height: %v", n, files[n])
			}
		}
		if orig, _ := files["original"].(map[string]any); orig != nil && (orig["sha256"] != sha || orig["width"] != float64(1200) || orig["height"] != float64(1500)) {
			t.Errorf("original: %v", orig)
		}
		if mf, ok := rec["missing_files"].([]any); !ok || len(mf) != 0 {
			t.Errorf("missing_files should be [] for a complete asset, got %#v", rec["missing_files"])
		}
		if prod := ing.must(200, "GET", "/obx/v1/ingest/products/V-1", nil); prod["missing_files"] != nil {
			t.Errorf("missing_files is for assets only: %v", prod["missing_files"])
		}
		// Writing back what GET returned (defaults and the node's own
		// renditions spelled out) changes nothing.
		if back := ing.must(200, "PUT", "/obx/v1/ingest/assets/V-1_BLK_1", rec["data"]); back["result"] != "unchanged" {
			t.Errorf("GET then PUT of the same data: %v", back["result"])
		}
		r = ing.do("PUT", "/obx/v1/ingest/products/V-1", map[string]any{"style_number": "V-1", "name": map[string]any{"en": "x"}}, "If-Match", `"nope"`)
		if r.Status != 412 {
			t.Errorf("if-match: %d", r.Status)
		}
	})

	t.Run("asset order swaps positions", func(t *testing.T) {
		img2, _ := os.ReadFile(filepath.Join(specDir(t), "ingest", "files", "KL-9001_GRN_2.png"))
		s2 := sha256.Sum256(img2)
		ing.must(201, "PUT", "/obx/v1/ingest/blobs/"+hex.EncodeToString(s2[:]), img2)
		ing.must(201, "PUT", "/obx/v1/ingest/assets/V-1_BLK_2", map[string]any{"product_external_id": "V-1", "color_code": "BLK", "role": "packshot", "position": 2, "files": map[string]any{"original": map[string]any{"sha256": hex.EncodeToString(s2[:])}}})
		r := ing.do("PUT", "/obx/v1/ingest/products/V-1/asset-order", map[string]any{"color_code": "BLK", "asset_external_ids": []string{"V-1_BLK_2", "V-1_BLK_1"}})
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var res []map[string]any
		_ = json.Unmarshal(r.Body, &res)
		if len(res) != 2 || res[0]["result"] != "updated" {
			t.Fatalf("%s", r.Body)
		}
		rec := ing.must(200, "GET", "/obx/v1/ingest/assets/V-1_BLK_2", nil)
		if rec["data"].(map[string]any)["position"].(float64) != 1 {
			t.Fatalf("position not swapped: %v", rec["data"])
		}
	})

	t.Run("multipart upload", func(t *testing.T) {
		data := bytes.Repeat([]byte{0}, 12<<20)
		copy(data, img) // a PNG header followed by padding: stored but not an image the renderer can use
		s := sha256.Sum256(data)
		hexs := hex.EncodeToString(s[:])
		up := ing.must(201, "POST", "/obx/v1/ingest/uploads", map[string]any{"sha256": hexs, "bytes": len(data), "content_type": "image/png"})
		validate(t, "Upload", up)
		id := up["id"].(string)
		part := int(up["part_bytes"].(float64))
		total := int(up["parts_total"].(float64))
		// Skip part 1 first: completing must fail.
		for p := 2; p <= total; p++ {
			end := min(p*part, len(data))
			ing.must(200, "PUT", fmt.Sprintf("/obx/v1/ingest/uploads/%s/parts/%d", id, p), data[(p-1)*part:end])
		}
		r := ing.do("POST", "/obx/v1/ingest/uploads/"+id+"/complete", nil)
		if r.Status != 409 || r.JSON(t)["obx_error"] != "upload_incomplete" {
			t.Fatalf("incomplete: %d %s", r.Status, r.Body)
		}
		st := ing.must(200, "GET", "/obx/v1/ingest/uploads/"+id, nil)
		if len(st["received_parts"].([]any)) != total-1 {
			t.Fatalf("received %v", st["received_parts"])
		}
		ing.must(200, "PUT", fmt.Sprintf("/obx/v1/ingest/uploads/%s/parts/1", id), data[:part])
		b := ing.do("POST", "/obx/v1/ingest/uploads/"+id+"/complete", nil)
		// The padded PNG cannot be read as an image; the node refuses it
		// after verifying the checksum. Either way the upload protocol ran.
		if b.Status != 200 && b.Status != 422 {
			t.Fatalf("complete: %d %s", b.Status, b.Body)
		}
	})

	t.Run("scopes and brand restriction", func(t *testing.T) {
		ro := n.ingestToken(admin, "ingest:read")
		r := ro.do("PUT", "/obx/v1/ingest/products/X-1", map[string]any{"style_number": "X-1", "name": map[string]any{"en": "x"}})
		if r.Status != 403 || r.JSON(t)["obx_error"] != "insufficient_scope" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		ing.must(201, "PUT", "/obx/v1/ingest/brands/other", map[string]any{"name": "Other"})
		m := admin.must(201, "POST", "/admin/api/ingest-tokens", map[string]any{"name": "Kestrel only", "scopes": []string{"ingest:read", "ingest:write"}, "brand_external_ids": []string{"kestrel"}})
		restricted := n.client()
		restricted.bearer = m["secret"].(string)
		r = restricted.do("PUT", "/obx/v1/ingest/products/X-2", map[string]any{"brand_external_id": "other", "style_number": "X-2", "name": map[string]any{"en": "x"}})
		if r.Status != 403 || r.JSON(t)["obx_error"] != "forbidden" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		admin.must(204, "DELETE", "/admin/api/ingest-tokens/"+m["token"].(map[string]any)["id"].(string), nil)
		r = restricted.do("GET", "/obx/v1/ingest", nil)
		if r.Status != 401 || r.JSON(t)["obx_error"] != "access_revoked" {
			t.Fatalf("revoked token: %d %s", r.Status, r.Body)
		}
	})
}
