package node

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"net/http"
	"strings"
	"testing"

	"github.com/openbrandexchange/node-go/internal/safehttp"
)

func TestSetupAndSessions(t *testing.T) {
	n := newTestNode(t)
	c := n.client()

	t.Run("before setup", func(t *testing.T) {
		r := c.do("GET", "/admin/api/settings", nil)
		if r.Status != 409 || r.JSON(t)["obx_error"] != "setup_required" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		r = c.do("GET", "/.well-known/obx", nil)
		if r.Status != 503 || r.JSON(t)["obx_error"] != "setup_required" {
			t.Fatalf("discovery before setup: %d %s", r.Status, r.Body)
		}
		if m := c.must(200, "GET", "/admin/api/setup", nil); m["required"] != true {
			t.Fatalf("%v", m)
		}
		c.session()
		r = c.do("POST", "/admin/api/setup", map[string]any{"setup_code": "WRONG", "company": map[string]any{"name": "X", "default_language": "en"},
			"admin": map[string]any{"name": "A", "email": "a@example.test", "password": "long enough password"}})
		if r.Status != 403 {
			t.Fatalf("wrong setup code: %d %s", r.Status, r.Body)
		}
		r = c.do("POST", "/admin/api/setup", map[string]any{"setup_code": testSetupCode, "company": map[string]any{"name": "X", "default_language": "en"},
			"admin": map[string]any{"name": "A", "email": "a@example.test", "password": "password12"}})
		if r.Status != 422 {
			t.Fatalf("common password must be refused: %d %s", r.Status, r.Body)
		}
	})

	admin := n.setup()

	t.Run("setup only once", func(t *testing.T) {
		other := n.client()
		other.session()
		r := other.do("POST", "/admin/api/setup", map[string]any{"setup_code": testSetupCode})
		if r.Status != 409 || r.JSON(t)["obx_error"] != "setup_completed" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("sessions and csrf", func(t *testing.T) {
		c := n.client()
		c.session()
		r := c.do("POST", "/admin/api/session/login", map[string]any{"email": adminEmail, "password": "wrong password!"})
		if r.Status != 401 || r.JSON(t)["obx_error"] != "invalid_credentials" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		r = c.do("POST", "/admin/api/session/login", map[string]any{"email": adminEmail, "password": adminPassword})
		if r.Status != 200 {
			t.Fatalf("login: %d %s", r.Status, r.Body)
		}
		var sessionCookie *http.Cookie
		for _, ck := range (&http.Response{Header: r.Header}).Cookies() {
			if ck.Name == "obx_session" {
				sessionCookie = ck
			}
		}
		if sessionCookie == nil || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode {
			t.Fatalf("session cookie flags: %+v", sessionCookie)
		}
		c.csrf = r.JSON(t)["csrf_token"].(string)
		token := c.csrf
		c.csrf = ""
		r = c.do("PATCH", "/admin/api/settings", map[string]any{"branding": map[string]any{"accent_color": "#123456"}})
		if r.Status != 403 || r.JSON(t)["obx_error"] != "csrf_failed" {
			t.Fatalf("missing csrf: %d %s", r.Status, r.Body)
		}
		c.csrf = token
		s := c.must(200, "PATCH", "/admin/api/settings", map[string]any{"branding": map[string]any{"accent_color": "#123456"}})
		if s["branding"].(map[string]any)["accent_color"] != "#123456" {
			t.Fatalf("settings %v", s)
		}
		// Bearer tokens are not accepted on the admin API.
		b := n.client()
		b.bearer = "obxi_something"
		if r := b.do("GET", "/admin/api/settings", nil); r.Status != 401 {
			t.Fatalf("bearer on admin API: %d", r.Status)
		}
		c.must(204, "POST", "/admin/api/session/logout", nil)
		if r := c.do("GET", "/admin/api/settings", nil); r.Status != 401 {
			t.Fatalf("after logout: %d", r.Status)
		}
	})

	t.Run("password reset", func(t *testing.T) {
		link, err := n.app.ResetLink(t.Context(), adminEmail)
		if err != nil {
			t.Fatal(err)
		}
		token := link[strings.LastIndex(link, "/")+1:]
		c := n.client()
		c.session()
		m := c.must(200, "POST", "/admin/api/password/reset", map[string]any{"token": token, "password": "a brand new password"})
		c.csrf = m["csrf_token"].(string)
		r := c.do("POST", "/admin/api/password/reset", map[string]any{"token": token, "password": "a brand new password"})
		if r.Status != 400 || r.JSON(t)["obx_error"] != "invite_invalid" {
			t.Fatalf("reuse: %d %s", r.Status, r.Body)
		}
		c.must(202, "POST", "/admin/api/password/forgot", map[string]any{"email": "nobody@example.test"})
	})
	_ = admin
}

func TestAccessRequestsAndRetailers(t *testing.T) {
	n := newTestNode(t)
	admin := n.setup()
	public := n.client()

	t.Run("approve with API access", func(t *testing.T) {
		ar := public.must(202, "POST", "/obx/v1/access-requests", map[string]any{
			"retailer": map[string]any{"name": "Fictional Boutique", "country": "NL"},
			"contact":  map[string]any{"name": "Rita Retailer", "email": "rita@boutique.example", "language": "nl"},
			"access":   []string{"portal", "api"},
		})
		validate(t, "AccessRequest", ar)
		poll := n.client()
		poll.bearer = ar["request_token"].(string)
		st := poll.must(200, "GET", "/obx/v1/access-requests/"+ar["id"].(string), nil)
		if st["status"] != "pending" || st["credentials"] != nil {
			t.Fatalf("%v", st)
		}
		list := admin.must(200, "GET", "/admin/api/access-requests?status=pending", nil)
		if len(list["items"].([]any)) != 1 {
			t.Fatalf("%v", list)
		}
		res := admin.must(200, "POST", "/admin/api/access-requests/"+ar["id"].(string)+"/approve", nil)
		if res["invite"] == nil || res["api_client"] == nil {
			t.Fatalf("approve: %v", res)
		}
		first := poll.must(200, "GET", "/obx/v1/access-requests/"+ar["id"].(string), nil)
		validate(t, "AccessRequest", first)
		creds := first["credentials"].(map[string]any)
		secret, _ := creds["client_secret"].(string)
		if !strings.HasPrefix(secret, "obxs_") || creds["claimed"] != true {
			t.Fatalf("first read: %v", creds)
		}
		second := poll.must(200, "GET", "/obx/v1/access-requests/"+ar["id"].(string), nil)
		if second["credentials"].(map[string]any)["client_secret"] != nil {
			t.Fatal("secret returned twice")
		}
		tok := n.token(creds["client_id"].(string), secret, "")
		api := n.client()
		api.bearer = tok
		api.must(200, "GET", "/obx/v1/grant", nil)
		// The invitation e-mail went to the contact in Dutch or the fallback.
		found := false
		for _, m := range n.app.LastMails() {
			if m.To == "rita@boutique.example" && strings.Contains(m.Body, "/invite/") {
				found = true
			}
		}
		if !found {
			t.Fatal("no invitation e-mail")
		}
		if r := admin.do("POST", "/admin/api/access-requests/"+ar["id"].(string)+"/approve", nil); r.Status != 409 {
			t.Fatalf("second approve: %d", r.Status)
		}
	})

	t.Run("reject with reason", func(t *testing.T) {
		ar := public.must(202, "POST", "/obx/v1/access-requests", map[string]any{
			"retailer": map[string]any{"name": "Unknown Shop"}, "contact": map[string]any{"name": "Uma", "email": "uma@shop.example"}})
		admin.must(200, "POST", "/admin/api/access-requests/"+ar["id"].(string)+"/reject", map[string]any{"reason": "We only sell through selected stores."})
		poll := n.client()
		poll.bearer = ar["request_token"].(string)
		st := poll.must(200, "GET", "/obx/v1/access-requests/"+ar["id"].(string), nil)
		if st["status"] != "rejected" || st["reason"] != "We only sell through selected stores." {
			t.Fatalf("%v", st)
		}
	})

	t.Run("invite once, block retailer", func(t *testing.T) {
		ret := admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Blockable Store"})
		inv := admin.must(201, "POST", "/admin/api/users/invites", map[string]any{"email": "bob@store.example", "name": "Bob", "role": "retailer", "retailer_id": ret["id"]})
		link := inv["invite_url"].(string)
		token := link[strings.LastIndex(link, "/")+1:]
		u := n.client()
		u.session()
		info := u.must(200, "GET", "/admin/api/invites/"+token, nil)
		if info["email"] != "bob@store.example" || info["retailer_name"] != "Blockable Store" {
			t.Fatalf("%v", info)
		}
		m := u.must(200, "POST", "/admin/api/invites/"+token+"/accept", map[string]any{"password": "bobs secure password"})
		u.csrf = m["csrf_token"].(string)
		if r := u.do("POST", "/admin/api/invites/"+token+"/accept", map[string]any{"password": "bobs secure password"}); r.Status != 400 {
			t.Fatalf("invite reused: %d", r.Status)
		}
		u.must(200, "GET", "/admin/api/portal/viewer", nil)
		if r := u.do("GET", "/admin/api/retailers", nil); r.Status != 403 {
			t.Fatalf("retailer on admin endpoint: %d", r.Status)
		}
		cl := admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": ret["id"], "name": "Store PIM"})
		api := n.client()
		api.bearer = n.token(cl["client"].(map[string]any)["client_id"].(string), cl["client_secret"].(string), "")
		api.must(200, "GET", "/obx/v1/grant", nil)
		admin.must(200, "PATCH", "/admin/api/retailers/"+ret["id"].(string), map[string]any{"status": "blocked"})
		if r := u.do("GET", "/admin/api/portal/viewer", nil); r.Status != 401 {
			t.Fatalf("blocked retailer session: %d", r.Status)
		}
		if r := api.do("GET", "/obx/v1/grant", nil); r.JSON(t)["obx_error"] != "access_revoked" {
			t.Fatalf("blocked retailer API: %s", r.Body)
		}
	})

	t.Run("secrets shown once and rotation", func(t *testing.T) {
		ret := admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Rotating Store"})
		cl := admin.must(201, "POST", "/admin/api/api-clients", map[string]any{"retailer_id": ret["id"], "name": "PIM"})
		id := cl["client"].(map[string]any)["client_id"].(string)
		old := cl["client_secret"].(string)
		list := admin.must(200, "GET", "/admin/api/api-clients?retailer_id="+ret["id"].(string), nil)
		if strings.Contains(string(mustJSON(list)), old) {
			t.Fatal("secret listed")
		}
		rot := admin.must(200, "POST", "/admin/api/api-clients/"+cl["client"].(map[string]any)["id"].(string)+"/rotate", nil)
		newSecret := rot["client_secret"].(string)
		n.token(id, newSecret, "")
		form := "grant_type=client_credentials"
		req, _ := http.NewRequest("POST", n.url+"/obx/v1/oauth/token", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(id, old)
		res, _ := http.DefaultClient.Do(req)
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatalf("old secret still works: %d", res.StatusCode)
		}
		tok := admin.must(201, "POST", "/admin/api/ingest-tokens", map[string]any{"name": "CI", "scopes": []string{"ingest:read"}})
		oldTok := tok["secret"].(string)
		rt := admin.must(200, "POST", "/admin/api/ingest-tokens/"+tok["token"].(map[string]any)["id"].(string)+"/rotate", nil)
		c := n.client()
		c.bearer = oldTok
		if r := c.do("GET", "/obx/v1/ingest", nil); r.Status != 401 {
			t.Fatalf("old ingest token: %d", r.Status)
		}
		c.bearer = rt["secret"].(string)
		c.must(200, "GET", "/obx/v1/ingest", nil)
	})

	t.Run("import source SSRF and write-only credentials", func(t *testing.T) {
		n.app.Policy = safehttp.Policy{AllowPrivate: false}
		defer func() { n.app.Policy = safehttp.Policy{AllowPrivate: true} }()
		r := admin.do("POST", "/admin/api/import-sources", map[string]any{"name": "Evil", "kind": "https_url",
			"config": map[string]any{"url": "https://127.0.0.1/products.csv"}})
		if r.Status != 422 || r.JSON(t)["obx_error"] != "unsafe_url" {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		r = admin.do("POST", "/admin/api/import-sources", map[string]any{"name": "Metadata", "kind": "https_url",
			"config": map[string]any{"url": "https://169.254.169.254/latest"}})
		if r.Status != 422 {
			t.Fatalf("metadata address: %d %s", r.Status, r.Body)
		}
		src := admin.must(201, "POST", "/admin/api/import-sources", map[string]any{"name": "Nightly bucket", "kind": "s3",
			"schedule": map[string]any{"frequency": "daily", "time": "02:30", "timezone": "Europe/Amsterdam"},
			"config":   map[string]any{"region": "eu-west-1", "bucket": "kestrel-exports", "prefix": "obx/"},
			"credentials": map[string]any{"access_key_id": "AKIAFAKE", "secret_access_key": "super-secret-value"}})
		validate(t, "ImportSource", src)
		body := string(mustJSON(src))
		if strings.Contains(body, "super-secret-value") || strings.Contains(body, "AKIAFAKE") {
			t.Fatal("credentials returned")
		}
		if src["credentials_set"].(map[string]any)["secret_access_key"] != true || src["next_run_at"] == nil {
			t.Fatalf("%v", src)
		}
	})
}

func TestPortal(t *testing.T) {
	k := newKestrelNode(t)
	ret := k.admin.must(201, "POST", "/admin/api/retailers", map[string]any{"name": "Portal Store", "brand_ids": []string{k.brands["kestrel"]}})
	inv := k.admin.must(201, "POST", "/admin/api/users/invites", map[string]any{"email": "pia@portal.example", "name": "Pia", "role": "retailer", "retailer_id": ret["id"], "language": "de"})
	link := inv["invite_url"].(string)
	u := k.client()
	u.session()
	m := u.must(200, "POST", "/admin/api/invites/"+link[strings.LastIndex(link, "/")+1:]+"/accept", map[string]any{"password": "pias secure password"})
	u.csrf = m["csrf_token"].(string)

	t.Run("viewer, list, facets, detail", func(t *testing.T) {
		v := u.must(200, "GET", "/admin/api/portal/viewer", nil)
		if v["preview"] != false || len(v["brands"].([]any)) != 1 {
			t.Fatalf("%v", v)
		}
		list := u.must(200, "GET", "/admin/api/portal/products?limit=100", nil)
		if list["total"].(float64) != 6 {
			t.Fatalf("restricted retailer sees %v products", list["total"])
		}
		for _, p := range list["items"].([]any) {
			if strings.HasPrefix(p.(map[string]any)["style_number"].(string), "KT-") {
				t.Fatal("kestrel-trail product visible to a kestrel-only retailer")
			}
		}
		f := u.must(200, "GET", "/admin/api/portal/products?item_type=jacket", nil)
		if f["total"].(float64) != 1 {
			t.Fatalf("item_type filter: %v", f["total"])
		}
		q := u.must(200, "GET", "/admin/api/portal/products?q=merino", nil)
		if q["total"].(float64) != 1 {
			t.Fatalf("search: %v", q["total"])
		}
		c := u.must(200, "GET", "/admin/api/portal/products?color=NVY", nil)
		if c["total"].(float64) < 1 {
			t.Fatalf("color filter: %v", c["total"])
		}
		facets := u.must(200, "GET", "/admin/api/portal/facets", nil)
		if len(facets["brands"].([]any)) != 1 || len(facets["item_types"].([]any)) == 0 {
			t.Fatalf("facets %v", facets)
		}
		detail := u.must(200, "GET", "/admin/api/portal/products/"+k.products["KO-1001"]["id"].(string), nil)
		validate(t, "ProductDetail", detail)
		if r := u.do("GET", "/admin/api/portal/products/"+k.products["KT-2001"]["id"].(string), nil); r.Status != 404 {
			t.Fatalf("other brand's product: %d", r.Status)
		}
	})

	t.Run("paste EANs", func(t *testing.T) {
		res := u.must(200, "POST", "/admin/api/portal/gtin-lookup", map[string]any{"text": "2000100101025\n2000200101031; 12345"})
		results := res["results"].([]any)
		if len(results) != 3 || results[0].(map[string]any)["status"] != "matched" || results[1].(map[string]any)["status"] != "unmatched" {
			t.Fatalf("%v", results)
		}
	})

	t.Run("selection, zips and CSV", func(t *testing.T) {
		ids := []string{k.products["KO-1001"]["id"].(string), k.products["KO-1005"]["id"].(string), k.products["KT-2001"]["id"].(string)}
		sel := u.must(200, "PUT", "/admin/api/portal/selection", map[string]any{"product_ids": ids})
		if len(sel["product_ids"].([]any)) != 2 {
			t.Fatalf("invisible id must be dropped: %v", sel)
		}
		r := u.do("GET", "/admin/api/portal/selection/download.zip?variant=web", nil)
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		z, err := zip.NewReader(bytes.NewReader(r.Body), int64(len(r.Body)))
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, f := range z.File {
			names[f.Name] = true
		}
		if !names["KO-1001/KO-1001_NVY_1_front_web.jpg"] || !names["KO-1005/KO-1005_COG_3_inside_web.jpg"] || len(names) != 7 {
			t.Fatalf("zip entries %v", names)
		}
		pz := u.do("GET", "/admin/api/portal/products/"+ids[0]+"/download.zip", nil)
		if pz.Status != 200 {
			t.Fatalf("product zip %d", pz.Status)
		}
		csvResp := u.do("GET", "/admin/api/portal/selection/data.csv?delimiter=semicolon", nil)
		if csvResp.Status != 200 || !bytes.HasPrefix(csvResp.Body, []byte("\xef\xbb\xbf")) {
			t.Fatalf("csv %d", csvResp.Status)
		}
		rd := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(csvResp.Body, []byte("\xef\xbb\xbf"))))
		rd.Comma = ';'
		rows, err := rd.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if rows[0][0] != "brand" || rows[0][len(rows[0])-1] != "image_files" || len(rows) != 1+8+1 {
			t.Fatalf("csv header %v, %d rows", rows[0], len(rows))
		}
		img := u.do("GET", "/admin/api/portal/assets/"+k.asset(k.all, "KO-1001_NVY_1")["id"].(string)+"/download?variant=original", nil)
		if img.Status != 200 || !strings.Contains(img.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("image download %d", img.Status)
		}
	})

	t.Run("background download job", func(t *testing.T) {
		job := u.must(202, "POST", "/admin/api/portal/download-jobs", map[string]any{"source": "selection", "variant": "square"})
		k.app.RunJobsNow(t.Context())
		st := u.must(200, "GET", "/admin/api/portal/download-jobs/"+job["id"].(string), nil)
		if st["status"] != "succeeded" || st["file_url"] == nil {
			t.Fatalf("%v", st)
		}
		f := u.do("GET", st["file_url"].(string), nil)
		z, err := zip.NewReader(bytes.NewReader(f.Body), int64(len(f.Body)))
		if err != nil || len(z.File) != 8 { // 7 images + data.csv
			t.Fatalf("download zip: %v, %d files", err, len(z.File))
		}
		other := k.client()
		other.login(adminEmail, adminPassword)
		if r := other.do("GET", "/admin/api/portal/download-jobs/"+job["id"].(string), nil); r.Status != 404 {
			t.Fatalf("someone else's job: %d", r.Status)
		}
	})

	t.Run("statistics count retailer downloads, not admin previews", func(t *testing.T) {
		stats := k.admin.must(200, "GET", "/admin/api/stats?days=7", nil)
		totals := stats["totals"].(map[string]any)
		if totals["zips"].(float64) < 2 || totals["csv_exports"].(float64) != 1 {
			t.Fatalf("totals %v", totals)
		}
		before := totals["downloads"].(float64)
		k.admin.must(200, "GET", "/admin/api/portal/products", nil)
		r := k.admin.do("GET", "/admin/api/portal/products/"+k.products["KT-2001"]["id"].(string)+"/download.zip", nil)
		if r.Status != 200 {
			t.Fatalf("admin preview zip %d", r.Status)
		}
		after := k.admin.must(200, "GET", "/admin/api/stats?days=7", nil)["totals"].(map[string]any)["downloads"].(float64)
		if after != before {
			t.Fatalf("admin preview was logged: %v → %v", before, after)
		}
	})
}

func TestCSVFormulaEscaping(t *testing.T) {
	for in, want := range map[string]string{"=SUM(A1)": "'=SUM(A1)", "+31": "+31", "-5": "-5", "@cmd": "'@cmd", "2000100101025": "2000100101025", "Navy": "Navy", "-x": "'-x"} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
