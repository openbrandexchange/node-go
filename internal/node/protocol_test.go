package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kestrelNode is a node set up like the conformance reference path, with
// the Kestrel fixtures imported and three retailers:
// all (every brand), restricted (only brand kestrel), noOrig (no originals).
type kestrelNode struct {
	*testNode
	admin, all, restricted, noOrig *client
	products                       map[string]map[string]any // external id → ProductDetail (all)
	brands                         map[string]string         // external id → id
}

func newKestrelNode(t *testing.T) *kestrelNode {
	n := newTestNode(t)
	admin := n.setup()
	run := n.importKestrel(admin)
	if run["status"] != "succeeded" {
		t.Fatalf("import failed: %v", run)
	}
	validate(t, "ImportRun", run)
	counts := run["counts"].(map[string]any)
	if counts["errors"].(float64) != 0 {
		errs := admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string)+"/errors", nil)
		t.Fatalf("import errors: %v", errs)
	}
	k := &kestrelNode{testNode: n, admin: admin, brands: map[string]string{}}
	brands := admin.must(200, "GET", "/admin/api/catalog/brands", nil)
	for _, b := range brands["items"].([]any) {
		bm := b.(map[string]any)
		k.brands[bm["external_id"].(string)] = bm["id"].(string)
	}
	if k.brands["kestrel"] == "" || k.brands["kestrel-trail"] == "" {
		t.Fatalf("brands: %v", k.brands)
	}
	k.all, _ = n.apiClient(admin, "All Brands Store", map[string]any{})
	k.restricted, _ = n.apiClient(admin, "Kestrel Only Store", map[string]any{"brand_ids": []string{k.brands["kestrel"]}})
	k.noOrig, _ = n.apiClient(admin, "No Originals Store", map[string]any{"allow_original": false})
	k.products = map[string]map[string]any{}
	page := k.all.must(200, "GET", "/obx/v1/products?limit=100", nil)
	for _, p := range page["items"].([]any) {
		pm := p.(map[string]any)
		k.products[pm["external_id"].(string)] = pm
	}
	return k
}

func (k *kestrelNode) asset(c *client, ext string) map[string]any {
	for _, p := range k.products {
		for _, a := range p["assets"].([]any) {
			am := a.(map[string]any)
			if am["external_id"] == ext {
				return c.must(200, "GET", "/obx/v1/assets/"+am["id"].(string), nil)
			}
		}
	}
	k.t.Fatalf("asset %s not found", ext)
	return nil
}

func TestKestrelProtocol(t *testing.T) {
	k := newKestrelNode(t)

	t.Run("discovery", func(t *testing.T) {
		r := k.client().do("GET", "/.well-known/obx", nil)
		if r.Status != 200 || r.Header.Get("Access-Control-Allow-Origin") != "*" || r.Header.Get("OBX-Version") != "0.1" {
			t.Fatalf("discovery: %d %v", r.Status, r.Header)
		}
		d := r.JSON(t)
		validate(t, "DiscoveryDocument", d)
		if !strings.HasSuffix(d["api_base"].(string), "/obx/v1") {
			t.Errorf("api_base %v", d["api_base"])
		}
		caps := strings.Join(toStrings(d["capabilities"]), ",")
		for _, c := range []string{"changes", "products", "by_gtin", "renditions"} {
			if !strings.Contains(caps, c) {
				t.Errorf("capability %s missing", c)
			}
		}
		if langs := toStrings(d["languages"]); len(langs) != 2 || langs[0] != "nl" {
			t.Errorf("languages %v", langs)
		}
	})

	t.Run("snapshot counts and order", func(t *testing.T) {
		changes, _, pages := drainChanges(t, k.all, "", 500)
		want := readFixture(t, "expected/counts.json").(map[string]any)
		got := map[string]int{}
		order := []string{}
		for _, c := range changes {
			typ := c["type"].(string)
			got[typ]++
			if len(order) == 0 || order[len(order)-1] != typ {
				order = append(order, typ)
			}
			validate(t, "Change", c)
		}
		for _, typ := range []string{"brands", "products", "variants", "assets"} {
			if float64(got[strings.TrimSuffix(typ, "s")]) != want[typ].(float64) {
				t.Errorf("%s: want %v, got %d", typ, want[typ], got[strings.TrimSuffix(typ, "s")])
			}
		}
		if strings.Join(order, ",") != "brand,product,variant,asset" {
			t.Errorf("type order %v", order)
		}
		last := pages[len(pages)-1]
		if last["phase"] != "live" {
			t.Errorf("feed must end in the live phase")
		}
		for _, p := range pages[:len(pages)-1] {
			if p["phase"] != "snapshot" || p["has_more"] != true {
				t.Errorf("snapshot pages must have has_more=true")
			}
		}
	})

	t.Run("paging with limit 1 gives the same entries", func(t *testing.T) {
		a, _, _ := drainChanges(t, k.all, "", 500)
		b, _, _ := drainChanges(t, k.all, "", 1)
		if len(a) != len(b) {
			t.Fatalf("limit 500: %d entries, limit 1: %d", len(a), len(b))
		}
		for i := range a {
			if a[i]["id"] != b[i]["id"] || a[i]["type"] != b[i]["type"] {
				t.Fatalf("entry %d differs", i)
			}
		}
	})

	t.Run("garbage cursor", func(t *testing.T) {
		r := k.all.do("GET", "/obx/v1/changes?cursor=garbage", nil)
		if r.Status != 400 || r.JSON(t)["obx_error"] != "cursor_invalid" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		validate(t, "Problem", r.JSON(t))
	})

	t.Run("expected products", func(t *testing.T) {
		for _, ext := range []string{"KO-1001", "KO-1005"} {
			p := k.all.must(200, "GET", "/obx/v1/products/"+k.products[ext]["id"].(string), nil)
			validate(t, "ProductDetail", p)
			match(t, ext, readFixture(t, "expected/product-"+ext+".json"), p, nil)
		}
	})

	t.Run("product etag", func(t *testing.T) {
		path := "/obx/v1/products/" + k.products["KO-1001"]["id"].(string)
		r := k.all.do("GET", path, nil)
		r2 := k.all.do("GET", path, nil, "If-None-Match", r.Header.Get("ETag"))
		if r2.Status != 304 {
			t.Fatalf("want 304, got %d", r2.Status)
		}
	})

	t.Run("gtin lookup", func(t *testing.T) {
		req := readFixture(t, "expected/gtin-lookup.request.json")
		res := k.all.must(200, "POST", "/obx/v1/products:by-gtin", req)
		validate(t, "GtinLookupResult", res)
		prods := map[string]any{}
		for ext, p := range k.products {
			prods[ext] = p
		}
		// Rendition URLs differ per call: compare the products without them.
		stripURLs(res)
		for _, p := range prods {
			stripURLs(p)
		}
		match(t, "gtin", readFixture(t, "expected/gtin-lookup.json"), res, prods)
		many := make([]string, 2001)
		for i := range many {
			many[i] = "2000100101025"
		}
		r := k.all.do("POST", "/obx/v1/products:by-gtin", map[string]any{"gtins": many})
		if r.Status != 400 || r.JSON(t)["obx_error"] != "limit_exceeded" {
			t.Fatalf("2001 GTINs: %d %s", r.Status, r.Body)
		}
	})

	t.Run("expected assets", func(t *testing.T) {
		for file, ext := range map[string]string{"asset-ai-generated": "KO-1003_RST_3", "asset-embargoed": "KO-1002_BLK_2", "asset-large-original": "KO-1002_OLV_1"} {
			a := k.asset(k.all, ext)
			validate(t, "Asset", a)
			match(t, file, readFixture(t, "expected/"+file+".json"), a, nil)
		}
		a := k.asset(k.noOrig, "KO-1001_NVY_1")
		validate(t, "Asset", a)
		match(t, "asset-without-original-scope", readFixture(t, "expected/asset-without-original-scope.json"), a, nil)
	})

	t.Run("renditions", func(t *testing.T) {
		a := k.asset(k.all, "KO-1002_OLV_1")
		rends := a["renditions"].(map[string]any)
		orig := rends["original"].(map[string]any)
		want, _ := os.ReadFile(filepath.Join(specDir(t), "kestrel", "images", "KO-1002_OLV_1_front.png"))
		// Signed URL works without Authorization.
		plain := k.client()
		r := plain.do("GET", orig["url"].(string), nil)
		if r.Status != 200 || !bytes.Equal(r.Body, want) {
			t.Fatalf("original: %d, identical=%v", r.Status, bytes.Equal(r.Body, want))
		}
		sum := sha256.Sum256(want)
		if r.Header.Get("OBX-Content-SHA256") != hex.EncodeToString(sum[:]) || r.Header.Get("ETag") != `"`+hex.EncodeToString(sum[:])+`"` {
			t.Errorf("headers %v", r.Header)
		}
		if r.Header.Get("Accept-Ranges") != "bytes" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "inline") {
			t.Errorf("headers %v", r.Header)
		}
		rr := plain.do("GET", orig["url"].(string), nil, "Range", "bytes=0-9")
		if rr.Status != 206 || len(rr.Body) != 10 {
			t.Errorf("range: %d %d", rr.Status, len(rr.Body))
		}
		nm := plain.do("GET", orig["url"].(string), nil, "If-None-Match", r.Header.Get("ETag"))
		if nm.Status != 304 {
			t.Errorf("if-none-match: %d", nm.Status)
		}
		// Tampered signature is refused.
		bad := strings.Replace(orig["url"].(string), "sig=", "sig=00", 1)
		if b := plain.do("GET", bad, nil); b.Status != 401 {
			t.Errorf("tampered signature: %d", b.Status)
		}
		for name, max := range map[string]int{"web": 2000, "thumb": 600, "square": 2000} {
			rd := rends[name].(map[string]any)
			rr := k.all.do("GET", "/obx/v1/assets/"+a["id"].(string)+"/renditions/"+name, nil)
			if rr.Status != 200 || rr.Header.Get("Content-Type") != "image/jpeg" {
				t.Fatalf("%s: %d %s", name, rr.Status, rr.Header.Get("Content-Type"))
			}
			sum := sha256.Sum256(rr.Body)
			if hex.EncodeToString(sum[:]) != rd["sha256"] {
				t.Errorf("%s: bytes do not hash to sha256", name)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(rr.Body))
			if err != nil || cfg.Width > max || cfg.Height > max || float64(cfg.Width) != rd["width"] {
				t.Errorf("%s: %dx%d (err %v)", name, cfg.Width, cfg.Height, err)
			}
			if name == "square" && cfg.Width != cfg.Height {
				t.Errorf("square is %dx%d", cfg.Width, cfg.Height)
			}
			if !strings.Contains(rr.Header.Get("Content-Disposition"), "_"+name+".jpg") {
				t.Errorf("%s disposition %s", name, rr.Header.Get("Content-Disposition"))
			}
		}
	})

	t.Run("originals not allowed", func(t *testing.T) {
		a := k.asset(k.all, "KO-1001_NVY_1")
		r := k.noOrig.do("GET", "/obx/v1/assets/"+a["id"].(string)+"/renditions/original", nil)
		if r.Status != 403 || r.JSON(t)["obx_error"] != "original_not_allowed" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		ch, _, _ := drainChanges(t, k.noOrig, "", 500)
		for _, c := range ch {
			if c["type"] == "asset" {
				if _, ok := c["data"].(map[string]any)["renditions"].(map[string]any)["original"]; ok {
					t.Fatal("original rendition offered to a retailer without originals")
				}
			}
		}
	})

	t.Run("restricted retailer sees only kestrel", func(t *testing.T) {
		ch, _, _ := drainChanges(t, k.restricted, "", 500)
		n := 0
		for _, c := range ch {
			if c["brand_id"] != k.brands["kestrel"] {
				t.Fatalf("restricted retailer saw %v", c)
			}
			n++
		}
		if n != 1+6+34+23 {
			// 1 brand, KO-1001..1006, their variants and images
			t.Logf("restricted snapshot has %d entries", n)
		}
		trail := k.products["KT-2001"]["id"].(string)
		r := k.restricted.do("GET", "/obx/v1/products/"+trail, nil)
		if r.Status != 404 {
			t.Fatalf("invisible product must be 404, got %d", r.Status)
		}
		res := k.restricted.must(200, "POST", "/obx/v1/products:by-gtin", map[string]any{"gtins": []string{"2000200101031"}})
		if res["results"].([]any)[0].(map[string]any)["status"] != "unmatched" {
			t.Fatalf("restricted lookup matched another brand: %v", res)
		}
		brands := k.restricted.must(200, "GET", "/obx/v1/brands", nil)
		if len(brands["items"].([]any)) != 1 {
			t.Fatalf("brands %v", brands)
		}
	})

	t.Run("products pagination", func(t *testing.T) {
		seen := map[string]bool{}
		cursor := ""
		for i := 0; i < 20; i++ {
			q := "/obx/v1/products?limit=3"
			if cursor != "" {
				q += "&cursor=" + url.QueryEscape(cursor)
			}
			page := k.all.must(200, "GET", q, nil)
			for _, p := range page["items"].([]any) {
				validate(t, "ProductDetail", p)
				seen[p.(map[string]any)["id"].(string)] = true
			}
			if page["next_cursor"] == nil {
				break
			}
			cursor = page["next_cursor"].(string)
		}
		if len(seen) != 8 {
			t.Fatalf("paged %d products", len(seen))
		}
		id := k.products["KO-1001"]["id"].(string)
		page := k.all.must(200, "GET", "/obx/v1/products?ids="+id+",prd_unknown", nil)
		if len(page["items"].([]any)) != 1 {
			t.Fatalf("ids filter: %v", page)
		}
	})

	t.Run("auth errors", func(t *testing.T) {
		plain := k.client()
		r := plain.do("GET", "/obx/v1/changes", nil)
		if r.Status != 401 || r.JSON(t)["obx_error"] != "unauthorized" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		plain.bearer = "obxa_bogus.token"
		r = plain.do("GET", "/obx/v1/changes", nil)
		if r.Status != 401 || r.JSON(t)["obx_error"] != "invalid_token" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		form := url.Values{"grant_type": {"client_credentials"}}
		req, _ := http.NewRequest("POST", k.url+"/obx/v1/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth("obxc_unknown", "wrong")
		res, _ := http.DefaultClient.Do(req)
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		res.Body.Close()
		if res.StatusCode != 401 || m["error"] != "invalid_client" {
			t.Fatalf("wrong secret: %d %v", res.StatusCode, m)
		}
		r = k.client().do("GET", "/obx/v1/does-not-exist", nil)
		if r.Status != 404 || r.JSON(t)["obx_error"] != "not_found" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		r = k.all.do("DELETE", "/obx/v1/changes", nil)
		if r.Status != 405 || r.JSON(t)["obx_error"] != "method_not_allowed" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("grant change writes a resync entry", func(t *testing.T) {
		c, info := k.apiClient(k.admin, "Grant Changer", map[string]any{})
		_, cursor, _ := drainChanges(t, c, "", 500)
		ret := info["retailer"].(map[string]any)
		k.admin.must(200, "PATCH", "/admin/api/retailers/"+ret["id"].(string), map[string]any{"brand_ids": []string{k.brands["kestrel"]}})
		ch, _, _ := drainChanges(t, c, cursor, 100)
		if len(ch) != 1 || ch[0]["type"] != "grant" || ch[0]["resync"] != true {
			t.Fatalf("want one grant entry with resync, got %v", ch)
		}
		g := k.all.must(200, "GET", "/obx/v1/grant", nil)
		validate(t, "Grant", g)
	})

	t.Run("revoked client", func(t *testing.T) {
		c, info := k.apiClient(k.admin, "Soon Revoked", map[string]any{})
		c.must(200, "GET", "/obx/v1/grant", nil)
		k.admin.must(204, "DELETE", "/admin/api/api-clients/"+info["api_client"].(map[string]any)["id"].(string), nil)
		r := c.do("GET", "/obx/v1/grant", nil)
		if r.Status != 401 || r.JSON(t)["obx_error"] != "access_revoked" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("a new key gets catalog and media scopes; usage:write only when asked for", func(t *testing.T) {
		ret := k.admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Scope Shop"})
		def := k.admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": ret["id"], "name": "Scope Shop PIM"})
		if got := fmt.Sprint(def["client"].(map[string]any)["scopes"]); got != "[catalog:read media:original media:read]" {
			t.Fatalf("default scopes %s", got)
		}
		asked := k.admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": ret["id"], "name": "Scope Shop stats", "scopes": []string{"catalog:read", "usage:write"}})
		if got := fmt.Sprint(asked["client"].(map[string]any)["scopes"]); got != "[catalog:read usage:write]" {
			t.Fatalf("requested scopes %s", got)
		}
	})

	t.Run("usage appears in stats", func(t *testing.T) {
		a := k.asset(k.all, "KO-1001_NVY_1")
		// Reporting usage needs a key that asked for usage:write.
		var retailerID string
		for _, it := range k.admin.must(200, "GET", "/admin/api/retailers", nil)["items"].([]any) {
			if r := it.(map[string]any); r["name"] == "All Brands Store" {
				retailerID = r["id"].(string)
			}
		}
		cl := k.admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": retailerID, "name": "Stats reporter",
			"scopes": []string{"catalog:read", "media:read", "usage:write"}})
		reporter := k.client()
		reporter.bearer = k.token(cl["client"].(map[string]any)["client_id"].(string), cl["client_secret"].(string), "")
		res := reporter.must(202, "POST", "/obx/v1/usage", map[string]any{"events": []any{
			map[string]any{"asset_id": a["id"], "event": "published", "channel": "webshop", "at": "2026-09-01T10:00:00Z", "count": 3},
			map[string]any{"asset_id": "ast_unknown", "event": "synced", "at": "2026-09-01T10:00:00Z"},
		}})
		if res["accepted"].(float64) != 1 {
			t.Fatalf("accepted %v", res)
		}
		ev := k.admin.must(200, "GET", "/admin/api/stats/events?kind=usage", nil)
		if len(ev["items"].([]any)) != 1 {
			t.Fatalf("usage events %v", ev)
		}
	})
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// stripURLs replaces rendition URLs and expiries with placeholders so two
// reads of the same product compare equal.
func stripURLs(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == "url" || k == "url_expires_at" {
				t[k] = "x"
				continue
			}
			stripURLs(x)
		}
	case []any:
		for _, x := range t {
			stripURLs(x)
		}
	}
}
